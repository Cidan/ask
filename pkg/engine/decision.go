package engine

import (
	"context"
	"errors"
	"strconv"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	"google.golang.org/genai"
)

// ErrNoDecisionModel means no decision provider has its credentials.
var ErrNoDecisionModel = errors.New("no decision model is configured — add one under /config → Decisions")

// DeciderBuilder builds the decision model cfg selects. It is the seam for
// every decision call, as ModelBuilder is for LLMs.
var DeciderBuilder = func(ctx context.Context, cfg config.Config) (providers.Decider, providers.DecisionTarget, error) {
	target, ok := providers.ResolveDecisionTarget(cfg)
	if !ok {
		return nil, providers.DecisionTarget{}, ErrNoDecisionModel
	}
	d, err := target.Provider.BuildDecider(ctx, cfg.ProviderConfig(target.Provider.ID()), target.Model.ID)
	if err != nil {
		return nil, target, err
	}
	return d, target, nil
}

// DecisionsAvailable reports whether the saved config has a decision model
// that can run — what decides whether a session's prompt steers it toward
// decide. A session's prompt is built once, so a key added mid-session
// steers the next session.
func DecisionsAvailable() bool {
	cfg, _ := config.Load()
	_, ok := providers.ResolveDecisionTarget(cfg)
	return ok
}

const (
	memoryGateKey = "durable"
	// memoryGateSkipBelow is the probability of "durable" under which a
	// turn's extraction is skipped. It is low on purpose: a lost fact costs
	// more than an extraction call.
	memoryGateSkipBelow = 0.2
)

var memoryGateQuestion = providers.Question{
	Kind: providers.QuestionYesNo,
	Instructions: "Does this exchange reveal a durable fact worth remembering in future sessions: the user's role, expertise, or preferences; " +
		"how they want work done; a project goal, decision, or constraint the code does not record; or where something lives in an external system?",
	Yes: "The user states or confirms a preference, a decision, a constraint, a correction of how to work, or a fact about themselves or an external system.",
	No:  "Only task progress, code, questions, or facts the repository already records.",
}

// memoryWorthExtracting asks the decision model whether turn holds anything
// durable. It reports true — extract — when the gate is off, no decision
// model can run, or the call fails: the gate only ever saves a call.
func memoryWorthExtracting(ctx context.Context, cfg config.Config, turn MemoryTurn) bool {
	if !config.MemoryExtractGateEnabled(cfg) {
		return true
	}
	d, target, err := DeciderBuilder(ctx, cfg)
	if err != nil {
		if !errors.Is(err, ErrNoDecisionModel) {
			DebugLog("memory gate: %v", err)
		}
		return true
	}
	input := "User:\n" + clipHead(turn.Prompt, memoryExtractPromptCap) + "\n\nAssistant:\n" + clipTail(turn.Response, memoryExtractResponseCap)
	res, err := d.Decide(ctx, providers.DecisionRequest{
		Input:     []*genai.Part{genai.NewPartFromText(input)},
		Questions: map[string]providers.Question{memoryGateKey: memoryGateQuestion},
	})
	reportDecisionUsage(turn.OnUsage, res.Usage)
	if err != nil {
		DebugLog("memory gate %s: %v", target.Label(), err)
		return true
	}
	yes := res.Answers[memoryGateKey].Yes
	DebugLog("memory gate %s: p(durable)=%.2f", target.Label(), yes)
	return yes >= memoryGateSkipBelow
}

const (
	// memoryRecallKeepAbove is the probability of relevance a recalled
	// concept needs to stay in the turn's memory block.
	memoryRecallKeepAbove = 0.3
	memoryRecallBodyCap   = 400
)

const memoryRecallInstruction = "Would this remembered fact help with the request above, or change how it should be carried out?\n\nRemembered fact: "

// decisionRecallFilter is the memory.RelevanceFilter that asks the decision
// model, one yes/no question per recalled concept, whether it bears on the
// turn's prompt.
type decisionRecallFilter struct {
	decider providers.Decider
	onUsage func(providers.Usage)
}

func (f decisionRecallFilter) Relevant(ctx context.Context, prompt string, concepts []memory.Concept) ([]memory.Concept, error) {
	if len(concepts) == 0 {
		return concepts, nil
	}
	questions := make(map[string]providers.Question, len(concepts))
	for _, c := range concepts {
		fact := c.Title
		if c.Body != "" && c.Body != c.Title {
			fact += "\n" + clipHead(c.Body, memoryRecallBodyCap)
		}
		questions[recallQuestionKey(c.ID)] = providers.Question{Kind: providers.QuestionYesNo, Instructions: memoryRecallInstruction + fact}
	}
	res, err := f.decider.Decide(ctx, providers.DecisionRequest{
		Input:     []*genai.Part{genai.NewPartFromText("Request:\n" + clipHead(prompt, memoryExtractPromptCap))},
		Questions: questions,
	})
	reportDecisionUsage(f.onUsage, res.Usage)
	if err != nil {
		return nil, err
	}
	kept := make([]memory.Concept, 0, len(concepts))
	for _, c := range concepts {
		if res.Answers[recallQuestionKey(c.ID)].Yes >= memoryRecallKeepAbove {
			kept = append(kept, c)
		}
	}
	return kept, nil
}

func recallQuestionKey(id int64) string { return "m" + strconv.FormatInt(id, 10) }

// MemoryRecallFilter returns the decision-model filter for the per-turn
// memory recall, or nil when the filter is off or no decision model can
// run. onUsage receives each call's usage record.
func MemoryRecallFilter(ctx context.Context, cfg config.Config, onUsage func(providers.Usage)) memory.RelevanceFilter {
	if !config.MemoryRecallFilterEnabled(cfg) {
		return nil
	}
	d, target, err := DeciderBuilder(ctx, cfg)
	if err != nil {
		if !errors.Is(err, ErrNoDecisionModel) {
			DebugLog("memory recall filter %s: %v", target.Label(), err)
		}
		return nil
	}
	return decisionRecallFilter{decider: d, onUsage: onUsage}
}

func reportDecisionUsage(onUsage func(providers.Usage), u providers.Usage) {
	if onUsage != nil && (u.InputTokens > 0 || u.CostKnown()) {
		onUsage(u)
	}
}
