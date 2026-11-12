package providers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/genai"
)

// QuestionKind is one of the three closed-set question shapes a decision
// model answers.
type QuestionKind string

const (
	// QuestionYesNo is answered with the probability that the answer is yes.
	QuestionYesNo QuestionKind = "yes_no"
	// QuestionChoice picks one option out of a listed set.
	QuestionChoice QuestionKind = "choice"
	// QuestionScore rates on an ordered scale of levels.
	QuestionScore QuestionKind = "score"
)

// Question is one closed-set question. Instructions is the whole question:
// the model never sees the key the question is filed under.
type Question struct {
	Kind         QuestionKind
	Instructions string
	// Yes and No optionally say what counts as each answer (yes/no only).
	Yes, No string
	// Options maps each choice to a description of when it applies (choice
	// only). An empty description is allowed when the key explains itself.
	Options map[string]string
	// Levels is the scale, lowest first (score only).
	Levels []string
}

// DecisionRequest is one call: every question is asked about the same
// input, independently of the others.
type DecisionRequest struct {
	// Input is what the questions are asked about — text parts and media
	// parts (InlineData or FileData).
	Input     []*genai.Part
	Questions map[string]Question
}

// Validate checks the request's shape against what every decision model
// accepts. Provider-specific limits (question count, key format) are the
// provider's to enforce.
func (r DecisionRequest) Validate() error {
	if len(r.Input) == 0 {
		return errors.New("decision: input is empty")
	}
	if len(r.Questions) == 0 {
		return errors.New("decision: no questions")
	}
	for key, q := range r.Questions {
		if strings.TrimSpace(key) == "" {
			return errors.New("decision: a question has an empty key")
		}
		if strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("decision: question %q has no instructions", key)
		}
		switch q.Kind {
		case QuestionYesNo:
		case QuestionChoice:
			if len(q.Options) < 2 {
				return fmt.Errorf("decision: choice question %q needs at least 2 options", key)
			}
			for opt := range q.Options {
				if strings.TrimSpace(opt) == "" {
					return fmt.Errorf("decision: choice question %q has an empty option", key)
				}
			}
		case QuestionScore:
			if len(q.Levels) < 2 {
				return fmt.Errorf("decision: score question %q needs at least 2 levels", key)
			}
		default:
			return fmt.Errorf("decision: question %q has unknown type %q (want yes_no, choice, or score)", key, q.Kind)
		}
	}
	return nil
}

// Answer is a decision model's answer to one question.
type Answer struct {
	Kind QuestionKind
	// Confidence is the probability of the most likely answer.
	Confidence float64
	// Yes is the probability that a yes/no answer is yes.
	Yes float64
	// Choice is the most probable option of a choice question.
	Choice string
	// Probabilities is a choice question's probability per option.
	Probabilities map[string]float64
	// Score is a score question's expected level: the level indexes (0 is
	// the lowest) weighted by their probabilities.
	Score float64
	// Levels is a score question's probability per level, lowest first.
	Levels []float64
}

// Level is a score answer's most probable level index.
func (a Answer) Level() int {
	best := 0
	for i, p := range a.Levels {
		if p > a.Levels[best] {
			best = i
		}
	}
	return best
}

// RankedOptions lists a choice answer's options, most probable first.
func (a Answer) RankedOptions() []string {
	out := make([]string, 0, len(a.Probabilities))
	for k := range a.Probabilities {
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := a.Probabilities[out[i]], a.Probabilities[out[j]]
		if pi != pj {
			return pi > pj
		}
		return out[i] < out[j]
	})
	return out
}

// DecisionResult is one call's answers, keyed like the request's
// questions, and what the call cost.
type DecisionResult struct {
	Answers map[string]Answer
	Usage   Usage
}

// DecisionBlockedError is a request the provider refused to answer (a
// safety filter).
type DecisionBlockedError struct {
	Reason  string
	Message string
}

func (e *DecisionBlockedError) Error() string {
	if e.Message != "" {
		return "decision blocked (" + e.Reason + "): " + e.Message
	}
	return "decision blocked: " + e.Reason
}

// Decider answers closed-set questions about one input with probabilities.
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error)
}

// DecisionModel is one model a decision provider serves.
type DecisionModel struct {
	ID          string
	DisplayName string
	// InputUSDPerMTok prices input; decision models write no output. Zero
	// means the price is unknown.
	InputUSDPerMTok float64
	// ContextWindow is the input plus questions budget in tokens.
	ContextWindow int64
	// MaxQuestions bounds the questions in one request.
	MaxQuestions int
}

// DecisionProvider is a backend that serves decision models. It shares
// Provider's identity and settings shape, and its configuration lives in
// the same Config.Providers block, so a provider that serves both chat and
// decision models (Vertex, once Google ships decisions there) is one id and
// one set of credentials registered in both registries.
type DecisionProvider interface {
	ID() string
	DisplayName() string
	Settings() []SettingField
	Configured(pc config.ProviderConfig) bool
	// DecisionModels is the provider's catalog; the first is its default.
	DecisionModels() []DecisionModel
	// BuildDecider constructs a Decider for modelID from pc.
	BuildDecider(ctx context.Context, pc config.ProviderConfig, modelID string) (Decider, error)
}

var (
	decisionRegistryMu sync.RWMutex
	decisionRegistry   []DecisionProvider
)

var builtinDecisionProviders = []DecisionProvider{GeminiAPI{}}

func init() {
	for _, p := range builtinDecisionProviders {
		RegisterDecisionProvider(p)
	}
}

// RegisterDecisionProvider adds p to the decision registry, replacing an
// earlier registration with the same id in place. A malformed provider
// panics, as Register does.
func RegisterDecisionProvider(p DecisionProvider) {
	if p == nil {
		panic("providers: RegisterDecisionProvider(nil)")
	}
	if err := validateSettings(p.ID(), p.Settings()); err != nil {
		panic(err)
	}
	if len(p.DecisionModels()) == 0 {
		panic(fmt.Sprintf("providers: decision provider %s lists no models", p.ID()))
	}
	decisionRegistryMu.Lock()
	defer decisionRegistryMu.Unlock()
	for i, q := range decisionRegistry {
		if q.ID() == p.ID() {
			decisionRegistry[i] = p
			return
		}
	}
	decisionRegistry = append(decisionRegistry, p)
}

// GetDecisionProvider returns the registered decision provider with id.
func GetDecisionProvider(id string) (DecisionProvider, bool) {
	decisionRegistryMu.RLock()
	defer decisionRegistryMu.RUnlock()
	for _, p := range decisionRegistry {
		if p.ID() == id {
			return p, true
		}
	}
	return nil, false
}

// DecisionProviders returns the registered decision providers in
// registration order.
func DecisionProviders() []DecisionProvider {
	decisionRegistryMu.RLock()
	defer decisionRegistryMu.RUnlock()
	return append([]DecisionProvider(nil), decisionRegistry...)
}

// DecisionTarget is the provider and model a decision call runs on.
type DecisionTarget struct {
	Provider DecisionProvider
	Model    DecisionModel
}

// Label is "<provider id>/<model id>".
func (t DecisionTarget) Label() string {
	if t.Provider == nil {
		return ""
	}
	return t.Provider.ID() + "/" + t.Model.ID
}

// DecisionModelFor returns p's catalog entry for modelID; an id the
// catalog does not list is still usable, with unknown price and limits.
func DecisionModelFor(p DecisionProvider, modelID string) DecisionModel {
	models := p.DecisionModels()
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return models[0]
	}
	for _, m := range models {
		if m.ID == modelID {
			return m
		}
	}
	return DecisionModel{ID: modelID, DisplayName: modelID}
}

// ResolveDecisionTarget picks the decision model cfg selects: the
// configured provider and model, else the first registered decision
// provider that has its credentials, on its default model. It reports false
// when no decision provider can run.
func ResolveDecisionTarget(cfg config.Config) (DecisionTarget, bool) {
	if id := strings.TrimSpace(cfg.Decisions.Provider); id != "" {
		p, ok := GetDecisionProvider(id)
		if !ok || !p.Configured(cfg.ProviderConfig(id)) {
			return DecisionTarget{}, false
		}
		return DecisionTarget{Provider: p, Model: DecisionModelFor(p, cfg.Decisions.Model)}, true
	}
	for _, p := range DecisionProviders() {
		if p.Configured(cfg.ProviderConfig(p.ID())) {
			return DecisionTarget{Provider: p, Model: DecisionModelFor(p, "")}, true
		}
	}
	return DecisionTarget{}, false
}

// DecisionUsage prices a decision call that read inputTokens on target.
// Decision models write no output.
func DecisionUsage(target DecisionTarget, inputTokens int) Usage {
	u := Usage{Model: target.Model.ID, InputTokens: inputTokens}
	if target.Provider != nil {
		u.Provider = target.Provider.ID()
	}
	if target.Model.InputUSDPerMTok > 0 {
		u.CostUSD = float64(inputTokens) * target.Model.InputUSDPerMTok / 1e6
		u.CostSource = CostPriced
	}
	return u
}
