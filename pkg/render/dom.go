package render

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

type box struct {
	tag       string
	id        string
	classes   []string
	styleAttr string

	parent   *box
	children []*box

	isText bool
	text   string
	disp   string // block, flex, inline, inline-block, break, text, none

	style *computed

	// layout results (CSS px, absolute; border-box top-left)
	x, y, w, h         float64
	mt, mr, mb, ml     float64
	pt, pr, pb, pl     float64
	bt, br, bb, bl     float64
	contentW, contentH float64
	runs               []textRun
}

var spanLike = map[string]bool{
	"span": true, "a": true, "strong": true, "em": true, "b": true, "i": true,
	"label": true, "small": true, "code": true, "u": true, "mark": true,
}

var skipTags = map[string]bool{
	"head": true, "script": true, "style": true, "title": true, "meta": true,
	"link": true, "base": true, "noscript": true, "template": true,
}

func collectStyleText(n *html.Node, out *strings.Builder) {
	if n.Type == html.ElementNode && n.DataAtom == atom.Style {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				out.WriteString(c.Data)
				out.WriteString("\n")
			}
		}
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectStyleText(c, out)
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func (e *engine) buildBox(n *html.Node, parent *box, rules []styleRule, parentStyle *computed) *box {
	switch n.Type {
	case html.TextNode:
		if parent == nil {
			return nil
		}
		b := &box{
			tag:    "#text",
			isText: true,
			text:   n.Data,
			disp:   "text",
			style:  parentStyle,
			parent: parent,
		}
		return b
	case html.ElementNode:
		tag := strings.ToLower(n.Data)
		if skipTags[tag] {
			return nil
		}
		b := &box{
			tag:       tag,
			id:        attr(n, "id"),
			styleAttr: attr(n, "style"),
			parent:    parent,
		}
		if cls := attr(n, "class"); cls != "" {
			b.classes = strings.Fields(cls)
		}
		b.style = e.styleFor(b, rules, parentStyle)
		if b.style.display == "none" {
			return nil
		}
		b.disp = e.resolveDisplay(tag, b.style.display)
		if tag == "br" {
			b.disp = "break"
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			child := e.buildBox(c, b, rules, b.style)
			if child != nil {
				b.children = append(b.children, child)
			}
		}
		return b
	default:
		return nil
	}
}

func (e *engine) resolveDisplay(tag, display string) string {
	switch display {
	case "block", "flex":
		return display
	case "inline", "inline-block":
		if spanLike[tag] {
			return "inline"
		}
		e.warn("inline/inline-block approximated as block")
		return "block"
	default:
		return "block"
	}
}
