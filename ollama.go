package main

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

const ollamaModelOption = "ollama (configure...)"

const ollamaBoxWidth = 58

func (m model) cursorOnOllamaConfig() bool {
	if m.askMode != askForModel || m.isOnConfirmTab() {
		return false
	}
	q := m.askQuestions[m.askTab]
	if m.askCursor < 0 || m.askCursor >= len(q.options) {
		return false
	}
	return q.options[m.askCursor] == ollamaModelOption
}

func (m model) startAskOllamaConfig() model {
	cfg, _ := loadConfig()
	m.askOllamaActive = true
	m.askOllamaField = 0
	m.askOllamaHost = cfg.Claude.Ollama.Host
	m.askOllamaModel = cfg.Claude.Ollama.Model
	m.askOllamaErr = ""
	return m
}

func (m model) clearAskOllamaConfig() model {
	m.askOllamaActive = false
	m.askOllamaField = 0
	m.askOllamaHost = ""
	m.askOllamaModel = ""
	m.askOllamaErr = ""
	return m
}

func (m model) updateAskOllamaConfig(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.Mod == tea.ModCtrl && msg.Code == 'd' {
		return m, closeTabCmd(m.id)
	}
	switch {
	case msg.Mod == tea.ModCtrl && msg.Code == 'c':
		return m.clearAskOllamaConfig(), nil
	case msg.Code == tea.KeyEsc:
		return m.clearAskOllamaConfig(), nil
	case msg.Code == tea.KeyTab, msg.Code == tea.KeyDown, msg.Code == tea.KeyUp:
		m.askOllamaField = 1 - m.askOllamaField
		return m, nil
	case msg.Code == tea.KeyEnter:
		return m.saveOllamaConfig()
	case msg.Code == tea.KeyBackspace:
		if m.askOllamaField == 0 && m.askOllamaHost != "" {
			r := []rune(m.askOllamaHost)
			m.askOllamaHost = string(r[:len(r)-1])
		} else if m.askOllamaField == 1 && m.askOllamaModel != "" {
			r := []rune(m.askOllamaModel)
			m.askOllamaModel = string(r[:len(r)-1])
		}
		m.askOllamaErr = ""
		return m, nil
	}
	if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
		if m.askOllamaField == 0 {
			m.askOllamaHost += msg.Text
		} else {
			m.askOllamaModel += msg.Text
		}
		m.askOllamaErr = ""
		return m, nil
	}
	return m, nil
}

func (m model) saveOllamaConfig() (tea.Model, tea.Cmd) {
	host := strings.TrimSpace(m.askOllamaHost)
	modelName := strings.TrimSpace(m.askOllamaModel)
	if err := validateOllamaHost(host); err != nil {
		m.askOllamaErr = err.Error()
		m.askOllamaField = 0
		return m, nil
	}
	if modelName == "" {
		m.askOllamaErr = "model is required"
		m.askOllamaField = 1
		return m, nil
	}
	cfg, _ := loadConfig()
	cfg.Claude.Ollama.Host = host
	cfg.Claude.Ollama.Model = modelName
	if err := saveConfig(cfg); err != nil {
		debugLog("saveConfig err: %v", err)
		m.askOllamaErr = "could not save: " + err.Error()
		return m, nil
	}
	m.ollamaHost = host
	m.ollamaModel = modelName
	m = m.clearAskOllamaConfig()
	if m.askMode == askForModel && len(m.askQuestions) > 0 && len(m.askAnswers) > 0 {
		for i, opt := range m.askQuestions[0].options {
			if opt == ollamaModelOption {
				m.askAnswers[0].picks = map[int]bool{i: true}
				m.askCursor = i
				break
			}
		}
	}
	m = m.advanceAskTab()
	if m.askMode == askForModel && m.isOnConfirmTab() {
		return m.submitAsk()
	}
	return m, nil
}

func ollamaBaseURL(host string) string {
	s := strings.TrimSpace(host)
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return s
	}
	return "http://" + s
}

func validateOllamaHost(s string) error {
	if s == "" {
		return errors.New("host is required")
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return errors.New("invalid URL")
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("invalid port")
			}
		}
		return nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return errors.New("use host:port or https://url")
	}
	if host == "" {
		return errors.New("missing host")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("invalid port")
	}
	return nil
}

func (m model) viewAskOllamaConfig() string {
	innerW := ollamaBoxWidth - 6
	if innerW > m.width-6 {
		innerW = m.width - 6
	}
	if innerW < 30 {
		innerW = 30
	}

	title := ollamaTitleStyle.Render("Configure Ollama")

	hostField := renderOllamaField(m.askOllamaHost, m.askOllamaField == 0, "localhost:11434 or https://host:port")
	modelField := renderOllamaField(m.askOllamaModel, m.askOllamaField == 1, "llama3.2")

	hostArrow := "  "
	if m.askOllamaField == 0 {
		hostArrow = ollamaActiveArrow.Render("› ")
	}
	modelArrow := "  "
	if m.askOllamaField == 1 {
		modelArrow = ollamaActiveArrow.Render("› ")
	}

	parts := []string{
		title,
		"",
		ollamaLabelStyle.Render("Host"),
		hostArrow + hostField,
		"",
		ollamaLabelStyle.Render("Model"),
		modelArrow + modelField,
	}
	if m.askOllamaErr != "" {
		parts = append(parts, "", ollamaErrStyle.Render("✗ "+m.askOllamaErr))
	}
	parts = append(parts, "", ollamaHelpStyle.Render("↑↓/tab switch · enter save · esc cancel"))

	return ollamaBoxStyle.Width(innerW).Render(strings.Join(parts, "\n"))
}

func renderOllamaField(value string, active bool, placeholder string) string {
	if value == "" {
		if active {
			return askPlaceholder.Render(placeholder) + askCaretStyle.Render("▏")
		}
		return askPlaceholder.Render(placeholder)
	}
	if active {
		return value + askCaretStyle.Render("▏")
	}
	return value
}
