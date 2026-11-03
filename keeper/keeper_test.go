package keeper

import (
	"net/http"
	"testing"
	"time"
)

var (
	now = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	L   = 5 * time.Hour
	tol = 15 * time.Minute
)

func TestNoStaggerStartsEveryIdleWindow(t *testing.T) {
	d := Plan([]Account{
		{ID: "a"},
		{ID: "b", Active: true, ResetsAt: now.Add(time.Hour)},
		{ID: "c", Skip: true},
	}, now, L, false, tol)
	if len(d) != 1 || d[0].ID != "a" || !d[0].StartNow {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestStaggerWaitsForHalfAWindowWithTwoAccounts(t *testing.T) {
	// b started 1h ago (resets in 4h): a should start 2h30 after b, i.e. in 1h30.
	d := Plan([]Account{{ID: "a"}, {ID: "b", Active: true, ResetsAt: now.Add(4 * time.Hour)}}, now, L, true, tol)
	if len(d) != 1 || d[0].StartNow {
		t.Fatalf("decisions = %+v", d)
	}
	if got, want := d[0].At, now.Add(90*time.Minute); !got.Equal(want) {
		t.Fatalf("start at %v, want %v", got, want)
	}
}

func TestStaggerStartsNowWhenAlreadySpread(t *testing.T) {
	// b started 2h30 ago: now is the ideal moment for a.
	d := Plan([]Account{{ID: "a"}, {ID: "b", Active: true, ResetsAt: now.Add(150 * time.Minute)}}, now, L, true, tol)
	if len(d) != 1 || !d[0].StartNow {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestStaggerBothIdleStartsOneNowAndSpacesTheOther(t *testing.T) {
	d := Plan([]Account{{ID: "b"}, {ID: "a"}}, now, L, true, tol)
	if len(d) != 2 || d[0].ID != "a" || !d[0].StartNow {
		t.Fatalf("decisions = %+v", d)
	}
	if d[1].ID != "b" || d[1].StartNow || !d[1].At.Equal(now.Add(150*time.Minute)) {
		t.Fatalf("second = %+v", d[1])
	}
}

func TestStaggerThreeAccounts(t *testing.T) {
	// Starts at -1h40 and now+0 for the actives would be ideal; with b at -1h40 (resets in 3h20)
	// and c just started (resets in 5h), a belongs at +1h40.
	d := Plan([]Account{
		{ID: "a"},
		{ID: "b", Active: true, ResetsAt: now.Add(200 * time.Minute)},
		{ID: "c", Active: true, ResetsAt: now.Add(L)},
	}, now, L, true, 0)
	if len(d) != 1 || !d[0].At.Equal(now.Add(100*time.Minute)) {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestSkippedAccountsDoNotCountForSpacing(t *testing.T) {
	// Only a and b take part: spacing 2h30, so with b just started a waits 2h30.
	d := Plan([]Account{{ID: "a"}, {ID: "b", Active: true, ResetsAt: now.Add(L)}, {ID: "c", Skip: true}}, now, L, true, tol)
	if len(d) != 1 || !d[0].At.Equal(now.Add(150*time.Minute)) {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestExpiredWindowCountsAsIdle(t *testing.T) {
	d := Plan([]Account{{ID: "a", Active: true, ResetsAt: now.Add(-time.Minute)}}, now, L, true, tol)
	if len(d) != 1 || !d[0].StartNow {
		t.Fatalf("decisions = %+v", d)
	}
}

func TestParseUsage(t *testing.T) {
	u, err := ParseUsage([]byte(`{"limits":[
		{"kind":"session","group":"session","percent":37,"resets_at":"2026-11-03T14:00:00Z"},
		{"kind":"weekly_all","group":"weekly","percent":20,"resets_at":"2026-11-04T13:00:00Z"}]}`))
	if err != nil || !u.HasSession || u.WeeklyPct != 20 || !u.SessionResets.Equal(time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("limits shape = %+v, %v", u, err)
	}
	u, err = ParseUsage([]byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":100,"resets_at":"2026-11-04T13:00:00+00:00"}}`))
	if err != nil || !u.HasSession || !u.SessionResets.IsZero() || u.WeeklyPct != 100 {
		t.Fatalf("legacy shape = %+v, %v", u, err)
	}
	u, err = ParseUsage([]byte(`{"five_hour":null,"seven_day":{"utilization":3}}`))
	if err != nil || u.HasSession {
		t.Fatalf("no session = %+v, %v", u, err)
	}
}

func TestParseCodexUsage(t *testing.T) {
	// Plus-style: 5h primary running, weekly secondary.
	u, err := ParseCodexUsage([]byte(`{"rate_limit":{"limit_reached":false,
		"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1793718000},
		"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1794000000}}}`), now)
	if err != nil || !u.HasSession || !u.SessionResets.Equal(time.Unix(1793718000, 0).UTC()) || u.WeeklyPct != 40 {
		t.Fatalf("plus = %+v, %v", u, err)
	}
	// Free plan: one 30-day window, no 5-hour limit.
	u, err = ParseCodexUsage([]byte(`{"plan_type":"free","rate_limit":{"limit_reached":false,
		"primary_window":{"used_percent":1,"limit_window_seconds":2592000,"reset_at":1796251586},"secondary_window":null}}`), now)
	if err != nil || u.HasSession || u.WeeklyPct != 1 {
		t.Fatalf("free = %+v, %v", u, err)
	}
	// Refused without a running 5-hour window: treat as the longer limit being full.
	u, _ = ParseCodexUsage([]byte(`{"rate_limit":{"limit_reached":true,
		"primary_window":{"used_percent":100,"limit_window_seconds":2592000,"reset_at":1796251586}}}`), now)
	if u.WeeklyPct != 100 {
		t.Fatalf("limit reached = %+v", u)
	}
}

func TestCodexSessionFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "3")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-After-Seconds", "17940")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	h.Set("X-Codex-Secondary-Reset-At", "1794000000")
	if got := CodexSessionFromHeaders(h, now); !got.Equal(now.Add(17940 * time.Second)) {
		t.Fatalf("session = %v", got)
	}
	h = http.Header{}
	h.Set("X-Codex-Primary-Window-Minutes", "43200")
	h.Set("X-Codex-Primary-Reset-At", "1796251586")
	if got := CodexSessionFromHeaders(h, now); !got.IsZero() {
		t.Fatalf("monthly only = %v", got)
	}
}
