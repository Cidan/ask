package workflow

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

var workflowSourceNonce atomic.Uint64

type SourceKind int

const (
	SourceKindIssue SourceKind = iota
	SourceKindChat
	SourceKindText
)

type ChatTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type Source struct {
	Kind           SourceKind `json:"kind"`
	IssueDisplay   string     `json:"issue_display,omitempty"`
	IssueKey       string     `json:"issue_key,omitempty"`
	ChatLabel      string     `json:"chat_label,omitempty"`
	ChatKey        string     `json:"chat_key,omitempty"`
	ChatTranscript []ChatTurn `json:"chat_transcript,omitempty"`
	TextLabel      string     `json:"text_label,omitempty"`
	TextKey        string     `json:"text_key,omitempty"`
	TextAppend     string     `json:"text_append,omitempty"`
}

func (s Source) Key() string {
	switch s.Kind {
	case SourceKindIssue:
		return s.IssueKey
	case SourceKindChat:
		return s.ChatKey
	case SourceKindText:
		return s.TextKey
	}
	return ""
}

func (s Source) Display() string {
	switch s.Kind {
	case SourceKindIssue:
		return s.IssueDisplay
	case SourceKindChat:
		return s.ChatLabel
	case SourceKindText:
		return s.TextLabel
	}
	return ""
}

func (s Source) RefBlock() string {
	switch s.Kind {
	case SourceKindIssue:
		if s.IssueDisplay == "" {
			return ""
		}
		return "Reference: " + s.IssueDisplay
	case SourceKindChat:
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
	case SourceKindText:
		body := strings.TrimSpace(s.TextAppend)
		if body == "" {
			return ""
		}
		return "Reference:\n" + body
	}
	return ""
}

func NewTextSource(originTabID int, appendText string) Source {
	body := strings.TrimSpace(appendText)
	label := fmt.Sprintf("text (%d chars)", len([]rune(body)))
	if body == "" {
		label = "text (empty)"
	}
	key := fmt.Sprintf("text:%d:%d:%d", originTabID, time.Now().UnixNano(), workflowSourceNonce.Add(1))
	return Source{
		Kind:       SourceKindText,
		TextLabel:  label,
		TextKey:    key,
		TextAppend: body,
	}
}

func ChatTurnCountLabel(n int) string {
	switch n {
	case 0:
		return "no turns"
	case 1:
		return "1 turn"
	default:
		return fmt.Sprintf("%d turns", n)
	}
}

func NewChatSource(originTabID int, turns []ChatTurn) Source {
	label := fmt.Sprintf("chat (%s)", ChatTurnCountLabel(len(turns)))
	key := fmt.Sprintf("chat:%d:%d:%d", originTabID, time.Now().UnixNano(), workflowSourceNonce.Add(1))
	return Source{
		Kind:           SourceKindChat,
		ChatLabel:      label,
		ChatKey:        key,
		ChatTranscript: turns,
	}
}
