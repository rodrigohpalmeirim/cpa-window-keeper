// Package keeper decides when to start each account's 5-hour usage window.
//
// A Claude subscription's 5-hour window starts with the first request after the previous one ended,
// so an idle account wastes time it could be counting down. Starting every window the moment it is
// free means a reset is never more than 5 hours away. With several accounts, spreading the starts
// evenly (staggering) also spreads the resets: when a burst exhausts every account, the next refill
// comes within 5h/N instead of up to 5h. Staggering never costs capacity: a window only waits while
// its account is idle, and real traffic starts it anyway.
package keeper

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Account is one account's current state as read from its usage endpoint.
type Account struct {
	ID    string
	Label string
	// Active reports a running 5-hour window that ends at ResetsAt.
	Active   bool
	ResetsAt time.Time
	// Skip excludes an idle account from starting (weekly limit reached, no 5-hour limit, recent
	// failed start). An active skipped account still counts for spacing.
	Skip bool
}

// Decision says what to do with one idle account.
type Decision struct {
	ID       string
	StartNow bool
	// At is when the window should start, if not now.
	At time.Time
}

// Plan returns a decision for every idle, non-skipped account. Without stagger every idle window
// starts now. With stagger, starts are spread length/n apart (n = accounts taking part): an idle
// account starts now if that keeps it at least spacing-tolerance from every other start, otherwise
// at the first moment a full spacing is reached, or at the most spread-out moment if none is.
func Plan(accts []Account, now time.Time, length time.Duration, stagger bool, tolerance time.Duration) []Decision {
	var phases []time.Time // window starts, taken modulo length
	var idle []Account
	n := 0
	for _, a := range accts {
		switch {
		case a.Active && a.ResetsAt.After(now):
			phases = append(phases, a.ResetsAt.Add(-length))
			n++
		case !a.Skip:
			idle = append(idle, a)
			n++
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].ID < idle[j].ID })
	spacing := time.Duration(0)
	if n > 0 {
		spacing = length / time.Duration(n)
	}
	var out []Decision
	for _, a := range idle {
		at := now
		if stagger && len(phases) > 0 && clearance(now, phases, length) < spacing-tolerance {
			at = bestStart(phases, now, length, spacing)
		}
		phases = append(phases, at)
		if at.Sub(now) < time.Minute {
			out = append(out, Decision{ID: a.ID, StartNow: true})
		} else {
			out = append(out, Decision{ID: a.ID, At: at})
		}
	}
	return out
}

// bestStart is the first minute from now at which a start would be at least want away from every
// phase, or the minute with the largest clearance within one window length.
func bestStart(phases []time.Time, now time.Time, length, want time.Duration) time.Time {
	best, bestGap := now, time.Duration(-1)
	for t := now; t.Before(now.Add(length)); t = t.Add(time.Minute) {
		gap := clearance(t, phases, length)
		if gap >= want {
			return t
		}
		if gap > bestGap {
			best, bestGap = t, gap
		}
	}
	return best
}

// clearance is the smallest circular distance (modulo length) from t to any phase.
func clearance(t time.Time, phases []time.Time, length time.Duration) time.Duration {
	min := length
	for _, p := range phases {
		d := t.Sub(p) % length
		if d < 0 {
			d += length
		}
		if length-d < d {
			d = length - d
		}
		if d < min {
			min = d
		}
	}
	return min
}

// Usage is what the keeper needs from https://api.anthropic.com/api/oauth/usage.
type Usage struct {
	// HasSession reports a 5-hour ("session") limit entry; idle accounts may omit it.
	HasSession bool
	// SessionResets is when the running 5-hour window ends; zero when none is running.
	SessionResets time.Time
	// WeeklyPct is the all-models weekly utilization in percent.
	WeeklyPct float64
}

// ParseUsage reads the 5-hour and weekly limits, in the current ("limits" list) or the older
// (named windows) response shape.
func ParseUsage(body []byte) (Usage, error) {
	var u Usage
	var doc struct {
		Limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
		} `json:"limits"`
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization *float64 `json:"utilization"`
		} `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return u, err
	}
	for _, l := range doc.Limits {
		switch l.Kind {
		case "session":
			u.HasSession = true
			u.SessionResets = parseTime(l.ResetsAt)
		case "weekly_all":
			if l.Percent != nil {
				u.WeeklyPct = *l.Percent
			}
		}
	}
	if len(doc.Limits) == 0 {
		if doc.FiveHour != nil {
			u.HasSession = true
			u.SessionResets = parseTime(doc.FiveHour.ResetsAt)
		}
		if doc.SevenDay != nil && doc.SevenDay.Utilization != nil {
			u.WeeklyPct = *doc.SevenDay.Utilization
		}
	}
	return u, nil
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
