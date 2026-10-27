package watcher

import "time"

// NextDue is when the watcher's next check is due: what the running window
// last computed, else what the schedule says. Zero for a paused watcher or
// an unparseable schedule.
func NextDue(d Def, st *Status) time.Time {
	if d.Paused {
		return time.Time{}
	}
	if st != nil && !st.NextRun.IsZero() {
		return st.NextRun
	}
	sch, err := ParseSchedule(d.Schedule)
	if err != nil {
		return time.Time{}
	}
	var lastEnded time.Time
	if st != nil {
		lastEnded = st.LastEnded
	}
	return sch.Next(lastEnded, d.CreatedAt)
}

// Summary describes what a watcher is doing, in a sentence, for the agent.
func Summary(d Def, st *Status) string {
	switch {
	case st != nil && st.Phase == PhaseScript:
		return "running its script now"
	case st != nil && st.Phase == PhaseApproval:
		return "its model run is waiting for the user to approve " + st.Approval
	case st != nil && st.Phase == PhaseModel:
		if st.Activity != "" {
			return "its model run is working: " + st.Activity
		}
		return "its model run is working"
	case d.Paused:
		return "paused"
	}
	s := "next check " + formatWhen(NextDue(d, st))
	if st == nil || st.LastCheck.IsZero() {
		return s + "; it has not run yet"
	}
	s += "; last check " + formatWhen(st.LastCheck)
	if st.LastNote != "" {
		return s + " (" + st.LastNote + ")"
	}
	if r, ok := st.LatestRun(); ok {
		switch r.Status {
		case RunFailed:
			return s + " (failed: " + r.Error + ")"
		case RunStopped:
			return s + " (stopped)"
		case RunDone:
			return s + " (printed output; the model handled it)"
		}
	}
	return s
}

func formatWhen(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
