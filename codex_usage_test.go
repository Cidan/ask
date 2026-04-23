package main

import (
	"testing"
	"time"
)

func TestCodexEventToMsgs_RateLimitsUpdatedEmitsCodexUsage(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{
		"method": "account/rateLimits/updated",
		"params": {
			"rateLimits": {
				"primary":   {"usedPercent": 23, "resetsAt": 1776984000, "windowDurationMins": 300},
				"secondary": {"usedPercent": 3,  "resetsAt": 1777593600, "windowDurationMins": 10080},
				"planType": "plus",
				"limitId": "codex",
				"limitName": "Plus 5h"
			}
		}
	}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d: %+v", len(msgs), msgs)
	}
	u, ok := msgs[0].(codexUsageMsg)
	if !ok {
		t.Fatalf("want codexUsageMsg, got %T", msgs[0])
	}
	if u.proc != proc {
		t.Errorf("proc pointer not propagated")
	}
	if u.primary.usedPercent != 23 {
		t.Errorf("primary usedPercent=%d want 23", u.primary.usedPercent)
	}
	if u.secondary.usedPercent != 3 {
		t.Errorf("secondary usedPercent=%d want 3", u.secondary.usedPercent)
	}
	wantP := time.Unix(1776984000, 0).UTC()
	if !u.primary.resetsAt.Equal(wantP) {
		t.Errorf("primary resetsAt=%v want %v", u.primary.resetsAt, wantP)
	}
	wantS := time.Unix(1777593600, 0).UTC()
	if !u.secondary.resetsAt.Equal(wantS) {
		t.Errorf("secondary resetsAt=%v want %v", u.secondary.resetsAt, wantS)
	}
}

func TestCodexEventToMsgs_RateLimitsMissingSecondaryStillEmits(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{
		"method": "account/rateLimits/updated",
		"params": {"rateLimits": {"primary": {"usedPercent": 10}}}
	}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	u := msgs[0].(codexUsageMsg)
	if u.primary.usedPercent != 10 {
		t.Errorf("primary=%d want 10", u.primary.usedPercent)
	}
	if u.secondary.usedPercent != 0 {
		t.Errorf("missing secondary should be zero-valued, got %d", u.secondary.usedPercent)
	}
	if !u.secondary.resetsAt.IsZero() {
		t.Errorf("missing secondary resetsAt should be zero time")
	}
}

func TestCodexEventToMsgs_RateLimitsNullPayloadNoMsg(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"account/rateLimits/updated","params":{}}`)
	if msgs := codexEventToMsgs(ev, proc); len(msgs) != 0 {
		t.Errorf("missing rateLimits should emit nothing, got %+v", msgs)
	}
}

func TestCodexEventToMsgs_TokenUsageUpdatedEmitsCodexContext(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{
		"method": "thread/tokenUsage/updated",
		"params": {
			"threadId": "t1",
			"turnId": "r1",
			"tokenUsage": {
				"last":  {"inputTokens": 100, "outputTokens": 50, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 150},
				"total": {"inputTokens": 30000, "outputTokens": 5000, "cachedInputTokens": 0, "reasoningOutputTokens": 0, "totalTokens": 35000},
				"modelContextWindow": 400000
			}
		}
	}`)
	msgs := codexEventToMsgs(ev, proc)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	c, ok := msgs[0].(codexContextMsg)
	if !ok {
		t.Fatalf("want codexContextMsg, got %T", msgs[0])
	}
	if c.tokens != 35000 {
		t.Errorf("tokens=%d want 35000 (total.totalTokens)", c.tokens)
	}
	if c.window != 400000 {
		t.Errorf("window=%d want 400000", c.window)
	}
}

func TestCodexEventToMsgs_TokenUsageMissingWindowIsZero(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{
		"method": "thread/tokenUsage/updated",
		"params": {
			"threadId":"t","turnId":"r",
			"tokenUsage": {
				"last":  {"inputTokens":0,"outputTokens":0,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":0},
				"total": {"inputTokens":0,"outputTokens":0,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":200}
			}
		}
	}`)
	msgs := codexEventToMsgs(ev, proc)
	c := msgs[0].(codexContextMsg)
	if c.window != 0 {
		t.Errorf("missing modelContextWindow should be 0, got %d", c.window)
	}
	if c.tokens != 200 {
		t.Errorf("tokens=%d want 200", c.tokens)
	}
}

func TestCodexEventToMsgs_TokenUsageNullPayloadNoMsg(t *testing.T) {
	proc := &providerProc{}
	ev := parseCodexEvent(t, `{"method":"thread/tokenUsage/updated","params":{"threadId":"t","turnId":"r"}}`)
	if msgs := codexEventToMsgs(ev, proc); len(msgs) != 0 {
		t.Errorf("missing tokenUsage should emit nothing, got %+v", msgs)
	}
}
