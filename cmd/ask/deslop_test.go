package main

import (
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestDeslopBlockKeyTrims(t *testing.T) {
	// Live emit trims the joined block; /resume trims its reconstruction.
	// The key must ignore surrounding whitespace so the two match.
	if deslopBlockKey("hello world") != deslopBlockKey("  hello world\n") {
		t.Fatal("deslopBlockKey must ignore surrounding whitespace")
	}
	if deslopBlockKey("a") == deslopBlockKey("b") {
		t.Fatal("distinct text must produce distinct keys")
	}
}

func TestDeslopSidecarRoundTrip(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()

	// The sidecar path is derived from cwd, so a save then load with the
	// same cwd must round-trip, and merges must accumulate.
	if err := st.mergeDeslop("ses-1", cwd, map[string]string{"k1": "v1"}); err != nil {
		t.Fatalf("mergeDeslop: %v", err)
	}
	if err := st.mergeDeslop("ses-1", cwd, map[string]string{"k2": "v2"}); err != nil {
		t.Fatalf("mergeDeslop: %v", err)
	}
	got := st.loadDeslop("ses-1", cwd)
	if got["k1"] != "v1" || got["k2"] != "v2" {
		t.Fatalf("sidecar lost entries across merges: %+v", got)
	}
	if len(st.loadDeslop("missing", cwd)) != 0 {
		t.Fatal("absent sidecar must load as empty, not error")
	}
}

func TestLoadTranscriptAppliesDeslop(t *testing.T) {
	raw := "Here is the honest, rock-solid seam: FOO-1"
	cleaned := "The issue is FOO-1."
	deslop := map[string]string{deslopBlockKey(raw): cleaned}

	events := []*session.Event{
		{
			Author: "user",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText("find it", genai.RoleUser),
			},
			Timestamp: time.Now(),
		},
		{
			Author: "ask_coder",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText(raw, genai.RoleModel),
			},
			Timestamp: time.Now(),
		},
	}

	// With the sidecar, the assistant block shows the desloped text.
	items, err := loadTranscriptFromEvents(events, deslop)
	if err != nil {
		t.Fatalf("loadTranscriptFromEvents: %v", err)
	}
	if len(items) != 2 || items[1].kind != trAssistant {
		t.Fatalf("expected [user, assistant], got %+v", items)
	}
	if items[1].text != cleaned {
		t.Fatalf("assistant block should show desloped text, got %q", items[1].text)
	}

	// Without the sidecar, the raw text is shown unchanged — the session
	// data is never lost.
	rawItems, _ := loadTranscriptFromEvents(events, nil)
	if rawItems[1].text != raw {
		t.Fatalf("without sidecar the raw block must be shown, got %q", rawItems[1].text)
	}
}

func TestLoadTranscriptDeslopSurvivesResume(t *testing.T) {
	isolateHome(t)
	st := &agentSessionStore{provider: "vertex"}
	cwd := t.TempDir()

	raw := "belt and suspenders, to be honest"
	cleaned := "double-checked"

	events := []*session.Event{
		{
			Author: "ask_coder",
			LLMResponse: adkmodel.LLMResponse{
				Content: genai.NewContentFromText(raw, genai.RoleModel),
			},
			Timestamp: time.Now(),
		},
	}
	if err := st.saveEvents("ses-resume", cwd, events); err != nil {
		t.Fatalf("saveEvents: %v", err)
	}
	if err := st.mergeDeslop("ses-resume", cwd, map[string]string{deslopBlockKey(raw): cleaned}); err != nil {
		t.Fatalf("mergeDeslop: %v", err)
	}

	items, err := st.loadTranscript("ses-resume")
	if err != nil {
		t.Fatalf("loadTranscript: %v", err)
	}
	if len(items) != 1 || items[0].text != cleaned {
		t.Fatalf("resume must show the desloped block, got %+v", items)
	}
}

func TestDeslopToggleWritesConfig(t *testing.T) {
	isolateHome(t)
	m := newTestModel(t, newFakeProvider())

	res, _ := m.handleGlobalConfigEnter("deslop")
	if cfg, _ := loadConfig(); cfg.Deslop.Enabled == nil || !*cfg.Deslop.Enabled {
		t.Fatalf("first toggle should enable deslop: %+v", cfg.Deslop)
	}

	mm := res.(model)
	if _, _ = mm.handleGlobalConfigEnter("deslop"); true {
		if cfg, _ := loadConfig(); cfg.Deslop.Enabled == nil || *cfg.Deslop.Enabled {
			t.Fatalf("second toggle should disable deslop: %+v", cfg.Deslop)
		}
	}
}
