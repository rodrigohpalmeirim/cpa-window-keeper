package keeper

import (
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
