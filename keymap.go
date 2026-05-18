package main

import (
	"fmt"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// Action identifies a remappable global shortcut. Per-screen navigation
// keys (kanban j/k, workflow builder Tab, modal arrow keys) are
// deliberately *not* part of this surface — they stay inline in their
// own handlers. Adding a new global shortcut requires four edits:
//  1. New const here.
//  2. defaultKeyBindings entry.
//  3. actionGroups entry (label + group shown in /config).
//  4. KeyMap.Matches lookup at the dispatch site.
type Action string

// Ctrl+C and modal-escape Ctrl+D are deliberately *not* on this list.
// Ctrl+C is the universal cancel and every screen/modal needs it to
// always work; modal-escape Ctrl+D (the /config modal, provider
// switcher, approval, ask-question, ollama screen) stays hardcoded
// for the same reason — Esc already closes those, but Ctrl+D is the
// canonical exit so its dispatcher mustn't follow a rebound key into
// nowhere. Same for j/k/arrow navigation inside the kanban / workflow
// builder / pickers. ActionTabClose covers the tab-level close path
// (chat dispatcher, issues / workflows / workflow-run tabs, pickers,
// close-confirm bypass) — six sites that all want to follow the
// user's rebind.
const (
	ActionScreenIssues    Action = "screen.issues"
	ActionScreenPRs       Action = "screen.prs"
	ActionScreenWorkflows Action = "screen.workflows"
	ActionScreenAsk       Action = "screen.ask"
	ActionProviderSwitch  Action = "provider.switch"
	ActionChatWorkflow    Action = "chat.workflow"
	ActionReload          Action = "screen.reload"
	ActionTabNew          Action = "tab.new"
	ActionTabClose        Action = "tab.close"
	ActionTabPrev         Action = "tab.prev"
	ActionTabNext         Action = "tab.next"
	ActionAppSuspend      Action = "app.suspend"
)

// KeyBinding is a parsed Mod+Code pair. The zero value (Mod==0,
// Code==0) is "unbound" — Matches returns false, String returns "".
// Setting an action to the zero value in config disables it.
type KeyBinding struct {
	Mod  tea.KeyMod
	Code rune
}

const supportedKeyBindingMods = tea.ModCtrl | tea.ModAlt | tea.ModShift | tea.ModMeta | tea.ModHyper | tea.ModSuper

func (b KeyBinding) Matches(msg tea.KeyPressMsg) bool {
	if b.Code == 0 {
		return false
	}
	return msg.Mod == b.Mod && msg.Code == b.Code
}

func (b KeyBinding) String() string {
	if b.Code == 0 {
		return ""
	}
	if b.Mod&^supportedKeyBindingMods != 0 {
		return ""
	}
	var parts []string
	if b.Mod&tea.ModCtrl != 0 {
		parts = append(parts, "ctrl")
	}
	if b.Mod&tea.ModAlt != 0 {
		parts = append(parts, "alt")
	}
	if b.Mod&tea.ModShift != 0 {
		parts = append(parts, "shift")
	}
	if b.Mod&tea.ModMeta != 0 {
		parts = append(parts, "meta")
	}
	if b.Mod&tea.ModHyper != 0 {
		parts = append(parts, "hyper")
	}
	if b.Mod&tea.ModSuper != 0 {
		parts = append(parts, "super")
	}
	key := keyCodeName(b.Code)
	if key == "" {
		return ""
	}
	parts = append(parts, key)
	return strings.Join(parts, "+")
}

// keyCodeName renders a Code rune as the lowercase string we use in
// config files (and in the modal label). Names round-trip through
// namedKeyCodes — every name returned here must also parse.
func keyCodeName(c rune) string {
	if n, ok := codeToName[c]; ok {
		return n
	}
	if !utf8.ValidRune(c) || !unicode.IsPrint(c) {
		return ""
	}
	return strings.ToLower(string(c))
}

// namedKeyCodes maps the parseable string forms to bubbletea Code
// runes. Aliases (esc/escape, pgup/pageup, …) all resolve to the same
// rune so users can write whichever feels natural.
var namedKeyCodes = map[string]rune{
	"left":      tea.KeyLeft,
	"right":     tea.KeyRight,
	"up":        tea.KeyUp,
	"down":      tea.KeyDown,
	"enter":     tea.KeyEnter,
	"return":    tea.KeyEnter,
	"esc":       tea.KeyEsc,
	"escape":    tea.KeyEsc,
	"tab":       tea.KeyTab,
	"space":     tea.KeySpace,
	"plus":      '+',
	"home":      tea.KeyHome,
	"end":       tea.KeyEnd,
	"pgup":      tea.KeyPgUp,
	"pgdn":      tea.KeyPgDown,
	"pageup":    tea.KeyPgUp,
	"pagedown":  tea.KeyPgDown,
	"del":       tea.KeyDelete,
	"delete":    tea.KeyDelete,
	"backspace": tea.KeyBackspace,
	"insert":    tea.KeyInsert,
}

// codeToName is the canonical reverse — pick one display form per
// Code so String() output is stable. Aliases that share a Code (esc
// vs escape) only appear here under their canonical name.
var codeToName = map[rune]string{
	tea.KeyLeft:      "left",
	tea.KeyRight:     "right",
	tea.KeyUp:        "up",
	tea.KeyDown:      "down",
	tea.KeyEnter:     "enter",
	tea.KeyEsc:       "esc",
	tea.KeyTab:       "tab",
	tea.KeySpace:     "space",
	'+':              "plus",
	tea.KeyHome:      "home",
	tea.KeyEnd:       "end",
	tea.KeyPgUp:      "pgup",
	tea.KeyPgDown:    "pgdn",
	tea.KeyDelete:    "del",
	tea.KeyBackspace: "backspace",
	tea.KeyInsert:    "insert",
}

// ParseKeyBinding accepts "ctrl+w", "ctrl+shift+left", "f", or "" and
// returns the binding. Empty input is the zero value (unbound) — used
// to explicitly disable an action in config.
func ParseKeyBinding(s string) (KeyBinding, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return KeyBinding{}, nil
	}
	parts := strings.Split(s, "+")
	var mod tea.KeyMod
	var key string
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return KeyBinding{}, fmt.Errorf("empty token in %q", s)
		}
		switch p {
		case "ctrl", "control":
			mod |= tea.ModCtrl
		case "alt", "opt", "option":
			mod |= tea.ModAlt
		case "shift":
			mod |= tea.ModShift
		case "meta":
			mod |= tea.ModMeta
		case "hyper":
			mod |= tea.ModHyper
		case "super":
			mod |= tea.ModSuper
		default:
			if i != len(parts)-1 {
				return KeyBinding{}, fmt.Errorf("unknown modifier %q in %q", p, s)
			}
			key = p
		}
	}
	if key == "" {
		return KeyBinding{}, fmt.Errorf("missing key in %q", s)
	}
	if c, ok := namedKeyCodes[key]; ok {
		return KeyBinding{Mod: mod, Code: c}, nil
	}
	rs := []rune(key)
	if len(rs) != 1 {
		return KeyBinding{}, fmt.Errorf("unknown key %q in %q", key, s)
	}
	if rs[0] == 0 || !unicode.IsPrint(rs[0]) {
		return KeyBinding{}, fmt.Errorf("unknown key %q in %q", key, s)
	}
	return KeyBinding{Mod: mod, Code: rs[0]}, nil
}

// KeyMap is the resolved action → binding table used at dispatch.
// Always built via DefaultKeyMap() or LoadKeyMapFromConfig() so every
// known action has an entry (no nil-checks at lookup sites).
type KeyMap map[Action]KeyBinding

func DefaultKeyMap() KeyMap {
	out := make(KeyMap, len(defaultKeyBindings))
	for action, b := range defaultKeyBindings {
		out[action] = b
	}
	return out
}

var defaultKeyBindings = map[Action]KeyBinding{
	ActionScreenIssues:    {Mod: tea.ModCtrl, Code: 'i'},
	ActionScreenPRs:       {Mod: tea.ModCtrl, Code: 'p'},
	ActionScreenWorkflows: {Mod: tea.ModCtrl, Code: 'w'},
	ActionScreenAsk:       {Mod: tea.ModCtrl, Code: 'o'},
	ActionProviderSwitch:  {Mod: tea.ModCtrl, Code: 'b'},
	ActionChatWorkflow:    {Mod: tea.ModCtrl, Code: 'f'},
	ActionReload:          {Mod: tea.ModCtrl, Code: 'r'},
	ActionTabNew:          {Mod: tea.ModCtrl, Code: 't'},
	ActionTabClose:        {Mod: tea.ModCtrl, Code: 'd'},
	ActionTabPrev:         {Mod: tea.ModCtrl, Code: tea.KeyLeft},
	ActionTabNext:         {Mod: tea.ModCtrl, Code: tea.KeyRight},
	ActionAppSuspend:      {Mod: tea.ModCtrl, Code: 'z'},
}

func init() {
	for i := 1; i <= 63; i++ {
		name := fmt.Sprintf("f%d", i)
		code := tea.KeyF1 + rune(i-1)
		namedKeyCodes[name] = code
		codeToName[code] = name
	}
}

// Matches is the dispatch-site lookup. Missing actions fall back to
// the default so a partially-populated map still works.
func (k KeyMap) Matches(action Action, msg tea.KeyPressMsg) bool {
	if b, ok := k[action]; ok {
		return b.Matches(msg)
	}
	return defaultKeyBindings[action].Matches(msg)
}

// Binding returns the binding for action, falling back to the default
// when the map has no explicit entry. Used by the config modal to
// render the current value.
func (k KeyMap) Binding(action Action) KeyBinding {
	if b, ok := k[action]; ok {
		return b
	}
	return defaultKeyBindings[action]
}

// MarshalConfig returns the {action: "ctrl+w"} JSON shape, omitting
// entries that match the default so the on-disk file stays minimal
// for users on stock settings.
func (k KeyMap) MarshalConfig() map[string]string {
	out := map[string]string{}
	for action, b := range k {
		def, ok := defaultKeyBindings[action]
		if !ok {
			continue
		}
		if b == def {
			continue
		}
		out[string(action)] = b.String()
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LoadKeyMapFromConfig builds a KeyMap from the JSON shape. Unknown
// actions are skipped (forward-compat with future versions that
// retire an action). Malformed bindings are skipped with a debugLog
// warning so a typo in ask.json doesn't crash startup. The result
// always contains every default — overrides layer on top.
func LoadKeyMapFromConfig(raw map[string]string) KeyMap {
	km := DefaultKeyMap()
	for k, v := range raw {
		action := Action(k)
		if _, known := defaultKeyBindings[action]; !known {
			debugLog("ignoring unknown keybinding action %q", k)
			continue
		}
		b, err := ParseKeyBinding(v)
		if err != nil {
			debugLog("ignoring keybinding %s=%q: %v", k, v, err)
			continue
		}
		km[action] = b
	}
	return km
}

// actionMetaItem pairs an Action with the label the /config picker
// renders for it. Exported only as a shape — instances live inside
// actionGroups and the derived flat actionMeta.
type actionMetaItem struct {
	Action Action
	Label  string
}

// actionMetaGroup is one heading + its items in the /config picker.
// Adding a group is purely cosmetic: dispatch / persistence /
// cursor math all walk the flat actionMeta, which is derived from
// these groups by flattenActionGroups.
type actionMetaGroup struct {
	Heading string
	Items   []actionMetaItem
}

// actionGroups drives the /config keybindings picker layout. The
// heading order here is the render order; the item order inside each
// group is also the cursor walk order (Up/Down skip across group
// boundaries via the flattened actionMeta).
var actionGroups = []actionMetaGroup{
	{Heading: "Screens", Items: []actionMetaItem{
		{ActionScreenIssues, "Issues screen"},
		{ActionScreenPRs, "PRs screen"},
		{ActionScreenWorkflows, "Workflows screen"},
		{ActionScreenAsk, "Ask (chat) screen"},
	}},
	{Heading: "Tabs", Items: []actionMetaItem{
		{ActionTabNew, "New tab"},
		{ActionTabClose, "Close tab"},
		{ActionTabPrev, "Previous tab"},
		{ActionTabNext, "Next tab"},
	}},
	{Heading: "Pickers & dispatch", Items: []actionMetaItem{
		{ActionProviderSwitch, "Provider switch"},
		{ActionChatWorkflow, "Run workflow on chat"},
		{ActionReload, "Reload issues/PRs"},
	}},
	{Heading: "App", Items: []actionMetaItem{
		{ActionAppSuspend, "Suspend ask"},
	}},
}

// actionMeta is the flat in-order item list derived from actionGroups.
// The picker's cursor indexes into this slice and persistence code
// looks up actions by position here, so the picker can stay
// group-aware in rendering while every other site treats the action
// set as a flat ordered list.
var actionMeta = flattenActionGroups(actionGroups)

func flattenActionGroups(groups []actionMetaGroup) []actionMetaItem {
	var out []actionMetaItem
	for _, g := range groups {
		out = append(out, g.Items...)
	}
	return out
}

// keyHintFor returns the user-facing display form of action's current
// binding, suitable for inline use in a hint/toast string. Returns ""
// for unbound actions so callers can drop the surrounding clause via
// joinHintClauses (otherwise a rebound-to-empty action would print a
// dangling " · " or " opens the builder" with no key prefix).
func keyHintFor(action Action) string {
	return currentKeyMap().Binding(action).String()
}

// keyClause renders "<key> <suffix>" for an action, suitable for one
// segment of a · -joined hint footer. Returns "" when the action is
// unbound so joinHintClauses can drop the clause entirely — printing
// "  reload" with a leading-empty key would look broken.
func keyClause(action Action, suffix string) string {
	k := keyHintFor(action)
	if k == "" {
		return ""
	}
	return k + " " + suffix
}

// joinHintClauses joins the non-empty clauses with " · ", the
// canonical separator used by every hint footer in the app. Empty
// clauses (produced by keyClause / keyHintFor for unbound actions) are
// dropped instead of leaving a hanging separator.
func joinHintClauses(parts ...string) string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// workflowsBuilderHint is the toast/picker string shown when the user
// triggers a workflow action but no workflows are configured. When
// ActionScreenWorkflows is bound, the hint names the live shortcut so
// rebinds stay in sync; when unbound, it falls back to the slash
// command path, which is always available.
func workflowsBuilderHint() string {
	if k := keyHintFor(ActionScreenWorkflows); k != "" {
		return "no workflows configured · " + k + " opens the builder"
	}
	return "no workflows configured · open the builder from /workflows"
}

// Process-wide cached keymap. Dispatch happens on every keypress, so
// re-reading ~/.config/ask/ask.json each time would be wasteful;
// instead currentKeyMap() loads once lazily and invalidateKeyMapCache()
// drops the cache after a /config save. Cache is package-scoped because
// keybindings are global state — every tab in every goroutine sees the
// same map.
var (
	keyMapMu    sync.RWMutex
	keyMapCache KeyMap
)

func stripKeyLockModifiers(mod tea.KeyMod) tea.KeyMod {
	return mod &^ (tea.ModCapsLock | tea.ModNumLock | tea.ModScrollLock)
}

func normalizeKeyPressMsg(msg tea.KeyPressMsg) tea.KeyPressMsg {
	msg.Mod = stripKeyLockModifiers(msg.Mod)
	return msg
}

// currentKeyMap returns the process-wide keymap, loading from
// ~/.config/ask/ask.json on first call. Safe for concurrent use.
func currentKeyMap() KeyMap {
	keyMapMu.RLock()
	km := keyMapCache
	keyMapMu.RUnlock()
	if km != nil {
		return km
	}
	keyMapMu.Lock()
	if keyMapCache == nil {
		cfg, _ := loadConfig()
		keyMapCache = LoadKeyMapFromConfig(cfg.Keybindings)
	}
	km = keyMapCache
	keyMapMu.Unlock()
	return km
}

// invalidateKeyMapCache drops the cached keymap; the next
// currentKeyMap call re-reads from disk. Call this after writing
// cfg.Keybindings to the config file.
func invalidateKeyMapCache() {
	keyMapMu.Lock()
	keyMapCache = nil
	keyMapMu.Unlock()
}

// setKeyMapForTesting overrides the cache directly. Tests use this
// to install a custom keymap without touching disk; production code
// must not call it.
func setKeyMapForTesting(km KeyMap) {
	keyMapMu.Lock()
	keyMapCache = km
	keyMapMu.Unlock()
}
