package main

import (
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// This file implements the legacy process management helpers
// exclusively for the test suite.

func (m *model) ensureProc() error {
	if m.proc != nil {
		return nil
	}
	rootCwd, err := os.Getwd()
	if err != nil {
		return err
	}
	m.preMintNativeSessionIfNeeded()
	args := m.sessionArgs()
	args, m.worktreeName, err = prepareProviderSessionAt(args, m.worktreeName, rootCwd)
	if err != nil {
		return err
	}
	proc, ch, err := m.provider.StartSession(args)
	if err != nil {
		return err
	}
	m.proc = proc
	m.streamCh = ch
	m.sessionMinted = false
	return nil
}

func startAndSendProviderCmd(p Provider, args ProviderSessionArgs, worktreeName string, turn providerQueuedTurn, seq uint64) tea.Cmd {
	tabID := args.TabID
	providerID := p.ID()
	displayName := p.DisplayName()
	return func() tea.Msg {
		args, worktreeName, err := prepareProviderSession(args, worktreeName)
		if err != nil {
			return providerStartDoneMsg{
				tabID:      tabID,
				seq:        seq,
				providerID: providerID,
				err:        fmt.Errorf("could not start %s: %w", displayName, err),
				turn:       turn,
			}
		}
		proc, ch, err := p.StartSession(args)
		if err != nil {
			return providerStartDoneMsg{
				tabID:      tabID,
				seq:        seq,
				providerID: providerID,
				err:        fmt.Errorf("could not start %s: %w", displayName, err),
				turn:       turn,
			}
		}
		if err := p.Send(proc, turn.text, turn.attachments); err != nil {
			proc.kill()
			return providerStartDoneMsg{
				tabID:      tabID,
				seq:        seq,
				providerID: providerID,
				err:        fmt.Errorf("write to %s failed: %w", displayName, err),
				turn:       turn,
			}
		}
		return providerStartDoneMsg{
			tabID:        tabID,
			seq:          seq,
			providerID:   providerID,
			proc:         proc,
			streamCh:     ch,
			worktreeName: worktreeName,
			turn:         turn,
		}
	}
}

func (m model) handleProviderStartDone(msg providerStartDoneMsg) (tea.Model, tea.Cmd) {
	if msg.tabID != m.id {
		return m, nil
	}
	if !m.procStarting || msg.seq != m.procStartSeq ||
		m.provider == nil || msg.providerID != m.provider.ID() {
		if msg.proc != nil {
			msg.proc.kill()
		}
		return m, nil
	}

	m.procStarting = false
	if msg.err != nil {
		debugLog("provider start/send err: %v", msg.err)
		m.testBusy = false
		m.status = ""
		m.todos = nil
		m.queuedTurns = nil
		if len(msg.turn.attachments) > 0 && len(m.pending) == 0 {
			m.pending = append([]pendingAttachment(nil), msg.turn.attachments...)
		}
		m.appendHistory(outputStyle.Render(errStyle.Render(msg.err.Error())))
		return m, nil
	}

	m.proc = msg.proc
	m.streamCh = msg.streamCh
	m.worktreeName = msg.worktreeName
	m.testBusy = true
	m.status = "thinking…"

	if m.sessionID == "" {
		if id := m.provider.NativeSessionID(m.proc); id != "" {
			m.sessionID = id
			m.recordVirtualSession(id)
		}
	}

	queued := m.queuedTurns
	m.queuedTurns = nil
	for _, turn := range queued {
		if err := m.provider.Send(m.proc, turn.text, turn.attachments); err != nil {
			debugLog("provider queued send err: %v", err)
			m.appendHistory(outputStyle.Render(errStyle.Render("write to " + m.provider.DisplayName() + " failed: " + err.Error())))
			m.killProc()
			return m, nil
		}
	}
	if m.streamCh != nil {
		return m, nextStreamCmd(m.streamCh)
	}
	return m, nil
}

func cancelWatchdogCmd(p *providerProc) tea.Cmd {
	return tea.Tick(10*time.Second, func(time.Time) tea.Msg {
		return cancelWatchdogMsg{proc: p}
	})
}
