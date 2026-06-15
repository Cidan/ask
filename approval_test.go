package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newApprovalModel returns a model with a live approval request wired
// up. The reply channel is buffered (1) so the approval handlers
// don't block — they send one value and clear the modal.
func newApprovalModel(t *testing.T, toolName string, input map[string]any) (model, chan approvalReply) {
	t.Helper()
	m := newTestModel(t, newFakeProvider())
	reply := make(chan approvalReply, 1)
	m = m.startApproval(approvalRequestMsg{
		tabID:     m.id,
		toolName:  toolName,
		input:     input,
		toolUseID: "test-tool-use-id",
		reply:     reply,
	})
	return m, reply
}

// TestStartApproval_SetsModeAndFields covers the contract: startApproval
// flips into modeApproval, copies the request's tool/input/reply, and
// resets the choice cursor to 0.
func TestStartApproval_SetsModeAndFields(t *testing.T) {
	reply := make(chan approvalReply, 1)
	m := newTestModel(t, newFakeProvider())
	got := m.startApproval(approvalRequestMsg{
		tabID:     m.id,
		toolName:  "Edit",
		input:     map[string]any{"file_path": "/tmp/x.go"},
		toolUseID: "id-1",
		reply:     reply,
	})
	if got.mode != modeApproval {
		t.Errorf("mode=%v want modeApproval", got.mode)
	}
	if got.approvalTool != "Edit" {
		t.Errorf("approvalTool=%q want Edit", got.approvalTool)
	}
	if got.approvalInput["file_path"] != "/tmp/x.go" {
		t.Errorf("approvalInput not copied; got %v", got.approvalInput)
	}
	if got.approvalReply == nil {
		t.Error("approvalReply should be wired up")
	}
	if got.approvalChoice != 0 {
		t.Errorf("approvalChoice=%d want 0", got.approvalChoice)
	}
}

// TestClearApproval_RestoresInputMode is the inverse of startApproval:
// every field is wiped, mode is back to modeInput, and the choice
// cursor is zeroed.
func TestClearApproval_RestoresInputMode(t *testing.T) {
	m, _ := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x"})
	m.approvalChoice = 2
	got := m.clearApproval()
	if got.mode != modeInput {
		t.Errorf("mode=%v want modeInput", got.mode)
	}
	if got.approvalTool != "" {
		t.Errorf("approvalTool should be empty; got %q", got.approvalTool)
	}
	if got.approvalInput != nil {
		t.Errorf("approvalInput should be nil; got %v", got.approvalInput)
	}
	if got.approvalReply != nil {
		t.Error("approvalReply should be nil after clear")
	}
	if got.approvalChoice != 0 {
		t.Errorf("approvalChoice=%d want 0", got.approvalChoice)
	}
}

// TestSendApproval_AllowSendsAllowTrue is the y/Enter-on-Allow path.
func TestSendApproval_AllowSendsAllowTrue(t *testing.T) {
	m, reply := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x"})
	m = m.sendApproval(approvalChoiceAllow)
	select {
	case got := <-reply:
		if !got.allow {
			t.Errorf("allow=false; want true")
		}
		if got.remember != nil {
			t.Errorf("allow should not produce a remember rule; got %+v", got.remember)
		}
	default:
		t.Fatal("allow reply not delivered")
	}
	if m.mode != modeInput {
		t.Errorf("mode after send should be modeInput; got %v", m.mode)
	}
}

// TestSendApproval_AlwaysProducesRememberRule is the 'a' path: the
// reply carries allow=true AND a permissionRule for the tool+input
// so the next invocation of the same tool is auto-approved.
func TestSendApproval_AlwaysProducesRememberRule(t *testing.T) {
	m, reply := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x/y.go"})
	m = m.sendApproval(approvalChoiceAlways)
	select {
	case got := <-reply:
		if !got.allow {
			t.Errorf("always-allow should set allow=true; got false")
		}
		if got.remember == nil {
			t.Fatal("always should populate remember rule")
		}
		if got.remember.toolName != "Edit" {
			t.Errorf("remember.toolName=%q want Edit", got.remember.toolName)
		}
		if got.remember.ruleContent != "/x/y.go" {
			t.Errorf("remember.ruleContent=%q want /x/y.go", got.remember.ruleContent)
		}
	default:
		t.Fatal("always reply not delivered")
	}
}

// TestSendApproval_DenySendsAllowFalse is the n/Esc path.
func TestSendApproval_DenySendsAllowFalse(t *testing.T) {
	m, reply := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x"})
	m = m.sendApproval(approvalChoiceDeny)
	select {
	case got := <-reply:
		if got.allow {
			t.Errorf("deny should set allow=false; got true")
		}
		if got.remember != nil {
			t.Errorf("deny should not produce a remember rule; got %+v", got.remember)
		}
	default:
		t.Fatal("deny reply not delivered")
	}
}

// TestUpdateApproval_PressKeys exercises the keymap. Each case
// expects (choice, reply) on the channel after one keystroke.
func TestUpdateApproval_PressKeys(t *testing.T) {
	cases := []struct {
		name     string
		keys     []tea.KeyPressMsg
		wantPick int
		wantAllow bool
		wantHasReply bool
	}{
		{"y allow", []tea.KeyPressMsg{{Code: 'y'}}, approvalChoiceAllow, true, true},
		{"n deny", []tea.KeyPressMsg{{Code: 'n'}}, approvalChoiceDeny, false, true},
		{"esc deny", []tea.KeyPressMsg{{Code: tea.KeyEsc}}, approvalChoiceDeny, false, true},
		{"a always", []tea.KeyPressMsg{{Code: 'a'}}, approvalChoiceAlways, true, true},
		{"right advances", []tea.KeyPressMsg{{Code: tea.KeyRight}}, 1, false, false},
		{"left clamps 0", []tea.KeyPressMsg{{Code: tea.KeyLeft}}, 0, false, false},
		{"tab cycles 0->1", []tea.KeyPressMsg{{Code: tea.KeyTab}}, 1, false, false},
		{"tab wraps 2->0", []tea.KeyPressMsg{{Code: tea.KeyRight}, {Code: tea.KeyRight}, {Code: tea.KeyTab}}, 0, false, false},
		{"enter on choice submits", []tea.KeyPressMsg{{Code: tea.KeyRight}, {Code: tea.KeyEnter}}, approvalChoiceAllow, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, reply := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x"})
			var lastCmd tea.Cmd
			for _, k := range tc.keys {
				mm, cmd := m.updateApproval(k)
				m = mm.(model)
				lastCmd = cmd
			}
			// Drain any commands the chain produced.
			_ = lastCmd
			if tc.wantHasReply {
				select {
				case got := <-reply:
					if got.allow != tc.wantAllow {
						t.Errorf("reply.allow=%v want %v", got.allow, tc.wantAllow)
					}
				default:
					t.Errorf("expected reply on channel; got none")
				}
			} else {
				select {
				case got := <-reply:
					t.Errorf("did NOT expect reply; got %+v", got)
				default:
				}
			}
			// If we expected a submission, the post-state should be
			// back in modeInput.
			if tc.wantHasReply && m.mode != modeInput {
				t.Errorf("after submission mode=%v want modeInput", m.mode)
			}
		})
	}
}

// TestUpdateApproval_CtrlCKillsProcAndAppendsCancelled: the brief
// says Ctrl+C sends deny, kills the proc, and appends a "cancelled"
// history entry — distinct from plain Esc which just denies.
func TestUpdateApproval_CtrlCKillsProcAndAppendsCancelled(t *testing.T) {
	m, reply := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x"})
	m.proc = &providerProc{stdin: &bufferCloser{}}
	mm, _ := m.updateApproval(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'c'})
	got := mm.(model)
	if got.proc != nil {
		t.Errorf("Ctrl+C should kill proc; got %+v", got.proc)
	}
	if len(got.history) != 1 {
		t.Fatalf("expected 1 history entry; got %d", len(got.history))
	}
	if !strings.Contains(stripAnsi(got.history[0].text), "cancelled") {
		t.Errorf("history should mention 'cancelled'; got %q", got.history[0].text)
	}
	select {
	case r := <-reply:
		if r.allow {
			t.Errorf("Ctrl+C should send deny; got allow=true")
		}
	default:
		t.Fatal("Ctrl+C should still send reply on channel")
	}
}

// TestUpdateApproval_CtrlDClosesTab: the modal's only way out
// without answering is Ctrl+D — same as every other surface.
func TestUpdateApproval_CtrlDClosesTab(t *testing.T) {
	m, _ := newApprovalModel(t, "Edit", map[string]any{"file_path": "/x"})
	_, cmd := m.updateApproval(tea.KeyPressMsg{Mod: tea.ModCtrl, Code: 'd'})
	if cmd == nil {
		t.Fatal("Ctrl+D should emit a closeTabCmd; got nil")
	}
}

// TestApprovalSummary covers every input shape the doc comment
// promises: file tools → file_path, Bash → command (with width
// clamp), Glob/Grep → pattern, WebFetch → url, WebSearch → query,
// Task → subagent_type, ApplyPatch/FileChange → file_path or
// reason, empty input → "(no arguments)", other → "(arguments
// hidden)".
func TestApprovalSummary(t *testing.T) {
	cases := []struct {
		name     string
		tool     string
		input    map[string]any
		wantText string // substring expected in the rendered output
	}{
		{"Edit file_path", "Edit", map[string]any{"file_path": "/x/y.go"}, "y.go"},
		{"Bash command", "Bash", map[string]any{"command": "ls -la"}, "ls -la"},
		{"Glob pattern", "Glob", map[string]any{"pattern": "*.go"}, "*.go"},
		{"Grep pattern", "Grep", map[string]any{"pattern": "TODO"}, "TODO"},
		{"WebFetch url", "WebFetch", map[string]any{"url": "https://example.com"}, "example.com"},
		{"WebSearch query", "WebSearch", map[string]any{"query": "how to foo"}, "how to foo"},
		{"Task subagent_type", "Task", map[string]any{"subagent_type": "explore"}, "explore"},
		{"ApplyPatch file_path", "ApplyPatch", map[string]any{"file_path": "/a/b"}, "b"},
		{"ApplyPatch reason", "ApplyPatch", map[string]any{"reason": "rename"}, "rename"},
		{"FileChange file_path", "FileChange", map[string]any{"file_path": "/a/b"}, "b"},
		{"empty input", "Bash", map[string]any{}, "(no arguments)"},
		{"unknown tool", "FutureTool", map[string]any{"x": 1}, "(arguments hidden)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalSummary(tc.tool, tc.input, 80)
			stripped := stripAnsi(got)
			if !strings.Contains(stripped, tc.wantText) {
				t.Errorf("approvalSummary(%s, ...)=%q missing %q", tc.tool, stripped, tc.wantText)
			}
		})
	}
}

// TestApprovalSummary_BashClampsToTwoLines covers the brief's width
// + max-lines clamp: a long multi-line Bash command gets truncated
// to two lines plus a trailing ellipsis line.
func TestApprovalSummary_BashClampsToTwoLines(t *testing.T) {
	multi := "echo one\necho two\necho three\necho four"
	got := approvalSummary("Bash", map[string]any{"command": multi}, 80)
	stripped := stripAnsi(got)
	lines := strings.Split(stripped, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines; got %d: %q", len(lines), stripped)
	}
	if !strings.Contains(stripped, "echo one") || !strings.Contains(stripped, "echo two") {
		t.Errorf("first two echo lines should be present; got %q", stripped)
	}
	if strings.Contains(stripped, "echo three") || strings.Contains(stripped, "echo four") {
		t.Errorf("echo three/four should be truncated; got %q", stripped)
	}
	if !strings.Contains(stripped, "…") {
		t.Errorf("overflow should render trailing ellipsis line; got %q", stripped)
	}
}

// TestTruncateFromLeft is the small-text utility: a wide-enough
// width returns input verbatim, narrower prepends an ellipsis and
// keeps the tail. Width<=0 returns ""; width==1 returns the last
// rune.
func TestTruncateFromLeft(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"width 0", "abcdef", 0, ""},
		{"width negative", "abcdef", -3, ""},
		{"fits", "abc", 10, "abc"},
		{"exactly fits", "abc", 3, "abc"},
		{"width 1 last rune", "abc", 1, "c"},
		{"width 2 ellipsis+last", "abcdef", 2, "…f"},
		{"width 4 ellipsis+last 3", "abcdef", 4, "…def"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateFromLeft(tc.in, tc.width); got != tc.want {
				t.Errorf("truncateFromLeft(%q, %d)=%q want %q", tc.in, tc.width, got, tc.want)
			}
		})
	}
}

// TestTruncateFromLeft_MultiByteSafe: lipgloss.Width treats runes
// consistently. Multi-byte runes count as 1 width, so a 2-rune
// string at width=2 should keep both (no truncation, no ellipsis).
func TestTruncateFromLeft_MultiByteSafe(t *testing.T) {
	in := "héllo" // 'é' is 2 bytes but 1 rune
	got := truncateFromLeft(in, 5)
	if got != in {
		t.Errorf("fits unchanged; got %q want %q", got, in)
	}
}

// TestFirstLinesClamped: maxLines<1 is clamped to 1; lines longer
// than `width` are width-truncated; over `maxLines` lines get a
// trailing `…` line.
func TestFirstLinesClamped(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		width    int
		maxLines int
		wantText string
	}{
		{"maxLines 0 clamped to 1", "a\nb\nc", 80, 0, "a"},
		{"under limit", "a\nb", 80, 5, "a"},
		{"exactly at limit", "a\nb\nc", 80, 3, "c"},
		{"over limit", "a\nb\nc\nd", 80, 2, "…"},
		{"wide line truncated", "averyverylongword", 5, 1, "aver…"}, // truncate adds ellipsis
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripAnsi(firstLinesClamped(tc.in, tc.width, tc.maxLines))
			if !strings.Contains(got, tc.wantText) {
				t.Errorf("firstLinesClamped(%q, %d, %d)=%q missing %q", tc.in, tc.width, tc.maxLines, got, tc.wantText)
			}
		})
	}
}
