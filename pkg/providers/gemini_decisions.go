package providers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/genai"
	"google.golang.org/genai/interactions/models/decisions"
	"google.golang.org/genai/interactions/models/interactions"
	"google.golang.org/genai/interactions/models/operations"
)

const (
	GeminiAPIProviderID = "gemini"
	GeminiEnvAPIKey     = "GEMINI_API_KEY"
	GeminiFieldAPIKey   = "apiKey"

	GeminiDecision1 = "gemini-decision-1"

	// geminiDecisionsAPIVersion is the only version that serves the
	// Decisions API; the SDK's Gemini API default is v1.
	geminiDecisionsAPIVersion = "v1beta"
	geminiMaxChoiceOptions    = 255
	geminiMaxScoreLevels      = 255
)

// GeminiAPI is the Gemini Developer API (Google AI Studio), authenticated
// with an API key. It serves Google's decision models.
type GeminiAPI struct{}

var geminiSettings = []SettingField{{
	Key:    GeminiFieldAPIKey,
	Title:  "API key",
	Hint:   "Gemini API key from aistudio.google.com/apikey — on the free tier Google may use requests to improve its products",
	Secret: true,
	EnvKey: GeminiEnvAPIKey,
}}

var geminiDecisionModels = []DecisionModel{{
	ID:              GeminiDecision1,
	DisplayName:     "Gemini Decision 1",
	InputUSDPerMTok: 0.10,
	ContextWindow:   262_144,
	MaxQuestions:    1000,
}}

func (GeminiAPI) ID() string          { return GeminiAPIProviderID }
func (GeminiAPI) DisplayName() string { return "Gemini API" }
func (GeminiAPI) Settings() []SettingField {
	return append([]SettingField(nil), geminiSettings...)
}

func (GeminiAPI) Configured(pc config.ProviderConfig) bool {
	return SettingValue(pc, geminiSettings[0]) != ""
}

func (GeminiAPI) DecisionModels() []DecisionModel {
	return append([]DecisionModel(nil), geminiDecisionModels...)
}

// GeminiBaseURL overrides the Gemini API endpoint; empty is the SDK's
// default. Tests point it at an httptest server.
var GeminiBaseURL = ""

func (g GeminiAPI) BuildDecider(ctx context.Context, pc config.ProviderConfig, modelID string) (Decider, error) {
	key := SettingValue(pc, geminiSettings[0])
	if key == "" {
		return nil, MissingAPIKeyError(GeminiEnvAPIKey, "/config → Decisions")
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  key,
		Backend: genai.BackendGeminiAPI,
		HTTPOptions: genai.HTTPOptions{
			APIVersion: geminiDecisionsAPIVersion,
			BaseURL:    GeminiBaseURL,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	return &geminiDecider{client: client, target: DecisionTarget{Provider: g, Model: DecisionModelFor(g, modelID)}}, nil
}

var _ DecisionProvider = GeminiAPI{}

type geminiDecider struct {
	client *genai.Client
	target DecisionTarget
}

var geminiQuestionKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (d *geminiDecider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if err := req.Validate(); err != nil {
		return DecisionResult{}, err
	}
	if err := geminiCheckLimits(d.target.Model, req); err != nil {
		return DecisionResult{}, err
	}
	body, err := geminiDecisionBody(d.target.Model.ID, req)
	if err != nil {
		return DecisionResult{}, err
	}
	resp, err := d.client.Decisions.Create(ctx, operations.CreateDecisionRequest{Body: body})
	if err != nil {
		return DecisionResult{}, fmt.Errorf("gemini decisions: %w", err)
	}
	if resp == nil || resp.CreateDecisionResponse == nil {
		return DecisionResult{}, errors.New("gemini decisions: empty response")
	}
	return geminiDecisionResult(d.target, req, resp.CreateDecisionResponse)
}

func geminiCheckLimits(m DecisionModel, req DecisionRequest) error {
	if m.MaxQuestions > 0 && len(req.Questions) > m.MaxQuestions {
		return fmt.Errorf("gemini decisions: %d questions, at most %d per request", len(req.Questions), m.MaxQuestions)
	}
	for key, q := range req.Questions {
		if !geminiQuestionKey.MatchString(key) {
			return fmt.Errorf("gemini decisions: question key %q must be 1-64 letters, digits, '_' or '-'", key)
		}
		if len(q.Options) > geminiMaxChoiceOptions {
			return fmt.Errorf("gemini decisions: question %q has %d options, at most %d", key, len(q.Options), geminiMaxChoiceOptions)
		}
		if len(q.Levels) > geminiMaxScoreLevels {
			return fmt.Errorf("gemini decisions: question %q has %d levels, at most %d", key, len(q.Levels), geminiMaxScoreLevels)
		}
	}
	return nil
}

func geminiDecisionBody(modelID string, req DecisionRequest) (decisions.CreateDecisionRequest, error) {
	input, err := geminiDecisionInput(req.Input)
	if err != nil {
		return decisions.CreateDecisionRequest{}, err
	}
	questions := make(map[string]decisions.Question, len(req.Questions))
	for key, q := range req.Questions {
		questions[key] = geminiQuestion(q)
	}
	return decisions.CreateDecisionRequest{
		Model:     modelID,
		Input:     decisions.NewInput(input),
		Questions: questions,
	}, nil
}

func geminiQuestion(q Question) decisions.Question {
	switch q.Kind {
	case QuestionChoice:
		return decisions.NewQuestion(decisions.ChoiceQuestion{
			Instructions: decisions.NewChoiceQuestionInstructions(q.Instructions),
			Criteria:     q.Options,
		})
	case QuestionScore:
		return decisions.NewQuestion(decisions.ScoreQuestion{
			Instructions: decisions.NewScoreQuestionInstructions(q.Instructions),
			Criteria:     q.Levels,
		})
	}
	nq := decisions.NoulQuestion{Instructions: decisions.NewNoulQuestionInstructions(q.Instructions)}
	if q.Yes != "" || q.No != "" {
		c := &decisions.NoulCriteria{}
		if q.Yes != "" {
			c.TrueDescription = &q.Yes
		}
		if q.No != "" {
			c.FalseDescription = &q.No
		}
		nq.Criteria = c
	}
	return decisions.NewQuestion(nq)
}

// geminiDecisionInput converts genai parts to the Decisions API's content
// blocks. The API joins adjacent text blocks with no separator, so
// consecutive text parts are merged with a blank line between them.
func geminiDecisionInput(parts []*genai.Part) ([]decisions.DecisionInputContent, error) {
	var out []decisions.DecisionInputContent
	var text []string
	flush := func() {
		if len(text) == 0 {
			return
		}
		out = append(out, decisions.NewDecisionInputContent(interactions.TextContent{Text: strings.Join(text, "\n\n")}))
		text = nil
	}
	for _, p := range parts {
		if p == nil {
			continue
		}
		switch {
		case p.Text != "":
			text = append(text, p.Text)
		case p.InlineData != nil:
			flush()
			data := base64.StdEncoding.EncodeToString(p.InlineData.Data)
			c, err := geminiMediaContent(p.InlineData.MIMEType, &data, nil)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		case p.FileData != nil:
			flush()
			uri := p.FileData.FileURI
			c, err := geminiMediaContent(p.FileData.MIMEType, nil, &uri)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		default:
			return nil, errors.New("gemini decisions: input parts must be text, inline data, or file data")
		}
	}
	flush()
	if len(out) == 0 {
		return nil, errors.New("decision: input is empty")
	}
	return out, nil
}

func geminiMediaContent(mime string, data, uri *string) (decisions.DecisionInputContent, error) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(mime, "image/"):
		t := interactions.ImageContentMimeType(mime)
		return decisions.NewDecisionInputContent(interactions.ImageContent{Data: data, URI: uri, MimeType: &t}), nil
	case strings.HasPrefix(mime, "audio/"):
		t := interactions.AudioContentMimeType(mime)
		return decisions.NewDecisionInputContent(interactions.AudioContent{Data: data, URI: uri, MimeType: &t}), nil
	case strings.HasPrefix(mime, "video/"):
		t := interactions.VideoContentMimeType(mime)
		return decisions.NewDecisionInputContent(interactions.VideoContent{Data: data, URI: uri, MimeType: &t}), nil
	case mime != "":
		t := interactions.DocumentContentMimeType(mime)
		return decisions.NewDecisionInputContent(interactions.DocumentContent{Data: data, URI: uri, MimeType: &t}), nil
	}
	return decisions.DecisionInputContent{}, errors.New("gemini decisions: media input needs a MIME type")
}

func geminiDecisionResult(target DecisionTarget, req DecisionRequest, resp *decisions.CreateDecisionResponse) (DecisionResult, error) {
	inputTokens := 0
	if resp.Usage != nil && resp.Usage.TotalInputTokens != nil {
		inputTokens = *resp.Usage.TotalInputTokens
	}
	result := DecisionResult{Answers: make(map[string]Answer, len(resp.Answers)), Usage: DecisionUsage(target, inputTokens)}
	if fb := resp.PromptFeedback; fb != nil && fb.BlockReason != nil {
		blocked := &DecisionBlockedError{Reason: string(*fb.BlockReason)}
		if fb.BlockReasonMessage != nil {
			blocked.Message = *fb.BlockReasonMessage
		}
		return result, blocked
	}
	for key, q := range req.Questions {
		raw, ok := resp.Answers[key]
		if !ok {
			return result, fmt.Errorf("gemini decisions: no answer for %q", key)
		}
		a, err := geminiAnswer(q, raw)
		if err != nil {
			return result, fmt.Errorf("gemini decisions: answer for %q: %w", key, err)
		}
		result.Answers[key] = a
	}
	return result, nil
}

func geminiAnswer(q Question, a decisions.Answer) (Answer, error) {
	switch {
	case a.NoulAnswer != nil:
		return Answer{Kind: QuestionYesNo, Yes: a.NoulAnswer.Noul, Confidence: a.NoulAnswer.Confidence}, nil
	case a.ChoiceAnswer != nil:
		return Answer{
			Kind:          QuestionChoice,
			Choice:        a.ChoiceAnswer.Choice,
			Probabilities: a.ChoiceAnswer.Probabilities,
			Confidence:    a.ChoiceAnswer.Confidence,
		}, nil
	case a.ScoreAnswer != nil:
		levels := make([]float64, len(q.Levels))
		for k, p := range a.ScoreAnswer.Probabilities {
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(levels) {
				return Answer{}, fmt.Errorf("unexpected level %q", k)
			}
			levels[i] = p
		}
		return Answer{Kind: QuestionScore, Score: a.ScoreAnswer.Score, Levels: levels, Confidence: a.ScoreAnswer.Confidence}, nil
	}
	return Answer{}, fmt.Errorf("unsupported answer type")
}
