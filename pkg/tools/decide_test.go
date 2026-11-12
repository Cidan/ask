package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
)

// fakeDecider answers every question through answer and records each
// request.
type fakeDecider struct {
	mu     sync.Mutex
	reqs   []providers.DecisionRequest
	answer func(input string, key string, q providers.Question) (providers.Answer, error)
}

func (f *fakeDecider) Decide(_ context.Context, req providers.DecisionRequest) (providers.DecisionResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	var input []string
	for _, p := range req.Input {
		input = append(input, p.Text)
	}
	text := strings.Join(input, "\n")
	res := providers.DecisionResult{Answers: map[string]providers.Answer{}, Usage: providers.Usage{Provider: "gemini", Model: "fake-decision", InputTokens: 100, CostUSD: 0.00001, CostSource: providers.CostPriced}}
	for key, q := range req.Questions {
		a, err := f.answer(text, key, q)
		if err != nil {
			return res, err
		}
		res.Answers[key] = a
	}
	return res, nil
}

func useFakeDecider(t *testing.T, d providers.Decider) {
	t.Helper()
	prev := engine.DeciderBuilder
	engine.DeciderBuilder = func(context.Context, config.Config) (providers.Decider, providers.DecisionTarget, error) {
		if d == nil {
			return nil, providers.DecisionTarget{}, engine.ErrNoDecisionModel
		}
		return d, providers.DecisionTarget{Provider: providers.GeminiAPI{}, Model: providers.DecisionModel{ID: "fake-decision"}}, nil
	}
	t.Cleanup(func() { engine.DeciderBuilder = prev })
}

func runDecide(t *testing.T, env *ToolEnv, args string) (DecideResult, ToolResponse) {
	t.Helper()
	resp, err := RunToolWithJSON(testAgentCtx(), DecideTool(env), args)
	if err != nil {
		t.Fatal(err)
	}
	var out DecideResult
	if !resp.IsError {
		if err := json.Unmarshal([]byte(resp.Content), &out); err != nil {
			t.Fatalf("decode %q: %v", resp.Content, err)
		}
	}
	return out, resp
}

func TestDecideToolNoModelIsANotice(t *testing.T) {
	useFakeDecider(t, nil)
	out, resp := runDecide(t, NewToolEnv(t.TempDir(), 0, true, nil, nil), `{"input":"x","questions":[{"key":"a","type":"yes_no","question":"?"}]}`)
	if resp.IsError || out.Notice != DecideNoModelNotice {
		t.Fatalf("no decision model must answer with the notice: %+v %+v", out, resp)
	}
}

func TestDecideToolAnswersEveryKindAndRecordsSpend(t *testing.T) {
	d := &fakeDecider{answer: func(_ string, key string, q providers.Question) (providers.Answer, error) {
		switch q.Kind {
		case providers.QuestionYesNo:
			return providers.Answer{Kind: q.Kind, Yes: 0.94, Confidence: 0.94}, nil
		case providers.QuestionChoice:
			return providers.Answer{Kind: q.Kind, Choice: "replacements", Probabilities: map[string]float64{"replacements": 0.8, "billing": 0.2}}, nil
		}
		return providers.Answer{Kind: q.Kind, Score: 1.12, Levels: []float64{0.03, 0.82, 0.15}}, nil
	}}
	useFakeDecider(t, d)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("left earcup cuts out"), 0o644); err != nil {
		t.Fatal(err)
	}
	var spent []engine.SpendEvent
	env := NewToolEnv(dir, 3, true, func(ev engine.EngineEvent) {
		if s, ok := ev.(engine.SpendEvent); ok {
			spent = append(spent, s)
		}
	}, nil)

	out, resp := runDecide(t, env, `{
		"input": "The mug arrived cracked.",
		"files": ["shot.png", "notes.txt"],
		"questions": [
			{"key": "damaged", "type": "yes_no", "question": "Did it arrive damaged?", "yes": "broken on arrival"},
			{"key": "queue", "type": "choice", "question": "Which queue?", "options": [{"key": "billing", "when": "money"}, {"key": "replacements"}]},
			{"key": "severity", "type": "score", "question": "How bad?", "levels": ["cosmetic", "workaround", "blocked"]}
		]}`)
	if resp.IsError {
		t.Fatalf("decide failed: %s", resp.Content)
	}
	want := "damaged: yes — p(yes) 0.94\n" +
		"queue: replacements 0.80 · billing 0.20\n" +
		`severity: 1.12 on 0-2 · most likely 1 "workaround" (0.82)`
	if out.Answers != want || out.Model != "gemini/fake-decision" {
		t.Fatalf("answers:\n%s\nwant:\n%s\nmodel %q", out.Answers, want, out.Model)
	}

	req := d.reqs[0]
	if len(req.Input) != 3 || req.Input[0].Text != "The mug arrived cracked." {
		t.Fatalf("input parts: %+v", req.Input)
	}
	if img := req.Input[1].InlineData; img == nil || img.MIMEType != "image/png" {
		t.Errorf("an image file is inline image data: %+v", req.Input[1])
	}
	if !strings.Contains(req.Input[2].Text, "left earcup cuts out") {
		t.Errorf("a text file is read as text: %+v", req.Input[2])
	}
	if q := req.Questions["damaged"]; q.Kind != providers.QuestionYesNo || q.Yes != "broken on arrival" {
		t.Errorf("yes/no question: %+v", q)
	}
	if q := req.Questions["queue"]; q.Options["billing"] != "money" || len(q.Options) != 2 {
		t.Errorf("choice options: %+v", q.Options)
	}
	if len(spent) != 1 || spent[0].Source != SpendDecision || spent[0].TabID != 3 || spent[0].Usage.InputTokens != 100 {
		t.Fatalf("the call's spend is reported: %+v", spent)
	}
}

func TestDecideToolItemsAskEachItemOnItsOwn(t *testing.T) {
	d := &fakeDecider{answer: func(input string, _ string, q providers.Question) (providers.Answer, error) {
		if strings.Contains(input, "boom") {
			return providers.Answer{}, errors.New("upstream 500")
		}
		yes := 0.1
		if strings.Contains(input, "auth") {
			yes = 0.9
		}
		return providers.Answer{Kind: q.Kind, Yes: yes}, nil
	}}
	useFakeDecider(t, d)
	env := NewToolEnv(t.TempDir(), 0, true, nil, nil)
	out, resp := runDecide(t, env, `{
		"input": "Hits for the login bug:",
		"items": ["src/auth/login.go: token refresh", "docs/readme.md: install", "boom"],
		"questions": [{"key": "relevant", "type": "yes_no", "question": "Is this hit about authentication?"}]}`)
	if resp.IsError {
		t.Fatalf("decide failed: %s", resp.Content)
	}
	if len(d.reqs) != 3 {
		t.Fatalf("one request per item, got %d", len(d.reqs))
	}
	for _, r := range d.reqs {
		if r.Input[0].Text != "Hits for the login bug:" || len(r.Input) != 2 {
			t.Fatalf("each item follows the shared input: %+v", r.Input)
		}
	}
	for _, want := range []string{
		"[1] src/auth/login.go: token refresh\n  relevant: yes — p(yes) 0.90",
		"[2] docs/readme.md: install\n  relevant: no — p(yes) 0.10",
		"[3] boom\n  error: upstream 500",
	} {
		if !strings.Contains(out.Answers, want) {
			t.Errorf("answers missing %q:\n%s", want, out.Answers)
		}
	}

	_, resp = runDecide(t, env, `{"items": ["boom", "boom too"], "questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`)
	if !resp.IsError || !strings.Contains(resp.Content, "every request failed") {
		t.Errorf("every item failing is an error: %+v", resp)
	}
}

func TestDecideToolRejectsMalformedCalls(t *testing.T) {
	useFakeDecider(t, &fakeDecider{answer: func(string, string, providers.Question) (providers.Answer, error) {
		return providers.Answer{}, nil
	}})
	env := NewToolEnv(t.TempDir(), 0, true, nil, nil)
	many := make([]string, DecideMaxItems+1)
	for i := range many {
		many[i] = `"x"`
	}
	for name, args := range map[string]string{
		"no questions":   `{"input": "x", "questions": []}`,
		"duplicate key":  `{"input": "x", "questions": [{"key": "a", "type": "yes_no", "question": "?"}, {"key": "a", "type": "yes_no", "question": "?"}]}`,
		"bad type":       `{"input": "x", "questions": [{"key": "a", "type": "maybe", "question": "?"}]}`,
		"no input":       `{"questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`,
		"missing file":   `{"files": ["nope.png"], "questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`,
		"too many items": `{"items": [` + strings.Join(many, ",") + `], "questions": [{"key": "a", "type": "yes_no", "question": "?"}]}`,
		"dup option":     `{"input": "x", "questions": [{"key": "a", "type": "choice", "question": "?", "options": [{"key": "x"}, {"key": "x"}]}]}`,
	} {
		if _, resp := runDecide(t, env, args); !resp.IsError {
			t.Errorf("%s: want an error, got %s", name, resp.Content)
		}
	}
}

func TestDecideFilePartTypes(t *testing.T) {
	dir := t.TempDir()
	pdf := filepath.Join(dir, "spec.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.7"), 0o644); err != nil {
		t.Fatal(err)
	}
	part, err := decideFilePart(pdf)
	if err != nil || part.InlineData == nil || part.InlineData.MIMEType != "application/pdf" {
		t.Fatalf("pdf part: %+v %v", part, err)
	}
	if _, err := decideFilePart(dir); err == nil {
		t.Error("a directory is not a file")
	}
}
