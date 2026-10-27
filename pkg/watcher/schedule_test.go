package watcher

import (
	"strings"
	"testing"
	"time"
)

func TestParseSchedule_CanonicalForms(t *testing.T) {
	cases := map[string]string{
		"every 5m":                        "every 5m",
		"Every 5 minutes":                 "every 5m",
		"5m":                              "every 5m",
		"every 90s":                       "every 1m30s",
		"every 2 hours":                   "every 2h",
		"every 1h30m":                     "every 1h30m",
		"every 2d":                        "every 2d",
		"every minute":                    "every 1m",
		"every hour":                      "every 1h",
		"every day":                       "every 1d",
		"every 10m weekdays 09:00-18:00":  "every 10m weekdays 09:00-18:00",
		"every 10m weekdays 9am - 6pm":    "every 10m weekdays 09:00-18:00",
		"every 15m 22:00-02:00":           "every 15m 22:00-02:00",
		"every 30m mon-wed":               "every 30m mon,tue,wed",
		"daily 09:00":                     "daily 09:00",
		"daily at 9am, 1:30pm":            "daily 09:00,13:30",
		"every day at 9":                  "daily 09:00",
		"weekdays 08:30":                  "weekdays 08:30",
		"weekends 10:00":                  "weekends 10:00",
		"mon,thu 14:30":                   "mon,thu 14:30",
		"Monday, Thursday at 2:30pm":      "mon,thu 14:30",
		"fri-mon 12am":                    "mon,fri,sat,sun 00:00",
		"every monday 10:00":              "mon 10:00",
		"hourly":                          "hourly :00",
		"hourly :15,:45":                  "hourly :15,:45",
		"hourly at :45, :15":              "hourly :15,:45",
		"daily 12pm":                      "daily 12:00",
		"every 30 secs":                   "every 30s",
		"every 1 day":                     "every 1d",
		"every 10m daily 09:00-17:00":     "every 10m 09:00-17:00",
		"every 3 hrs weekends":            "every 3h weekends",
		"weekday 9:05":                    "weekdays 09:05",
		"sat,sun 23:59":                   "weekends 23:59",
		"tue 7am":                         "tue 07:00",
		"every 20m between 08:00 - 20:00": "every 20m 08:00-20:00",
		"every 1h mon,tue,wed,thu,fri":    "every 1h weekdays",
	}
	for in, want := range cases {
		s, err := ParseSchedule(in)
		if err != nil {
			t.Errorf("ParseSchedule(%q): %v", in, err)
			continue
		}
		if got := s.String(); got != want {
			t.Errorf("ParseSchedule(%q).String() = %q, want %q", in, got, want)
		}
		again, err := ParseSchedule(s.String())
		if err != nil || again.String() != want {
			t.Errorf("canonical %q does not round-trip: %v %q", want, err, again.String())
		}
	}
}

func TestParseSchedule_Rejects(t *testing.T) {
	cases := map[string]string{
		"":                       "unknown schedule",
		"sometimes":              "unknown schedule",
		"every 5s":               "too short",
		"every 60d":              "too long",
		"daily":                  "needs a time of day",
		"daily 25:00":            "unknown schedule",
		"daily 13pm":             "unknown schedule",
		"hourly :60":             "unknown schedule",
		"every 5m 09:00-09:00":   "are empty",
		"every 5m weekdays nope": "unknown schedule",
		"mon,funday 10:00":       "unknown schedule",
		"daily 09:00 extra":      "unknown schedule",
	}
	for in, want := range cases {
		_, err := ParseSchedule(in)
		if err == nil {
			t.Errorf("ParseSchedule(%q) succeeded, want error containing %q", in, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ParseSchedule(%q) error %q, want it to contain %q", in, err, want)
		}
	}
}

func mustSchedule(t *testing.T, s string) Schedule {
	t.Helper()
	sch, err := ParseSchedule(s)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", s, err)
	}
	return sch
}

// at builds a UTC time on the week of Monday 2026-10-26.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 10, 26+day, hour, minute, 0, 0, time.UTC)
}

func TestScheduleNext_IntervalCountsFromTheEndOfTheLastRun(t *testing.T) {
	s := mustSchedule(t, "every 5m")
	created := at(0, 9, 0)
	if got := s.Next(time.Time{}, created); !got.Equal(created) {
		t.Errorf("a watcher that never ran is due when created: got %v", got)
	}
	// A run that took six minutes: the next one is five minutes after it
	// ended, never back to back.
	ended := at(0, 9, 6)
	if got, want := s.Next(ended, created), at(0, 9, 11); !got.Equal(want) {
		t.Errorf("Next after a long run = %v, want %v", got, want)
	}
}

func TestScheduleNext_IntervalStaysInsideItsDaysAndHours(t *testing.T) {
	s := mustSchedule(t, "every 10m weekdays 09:00-18:00")
	// Friday 17:55 + 10m lands outside the window: Monday 09:00.
	if got, want := s.Next(at(4, 17, 55), at(0, 0, 0)), at(7, 9, 0); !got.Equal(want) {
		t.Errorf("Next past Friday's window = %v, want %v", got, want)
	}
	// Inside the window, the interval is plain.
	if got, want := s.Next(at(1, 10, 0), at(0, 0, 0)), at(1, 10, 10); !got.Equal(want) {
		t.Errorf("Next inside the window = %v, want %v", got, want)
	}
	// Created on a Saturday: waits for Monday's window to open.
	if got, want := s.Next(time.Time{}, at(5, 12, 0)), at(7, 9, 0); !got.Equal(want) {
		t.Errorf("first run created on a weekend = %v, want %v", got, want)
	}
}

func TestScheduleNext_OvernightWindow(t *testing.T) {
	s := mustSchedule(t, "every 30m 22:00-02:00")
	if got, want := s.Next(at(0, 23, 50), at(0, 0, 0)), at(1, 0, 20); !got.Equal(want) {
		t.Errorf("Next across midnight = %v, want %v", got, want)
	}
	if got, want := s.Next(at(1, 1, 45), at(0, 0, 0)), at(1, 22, 0); !got.Equal(want) {
		t.Errorf("Next after the window closes = %v, want %v", got, want)
	}
}

func TestScheduleNext_TimesOfDay(t *testing.T) {
	s := mustSchedule(t, "weekdays 09:00,13:30")
	// Never ran: the first slot after the watcher was created.
	if got, want := s.Next(time.Time{}, at(0, 10, 0)), at(0, 13, 30); !got.Equal(want) {
		t.Errorf("first run = %v, want %v", got, want)
	}
	// The 13:30 run ended at 13:42: next is tomorrow 09:00.
	if got, want := s.Next(at(0, 13, 42), at(0, 0, 0)), at(1, 9, 0); !got.Equal(want) {
		t.Errorf("Next after the last slot = %v, want %v", got, want)
	}
	// Friday afternoon skips the weekend.
	if got, want := s.Next(at(4, 14, 0), at(0, 0, 0)), at(7, 9, 0); !got.Equal(want) {
		t.Errorf("Next over the weekend = %v, want %v", got, want)
	}
}

func TestScheduleNext_MissedSlotsRunOnceNotOncePerSlot(t *testing.T) {
	s := mustSchedule(t, "daily 09:00")
	lastEnd := at(0, 9, 1)
	// Two days of slots missed while no ask was open: the next run is the
	// first slot after the last run, already in the past, so it runs once
	// now; after it ends the schedule moves on from there.
	due := s.Next(lastEnd, at(0, 0, 0))
	if !due.Equal(at(1, 9, 0)) {
		t.Fatalf("due = %v", due)
	}
	now := at(2, 15, 0)
	if !due.Before(now) {
		t.Fatalf("a missed slot should be due now")
	}
	if got, want := s.Next(now.Add(time.Minute), at(0, 0, 0)), at(3, 9, 0); !got.Equal(want) {
		t.Errorf("Next after the catch-up run = %v, want %v", got, want)
	}
}

func TestScheduleNext_Hourly(t *testing.T) {
	s := mustSchedule(t, "hourly :15,:45")
	if got, want := s.Next(at(0, 10, 20), at(0, 0, 0)), at(0, 10, 45); !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	if got, want := s.Next(at(0, 23, 50), at(0, 0, 0)), at(1, 0, 15); !got.Equal(want) {
		t.Errorf("Next across midnight = %v, want %v", got, want)
	}
}
