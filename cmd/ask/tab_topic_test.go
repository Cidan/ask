package main

import (
	"context"
	"iter"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/memory"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestSplitTitleAndTopic(t *testing.T) {
	cases := []struct {
		in, title, topic string
	}{
		{"Fix auth test\ntopic: Auth Tests", "Fix auth test", "auth tests"},
		{"\"Fix auth test.\"\nTopic: \"deploy\"", "Fix auth test", "deploy"},
		{"<think>hmm</think>Fix auth test\ntopic: memory design notes extra", "Fix auth test", "memory design notes"},
		{"Fix auth test\nauth", "Fix auth test", "auth"},
		{"Fix auth test", "Fix auth test", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		title, topic := splitTitleAndTopic(c.in)
		if title != c.title || topic != c.topic {
			t.Errorf("split(%q) = %q/%q, want %q/%q", c.in, title, topic, c.title, c.topic)
		}
	}
}

func TestTabTitleMsgSeedsTopicOnlyWhenEmpty(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())
	m.tabTitle = "seed"
	m2, _ := runUpdate(t, m, tabTitleMsg{tabID: m.id, title: "Refined", topic: "auth"})
	if m2.tabTopic != "auth" {
		t.Fatalf("title call must seed the topic: %q", m2.tabTopic)
	}
	m2.tabTopic = "memory"
	m3, _ := runUpdate(t, m2, tabTitleMsg{tabID: m.id, title: "Again", topic: "auth"})
	if m3.tabTopic != "memory" {
		t.Fatalf("a topic the session already inferred must win: %q", m3.tabTopic)
	}
}

func TestTabTopicMsgHandlerAndPersistence(t *testing.T) {
	isolateHome(t)
	now := time.Now().UTC()
	var vsID string
	if err := mutateVirtualSessions(func(store *virtualSessionStore) error {
		vsID = upsertVirtualSession(store, "", "/ws", "fake", "native-1", "/ws", "preview", now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t, newFakeProvider())
	m.virtualSessionID = vsID
	m.tabTitle = "seed"

	m2, _ := runUpdate(t, m, tabTopicMsg{tabID: 99, topic: "other"})
	if m2.tabTopic != "" {
		t.Fatal("foreign tabTopicMsg applied")
	}
	m3, _ := runUpdate(t, m, tabTopicMsg{tabID: m.id, topic: ""})
	if m3.tabTopic != "" {
		t.Fatal("empty topic applied")
	}
	m4, _ := runUpdate(t, m, tabTopicMsg{tabID: m.id, topic: "memory design"})
	if m4.tabTopic != "memory design" {
		t.Fatalf("topic = %q", m4.tabTopic)
	}
	store, err := loadVirtualSessions()
	if err != nil {
		t.Fatal(err)
	}
	if vs := store.findByID(vsID); vs.Topic != "memory design" || vs.Title != "seed" {
		t.Fatalf("persisted title/topic = %q/%q", vs.Title, vs.Topic)
	}
	if got := (&m4).sidebarTitle(); got != "seed · memory design" {
		t.Fatalf("sidebar title = %q", got)
	}

	// /new clears it, resume rehydrates it.
	next, _ := m4.handleCommand("/new")
	if cleared := next.(model); cleared.tabTopic != "" {
		t.Fatalf("/new kept the topic: %q", cleared.tabTopic)
	}
	fresh := newTestModel(t, newFakeProvider())
	resumed, _ := fresh.resumeVirtualSession(sessionEntry{id: vsID, virtualSessionID: vsID})
	if got := resumed.(model).tabTopic; got != "memory design" {
		t.Fatalf("rehydrated topic = %q", got)
	}
	if args := resumed.(model).sessionArgs(); args.Topic != "memory design" {
		t.Fatalf("session args must carry the topic: %+v", args.Topic)
	}
}

// A finished turn is handed to the concept extractor: the extraction
// call's spend reaches the cost meter, its topic reaches the tab, and
// the concepts land in the store.
func TestAgentSession_TurnEnqueuesMemoryExtraction(t *testing.T) {
	_ = memory.Close()
	if err := memory.Open(memory.Options{DBPath: filepath.Join(t.TempDir(), "m.db"), Embedder: memory.NewFakeEmbedder(512)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.SetMemoryExtractor(nil)
		_ = memory.Close()
	})
	origBuilder := engine.ModelBuilder
	t.Cleanup(func() { engine.ModelBuilder = origBuilder })
	engine.ModelBuilder = func(_ context.Context, p providers.Provider, _ config.Config, modelID string) (adkmodel.LLM, error) {
		return &mockADKModel{name: modelID, generateFunc: func(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
			return func(yield func(*adkmodel.LLMResponse, error) bool) {
				yield(&adkmodel.LLMResponse{
					Content:       genai.NewContentFromText(`{"topic":"greetings","concepts":[{"op":"new","kind":"user","scope":"global","title":"user greets first","body":"Opens sessions with hi."}]}`, genai.RoleModel),
					UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1_000_000, CandidatesTokenCount: 10},
				}, nil)
			}
		}}, nil
	}
	ex := engine.NewMemoryExtractor(engine.MemoryExtractorOptions{LoadConfig: func() (config.Config, error) {
		return config.Config{Provider: providers.VertexProviderID}, nil
	}})
	engine.SetMemoryExtractor(ex)
	t.Cleanup(ex.Close)

	origStream := engine.GenerateStream
	t.Cleanup(func() { engine.GenerateStream = origStream })
	engine.GenerateStream = func(ctx context.Context, client *genai.Client, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(genaiTextChunk("Hello world", 12, 5), nil)
		}
	}

	s := newTestAgentSession(t, nil)
	if err := s.queueTurn("hi there"); err != nil {
		t.Fatal(err)
	}
	readSessionMsgs(t, s.ch, isTurnComplete)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ex.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	var gotTopic, gotCost bool
	deadline := time.After(5 * time.Second)
	for !gotTopic || !gotCost {
		select {
		case msg := <-s.ch:
			switch v := msg.(type) {
			case tabTopicMsg:
				gotTopic = v.topic == "greetings" && v.proc == s.proc
			case costMsg:
				gotCost = v.costUSD > 0 && v.proc == s.proc
			}
		case <-deadline:
			t.Fatalf("topic=%v cost=%v never arrived", gotTopic, gotCost)
		}
	}
	if s.currentTopic() != "greetings" {
		t.Fatalf("session topic = %q", s.currentTopic())
	}
	top, err := memory.Default().Top(context.Background(), s.args.Cwd, 5)
	if err != nil || len(top) != 1 || top[0].Title != "user greets first" || top[0].Scope != memory.ScopeGlobal {
		t.Fatalf("extracted concepts = %+v err=%v", top, err)
	}
	if names := memory.Default().TopicNames(context.Background(), s.args.Cwd, 5); strings.Join(names, ",") != "greetings" {
		t.Fatalf("topics = %v", names)
	}
	var _ tea.Msg = tabTopicMsg{}
}
