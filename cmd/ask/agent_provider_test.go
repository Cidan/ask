package main

import (
	"context"
	"testing"

	"github.com/Cidan/ask/pkg/config"
	"github.com/Cidan/ask/pkg/engine"
	"github.com/Cidan/ask/pkg/providers"
	adkmodel "google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
)

func startArchiveSession(t *testing.T, args ProviderSessionArgs) *agentSession {
	t.Helper()
	proc, ch, err := agentAPIProvider{prov: windowProvider{id: "archtest", window: 8000}}.StartSession(args)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	// The session goroutine closes ch only after its last emit, which goes
	// through a package global the next test may swap.
	t.Cleanup(func() {
		proc.stdin.Close()
		for range ch {
		}
	})
	return proc.payload.(*agentSession)
}

// A tab's coder keeps its history next to its transcript — for a new
// session where the transcript will be written, for a resumed one wherever
// it already lives — so /resume brings the archive back with the session.
func TestStartSession_ArchivesNextToTheTranscript(t *testing.T) {
	isolateHome(t)
	prev := engine.ModelBuilder
	engine.ModelBuilder = func(context.Context, providers.Provider, config.Config, string) (adkmodel.LLM, error) {
		return &mockADKModel{name: "m"}, nil
	}
	t.Cleanup(func() { engine.ModelBuilder = prev })

	cwd := t.TempDir()
	fresh := startArchiveSession(t, ProviderSessionArgs{Cwd: cwd, TabID: 1, NewSessionID: "fresh-session"})
	want, err := engine.HistoryArchivePath(engine.NewFileSessionService("archtest", cwd), cwd, "fresh-session")
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.compactor.History(); got == nil || got.Path() != want {
		t.Fatalf("new session archive at %v, want %q", got, want)
	}

	elsewhere := t.TempDir()
	svc := engine.NewFileSessionService("archtest", elsewhere)
	if _, err := svc.Create(context.Background(), &adksession.CreateRequest{AppName: "ask", UserID: "user", SessionID: "old-session"}); err != nil {
		t.Fatal(err)
	}
	transcript, err := svc.PathFor("old-session")
	if err != nil {
		t.Fatal(err)
	}
	resumed := startArchiveSession(t, ProviderSessionArgs{Cwd: cwd, TabID: 2, SessionID: "old-session"})
	if got := resumed.compactor.History(); got == nil || got.Path() != transcript[:len(transcript)-len(".json")]+".history.db" {
		t.Fatalf("resumed session archive at %v, want next to %q", got, transcript)
	}
}
