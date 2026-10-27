package watcher

import (
	"fmt"
	"strings"
	"time"
)

// Prompt is the first message of a watcher's model run: its instructions,
// then what its script printed. A workflow target gets the same text as
// the run's source.
func Prompt(d Def, res ScriptResult, at time.Time) string {
	var b strings.Builder
	if instr := strings.TrimSpace(d.Instructions); instr != "" {
		b.WriteString("<instructions>\n")
		b.WriteString(instr)
		b.WriteString("\n</instructions>\n\n")
	}
	fmt.Fprintf(&b, "The watcher %q ran `%s` in %s at %s and printed:\n\n",
		d.Name, d.RunsLabel(), d.Cwd, at.Local().Format("2006-01-02 15:04 MST"))
	b.WriteString("<output>\n")
	b.WriteString(strings.TrimRight(res.Stdout, "\n"))
	b.WriteString("\n</output>")
	if res.StdoutBytes > len(res.Stdout) {
		fmt.Fprintf(&b, "\n\nThe output was %d bytes; the middle was cut to fit.", res.StdoutBytes)
	}
	return b.String()
}
