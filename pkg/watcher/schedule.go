package watcher

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MinInterval is the shortest interval a schedule may name.
	MinInterval = 10 * time.Second
	// MaxInterval is the longest.
	MaxInterval = 30 * 24 * time.Hour
)

// Schedule is a parsed watcher schedule, in one of three shapes:
//
//	every 5m                        an interval, counted from the end of the last run
//	every 10m weekdays 09:00-18:00  an interval that only runs inside those days and hours
//	daily 09:00                     times of day ("weekdays", "weekends", "mon,thu", "mon-fri" too)
//	hourly :15,:45                  minutes past every hour
//
// Times are local. Whatever the shape, a run is never due until the
// previous one has ended: an interval counts from that end, and a time of
// day is the first one after it.
type Schedule struct {
	interval time.Duration
	days     [7]bool
	allDays  bool
	// window bounds an interval to hours of the day, in minutes since
	// midnight; end <= start crosses midnight. hasWindow is false for an
	// interval that runs at any hour.
	hasWindow   bool
	windowStart int
	windowEnd   int
	// times are a calendar schedule's times of day, minutes since midnight.
	times []int
	// minutes are an hourly schedule's minutes past the hour.
	minutes []int
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday,
	"mon": time.Monday, "monday": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday,
	"fri": time.Friday, "friday": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday,
}

var weekdayShort = [7]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

var (
	commaSpaceRE = regexp.MustCompile(`\s*,\s*`)
	dashSpaceRE  = regexp.MustCompile(`\s*-\s*`)
	clockRE      = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?(am|pm)?$`)
)

func scheduleError(s string) error {
	return fmt.Errorf(`unknown schedule %q: try "every 5m", "every 2h", "every 10m weekdays 09:00-18:00", "daily 09:00", "weekdays 08:30,13:00", "mon,thu 14:30", or "hourly :15"`, s)
}

// ParseSchedule parses a schedule string. See Schedule for the grammar.
func ParseSchedule(s string) (Schedule, error) {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = commaSpaceRE.ReplaceAllString(norm, ",")
	norm = dashSpaceRE.ReplaceAllString(norm, "-")
	var fields []string
	for _, f := range strings.Fields(norm) {
		switch f {
		case "at", "on", "every", "between":
			if f == "every" && len(fields) == 0 {
				fields = append(fields, f)
			}
			continue
		}
		fields = append(fields, f)
	}
	if len(fields) == 0 {
		return Schedule{}, scheduleError(s)
	}
	var sch Schedule
	var err error
	switch {
	case fields[0] == "hourly":
		sch, err = parseHourly(fields[1:])
	case fields[0] == "every":
		sch, err = parseEvery(fields[1:])
	default:
		if _, _, ok := parseDuration(fields); ok {
			sch, err = parseEvery(fields)
		} else {
			sch, err = parseCalendar(fields)
		}
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "unknown schedule") {
			return Schedule{}, scheduleError(s)
		}
		return Schedule{}, err
	}
	return sch, nil
}

// parseEvery parses what follows "every": a duration with optional days
// and hours, or a day spec followed by times ("every day 09:00", "every
// monday 10:00"), which is a calendar schedule.
func parseEvery(fields []string) (Schedule, error) {
	if len(fields) == 0 {
		return Schedule{}, scheduleError("")
	}
	switch fields[0] {
	case "minute":
		fields = append([]string{"1m"}, fields[1:]...)
	case "hour":
		fields = append([]string{"1h"}, fields[1:]...)
	case "day":
		if len(fields) == 1 {
			fields = []string{"24h"}
		} else {
			return parseCalendar(append([]string{"daily"}, fields[1:]...))
		}
	}
	d, used, ok := parseDuration(fields)
	if !ok {
		return parseCalendar(fields)
	}
	if d < MinInterval {
		return Schedule{}, fmt.Errorf("schedule interval %s is too short: the minimum is %s", formatDuration(d), formatDuration(MinInterval))
	}
	if d > MaxInterval {
		return Schedule{}, fmt.Errorf("schedule interval %s is too long: the maximum is %s", formatDuration(d), formatDuration(MaxInterval))
	}
	sch := Schedule{interval: d}
	rest := fields[used:]
	sch.days, sch.allDays = allWeek(), true
	if len(rest) > 0 {
		if days, ok := parseDays(rest[0]); ok {
			sch.days, sch.allDays = days, days == allWeek()
			rest = rest[1:]
		}
	}
	if len(rest) > 0 {
		start, end, ok := strings.Cut(rest[0], "-")
		if !ok {
			return Schedule{}, scheduleError("")
		}
		a, okA := parseClock(start)
		b, okB := parseClock(end)
		if !okA || !okB {
			return Schedule{}, scheduleError("")
		}
		if a == b {
			return Schedule{}, fmt.Errorf("schedule hours %s-%s are empty", formatClock(a), formatClock(b))
		}
		sch.hasWindow, sch.windowStart, sch.windowEnd = true, a, b
		rest = rest[1:]
	}
	if len(rest) > 0 {
		return Schedule{}, scheduleError("")
	}
	return sch, nil
}

func parseCalendar(fields []string) (Schedule, error) {
	if len(fields) == 0 {
		return Schedule{}, scheduleError("")
	}
	days, ok := parseDays(fields[0])
	if !ok {
		return Schedule{}, scheduleError("")
	}
	if len(fields) != 2 {
		if len(fields) == 1 {
			return Schedule{}, fmt.Errorf("schedule %q needs a time of day, e.g. %q", fields[0], fields[0]+" 09:00")
		}
		return Schedule{}, scheduleError("")
	}
	var times []int
	for _, t := range strings.Split(fields[1], ",") {
		c, ok := parseClock(t)
		if !ok {
			return Schedule{}, scheduleError("")
		}
		times = append(times, c)
	}
	return Schedule{days: days, allDays: days == allWeek(), times: uniqSorted(times)}, nil
}

func parseHourly(fields []string) (Schedule, error) {
	minutes := []int{0}
	switch len(fields) {
	case 0:
	case 1:
		minutes = nil
		for _, f := range strings.Split(fields[0], ",") {
			n, err := strconv.Atoi(strings.TrimPrefix(f, ":"))
			if err != nil || n < 0 || n > 59 {
				return Schedule{}, scheduleError("")
			}
			minutes = append(minutes, n)
		}
	default:
		return Schedule{}, scheduleError("")
	}
	return Schedule{days: allWeek(), allDays: true, minutes: uniqSorted(minutes)}, nil
}

// parseDuration reads an interval from the head of fields: "5m", "1h30m",
// "2d", or a number and a unit word ("5 minutes"). It reports how many
// fields it used.
func parseDuration(fields []string) (time.Duration, int, bool) {
	if len(fields) == 0 {
		return 0, 0, false
	}
	if d, ok := parseCompactDuration(fields[0]); ok {
		return d, 1, true
	}
	if len(fields) < 2 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0, 0, false
	}
	var unit time.Duration
	switch strings.TrimSuffix(fields[1], "s") {
	case "sec", "second":
		unit = time.Second
	case "min", "minute":
		unit = time.Minute
	case "hr", "hour":
		unit = time.Hour
	case "day":
		unit = 24 * time.Hour
	default:
		return 0, 0, false
	}
	return time.Duration(n) * unit, 2, true
}

func parseCompactDuration(s string) (time.Duration, bool) {
	var total time.Duration
	if days, rest, ok := strings.Cut(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, false
		}
		total = time.Duration(n) * 24 * time.Hour
		s = rest
		if s == "" {
			return total, total > 0
		}
	}
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return total + d, true
}

func allWeek() [7]bool { return [7]bool{true, true, true, true, true, true, true} }

// parseDays reads a day spec: daily, weekdays, weekends, a comma list of
// days, or a range like mon-fri.
func parseDays(s string) ([7]bool, bool) {
	var days [7]bool
	switch s {
	case "daily", "everyday", "day", "days":
		return allWeek(), true
	case "weekdays", "weekday":
		for d := time.Monday; d <= time.Friday; d++ {
			days[d] = true
		}
		return days, true
	case "weekends", "weekend":
		days[time.Saturday], days[time.Sunday] = true, true
		return days, true
	}
	for _, part := range strings.Split(s, ",") {
		if from, to, ok := strings.Cut(part, "-"); ok {
			a, okA := weekdayNames[from]
			b, okB := weekdayNames[to]
			if !okA || !okB {
				return days, false
			}
			for d := a; ; d = (d + 1) % 7 {
				days[d] = true
				if d == b {
					break
				}
			}
			continue
		}
		d, ok := weekdayNames[part]
		if !ok {
			return days, false
		}
		days[d] = true
	}
	return days, true
}

// parseClock reads a time of day — 9, 09:30, 9am, 9:30pm, 21:00 — as
// minutes since midnight.
func parseClock(s string) (int, bool) {
	m := clockRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	mins := 0
	if m[2] != "" {
		mins, _ = strconv.Atoi(m[2])
	}
	switch m[3] {
	case "am":
		if h < 1 || h > 12 {
			return 0, false
		}
		if h == 12 {
			h = 0
		}
	case "pm":
		if h < 1 || h > 12 {
			return 0, false
		}
		if h != 12 {
			h += 12
		}
	}
	if h > 23 || mins > 59 {
		return 0, false
	}
	return h*60 + mins, true
}

func uniqSorted(in []int) []int {
	sort.Ints(in)
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// IsInterval reports whether the schedule counts an interval from the end
// of the last run.
func (s Schedule) IsInterval() bool { return s.interval > 0 }

// Next is when the next run is due: for an interval, its length after
// lastEnd (or anchor, the moment the watcher was created, when it never
// ran — so a new interval watcher runs straight away), moved into the
// schedule's days and hours; for times of day, the first one after lastEnd
// (or anchor). A due time in the past means "run now": missed runs are
// never made up, so at most one run follows.
func (s Schedule) Next(lastEnd, anchor time.Time) time.Time {
	from := lastEnd
	if from.IsZero() {
		from = anchor
	}
	switch {
	case s.interval > 0:
		t := from
		if !lastEnd.IsZero() {
			t = lastEnd.Add(s.interval)
		}
		return s.nextOpen(t)
	case len(s.minutes) > 0:
		base := time.Date(from.Year(), from.Month(), from.Day(), from.Hour(), 0, 0, 0, from.Location())
		for h := 0; h <= 48; h++ {
			hour := base.Add(time.Duration(h) * time.Hour)
			for _, m := range s.minutes {
				if c := hour.Add(time.Duration(m) * time.Minute); c.After(from) {
					return c
				}
			}
		}
	default:
		for i := 0; i <= 8; i++ {
			day := time.Date(from.Year(), from.Month(), from.Day()+i, 0, 0, 0, 0, from.Location())
			if !s.days[day.Weekday()] {
				continue
			}
			for _, t := range s.times {
				if c := atClock(day, t); c.After(from) {
					return c
				}
			}
		}
	}
	return from.Add(24 * time.Hour)
}

// nextOpen is t when an interval may run at t, else the next time its days
// and hours open.
func (s Schedule) nextOpen(t time.Time) time.Time {
	if s.open(t) {
		return t
	}
	start := 0
	if s.hasWindow {
		start = s.windowStart
	}
	for i := 0; i <= 8; i++ {
		day := time.Date(t.Year(), t.Month(), t.Day()+i, 0, 0, 0, 0, t.Location())
		if c := atClock(day, start); c.After(t) && s.open(c) {
			return c
		}
	}
	return t
}

func (s Schedule) open(t time.Time) bool {
	wd := t.Weekday()
	if !s.hasWindow {
		return s.days[wd]
	}
	tod := t.Hour()*60 + t.Minute()
	if s.windowStart < s.windowEnd {
		return s.days[wd] && tod >= s.windowStart && tod < s.windowEnd
	}
	return (tod >= s.windowStart && s.days[wd]) || (tod < s.windowEnd && s.days[(wd+6)%7])
}

func atClock(day time.Time, minutes int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), minutes/60, minutes%60, 0, 0, day.Location())
}

// String is the schedule in its canonical spelling.
func (s Schedule) String() string {
	switch {
	case s.interval > 0:
		parts := []string{"every " + formatDuration(s.interval)}
		if !s.allDays {
			parts = append(parts, s.daysLabel())
		}
		if s.hasWindow {
			parts = append(parts, formatClock(s.windowStart)+"-"+formatClock(s.windowEnd))
		}
		return strings.Join(parts, " ")
	case len(s.minutes) > 0:
		mins := make([]string, len(s.minutes))
		for i, m := range s.minutes {
			mins[i] = fmt.Sprintf(":%02d", m)
		}
		return "hourly " + strings.Join(mins, ",")
	default:
		times := make([]string, len(s.times))
		for i, t := range s.times {
			times[i] = formatClock(t)
		}
		return s.daysLabel() + " " + strings.Join(times, ",")
	}
}

func (s Schedule) daysLabel() string {
	if s.allDays {
		return "daily"
	}
	var weekdays, weekends [7]bool
	for d := time.Monday; d <= time.Friday; d++ {
		weekdays[d] = true
	}
	weekends[time.Saturday], weekends[time.Sunday] = true, true
	switch s.days {
	case weekdays:
		return "weekdays"
	case weekends:
		return "weekends"
	}
	var names []string
	for _, d := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday} {
		if s.days[d] {
			names = append(names, weekdayShort[d])
		}
	}
	return strings.Join(names, ",")
}

func formatClock(minutes int) string {
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// formatDuration spells an interval compactly: 90s → 1m30s, 48h → 2d.
func formatDuration(d time.Duration) string {
	var b strings.Builder
	if days := d / (24 * time.Hour); days > 0 {
		fmt.Fprintf(&b, "%dd", days)
		d -= days * 24 * time.Hour
	}
	if h := d / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		d -= h * time.Hour
	}
	if m := d / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		d -= m * time.Minute
	}
	if sec := d / time.Second; sec > 0 {
		fmt.Fprintf(&b, "%ds", sec)
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}
