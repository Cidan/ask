package workflow

import (
	"fmt"
	"strings"
)

type Scope string

const (
	ScopeUser   Scope = "user"
	ScopeRepo   Scope = "repo"
	ScopeGlobal Scope = "global"
	// ScopePlugin marks a workflow shipped by an installed plugin. It is
	// read-only: copy it into another scope to change it.
	ScopePlugin Scope = "plugin"
)

// Step represents a single agent step or a loop of inner steps.
type Step struct {
	Name          string `json:"name"`
	Kind          string `json:"kind,omitempty"` // "loop" or empty/omitted for standard step
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Prompt        string `json:"prompt,omitempty"`
	Steps         []Step `json:"steps,omitempty"`
	MaxIterations int    `json:"maxIterations,omitempty"`
	ExitCondition string `json:"exitCondition,omitempty"`
}

// Def defines a named sequence of workflow steps.
type Def struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       []Step `json:"steps"`
	Scope       Scope  `json:"-"`
	// Plugin is the "name@marketplace" that ships this workflow when
	// Scope == ScopePlugin.
	Plugin string `json:"-"`
}

// ReadOnly reports whether the workflow cannot be edited in place.
func (d Def) ReadOnly() bool { return d.Scope == ScopePlugin }

func (s Step) IsLoop() bool {
	return s.Kind == "loop"
}

func (d Def) EffectiveMaxIterations(s Step) int {
	if s.MaxIterations > 0 {
		return s.MaxIterations
	}
	return 20
}

func (d Def) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("workflow name is required")
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("workflow %q has no steps", d.Name)
	}
	for i, s := range d.Steps {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("step %d in workflow %q missing name", i+1, d.Name)
		}
		if s.Kind == "loop" {
			if len(s.Steps) == 0 {
				return fmt.Errorf("loop step %q in workflow %q has no inner steps", s.Name, d.Name)
			}
			for j, inner := range s.Steps {
				if strings.TrimSpace(inner.Name) == "" {
					return fmt.Errorf("inner step %d in loop %q missing name", j+1, s.Name)
				}
				if inner.Kind == "loop" {
					return fmt.Errorf("nested loops are not supported: %q", inner.Name)
				}
			}
		}
	}
	return nil
}
