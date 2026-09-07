package tools

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Cidan/ask/pkg/render"
	"google.golang.org/adk/v2/agent"
)

type renderDesignParams struct {
	HTML        string `json:"html" jsonschema:"the HTML markup to render; a compact subset — block/flex layout with common tags and class/id/style attributes"`
	CSS         string `json:"css,omitempty" jsonschema:"the CSS stylesheet; supports type/class/id/descendant and grouped selectors over a box-model, flex, and text subset. <style> blocks inside the html are also honored"`
	Width       int    `json:"width,omitempty" jsonschema:"viewport width in CSS pixels (default 1200)"`
	Height      int    `json:"height,omitempty" jsonschema:"viewport height in CSS pixels; omit to fit the rendered content"`
	Scale       int    `json:"scale,omitempty" jsonschema:"device pixel ratio for crisp text, 1-4 (default 2)"`
	Background  string `json:"background,omitempty" jsonschema:"page background color when the CSS sets none (default #ffffff)"`
	Description string `json:"description" jsonschema:"one short human-readable phrase (under 10 words) telling the user what this call is doing"`
}

// RenderDesignResult is the render_design tool's response. The rendered image
// is fed to the model out of band via the ImageSink; this text reports the
// dimensions and anything the engine could not render.
type RenderDesignResult struct {
	Content string `json:"content"`
	Width   int    `json:"width,omitempty"`
	Height  int    `json:"height,omitempty"`
}

// RenderDesignTool renders HTML+CSS to a PNG the model can inspect. The image
// is queued on the shared sink so the ImageInjectionHook can hand it to the
// model before the next model call; the tool's own text result names the
// dimensions and lists whatever the compact engine ignored.
func RenderDesignTool(sink *ImageSink) Tool {
	return NewTypedTool(
		"render_design",
		"Render a compact subset of HTML + CSS to an image and see the result. Use it to evaluate color, layout, spacing, borders, and typography decisions visually before committing to them — a lightweight in-process design canvas, no browser. Supply html and css; the rendered PNG is fed back to you as an image. Supports block and single-line flex layout, margins/padding/borders/border-radius, backgrounds, colors (named, #hex, rgb/rgba), text color/size/weight/style/alignment, and compiled-in font families (sans, mono, medium).",
		func(ctx agent.Context, p renderDesignParams) (RenderDesignResult, error) {
			if strings.TrimSpace(p.HTML) == "" {
				return RenderDesignResult{}, errors.New("html is required")
			}
			res, err := render.Render(p.HTML, p.CSS, render.Options{
				Width:      p.Width,
				Height:     p.Height,
				Scale:      p.Scale,
				Background: p.Background,
			})
			if err != nil {
				return RenderDesignResult{}, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Rendered a %d×%d PNG — the image is attached below for you to inspect.", res.Width, res.Height)
			if len(res.Warnings) > 0 {
				b.WriteString("\n\nIgnored while rendering (unsupported):")
				for _, w := range res.Warnings {
					b.WriteString("\n  - " + w)
				}
			}
			sink.Add(PendingImage{
				Data:    res.PNG,
				MIME:    "image/png",
				Caption: fmt.Sprintf("render_design output (%d×%d):", res.Width, res.Height),
			})
			return RenderDesignResult{Content: b.String(), Width: res.Width, Height: res.Height}, nil
		},
	)
}
