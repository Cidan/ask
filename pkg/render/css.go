package render

import (
	"io"
	"sort"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
)

var knownProps = map[string]bool{
	"display": true, "flex-direction": true, "justify-content": true,
	"align-items": true, "gap": true, "flex-grow": true, "flex": true,
	"width": true, "height": true, "min-width": true, "max-width": true,
	"margin": true, "margin-top": true, "margin-right": true, "margin-bottom": true, "margin-left": true,
	"padding": true, "padding-top": true, "padding-right": true, "padding-bottom": true, "padding-left": true,
	"box-sizing": true,
	"border":     true, "border-width": true, "border-style": true, "border-color": true,
	"border-top": true, "border-right": true, "border-bottom": true, "border-left": true, "border-radius": true,
	"background-color": true, "background": true,
	"color": true, "font-family": true, "font-size": true, "font-weight": true,
	"font-style": true, "line-height": true, "text-align": true,
}

var defaultTagStyles = map[string]map[string]string{
	"html":   {"display": "block"},
	"body":   {"display": "block", "margin": "0", "padding": "0"},
	"span":   {"display": "inline"},
	"a":      {"display": "inline"},
	"strong": {"display": "inline", "font-weight": "bold"},
	"b":      {"display": "inline", "font-weight": "bold"},
	"em":     {"display": "inline", "font-style": "italic"},
	"i":      {"display": "inline", "font-style": "italic"},
	"h1":     {"display": "block", "font-size": "2em", "font-weight": "bold"},
	"h2":     {"display": "block", "font-size": "1.5em", "font-weight": "bold"},
	"h3":     {"display": "block", "font-size": "1.17em", "font-weight": "bold"},
	"h4":     {"display": "block", "font-size": "1em", "font-weight": "bold"},
	"h5":     {"display": "block", "font-size": "0.83em", "font-weight": "bold"},
	"h6":     {"display": "block", "font-size": "0.67em", "font-weight": "bold"},
}

type simpleSel struct {
	universal bool
	typ       string
	id        string
	classes   []string
}

type complexSel struct {
	parts       []simpleSel // descendant chain; last element is the subject
	spec        [3]int
	unsupported bool
	raw         string
}

// cssDecl is a single property/value pair, decoupled from any parser AST.
type cssDecl struct {
	property string
	value    string
}

// parsedRule is one author rule read from a stylesheet. atRule, when set,
// marks an unsupported at-rule (its keyword, e.g. "@media"); such entries
// carry no selectors or declarations.
type parsedRule struct {
	atRule    string
	selectors []string
	decls     []cssDecl
}

type styleRule struct {
	sel   complexSel
	decls []cssDecl
	order int
}

func parseSelector(sel string) complexSel {
	raw := strings.TrimSpace(sel)
	cs := complexSel{raw: raw}
	if strings.ContainsAny(raw, ">+~[:") {
		cs.unsupported = true
		return cs
	}
	for _, tok := range strings.Fields(raw) {
		ss, ok := parseSimple(tok)
		if !ok {
			cs.unsupported = true
			return cs
		}
		cs.parts = append(cs.parts, ss)
	}
	if len(cs.parts) == 0 {
		cs.unsupported = true
		return cs
	}
	for _, p := range cs.parts {
		if p.id != "" {
			cs.spec[0]++
		}
		cs.spec[1] += len(p.classes)
		if p.typ != "" {
			cs.spec[2]++
		}
	}
	return cs
}

func parseSimple(tok string) (simpleSel, bool) {
	var ss simpleSel
	i := 0
	if tok == "*" {
		ss.universal = true
		return ss, true
	}
	// leading type
	for i < len(tok) && tok[i] != '.' && tok[i] != '#' {
		ss.typ += string(tok[i])
		i++
	}
	ss.typ = strings.ToLower(ss.typ)
	for i < len(tok) {
		switch tok[i] {
		case '.':
			i++
			start := i
			for i < len(tok) && tok[i] != '.' && tok[i] != '#' {
				i++
			}
			if i == start {
				return ss, false
			}
			ss.classes = append(ss.classes, tok[start:i])
		case '#':
			i++
			start := i
			for i < len(tok) && tok[i] != '.' && tok[i] != '#' {
				i++
			}
			if i == start {
				return ss, false
			}
			ss.id = tok[start:i]
		default:
			return ss, false
		}
	}
	return ss, true
}

func matchSimple(s simpleSel, b *box) bool {
	if s.typ != "" && s.typ != b.tag {
		return false
	}
	if s.id != "" && s.id != b.id {
		return false
	}
	for _, c := range s.classes {
		if !hasClass(b, c) {
			return false
		}
	}
	return true
}

func hasClass(b *box, c string) bool {
	for _, x := range b.classes {
		if x == c {
			return true
		}
	}
	return false
}

func matchComplex(cs complexSel, b *box) bool {
	if cs.unsupported || len(cs.parts) == 0 {
		return false
	}
	last := len(cs.parts) - 1
	if !matchSimple(cs.parts[last], b) {
		return false
	}
	cur := b.parent
	for i := last - 1; i >= 0; i-- {
		matched := false
		for cur != nil {
			if matchSimple(cs.parts[i], cur) {
				cur = cur.parent
				matched = true
				break
			}
			cur = cur.parent
		}
		if !matched {
			return false
		}
	}
	return true
}

// buildRules turns a parsed stylesheet into flat matchable rules and emits
// warnings for unsupported selectors and unknown properties.
func (e *engine) buildRules(sheet []parsedRule) []styleRule {
	var rules []styleRule
	order := 0
	for _, r := range sheet {
		if r.atRule != "" {
			e.warn("unsupported at-rule: " + r.atRule)
			continue
		}
		for _, d := range r.decls {
			if !knownProps[d.property] {
				e.warn("unsupported property: " + d.property)
			}
		}
		for _, selText := range r.selectors {
			cs := parseSelector(selText)
			if cs.unsupported {
				e.warn("unsupported selector: " + strings.TrimSpace(selText))
				continue
			}
			rules = append(rules, styleRule{sel: cs, decls: r.decls, order: order})
			order++
		}
	}
	return rules
}

// parseCSS reads a full stylesheet with the tdewolff streaming parser. It
// is best-effort: recoverable parse errors are skipped and reported through
// the returned error while the rules read so far are still returned.
func parseCSS(src string) ([]parsedRule, error) {
	p := css.NewParser(parse.NewInput(strings.NewReader(src)), false)
	var rules []parsedRule
	var cur parsedRule
	inRuleset := false
	atDepth := 0
	var perr error
	for {
		gt, _, data := p.Next()
		if atDepth > 0 {
			switch gt {
			case css.BeginAtRuleGrammar, css.BeginRulesetGrammar:
				atDepth++
			case css.EndAtRuleGrammar, css.EndRulesetGrammar:
				atDepth--
			case css.ErrorGrammar:
				if err := p.Err(); err != io.EOF {
					if perr == nil {
						perr = err
					}
					continue
				}
				return rules, perr
			}
			continue
		}
		switch gt {
		case css.ErrorGrammar:
			if err := p.Err(); err != io.EOF {
				if perr == nil {
					perr = err
				}
				continue
			}
			return rules, perr
		case css.AtRuleGrammar:
			rules = append(rules, parsedRule{atRule: string(data)})
		case css.BeginAtRuleGrammar:
			rules = append(rules, parsedRule{atRule: string(data)})
			atDepth = 1
		case css.BeginRulesetGrammar:
			cur = parsedRule{selectors: splitSelectors(p.Values())}
			inRuleset = true
		case css.DeclarationGrammar:
			if inRuleset {
				cur.decls = append(cur.decls, cssDecl{
					property: strings.ToLower(string(data)),
					value:    joinValues(p.Values()),
				})
			}
		case css.EndRulesetGrammar:
			if inRuleset {
				rules = append(rules, cur)
				inRuleset = false
			}
		}
	}
}

// parseInlineDecls reads a style="" attribute with the inline css parser.
func parseInlineDecls(s string) []cssDecl {
	p := css.NewParser(parse.NewInput(strings.NewReader(s)), true)
	var out []cssDecl
	for {
		gt, _, data := p.Next()
		if gt == css.ErrorGrammar {
			return out
		}
		if gt == css.DeclarationGrammar {
			out = append(out, cssDecl{
				property: strings.ToLower(string(data)),
				value:    joinValues(p.Values()),
			})
		}
	}
}

// splitSelectors reconstructs the selector-list text of a ruleset from its
// value tokens and splits it into individual selector strings.
func splitSelectors(vals []css.Token) []string {
	full := reconstructSelector(vals)
	var out []string
	for _, s := range strings.Split(full, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// reconstructSelector concatenates selector tokens back into text. The
// parser suppresses whitespace around combinators, so it is reinstated to
// preserve a readable form (e.g. "div > p").
func reconstructSelector(vals []css.Token) string {
	var sb strings.Builder
	for _, t := range vals {
		if len(t.Data) == 1 && (t.Data[0] == '>' || t.Data[0] == '+' || t.Data[0] == '~') {
			sb.WriteByte(' ')
			sb.Write(t.Data)
			sb.WriteByte(' ')
			continue
		}
		sb.Write(t.Data)
	}
	return sb.String()
}

// joinValues concatenates the value tokens of a declaration into a string.
func joinValues(vals []css.Token) string {
	var sb strings.Builder
	for _, t := range vals {
		sb.Write(t.Data)
	}
	return strings.TrimSpace(sb.String())
}

// styleFor computes the box's style by cascading defaults, matched author
// rules, and its inline style attribute onto the inherited parent style.
func (e *engine) styleFor(b *box, rules []styleRule, parent *computed) *computed {
	declared := map[string]string{}
	for k, v := range defaultTagStyles[b.tag] {
		declared[k] = v
	}

	var matched []styleRule
	for _, r := range rules {
		if matchComplex(r.sel, b) {
			matched = append(matched, r)
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		si, sj := matched[i].sel.spec, matched[j].sel.spec
		if si[0] != sj[0] {
			return si[0] < sj[0]
		}
		if si[1] != sj[1] {
			return si[1] < sj[1]
		}
		if si[2] != sj[2] {
			return si[2] < sj[2]
		}
		return matched[i].order < matched[j].order
	})
	for _, r := range matched {
		for _, d := range r.decls {
			if knownProps[d.property] {
				declared[d.property] = strings.TrimSpace(d.value)
			}
		}
	}

	if b.styleAttr != "" {
		for _, d := range parseInlineDecls(b.styleAttr) {
			if knownProps[d.property] {
				declared[d.property] = strings.TrimSpace(d.value)
			} else {
				e.warn("unsupported property: " + d.property)
			}
		}
	}

	return e.computeFromDeclared(declared, parent)
}
