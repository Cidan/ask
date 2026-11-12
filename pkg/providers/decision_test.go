package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"google.golang.org/genai"
)

func TestDecisionRequestValidate(t *testing.T) {
	text := []*genai.Part{genai.NewPartFromText("x")}
	cases := []struct {
		name string
		req  DecisionRequest
		want string
	}{
		{"no input", DecisionRequest{Questions: map[string]Question{"a": {Kind: QuestionYesNo, Instructions: "?"}}}, "input is empty"},
		{"no questions", DecisionRequest{Input: text}, "no questions"},
		{"empty key", DecisionRequest{Input: text, Questions: map[string]Question{" ": {Kind: QuestionYesNo, Instructions: "?"}}}, "empty key"},
		{"no instructions", DecisionRequest{Input: text, Questions: map[string]Question{"a": {Kind: QuestionYesNo}}}, "no instructions"},
		{"one option", DecisionRequest{Input: text, Questions: map[string]Question{"a": {Kind: QuestionChoice, Instructions: "?", Options: map[string]string{"x": ""}}}}, "at least 2 options"},
		{"empty option", DecisionRequest{Input: text, Questions: map[string]Question{"a": {Kind: QuestionChoice, Instructions: "?", Options: map[string]string{"x": "", "": "y"}}}}, "empty option"},
		{"one level", DecisionRequest{Input: text, Questions: map[string]Question{"a": {Kind: QuestionScore, Instructions: "?", Levels: []string{"low"}}}}, "at least 2 levels"},
		{"bad kind", DecisionRequest{Input: text, Questions: map[string]Question{"a": {Kind: "maybe", Instructions: "?"}}}, "unknown type"},
		{"ok", DecisionRequest{Input: text, Questions: map[string]Question{
			"a": {Kind: QuestionYesNo, Instructions: "?"},
			"b": {Kind: QuestionChoice, Instructions: "?", Options: map[string]string{"x": "", "y": ""}},
			"c": {Kind: QuestionScore, Instructions: "?", Levels: []string{"low", "high"}},
		}}, ""},
	}
	for _, tc := range cases {
		err := tc.req.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestAnswerHelpers(t *testing.T) {
	a := Answer{Levels: []float64{0.1, 0.7, 0.2}}
	if a.Level() != 1 {
		t.Errorf("Level = %d", a.Level())
	}
	c := Answer{Probabilities: map[string]float64{"b": 0.2, "a": 0.2, "c": 0.6}}
	if got := strings.Join(c.RankedOptions(), ","); got != "c,a,b" {
		t.Errorf("RankedOptions = %s", got)
	}
}

func TestResolveDecisionTarget(t *testing.T) {
	t.Setenv(GeminiEnvAPIKey, "")
	if _, ok := ResolveDecisionTarget(config.Config{}); ok {
		t.Fatal("no credentials must resolve to nothing")
	}

	var cfg config.Config
	cfg.SetProviderConfig(GeminiAPIProviderID, cfg.ProviderConfig(GeminiAPIProviderID).WithField(GeminiFieldAPIKey, "k"))
	got, ok := ResolveDecisionTarget(cfg)
	if !ok || got.Provider.ID() != GeminiAPIProviderID || got.Model.ID != GeminiDecision1 || got.Label() != "gemini/gemini-decision-1" {
		t.Fatalf("automatic resolve = %+v %v", got, ok)
	}

	cfg.Decisions = config.DecisionsConfig{Provider: GeminiAPIProviderID, Model: "gemini-decision-2"}
	got, ok = ResolveDecisionTarget(cfg)
	if !ok || got.Model.ID != "gemini-decision-2" || got.Model.InputUSDPerMTok != 0 {
		t.Fatalf("an unlisted model resolves with unknown price: %+v %v", got, ok)
	}

	cfg.Decisions = config.DecisionsConfig{Provider: "nosuch"}
	if _, ok := ResolveDecisionTarget(cfg); ok {
		t.Error("an unknown provider must not resolve")
	}

	t.Setenv(GeminiEnvAPIKey, "from-env")
	if got, ok := ResolveDecisionTarget(config.Config{}); !ok || got.Provider.ID() != GeminiAPIProviderID {
		t.Errorf("the env key configures the provider: %+v %v", got, ok)
	}
}

func TestDecisionUsagePricesInputOnly(t *testing.T) {
	target := DecisionTarget{Provider: GeminiAPI{}, Model: DecisionModelFor(GeminiAPI{}, "")}
	u := DecisionUsage(target, 2_000_000)
	if u.Provider != GeminiAPIProviderID || u.Model != GeminiDecision1 || u.InputTokens != 2_000_000 || u.OutputTokens != 0 {
		t.Fatalf("usage = %+v", u)
	}
	if u.CostSource != CostPriced || math.Abs(u.CostUSD-0.20) > 1e-9 {
		t.Errorf("cost = %v %q", u.CostUSD, u.CostSource)
	}
	unpriced := DecisionUsage(DecisionTarget{Provider: GeminiAPI{}, Model: DecisionModel{ID: "x"}}, 10)
	if unpriced.CostKnown() {
		t.Error("an unpriced model must leave the cost unknown")
	}
}

type badDecisionProvider struct {
	GeminiAPI
	settings []SettingField
	models   []DecisionModel
}

func (b badDecisionProvider) Settings() []SettingField        { return b.settings }
func (b badDecisionProvider) DecisionModels() []DecisionModel { return b.models }

func TestRegisterDecisionProviderRejectsMalformed(t *testing.T) {
	for name, p := range map[string]DecisionProvider{
		"reserved key": badDecisionProvider{settings: []SettingField{{Key: "model"}}, models: geminiDecisionModels},
		"no models":    badDecisionProvider{settings: geminiSettings},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: RegisterDecisionProvider must panic", name)
				}
			}()
			RegisterDecisionProvider(p)
		}()
	}
}

func TestGeminiBuildDeciderNeedsKey(t *testing.T) {
	t.Setenv(GeminiEnvAPIKey, "")
	if _, err := (GeminiAPI{}).BuildDecider(context.Background(), config.ProviderConfig{}, ""); err == nil || !strings.Contains(err.Error(), GeminiEnvAPIKey) {
		t.Fatalf("BuildDecider without a key: %v", err)
	}
}

// geminiDecisionsServer answers POST /v1beta/decisions with reply and
// records the request it got.
func geminiDecisionsServer(t *testing.T, reply string) (*map[string]any, *http.Header) {
	t.Helper()
	var body map[string]any
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/decisions" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		header = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	prev := GeminiBaseURL
	GeminiBaseURL = srv.URL
	t.Cleanup(func() { GeminiBaseURL = prev })
	return &body, &header
}

func TestGeminiDecideRoundTrip(t *testing.T) {
	body, header := geminiDecisionsServer(t, `{
		"answers": {
			"damaged": {"type": "noul", "noul": 0.94, "confidence": 0.94},
			"queue": {"type": "choice", "choice": "replacements", "probabilities": {"replacements": 0.8, "billing": 0.2}, "confidence": 0.8},
			"severity": {"type": "score", "score": 1.12, "legend": {"0": "low", "1": "mid", "2": "high"}, "probabilities": {"0": 0.03, "1": 0.82, "2": 0.15}, "confidence": 0.82}
		},
		"usage": {"total_input_tokens": 500000, "total_output_tokens": 0, "total_tokens": 500000}
	}`)
	var pc config.ProviderConfig
	d, err := (GeminiAPI{}).BuildDecider(context.Background(), pc.WithField(GeminiFieldAPIKey, "test-key"), "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Decide(context.Background(), DecisionRequest{
		Input: []*genai.Part{
			genai.NewPartFromText("order #1"),
			genai.NewPartFromText("the mug arrived cracked"),
			genai.NewPartFromBytes([]byte("PNG"), "image/png"),
		},
		Questions: map[string]Question{
			"damaged":  {Kind: QuestionYesNo, Instructions: "Did it arrive damaged?", Yes: "broken on arrival"},
			"queue":    {Kind: QuestionChoice, Instructions: "Which queue?", Options: map[string]string{"billing": "money", "replacements": "broken items"}},
			"severity": {Kind: QuestionScore, Instructions: "How bad?", Levels: []string{"low", "mid", "high"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := header.Get("X-Goog-Api-Key"); got != "test-key" {
		t.Errorf("api key header = %q", got)
	}
	b := *body
	if b["model"] != GeminiDecision1 {
		t.Errorf("model = %v", b["model"])
	}
	input, _ := b["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("adjacent text parts merge into one block, then the image: %v", b["input"])
	}
	if first := input[0].(map[string]any); first["type"] != "text" || first["text"] != "order #1\n\nthe mug arrived cracked" {
		t.Errorf("text block = %v", first)
	}
	if img := input[1].(map[string]any); img["type"] != "image" || img["mime_type"] != "image/png" || img["data"] != base64.StdEncoding.EncodeToString([]byte("PNG")) {
		t.Errorf("image block = %v", img)
	}
	qs := b["questions"].(map[string]any)
	if q := qs["damaged"].(map[string]any); q["type"] != "noul" || q["instructions"] != "Did it arrive damaged?" || q["criteria"].(map[string]any)["true_description"] != "broken on arrival" {
		t.Errorf("noul question = %v", q)
	}
	if _, ok := qs["damaged"].(map[string]any)["criteria"].(map[string]any)["false_description"]; ok {
		t.Error("an unset no-description must be omitted")
	}
	if q := qs["queue"].(map[string]any); q["type"] != "choice" || q["criteria"].(map[string]any)["replacements"] != "broken items" {
		t.Errorf("choice question = %v", q)
	}
	if q := qs["severity"].(map[string]any); q["type"] != "score" || len(q["criteria"].([]any)) != 3 {
		t.Errorf("score question = %v", q)
	}

	if a := res.Answers["damaged"]; a.Kind != QuestionYesNo || a.Yes != 0.94 || a.Confidence != 0.94 {
		t.Errorf("yes/no answer = %+v", a)
	}
	if a := res.Answers["queue"]; a.Kind != QuestionChoice || a.Choice != "replacements" || a.Probabilities["billing"] != 0.2 {
		t.Errorf("choice answer = %+v", a)
	}
	if a := res.Answers["severity"]; a.Kind != QuestionScore || a.Score != 1.12 || a.Level() != 1 || len(a.Levels) != 3 || a.Levels[2] != 0.15 {
		t.Errorf("score answer = %+v", a)
	}
	if res.Usage.InputTokens != 500000 || res.Usage.CostSource != CostPriced || math.Abs(res.Usage.CostUSD-0.05) > 1e-9 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestGeminiDecideBlockedAndLimits(t *testing.T) {
	geminiDecisionsServer(t, `{"prompt_feedback": {"block_reason": "safety", "block_reason_message": "harmful content"}, "usage": {"total_input_tokens": 10}}`)
	var pc config.ProviderConfig
	d, err := (GeminiAPI{}).BuildDecider(context.Background(), pc.WithField(GeminiFieldAPIKey, "k"), "")
	if err != nil {
		t.Fatal(err)
	}
	q := map[string]Question{"a": {Kind: QuestionYesNo, Instructions: "?"}}
	res, err := d.Decide(context.Background(), DecisionRequest{Input: []*genai.Part{genai.NewPartFromText("x")}, Questions: q})
	var blocked *DecisionBlockedError
	if !errors.As(err, &blocked) || blocked.Reason != "safety" || blocked.Message != "harmful content" {
		t.Fatalf("blocked = %v", err)
	}
	if res.Usage.InputTokens != 10 {
		t.Errorf("a blocked call still reports its usage: %+v", res.Usage)
	}

	bad := map[string]Question{"bad key!": {Kind: QuestionYesNo, Instructions: "?"}}
	if _, err := d.Decide(context.Background(), DecisionRequest{Input: []*genai.Part{genai.NewPartFromText("x")}, Questions: bad}); err == nil || !strings.Contains(err.Error(), "question key") {
		t.Errorf("key format: %v", err)
	}
	many := map[string]Question{}
	for i := 0; i <= 1000; i++ {
		many["q"+strconv.Itoa(i)] = Question{Kind: QuestionYesNo, Instructions: "?"}
	}
	if _, err := d.Decide(context.Background(), DecisionRequest{Input: []*genai.Part{genai.NewPartFromText("x")}, Questions: many}); err == nil || !strings.Contains(err.Error(), "at most 1000") {
		t.Errorf("question cap: %v", err)
	}
}
