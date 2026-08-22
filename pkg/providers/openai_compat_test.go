package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/openai/openai-go/v3"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// newCompatModel builds a shared model pointed at a test server, using
// OpenRouter's reasoning encoding so the wire shape matches production.
func newCompatModel(baseURL string) model.LLM {
	return NewOpenAICompatModel(OpenAICompatConfig{
		ModelID:         "test/model",
		APIKey:          "test-key",
		BaseURL:         baseURL,
		Headers:         map[string]string{"X-Title": "ask"},
		EncodeReasoning: openRouterReasoningEncoder(baseURL),
	})
}

// seedOpenRouterMeta resets the model-metadata cache and loads the given
// entries, marking it fetched so no lazy network call happens during a test.
func seedOpenRouterMeta(metas ...openRouterModelMeta) {
	openRouterMeta.mu.Lock()
	openRouterMeta.byID = map[string]openRouterModelMeta{}
	openRouterMeta.mu.Unlock()
	cacheOpenRouterMeta(metas)
}

func drain(seq func(func(*model.LLMResponse, error) bool)) ([]*model.LLMResponse, error) {
	var out []*model.LLMResponse
	var gotErr error
	seq(func(r *model.LLMResponse, e error) bool {
		if e != nil {
			gotErr = e
			return false
		}
		if r != nil {
			out = append(out, r)
		}
		return true
	})
	return out, gotErr
}

func TestOpenAICompat_NonStreamingRequestShape(t *testing.T) {
	seedOpenRouterMeta(openRouterModelMeta{
		ID:                "test/model",
		SupportsReasoning: true,
		SupportedEfforts:  []string{"low", "medium", "high"},
	})
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("missing bearer auth, got %q", got)
		}
		if got := r.Header.Get("X-Title"); got != "ask" {
			t.Errorf("custom header not sent, got %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"test/model",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer server.Close()

	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: "user", Parts: []*genai.Part{{Text: "hello"}}},
			{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "fc_1", Name: "get_weather", Args: map[string]any{"city": "boston"}}}}},
			{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "fc_1", Name: "get_weather", Response: map[string]any{"temp": 72}}}}},
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: "be helpful"}}},
			MaxOutputTokens:   2048,
			ThinkingConfig:    &genai.ThinkingConfig{ThinkingLevel: "LOW"},
			Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name:        "get_weather",
				Description: "gets weather",
				Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{
					"city": {Type: genai.TypeString},
				}, Required: []string{"city"}},
			}}}},
		},
	}

	if _, err := drain(newCompatModel(server.URL).GenerateContent(context.Background(), req, false)); err != nil {
		t.Fatalf("generate: %v", err)
	}

	if body["model"] != "test/model" {
		t.Errorf("model = %v", body["model"])
	}
	if body["max_tokens"].(float64) != 2048 {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	// Reasoning goes in the unified `reasoning: {effort}` object, not reasoning_effort.
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Errorf("reasoning block = %v", body["reasoning"])
	}
	if _, present := body["reasoning_effort"]; present {
		t.Errorf("should not send top-level reasoning_effort")
	}

	msgs, _ := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be helpful" {
		t.Errorf("system msg = %v", sys)
	}
	asst := msgs[2].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if asst["role"] != "assistant" || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %v", asst)
	}
	// The real provider-issued ID must survive the round trip, not "call_<name>".
	if id := tcs[0].(map[string]any)["id"]; id != "fc_1" {
		t.Errorf("tool_call id = %v, want fc_1", id)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "fc_1" {
		t.Errorf("tool msg = %v", tool)
	}
}

func TestOpenAICompat_SynthesizedToolCallIDWhenMissing(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	req := &model.LLMRequest{Contents: []*genai.Content{
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "read", Args: map[string]any{}}}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "read", Response: map[string]any{"ok": true}}}}},
	}}
	if _, err := drain(newCompatModel(server.URL).GenerateContent(context.Background(), req, false)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	asst := msgs[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	tool := msgs[1].(map[string]any)
	// Synthesized IDs must still pair the call and its response.
	if asst["id"] != "call_read" || tool["tool_call_id"] != "call_read" {
		t.Errorf("synthesized ids mismatch: call=%v resp=%v", asst["id"], tool["tool_call_id"])
	}
}

func TestOpenAICompat_ImagePartBecomesImageURL(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	req := &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{
		{Text: "what is this"},
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte{1, 2, 3}}},
	}}}}
	if _, err := drain(newCompatModel(server.URL).GenerateContent(context.Background(), req, false)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	msgs, _ := body["messages"].([]any)
	parts, ok := msgs[0].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("want 2 content parts, got %v", msgs[0].(map[string]any)["content"])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("part[1] type = %v", img["type"])
	}
	url := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image url = %q", url)
	}
}

func TestOpenAICompat_ResponseTranslation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls",
			"message":{"role":"assistant","content":"the answer","reasoning":"let me think",
			"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"do_it","arguments":"{\"x\":1}"}}]}}],
			"usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,
			"prompt_tokens_details":{"cached_tokens":20},"completion_tokens_details":{"reasoning_tokens":10}}}`))
	}))
	defer server.Close()

	resps, err := drain(newCompatModel(server.URL).GenerateContent(context.Background(), &model.LLMRequest{}, false))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	r := resps[0]
	if r.Partial || !r.TurnComplete {
		t.Errorf("final response flags: partial=%v complete=%v", r.Partial, r.TurnComplete)
	}
	if r.UsageMetadata == nil {
		t.Fatalf("usage metadata missing")
	}
	if r.UsageMetadata.PromptTokenCount != 100 || r.UsageMetadata.CandidatesTokenCount != 40 || r.UsageMetadata.TotalTokenCount != 140 {
		t.Errorf("usage = %+v", r.UsageMetadata)
	}
	if r.UsageMetadata.CachedContentTokenCount != 20 || r.UsageMetadata.ThoughtsTokenCount != 10 {
		t.Errorf("usage details = %+v", r.UsageMetadata)
	}

	var gotThought, gotText string
	var gotCall *genai.FunctionCall
	for _, p := range r.Content.Parts {
		switch {
		case p.Thought:
			gotThought = p.Text
		case p.FunctionCall != nil:
			gotCall = p.FunctionCall
		case p.Text != "":
			gotText = p.Text
		}
	}
	if gotThought != "let me think" {
		t.Errorf("reasoning = %q", gotThought)
	}
	if gotText != "the answer" {
		t.Errorf("text = %q", gotText)
	}
	if gotCall == nil || gotCall.ID != "call_abc" || gotCall.Name != "do_it" {
		t.Fatalf("function call = %+v", gotCall)
	}
	if gotCall.Args["x"].(float64) != 1 {
		t.Errorf("call args = %v", gotCall.Args)
	}
}

func TestOpenAICompat_Streaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_s\",\"type\":\"function\",\"function\":{\"name\":\"go\",\"arguments\":\"{}\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	resps, err := drain(newCompatModel(server.URL).GenerateContent(context.Background(), &model.LLMRequest{}, true))
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	var partialText strings.Builder
	var final *model.LLMResponse
	for _, r := range resps {
		if r.Partial {
			for _, p := range r.Content.Parts {
				if !p.Thought {
					partialText.WriteString(p.Text)
				}
			}
		} else {
			final = r
		}
	}
	if partialText.String() != "hello" {
		t.Errorf("streamed partial text = %q, want hello", partialText.String())
	}
	if final == nil {
		t.Fatal("no final non-partial response emitted")
	}
	// The final (persisted) event must carry the FULL accumulated text + tool call + usage.
	var finalText string
	var finalCall *genai.FunctionCall
	for _, p := range final.Content.Parts {
		if p.FunctionCall != nil {
			finalCall = p.FunctionCall
		} else if p.Text != "" && !p.Thought {
			finalText = p.Text
		}
	}
	if finalText != "hello" {
		t.Errorf("final aggregated text = %q, want hello", finalText)
	}
	if finalCall == nil || finalCall.ID != "call_s" || finalCall.Name != "go" {
		t.Errorf("final tool call = %+v", finalCall)
	}
	if final.UsageMetadata == nil || final.UsageMetadata.TotalTokenCount != 5 {
		t.Errorf("final usage = %+v", final.UsageMetadata)
	}
}

func TestOpenAICompat_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer server.Close()

	_, err := drain(newCompatModel(server.URL).GenerateContent(context.Background(), &model.LLMRequest{}, false))
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "test/model") {
		t.Errorf("error should name the model: %v", err)
	}
}

// The bug that made every tool call arrive with empty args: ADK's functiontool
// sets FunctionDeclaration.ParametersJsonSchema (built via jsonschema.For), not
// the *genai.Schema Parameters field. convertTools must read that, or tools ship
// parameterless and the model calls them with {}.
func TestToolParameters_UsesParametersJsonSchema(t *testing.T) {
	type bashArgs struct {
		Command     string `json:"command" jsonschema:"the shell command"`
		Description string `json:"description" jsonschema:"phrase describing the call"`
		Background  bool   `json:"run_in_background,omitempty"`
	}
	schema, err := jsonschema.For[bashArgs](nil)
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	// Mirror ADK: ParametersJsonSchema set, Parameters nil.
	decl := &genai.FunctionDeclaration{
		Name:                 "bash",
		Description:          "run a command",
		ParametersJsonSchema: schema,
	}

	params := toolParameters(decl)
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("wire params carry no properties: %#v", params)
	}
	if props["command"] == nil || props["description"] == nil {
		t.Errorf("properties missing command/description: %#v", props)
	}
	req := map[string]bool{}
	if reqRaw, ok := params["required"].([]any); ok {
		for _, r := range reqRaw {
			req[r.(string)] = true
		}
	}
	if !req["command"] || !req["description"] {
		t.Errorf("required = %v, want command+description", params["required"])
	}
	if _, present := params["$schema"]; present {
		t.Errorf("$schema must be stripped from function parameters")
	}

	// End-to-end: the parameters must survive SDK serialization onto the wire.
	b, _ := json.Marshal(openai.ChatCompletionNewParams{
		Model: "m",
		Tools: convertTools([]*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{decl}}}),
	})
	if !strings.Contains(string(b), `"command"`) || !strings.Contains(string(b), `"required"`) {
		t.Errorf("serialized tool dropped parameters: %s", string(b))
	}
}

func TestConvertSchema_Fidelity(t *testing.T) {
	s := &genai.Schema{
		Type:        genai.TypeObject,
		Description: "root",
		Properties: map[string]*genai.Schema{
			"name": {Type: genai.TypeString, Description: "the name"},
			"tags": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}},
		},
		Required: []string{"name"},
	}
	m := convertSchema(s)
	if m["type"] != "object" {
		t.Errorf("type = %v (want lowercased)", m["type"])
	}
	props := m["properties"].(map[string]any)
	if props["name"].(map[string]any)["type"] != "string" {
		t.Errorf("nested type not lowercased: %v", props["name"])
	}
	if props["tags"].(map[string]any)["items"].(map[string]any)["type"] != "string" {
		t.Errorf("array items type = %v", props["tags"])
	}
	req := m["required"].([]string)
	if len(req) != 1 || req[0] != "name" {
		t.Errorf("required = %v", req)
	}
}

// encodedEffort runs the OpenRouter reasoning encoder for a model and returns
// the effort it put on the wire, or "" when it sent no reasoning block.
func encodedEffort(t *testing.T, modelID, effort string) string {
	t.Helper()
	var params openai.ChatCompletionNewParams
	openRouterReasoningEncoder("http://unused")(&params, modelID, effort)
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		return ""
	}
	return reasoning["effort"].(string)
}

// TestOpenRouterReasoningMapping is the crux: ask's effort maps onto each
// model's real supported efforts — identity when supported, clamped to the
// nearest otherwise, dropped when the model has no reasoning, and passed
// through for unknown models (OpenRouter ignores unsupported params).
func TestOpenRouterReasoningMapping(t *testing.T) {
	seedOpenRouterMeta(
		openRouterModelMeta{ID: "full", SupportsReasoning: true, SupportedEfforts: []string{"minimal", "low", "medium", "high"}},
		openRouterModelMeta{ID: "restricted", SupportsReasoning: true, SupportedEfforts: []string{"medium", "high"}},
		openRouterModelMeta{ID: "reasoning-no-efforts", SupportsReasoning: true},
		openRouterModelMeta{ID: "no-reasoning", SupportsReasoning: false},
	)

	cases := []struct {
		name, model, effort, want string
	}{
		{"identity high", "full", "high", "high"},
		{"identity low", "full", "low", "low"},
		{"clamp low to nearest supported", "restricted", "low", "medium"},
		{"clamp high stays high", "restricted", "high", "high"},
		{"supports reasoning but no effort list passes through", "reasoning-no-efforts", "low", "low"},
		{"no reasoning drops the block", "no-reasoning", "high", ""},
		{"unknown model passes through", "some/unlisted-model", "medium", "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodedEffort(t, tc.model, tc.effort); got != tc.want {
				t.Errorf("model %q effort %q -> %q, want %q", tc.model, tc.effort, got, tc.want)
			}
		})
	}
}

func TestFetchOpenRouterModels_ParsesCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[
			{"id":"a/reasoner","context_length":200000,
			 "architecture":{"input_modalities":["text","image"]},
			 "reasoning":{"supported_efforts":["low","medium","high"]}},
			{"id":"b/tools-only","context_length":32000,
			 "architecture":{"input_modalities":["text"]},
			 "supported_parameters":["tools","reasoning"]},
			{"id":"c/plain","context_length":8192,
			 "architecture":{"input_modalities":["text"]},
			 "supported_parameters":["temperature"]}
		]}`))
	}))
	defer server.Close()

	metas, err := fetchOpenRouterModels(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(metas) != 3 {
		t.Fatalf("want 3 metas, got %d", len(metas))
	}
	byID := map[string]openRouterModelMeta{}
	for _, m := range metas {
		byID[m.ID] = m
	}
	if a := byID["a/reasoner"]; !a.SupportsReasoning || !a.SupportsImages || a.ContextLength != 200000 ||
		len(a.SupportedEfforts) != 3 {
		t.Errorf("a/reasoner = %+v", a)
	}
	// reasoning declared only via supported_parameters, no explicit effort list.
	if b := byID["b/tools-only"]; !b.SupportsReasoning || b.SupportsImages || len(b.SupportedEfforts) != 0 {
		t.Errorf("b/tools-only = %+v", b)
	}
	if c := byID["c/plain"]; c.SupportsReasoning || c.SupportsImages {
		t.Errorf("c/plain should have no reasoning/image: %+v", c)
	}
}
