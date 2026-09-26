package providers

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genai"
)

// A `claude -p` child holds its own conversation, and ask only ever sends it
// what ADK appended since the last call. Whenever the child has to start from
// history — a resumed or materialized session, or ask's compaction rewriting
// what the model sees — ask writes that history as a Claude Code transcript
// file and starts the child with `--resume <file>`. The child then holds the
// history as real tool_use / tool_result blocks, exactly as ask sends it to
// every other provider, rather than a text summary of it.

// ccMCPToolPrefix is how the child names ask's tools: ccArgv registers ask as
// the sdk MCP server "ask".
const ccMCPToolPrefix = "mcp__ask__"

// ccSeedNudge restarts a turn a rebuilt child was in the middle of. The child
// only runs on a user message, and a tool call left unanswered at the end of a
// seed is run again by the CLI rather than answered — so a mid-turn seed ends
// on the tool result and this line carries the turn on.
const ccSeedNudge = "[Context was compacted while you were mid-task. Your last tool call and its result are above; carry on with the task from there.]"

// ccSeedInterrupted answers a tool call the history left without a result (a
// turn abandoned mid-tool), which the API would otherwise reject.
const ccSeedInterrupted = "interrupted"

// ccSeedLine is one line of a Claude Code transcript: the fields the CLI's
// --resume loader needs (the uuid chain, a session id, a timestamp) and the
// message.
type ccSeedLine struct {
	Type        string        `json:"type"`
	UUID        string        `json:"uuid"`
	ParentUUID  *string       `json:"parentUuid"`
	IsSidechain bool          `json:"isSidechain"`
	SessionID   string        `json:"sessionId"`
	Timestamp   string        `json:"timestamp"`
	Cwd         string        `json:"cwd,omitempty"`
	Message     ccSeedMessage `json:"message"`
}

// ccSeedMessage is one Anthropic API message.
type ccSeedMessage struct {
	Role    string        `json:"role"`
	Content []ccSeedBlock `json:"content"`
}

// ccSeedBlock is one Anthropic content block: text, image, tool_use, or
// tool_result.
type ccSeedBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Source    *ccImageSource `json:"source,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     any            `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

// ccSeedMessages converts ADK contents into the Anthropic messages a child is
// seeded with.
//
// Thought parts are dropped: ask never keeps their signatures, and the API
// rejects an unsigned thinking block. Tool call ids are minted fresh and
// paired to their responses here — the seeded history never crosses the MCP
// bridge, and a history from another provider may carry no ids, or ids the
// API would not accept. Consecutive user contents become one message, with
// tool results first, the way the API expects a reply to a tool_use.
func ccSeedMessages(contents []*genai.Content) []ccSeedMessage {
	var out []ccSeedMessage
	var open []ccSeedCall
	minted := 0

	appendUser := func(blocks []ccSeedBlock) {
		if len(blocks) == 0 {
			return
		}
		n := len(out)
		if n == 0 || out[n-1].Role != "user" {
			out = append(out, ccSeedMessage{Role: "user", Content: blocks})
			return
		}
		var results, rest []ccSeedBlock
		for _, b := range append(out[n-1].Content, blocks...) {
			if b.Type == "tool_result" {
				results = append(results, b)
			} else {
				rest = append(rest, b)
			}
		}
		out[n-1].Content = append(results, rest...)
	}
	closeOpen := func() {
		var blocks []ccSeedBlock
		for _, c := range open {
			blocks = append(blocks, ccSeedBlock{Type: "tool_result", ToolUseID: c.id, Content: ccSeedInterrupted, IsError: true})
		}
		open = nil
		appendUser(blocks)
	}

	for _, c := range contents {
		if c == nil {
			continue
		}
		if isModelContent(c) {
			closeOpen()
			var blocks []ccSeedBlock
			for _, p := range c.Parts {
				switch {
				case p == nil || p.Thought:
				case p.FunctionCall != nil:
					minted++
					id := "toolu_ask_" + strconv.Itoa(minted)
					open = append(open, ccSeedCall{id: id, name: p.FunctionCall.Name, origID: p.FunctionCall.ID})
					var input any = map[string]any{}
					if p.FunctionCall.Args != nil {
						input = p.FunctionCall.Args
					}
					blocks = append(blocks, ccSeedBlock{Type: "tool_use", ID: id, Name: ccMCPToolPrefix + p.FunctionCall.Name, Input: input})
				case p.Text != "":
					blocks = append(blocks, ccSeedBlock{Type: "text", Text: p.Text})
				}
			}
			if len(blocks) > 0 {
				out = append(out, ccSeedMessage{Role: "assistant", Content: blocks})
			}
			continue
		}

		var results, rest []ccSeedBlock
		for _, p := range c.Parts {
			switch {
			case p == nil:
			case p.FunctionResponse != nil:
				id, ok := takeSeedCall(&open, p.FunctionResponse)
				if !ok {
					continue
				}
				text, isErr := ccToolResultText(p.FunctionResponse.Response)
				if text == "" {
					text = "(no output)"
				}
				results = append(results, ccSeedBlock{Type: "tool_result", ToolUseID: id, Content: text, IsError: isErr})
			case p.InlineData != nil && len(p.InlineData.Data) > 0:
				rest = append(rest, ccSeedBlock{Type: "image", Source: &ccImageSource{
					Type: "base64", MediaType: p.InlineData.MIMEType,
					Data: base64.StdEncoding.EncodeToString(p.InlineData.Data),
				}})
			case p.Text != "":
				rest = append(rest, ccSeedBlock{Type: "text", Text: p.Text})
			}
		}
		if len(results) == 0 && len(rest) > 0 {
			closeOpen()
		}
		appendUser(append(results, rest...))
	}
	closeOpen()
	return out
}

// ccSeedCall is a seeded tool_use still waiting for its result.
type ccSeedCall struct {
	id, name, origID string
}

// takeSeedCall pairs a function response with the open call it answers: by
// the original call id when both carry one, else the first open call of the
// same name, else the first open call. It reports false for a response whose
// call is not in the seed.
func takeSeedCall(open *[]ccSeedCall, fr *genai.FunctionResponse) (string, bool) {
	pick := -1
	for i, c := range *open {
		if fr.ID != "" && c.origID == fr.ID {
			pick = i
			break
		}
	}
	if pick < 0 {
		for i, c := range *open {
			if c.name == fr.Name {
				pick = i
				break
			}
		}
	}
	if pick < 0 && len(*open) > 0 {
		pick = 0
	}
	if pick < 0 {
		return "", false
	}
	id := (*open)[pick].id
	*open = append((*open)[:pick], (*open)[pick+1:]...)
	return id, true
}

// writeClaudeSeedFile writes contents as a Claude Code transcript to a
// uniquely named temp file and returns its path, or "" when contents carry no
// message. The child reads it once at startup; the caller removes it when the
// child is torn down.
func writeClaudeSeedFile(contents []*genai.Content, cwd string) (string, error) {
	msgs := ccSeedMessages(contents)
	if len(msgs) == 0 {
		return "", nil
	}
	f, err := os.CreateTemp("", "ask-claude-seed-*.jsonl")
	if err != nil {
		return "", fmt.Errorf("claude-code: create seed file: %w", err)
	}
	enc := json.NewEncoder(f)
	sessionID := uuid.NewString()
	start := time.Now().UTC()
	var parent *string
	for i, msg := range msgs {
		id := uuid.NewString()
		line := ccSeedLine{
			Type:       msg.Role,
			UUID:       id,
			ParentUUID: parent,
			SessionID:  sessionID,
			Timestamp:  start.Add(time.Duration(i) * time.Millisecond).Format("2006-01-02T15:04:05.000Z"),
			Cwd:        cwd,
			Message:    msg,
		}
		if err := enc.Encode(line); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return "", fmt.Errorf("claude-code: write seed file: %w", err)
		}
		parent = &id
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("claude-code: close seed file: %w", err)
	}
	return f.Name(), nil
}

// ccSplitSeed splits a request's history into what a fresh child is seeded
// with and the user content that starts its turn. input is nil when the
// history ends mid-turn (on a tool result); the caller sends ccSeedNudge.
func ccSplitSeed(contents []*genai.Content) (seed []*genai.Content, input *genai.Content) {
	if len(contents) == 0 {
		return nil, nil
	}
	last := contents[len(contents)-1]
	if isModelContent(last) || hasFunctionResponses(last) {
		return contents, nil
	}
	return contents[:len(contents)-1], last
}

// ccMark identifies one request content across rebuilds of the request: its
// key plus how many earlier contents share that key, since a history can
// repeat a message verbatim.
type ccMark struct {
	key     string
	ordinal int
}

// ccContentKey identifies a content by its role and first part. Only the
// first part counts: request hooks append parts to the latest user message on
// every call (the memory recall block), and that must not change its identity.
func ccContentKey(c *genai.Content) string {
	if c == nil {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(c.Role))
	var first *genai.Part
	for _, p := range c.Parts {
		if p != nil {
			first = p
			break
		}
	}
	switch {
	case first == nil:
	case first.FunctionCall != nil:
		h.Write([]byte("fc:" + first.FunctionCall.Name))
		if b, err := json.Marshal(first.FunctionCall.Args); err == nil {
			h.Write(b)
		}
	case first.FunctionResponse != nil:
		h.Write([]byte("fr:" + first.FunctionResponse.Name))
		if b, err := json.Marshal(first.FunctionResponse.Response); err == nil {
			h.Write(b)
		}
	case first.InlineData != nil:
		h.Write([]byte("img:" + strconv.Itoa(len(first.InlineData.Data))))
	default:
		if first.Thought {
			h.Write([]byte("th:"))
		}
		h.Write([]byte(first.Text))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func ccMarkAt(contents []*genai.Content, i int) ccMark {
	key := ccContentKey(contents[i])
	ordinal := 0
	for j := 0; j < i; j++ {
		if ccContentKey(contents[j]) == key {
			ordinal++
		}
	}
	return ccMark{key: key, ordinal: ordinal}
}

func ccLocateMark(contents []*genai.Content, m ccMark) (int, bool) {
	if m.key == "" {
		return 0, false
	}
	seen := 0
	for i := range contents {
		if ccContentKey(contents[i]) != m.key {
			continue
		}
		if seen == m.ordinal {
			return i, true
		}
		seen++
	}
	return 0, false
}
