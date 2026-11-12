package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
)

// scriptedDecider answers yes/no questions with yes(input, question) and
// records every request.
type scriptedDecider struct {
	mu   sync.Mutex
	reqs []providers.DecisionRequest
	yes  func(input string, q providers.Question) float64
	err  error
}

func (d *scriptedDecider) Decide(_ context.Context, req providers.DecisionRequest) (providers.DecisionResult, error) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
	res := providers.DecisionResult{Answers: map[string]providers.Answer{}, Usage: providers.Usage{Provider: providers.GeminiAPIProviderID, Model: "fake-decision", InputTokens: 40, CostUSD: 0.000004, CostSource: providers.CostPriced}}
	if d.err != nil {
		return res, d.err
	}
	for key, q := range req.Questions {
		res.Answers[key] = providers.Answer{Kind: providers.QuestionYesNo, Yes: d.yes(req.Input[0].Text, q)}
	}
	return res, nil
}

func (d *scriptedDecider) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reqs)
}

func useScriptedDecider(t *testing.T, d providers.Decider) {
	t.Helper()
	prev := DeciderBuilder
	DeciderBuilder = func(context.Context, config.Config) (providers.Decider, providers.DecisionTarget, error) {
		if d == nil {
			return nil, providers.DecisionTarget{}, ErrNoDecisionModel
		}
		return d, providers.DecisionTarget{Provider: providers.GeminiAPI{}, Model: providers.DecisionModel{ID: "fake-decision"}}, nil
	}
	t.Cleanup(func() { DeciderBuilder = prev })
}

func TestMemoryExtractorGateSkipsTurnsWithNothingDurable(t *testing.T) {
	openTestMemory(t)
	seen, mu := scriptExtractionModel(t, `{"topic":"style","concepts":[]}`)
	d := &scriptedDecider{yes: func(input string, _ providers.Question) float64 {
		if strings.Contains(input, "I prefer") {
			return 0.85
		}
		return 0.04
	}}
	useScriptedDecider(t, d)

	cfg := config.Config{Provider: providers.VertexProviderID}
	ex := NewMemoryExtractor(MemoryExtractorOptions{LoadConfig: func() (config.Config, error) { return cfg, nil }})
	t.Cleanup(ex.Close)
	var usageMu sync.Mutex
	var usages []providers.Usage
	run := func(prompt string) {
		t.Helper()
		ex.EnqueueTurn(MemoryTurn{Cwd: filepath.Join(t.TempDir(), "proj"), Prompt: prompt, Response: "done", Provider: providers.VertexProviderID,
			OnUsage: func(u providers.Usage) {
				usageMu.Lock()
				defer usageMu.Unlock()
				usages = append(usages, u)
			}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ex.Drain(ctx); err != nil {
			t.Fatal(err)
		}
	}
	extractions := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(*seen)
	}

	run("rename the variable")
	if extractions() != 0 || d.calls() != 1 {
		t.Fatalf("a turn judged to hold nothing durable skips extraction: extractions=%d decisions=%d", extractions(), d.calls())
	}
	if q := d.reqs[0].Questions[memoryGateKey]; q.Kind != providers.QuestionYesNo || !strings.Contains(d.reqs[0].Input[0].Text, "User:\nrename the variable") {
		t.Errorf("gate request: %+v %q", q, d.reqs[0].Input[0].Text)
	}
	usageMu.Lock()
	if len(usages) != 1 || usages[0].Provider != providers.GeminiAPIProviderID {
		t.Errorf("the gate's own call is reported: %+v", usages)
	}
	usageMu.Unlock()

	run("I prefer tabs over spaces")
	if extractions() != 1 {
		t.Fatalf("a durable turn is extracted: %d", extractions())
	}

	d.err = errors.New("upstream down")
	run("rename it again")
	if extractions() != 2 {
		t.Fatalf("a failed gate falls through to extraction: %d", extractions())
	}

	off := false
	cfg.Memory.ExtractGate = &off
	d.err = nil
	before := d.calls()
	run("rename once more")
	if extractions() != 3 || d.calls() != before {
		t.Fatalf("with the gate off every turn is extracted and no decision is made: extractions=%d decisions=%d", extractions(), d.calls()-before)
	}

	useScriptedDecider(t, nil)
	cfg.Memory.ExtractGate = nil
	run("and again")
	if extractions() != 4 {
		t.Fatalf("without a decision model every turn is extracted: %d", extractions())
	}
}

func TestMemoryRecallFilter(t *testing.T) {
	useScriptedDecider(t, nil)
	if MemoryRecallFilter(context.Background(), config.Config{}, nil) != nil {
		t.Fatal("no decision model, no filter")
	}

	d := &scriptedDecider{yes: func(_ string, q providers.Question) float64 {
		if strings.Contains(q.Instructions, "tabs") {
			return 0.7
		}
		return 0.1
	}}
	useScriptedDecider(t, d)
	off := false
	if MemoryRecallFilter(context.Background(), config.Config{Memory: config.MemoryConfig{RecallFilter: &off}}, nil) != nil {
		t.Fatal("a disabled filter is nil")
	}

	var spent []providers.Usage
	f := MemoryRecallFilter(context.Background(), config.Config{}, func(u providers.Usage) { spent = append(spent, u) })
	if f == nil {
		t.Fatal("a configured decision model filters recall")
	}
	long := strings.Repeat("x", 2*memoryRecallBodyCap)
	concepts := []memory.Concept{
		{ID: 7, Title: "prefers tabs", Body: long},
		{ID: 9, Title: "deploys on fridays", Body: "deploys on fridays"},
		{ID: 11, Title: "uses tabs in Go", Body: ""},
	}
	kept, err := f.Relevant(context.Background(), "format this file", concepts)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || kept[0].ID != 7 || kept[1].ID != 11 {
		t.Fatalf("kept %+v: relevant concepts keep their order", kept)
	}
	req := d.reqs[0]
	if req.Input[0].Text != "Request:\nformat this file" || len(req.Questions) != 3 {
		t.Fatalf("one question per concept over the prompt: %+v", req)
	}
	if q := req.Questions["m7"]; !strings.Contains(q.Instructions, "prefers tabs\n") || len(q.Instructions) > len(memoryRecallInstruction)+len("prefers tabs\n")+memoryRecallBodyCap+8 {
		t.Errorf("the body is clipped: %d bytes", len(q.Instructions))
	}
	if q := req.Questions["m9"]; strings.Count(q.Instructions, "deploys on fridays") != 1 {
		t.Errorf("a body equal to the title is not repeated: %q", q.Instructions)
	}
	if len(spent) != 1 {
		t.Errorf("the filter call is reported: %+v", spent)
	}

	d.err = errors.New("boom")
	if _, err := f.Relevant(context.Background(), "x", concepts); err == nil {
		t.Error("a failed call is the filter's error, so recall falls back")
	}
}
