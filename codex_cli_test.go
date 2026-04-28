package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestCodexCLIArgs_Defaults(t *testing.T) {
	want := []string{"app-server", "--listen", "stdio://"}
	got := codexCLIArgs(ProviderSessionArgs{})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codexCLIArgs(zero) = %v, want %v", got, want)
	}
}

func TestCodexCLIArgs_IgnoresArgsForNow(t *testing.T) {
	// MVP: config-option plumbing is deliberately out of scope. Prove every
	// populated field flows through unchanged so a future PR can plug in -c
	// overrides without hunting for surprise coupling.
	want := codexCLIArgs(ProviderSessionArgs{})
	got := codexCLIArgs(ProviderSessionArgs{
		Cwd:                "/tmp/x",
		MCPPort:            9999,
		Model:              "gpt-5",
		Effort:             "high",
		OllamaHost:         "localhost:11434",
		OllamaModel:        "llama3",
		SkipAllPermissions: true,
		Worktree:           true,
		SessionID:          "sess-1",
		ResumeCwd:          "/tmp/y",
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codexCLIArgs should ignore session args for MVP\n got=%v\nwant=%v", got, want)
	}
}

func TestCodexCLIArgs_UsesAppServerCommand(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{})
	if len(args) < 1 || args[0] != "app-server" {
		t.Fatalf("first arg must be the app-server subcommand (got %v)", args)
	}
}

func TestCodexCLIArgs_ListensOnStdio(t *testing.T) {
	// stdio:// is the default but we pass it explicitly so behavior can't
	// silently flip if the default changes upstream. This is what the MVP
	// relies on for the JSON-RPC pipe.
	args := codexCLIArgs(ProviderSessionArgs{})
	if argAfter(args, "--listen") != "stdio://" {
		t.Fatalf("--listen must be stdio://, got %v", args)
	}
}

func TestCodexCLIArgs_AddedDirsEmitWritableRootsOverride(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{
		AddedDirs: []string{"/a", "/b"},
	})
	got := argAfter(args, "-c")
	want := `sandbox_workspace_write.writable_roots=["/a","/b"]`
	if got != want {
		t.Fatalf("-c override = %q want %q; argv=%v", got, want, args)
	}
}

func TestCodexCLIArgs_AddedDirsAbsentOnEmpty(t *testing.T) {
	args := codexCLIArgs(ProviderSessionArgs{})
	if containsArg(args, "-c") {
		t.Errorf("empty AddedDirs should leave -c absent: %v", args)
	}
}

func TestCodexCLIArgs_AddedDirsQuoteEscaped(t *testing.T) {
	// Paths with special TOML characters must be quoted so codex's
	// parser doesn't choke. strconv.Quote handles \ and " for us.
	args := codexCLIArgs(ProviderSessionArgs{
		AddedDirs: []string{`/with "quotes"`, `/with\backslash`},
	})
	got := argAfter(args, "-c")
	if !strings.Contains(got, `"/with \"quotes\""`) || !strings.Contains(got, `"/with\\backslash"`) {
		t.Errorf("-c override should quote-escape paths; got %q; argv=%v", got, args)
	}
}
