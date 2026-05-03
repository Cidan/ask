package main

import (
	"fmt"
	"strings"
	"time"
)

// workflowSourceKind tags which payload the source carries. The
// runtime machinery (picker, tab, runner, banner) is source-agnostic;
// each kind contributes its own Key/Display/RefBlock so the chain
// rendering stays in one place.
type workflowSourceKind int

const (
	workflowSourceIssue workflowSourceKind = iota
	workflowSourceChat
)

// chatTurn is one filtered entry in a chat-source transcript. Role is
// "user" or "assistant"; text is the trimmed message body. Tool/system
// entries are dropped during construction so this list is what the
// agent actually sees.
type chatTurn struct {
	Role string
	Text string
}

// workflowSource is the input handed to a workflow run. It carries
// either an issue ref (the original GitHub-style flow) or a chat
// transcript snapshot (the Ctrl+F flow). Picker/runner/banner code
// reads through Key/Display/RefBlock so adding a third kind in the
// future is one switch arm per accessor and nothing else.
//
// The struct is intentionally not directly comparable (the chat
// transcript holds a slice). Equality checks in tests should compare
// individual fields (Kind, Issue, ChatKey, …) rather than the whole
// value.
type workflowSource struct {
	Kind workflowSourceKind

	Issue issueRef

	ChatLabel      string
	ChatKey        string
	ChatTranscript []chatTurn
}

// Key returns the canonical session-map key for the source. Issue
// sources reuse issueRef.Key (so disk-persisted entries from before
// the abstraction continue to round-trip); chat sources use the
// pre-computed ChatKey assigned at construction so two consecutive
// runs against the same tab don't collide.
func (s workflowSource) Key() string {
	switch s.Kind {
	case workflowSourceIssue:
		return s.Issue.Key()
	case workflowSourceChat:
		return s.ChatKey
	}
	return ""
}

// Display is the short user-facing label shown in the picker title
// and the workflow tab's banner. Issue sources stay on the existing
// "<project>#<n>" shape; chat sources show "chat (N turn(s))".
func (s workflowSource) Display() string {
	switch s.Kind {
	case workflowSourceIssue:
		return s.Issue.Display()
	case workflowSourceChat:
		return s.ChatLabel
	}
	return ""
}

// RefBlock is the prompt fragment the runner injects between the
// step's user-authored body and the optional previous-step output.
// Issue sources emit "Reference: <project>#<n>" verbatim (matching
// the original prompt shape so existing workflows keep working).
// Chat sources emit a multi-line transcript block. Empty string
// means "skip the reference section entirely" (used when the chat
// is empty or the source is unset in tests).
func (s workflowSource) RefBlock() string {
	switch s.Kind {
	case workflowSourceIssue:
		return "Reference: " + s.Issue.Display()
	case workflowSourceChat:
		if len(s.ChatTranscript) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("Reference (chat transcript):")
		for i, t := range s.ChatTranscript {
			if i == 0 {
				b.WriteString("\n")
			} else {
				b.WriteString("\n---\n")
			}
			b.WriteString(t.Role)
			b.WriteString(": ")
			b.WriteString(strings.TrimSpace(t.Text))
		}
		return b.String()
	}
	return ""
}

// issueWorkflowSource wraps an issueRef into a source so callers that
// already produced an issueRef (the issues-screen `f` path) don't have
// to construct the struct by hand.
func issueWorkflowSource(ref issueRef) workflowSource {
	return workflowSource{Kind: workflowSourceIssue, Issue: ref}
}

// chatWorkflowSource builds a chat-flavoured source from the tab's
// history. Walks `history` in order and keeps only `histUser` /
// `histResponse` entries — `histPrerendered` (tool calls, results,
// status banners, shell output, info messages) is dropped so the
// agent only sees real conversation turns. Empty messages (after
// whitespace trim) are skipped too. The Key embeds the spawning
// tabID plus the spawn timestamp so two Ctrl+F presses on the same
// tab produce distinct tracker entries instead of stomping each
// other.
func chatWorkflowSource(tabID int, history []historyEntry) workflowSource {
	turns := chatTurnsFromHistory(history)
	label := fmt.Sprintf("chat (%s)", chatTurnCountLabel(len(turns)))
	key := fmt.Sprintf("chat:%d:%d", tabID, time.Now().UnixNano())
	return workflowSource{
		Kind:           workflowSourceChat,
		ChatLabel:      label,
		ChatKey:        key,
		ChatTranscript: turns,
	}
}

// chatTurnsFromHistory is the pure filter used by chatWorkflowSource;
// pulled out so tests can exercise the filter without driving the
// timestamped key construction.
func chatTurnsFromHistory(history []historyEntry) []chatTurn {
	var out []chatTurn
	for _, e := range history {
		var role string
		switch e.kind {
		case histUser:
			role = "user"
		case histResponse:
			role = "assistant"
		default:
			continue
		}
		txt := strings.TrimSpace(e.text)
		if txt == "" {
			continue
		}
		out = append(out, chatTurn{Role: role, Text: txt})
	}
	return out
}

// chatTurnCountLabel returns "no turns", "1 turn", or "<N> turns" for
// the chat source's display label. Pulled out so the picker title and
// the banner pick up identical wording.
func chatTurnCountLabel(n int) string {
	switch n {
	case 0:
		return "no turns"
	case 1:
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
}
