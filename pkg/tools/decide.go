package tools

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/genai"
)

const (
	// DecideMaxItems bounds items mode: each item is its own request.
	DecideMaxItems = 100
	// DecideMaxFileBytes bounds one file added to the input.
	DecideMaxFileBytes  = 50 << 20
	decideConcurrency   = 8
	decideMaxShownProbs = 5
	decideItemPreview   = 80

	DecideToolDescription = `Answer closed-set questions about text or files with calibrated probabilities from a fast decision model: no generated text, about 200 ms and a fraction of a cent per call. A question is yes_no (the probability of yes), choice (one of 2-255 options), or score (2-255 ordered levels, lowest first); one call takes up to 1000 questions, each answered independently of the others. Use it to triage, filter, route, rank, or check many things at once — search hits, files, issues, log lines, screenshots, PDFs — instead of reading each yourself, and to settle a judgment you would otherwise guess at. input is the shared text; files adds images, audio, video, PDFs, or text files; items asks every question of each item on its own (up to 100), after the shared input. The model sees a question's text, never its key, so write the whole question there and keep each to one thing. Not for open-ended answers or extracting values that are not in a list.`

	// DecideNoModelNotice is the non-error reply when no decision model is
	// configured.
	DecideNoModelNotice = "No decision model is configured, so decide is unavailable. Finish the task without it, and tell the user they can add a Gemini API key under /config → Decisions to enable it."
)

// DecideParams is the decide tool's input.
type DecideParams struct {
	Input       string           `json:"input,omitempty" jsonschema:"text every question is asked about; with items, the context shared by every item"`
	Files       []string         `json:"files,omitempty" jsonschema:"paths of images, audio, video, PDFs, or text files to add to the input"`
	Items       []string         `json:"items,omitempty" jsonschema:"separate texts to classify: every question is asked of each item on its own (up to 100)"`
	Questions   []DecideQuestion `json:"questions" jsonschema:"1 to 1000 questions, each answered independently"`
	Description string           `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// DecideQuestion is one question of a decide call.
type DecideQuestion struct {
	Key      string         `json:"key" jsonschema:"label the answer comes back under: letters, digits, _ or -, up to 64 characters"`
	Type     string         `json:"type" jsonschema:"yes_no, choice, or score"`
	Question string         `json:"question" jsonschema:"the complete question — the model sees this, never the key"`
	Yes      string         `json:"yes,omitempty" jsonschema:"yes_no only: what counts as yes"`
	No       string         `json:"no,omitempty" jsonschema:"yes_no only: what counts as no"`
	Options  []DecideOption `json:"options,omitempty" jsonschema:"choice only: 2 to 255 options; add an 'other' option when an input may fit none"`
	Levels   []string       `json:"levels,omitempty" jsonschema:"score only: 2 to 255 evenly spaced levels, lowest first"`
}

// DecideOption is one option of a choice question.
type DecideOption struct {
	Key  string `json:"key" jsonschema:"the value returned when this option wins"`
	When string `json:"when,omitempty" jsonschema:"when this option applies; may be empty when the key explains itself"`
}

// DecideResult is the decide tool's response.
type DecideResult struct {
	Answers string `json:"answers,omitempty" jsonschema:"one line per question: yes_no as yes/no with p(yes), choice as options by probability, score as the expected level and the most likely one"`
	Model   string `json:"model,omitempty"`
	Notice  string `json:"notice,omitempty" jsonschema:"set when no decision model is configured"`
}

// DecideTool returns the decide tool: closed-set questions answered by the
// configured decision model.
func DecideTool(env *ToolEnv) Tool {
	return NewTypedTool("decide", DecideToolDescription, func(ctx agent.Context, p DecideParams) (DecideResult, error) {
		questions, order, err := decideQuestions(p.Questions)
		if err != nil {
			return DecideResult{}, err
		}
		if len(p.Items) > DecideMaxItems {
			return DecideResult{}, fmt.Errorf("at most %d items per call, got %d", DecideMaxItems, len(p.Items))
		}
		shared, err := decideSharedInput(env, p)
		if err != nil {
			return DecideResult{}, err
		}
		if len(shared) == 0 && len(p.Items) == 0 {
			return DecideResult{}, errors.New("input, files, or items is required")
		}
		cfg, _ := config.Load()
		d, target, err := engine.DeciderBuilder(ctx, cfg)
		if errors.Is(err, engine.ErrNoDecisionModel) {
			return DecideResult{Notice: DecideNoModelNotice}, nil
		}
		if err != nil {
			return DecideResult{}, err
		}
		ask := func(input []*genai.Part) (providers.DecisionResult, error) {
			res, err := d.Decide(ctx, providers.DecisionRequest{Input: input, Questions: questions})
			env.RecordSpend(SpendDecision, res.Usage)
			return res, err
		}
		if len(p.Items) == 0 {
			res, err := ask(shared)
			if err != nil {
				return DecideResult{}, err
			}
			return DecideResult{Answers: formatDecisionAnswers(order, questions, res.Answers, ""), Model: target.Label()}, nil
		}
		return decideItems(ctx, p.Items, shared, order, questions, ask, target.Label())
	})
}

func decideItems(ctx context.Context, items []string, shared []*genai.Part, order []string, questions map[string]providers.Question,
	ask func([]*genai.Part) (providers.DecisionResult, error), model string) (DecideResult, error) {
	answers := make([]map[string]providers.Answer, len(items))
	errs := make([]error, len(items))
	sem := make(chan struct{}, decideConcurrency)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			input := append(append([]*genai.Part(nil), shared...), genai.NewPartFromText(item))
			res, err := ask(input)
			answers[i], errs[i] = res.Answers, err
		}()
	}
	wg.Wait()

	var b strings.Builder
	failed := 0
	for i, item := range items {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, decideItemLabel(item))
		if errs[i] != nil {
			failed++
			fmt.Fprintf(&b, "  error: %v\n", errs[i])
			continue
		}
		b.WriteString(formatDecisionAnswers(order, questions, answers[i], "  "))
		b.WriteByte('\n')
	}
	if failed == len(items) {
		return DecideResult{}, fmt.Errorf("every item failed: %w", errs[0])
	}
	return DecideResult{Answers: strings.TrimRight(b.String(), "\n"), Model: model}, nil
}

func decideItemLabel(item string) string {
	line := strings.Join(strings.Fields(item), " ")
	if r := []rune(line); len(r) > decideItemPreview {
		return string(r[:decideItemPreview]) + "…"
	}
	return line
}

// decideQuestions converts the tool's questions into a decision request's,
// keeping their order for the reply.
func decideQuestions(in []DecideQuestion) (map[string]providers.Question, []string, error) {
	if len(in) == 0 {
		return nil, nil, errors.New("at least one question is required")
	}
	out := make(map[string]providers.Question, len(in))
	order := make([]string, 0, len(in))
	for _, q := range in {
		key := strings.TrimSpace(q.Key)
		if key == "" {
			return nil, nil, errors.New("every question needs a key")
		}
		if _, dup := out[key]; dup {
			return nil, nil, fmt.Errorf("question key %q is used twice", key)
		}
		pq := providers.Question{Instructions: strings.TrimSpace(q.Question)}
		switch strings.ToLower(strings.TrimSpace(q.Type)) {
		case "yes_no", "yesno", "yes/no", "bool", "boolean":
			pq.Kind, pq.Yes, pq.No = providers.QuestionYesNo, strings.TrimSpace(q.Yes), strings.TrimSpace(q.No)
		case "choice":
			pq.Kind = providers.QuestionChoice
			pq.Options = make(map[string]string, len(q.Options))
			for _, o := range q.Options {
				k := strings.TrimSpace(o.Key)
				if _, dup := pq.Options[k]; dup {
					return nil, nil, fmt.Errorf("question %q lists option %q twice", key, k)
				}
				pq.Options[k] = strings.TrimSpace(o.When)
			}
		case "score":
			pq.Kind, pq.Levels = providers.QuestionScore, q.Levels
		default:
			return nil, nil, fmt.Errorf("question %q: type must be yes_no, choice, or score, got %q", key, q.Type)
		}
		out[key] = pq
		order = append(order, key)
	}
	return out, order, nil
}

// decideSharedInput is the input every request carries: the text, then
// each file.
func decideSharedInput(env *ToolEnv, p DecideParams) ([]*genai.Part, error) {
	var parts []*genai.Part
	if text := strings.TrimSpace(p.Input); text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	for _, f := range p.Files {
		part, err := decideFilePart(env.AbsPath(f))
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func decideFilePart(path string) (*genai.Part, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > DecideMaxFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, larger than the %d-byte limit", path, info.Size(), DecideMaxFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, _ = strings.Cut(mimeType, ";")
	mimeType = strings.TrimSpace(mimeType)
	if strings.HasPrefix(mimeType, "text/") || mimeType == "application/json" || mimeType == "application/xml" {
		return genai.NewPartFromText("File " + path + ":\n" + string(data)), nil
	}
	return genai.NewPartFromBytes(data, mimeType), nil
}

// formatDecisionAnswers renders one line per question, in the order asked.
func formatDecisionAnswers(order []string, questions map[string]providers.Question, answers map[string]providers.Answer, indent string) string {
	lines := make([]string, 0, len(order))
	for _, key := range order {
		a, ok := answers[key]
		if !ok {
			lines = append(lines, indent+key+": (no answer)")
			continue
		}
		lines = append(lines, indent+key+": "+formatDecisionAnswer(questions[key], a))
	}
	return strings.Join(lines, "\n")
}

func formatDecisionAnswer(q providers.Question, a providers.Answer) string {
	switch a.Kind {
	case providers.QuestionYesNo:
		verdict := "no"
		if a.Yes >= 0.5 {
			verdict = "yes"
		}
		return fmt.Sprintf("%s — p(yes) %.2f", verdict, a.Yes)
	case providers.QuestionChoice:
		ranked := a.RankedOptions()
		shown := ranked
		if len(shown) > decideMaxShownProbs {
			shown = shown[:decideMaxShownProbs]
		}
		parts := make([]string, 0, len(shown)+1)
		for _, opt := range shown {
			parts = append(parts, fmt.Sprintf("%s %.2f", opt, a.Probabilities[opt]))
		}
		if len(ranked) > len(shown) {
			parts = append(parts, fmt.Sprintf("+%d more", len(ranked)-len(shown)))
		}
		return strings.Join(parts, " · ")
	case providers.QuestionScore:
		level := a.Level()
		label := ""
		if level < len(q.Levels) {
			label = fmt.Sprintf(" %q", q.Levels[level])
		}
		p := 0.0
		if level < len(a.Levels) {
			p = a.Levels[level]
		}
		return fmt.Sprintf("%.2f on 0-%d · most likely %d%s (%.2f)", a.Score, len(q.Levels)-1, level, label, p)
	}
	return "(unknown answer)"
}
