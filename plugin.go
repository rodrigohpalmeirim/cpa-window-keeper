package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"windowkeeper/keeper"
)

const windowLength = 5 * time.Hour

// ---- wire types (CPA plugin RPC schema v9) ----

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type configField struct {
	Name        string
	Type        string
	Description string
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	ConfigFields     []configField
}

type capabilities struct {
	ManagementAPI bool `json:"management_api"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type managementRoute struct {
	Method      string
	Path        string
	Description string
}

type managementRequest struct {
	Method string
	Path   string
	Query  map[string][]string
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type hostHTTPRequest struct {
	Method  string
	URL     string
	Headers http.Header
}

type hostHTTPResponse struct {
	StatusCode int
	Body       []byte
}

// ---- config ----

type config struct {
	// Mode "active" starts windows; "observe" only plans and reports.
	Mode    string `yaml:"mode"`
	Stagger *bool  `yaml:"stagger"`
	// Model is the request that starts a window: one tiny prompt.
	Model string `yaml:"model"`
	// Accounts limits the keeper to these auth IDs or emails; empty means every Claude account.
	Accounts      []string `yaml:"accounts"`
	CheckInterval string   `yaml:"check_interval"`
	Tolerance     string   `yaml:"tolerance"`

	checkEvery time.Duration
	tolerance  time.Duration
}

func parseConfig(raw []byte) (config, error) {
	c := config{}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("window-keeper config: %w", err)
		}
	}
	if c.Mode == "" {
		c.Mode = "active"
	}
	if c.Mode != "active" && c.Mode != "observe" {
		return c, fmt.Errorf("window-keeper: mode must be active or observe, got %q", c.Mode)
	}
	if c.Stagger == nil {
		on := true
		c.Stagger = &on
	}
	if c.Model == "" {
		c.Model = "claude-haiku-5-5"
	}
	var err error
	if c.checkEvery, err = durationOr(c.CheckInterval, 5*time.Minute); err != nil {
		return c, err
	}
	if c.tolerance, err = durationOr(c.Tolerance, 15*time.Minute); err != nil {
		return c, err
	}
	return c, nil
}

func durationOr(s string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("window-keeper: bad duration %q", s)
	}
	return d, nil
}

// ---- state ----

type account struct {
	ID        string    `json:"id"`
	AuthIndex string    `json:"-"`
	Label     string    `json:"label"`
	Usage     *usageOut `json:"usage,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	FetchErr  string    `json:"fetch_error,omitempty"`
	// PlannedAt is when a staggered window is due to start.
	PlannedAt time.Time `json:"planned_start,omitzero"`
	StartedAt time.Time `json:"last_start,omitzero"`
	StartErr  string    `json:"last_start_error,omitempty"`
	// PausedUntil holds off starts after a failure, or for an account without a 5-hour limit.
	PausedUntil time.Time `json:"paused_until,omitzero"`
	Note        string    `json:"note,omitempty"`

	usage keeper.Usage
}

type usageOut struct {
	Session   bool      `json:"has_5h_limit"`
	ResetsAt  time.Time `json:"5h_resets_at,omitzero"`
	WeeklyPct float64   `json:"weekly_pct"`
}

var (
	mu       sync.Mutex
	cfg      config
	accounts = map[string]*account{}
	stopCh   chan struct{}
)

// ---- dispatch ----

func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(req); err != nil {
			return nil, err
		}
		return okEnvelope(registrationInfo())
	case "plugin.quiesce":
		return okEnvelope(struct{}{})
	case "plugin.shutdown":
		shutdown()
		return okEnvelope(struct{}{})
	case "management.register":
		return okEnvelope(map[string]any{"routes": []managementRoute{
			{Method: "GET", Path: "/window-keeper/status",
				Description: "Each Claude account's 5-hour window, the planned (staggered) start of idle ones and the last start."},
			{Method: "POST", Path: "/window-keeper/start",
				Description: "Send the window-starting request through one account now (?account=<auth id or email>)."},
		}})
	case "management.handle":
		return handleManagement(req)
	}
	return errorEnvelope("unknown_method", "unknown method: "+method), nil
}

func registrationInfo() registration {
	return registration{
		SchemaVersion: schemaVersion,
		Metadata: metadata{
			Name: pluginID, Version: pluginVersion, Author: "rodrigohpalmeirim",
			GitHubRepository: "https://github.com/rodrigohpalmeirim/cpa-window-keeper",
			ConfigFields: []configField{
				{Name: "mode", Type: "string", Description: "active (start windows) or observe (plan and report only)."},
				{Name: "stagger", Type: "boolean", Description: "Spread the accounts' 5-hour windows evenly instead of starting every idle one at once (default true)."},
				{Name: "model", Type: "string", Description: "Model for the one-token request that starts a window (default claude-haiku-5-5)."},
				{Name: "accounts", Type: "array", Description: "Auth IDs or emails to manage; empty means every Claude account."},
				{Name: "check_interval", Type: "string", Description: "How often idle accounts' usage is re-read (default 5m)."},
				{Name: "tolerance", Type: "string", Description: "How far from the ideal spacing a start may be (default 15m)."},
			},
		},
		Capabilities: capabilities{ManagementAPI: true},
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	c, err := parseConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	mu.Lock()
	cfg = c
	if stopCh == nil {
		stopCh = make(chan struct{})
		go loop(stopCh)
	}
	mu.Unlock()
	logf("info", "window-keeper %s configured: mode=%s stagger=%v model=%s", pluginVersion, c.Mode, *c.Stagger, c.Model)
	return nil
}

func shutdown() {
	mu.Lock()
	defer mu.Unlock()
	if stopCh != nil {
		close(stopCh)
		stopCh = nil
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Only answer on the authenticated management prefix.
	if !strings.HasPrefix(req.Path, "/v0/management/") {
		return jsonResponse(404, map[string]string{"error": "not found"})
	}
	switch {
	case req.Method == "POST" && strings.HasSuffix(req.Path, "/window-keeper/start"):
		return manualStart(req)
	case !strings.HasSuffix(req.Path, "/window-keeper/status"):
		return jsonResponse(404, map[string]string{"error": "not found"})
	}
	mu.Lock()
	list := make([]*account, 0, len(accounts))
	for _, a := range accounts {
		list = append(list, a)
	}
	slices.SortFunc(list, func(a, b *account) int { return strings.Compare(a.ID, b.ID) })
	body, err := json.MarshalIndent(map[string]any{
		"now": time.Now().UTC(), "mode": cfg.Mode, "stagger": *cfg.Stagger, "model": cfg.Model, "accounts": list,
	}, "", " ")
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: 200,
		Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
}

func manualStart(req managementRequest) ([]byte, error) {
	want := ""
	if v := req.Query["account"]; len(v) > 0 {
		want = v[0]
	}
	mu.Lock()
	c := cfg
	var target *account
	for _, a := range accounts {
		if want != "" && (a.ID == want || a.Label == want) {
			target = a
		}
	}
	mu.Unlock()
	if target == nil {
		return jsonResponse(404, map[string]string{"error": "unknown account " + want})
	}
	err := startWindow(target.ID, c.Model)
	mu.Lock()
	target.StartedAt = time.Now()
	target.StartErr = ""
	if err != nil {
		target.StartErr = err.Error()
	}
	mu.Unlock()
	if err != nil {
		return jsonResponse(502, map[string]string{"error": err.Error()})
	}
	logf("info", "window-keeper: started %s's 5-hour window (manual)", target.Label)
	return jsonResponse(200, map[string]string{"started": target.Label})
}

func jsonResponse(status int, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: status,
		Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
}

// ---- the loop ----

func loop(stop chan struct{}) {
	timer := time.NewTimer(15 * time.Second) // let the host finish starting
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			guarded("tick", tick)
			timer.Reset(time.Minute)
		}
	}
}

func tick() {
	mu.Lock()
	c := cfg
	mu.Unlock()
	files, err := listClaudeAccounts(c.Accounts)
	if err != nil {
		logf("warn", "window-keeper: auth list: %v", err)
		return
	}
	now := time.Now()

	mu.Lock()
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.ID] = true
		a := accounts[f.ID]
		if a == nil {
			a = &account{ID: f.ID}
			accounts[f.ID] = a
		}
		a.AuthIndex, a.Label = f.AuthIndex, f.Label
	}
	for id := range accounts {
		if !seen[id] {
			delete(accounts, id)
		}
	}
	var due []*account
	for _, a := range accounts {
		if needsFetch(a, now, c.checkEvery) {
			due = append(due, a)
		}
	}
	mu.Unlock()

	for _, a := range due {
		u, err := fetchUsage(a.AuthIndex)
		mu.Lock()
		a.FetchedAt = time.Now()
		if err != nil {
			a.FetchErr = err.Error()
			mu.Unlock()
			logf("warn", "window-keeper: usage for %s: %v", a.Label, err)
			continue
		}
		a.FetchErr = ""
		checkStarted(a, u, a.FetchedAt)
		a.usage = u
		a.Usage = &usageOut{Session: u.HasSession, ResetsAt: u.SessionResets, WeeklyPct: u.WeeklyPct}
		mu.Unlock()
	}

	mu.Lock()
	now = time.Now()
	var in []keeper.Account
	for _, a := range accounts {
		if a.Usage == nil {
			continue // never read: unknown, leave it alone
		}
		active := a.usage.SessionResets.After(now)
		skip := a.usage.WeeklyPct >= 100 || now.Before(a.PausedUntil) || a.FetchErr != ""
		in = append(in, keeper.Account{ID: a.ID, Label: a.Label, Active: active, ResetsAt: a.usage.SessionResets, Skip: skip})
		if active || skip {
			a.PlannedAt = time.Time{}
		}
		switch {
		case a.usage.WeeklyPct >= 100:
			a.Note = "weekly limit reached"
		case active:
			a.Note = ""
		}
	}
	decisions := keeper.Plan(in, now, windowLength, *c.Stagger, c.tolerance)
	var start []*account
	for _, d := range decisions {
		a := accounts[d.ID]
		if d.StartNow {
			a.PlannedAt = time.Time{}
			start = append(start, a)
			continue
		}
		if !a.PlannedAt.Equal(d.At) {
			logf("info", "window-keeper: %s idle; staggered start planned at %s", a.Label, d.At.UTC().Format(time.RFC3339))
		}
		a.PlannedAt = d.At
	}
	mu.Unlock()

	for _, a := range start {
		if c.Mode != "active" {
			mu.Lock()
			a.Note = "observe mode: would start now"
			mu.Unlock()
			continue
		}
		err := startWindow(a.ID, c.Model)
		mu.Lock()
		a.StartedAt = time.Now()
		if err != nil {
			a.StartErr = err.Error()
			a.PausedUntil = a.StartedAt.Add(15 * time.Minute)
			logf("warn", "window-keeper: starting %s's window failed: %v", a.Label, err)
		} else {
			a.StartErr = ""
			logf("info", "window-keeper: started %s's 5-hour window", a.Label)
		}
		mu.Unlock()
	}
}

// needsFetch: never read, periodic refresh, the running window just ended, a planned start is
// due, or a start needs confirming.
func needsFetch(a *account, now time.Time, every time.Duration) bool {
	switch {
	case a.FetchedAt.IsZero(), now.Sub(a.FetchedAt) >= every:
		return true
	case !a.usage.SessionResets.IsZero() && now.After(a.usage.SessionResets.Add(20*time.Second)) && a.FetchedAt.Before(a.usage.SessionResets.Add(20*time.Second)):
		return true
	case !a.PlannedAt.IsZero() && !now.Before(a.PlannedAt):
		return true
	case !a.StartedAt.IsZero() && a.FetchedAt.Before(a.StartedAt) && now.Sub(a.StartedAt) >= 30*time.Second:
		return true
	}
	return false
}

// checkStarted looks at the first reading after a start: if no window is running, pause the
// account (longer when it reports no 5-hour limit at all) instead of retrying every minute.
func checkStarted(a *account, u keeper.Usage, now time.Time) {
	if a.StartedAt.IsZero() || a.StartErr != "" || a.FetchedAt.Sub(a.StartedAt) > 10*time.Minute || u.SessionResets.After(now) {
		return
	}
	if a.FetchedAt.Before(a.StartedAt) {
		return
	}
	if !u.HasSession {
		a.PausedUntil, a.Note = now.Add(6*time.Hour), "no 5-hour limit reported"
	} else {
		a.PausedUntil, a.Note = now.Add(time.Hour), "start request did not open a window"
	}
}

// ---- host calls ----

type authFile struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Provider  string `json:"provider"`
	Email     string `json:"email"`
	Label     string `json:"label"`
	Disabled  bool   `json:"disabled"`
}

func listClaudeAccounts(only []string) ([]authFile, error) {
	var list struct {
		Files []authFile `json:"files"`
	}
	if err := callHost("host.auth.list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	var out []authFile
	for _, f := range list.Files {
		p := strings.ToLower(f.Provider)
		if p == "" {
			p = strings.ToLower(f.Type)
		}
		if p != "claude" || f.Disabled {
			continue
		}
		if f.ID == "" {
			f.ID = f.Name
		}
		if f.Email != "" {
			f.Label = f.Email
		} else if f.Label == "" {
			f.Label = f.Name
		}
		if len(only) > 0 && !slices.Contains(only, f.ID) && !slices.Contains(only, f.Email) {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func fetchUsage(authIndex string) (keeper.Usage, error) {
	var got struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := callHost("host.auth.get", map[string]any{"auth_index": authIndex}, &got); err != nil {
		return keeper.Usage{}, err
	}
	var cred struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(got.JSON, &cred); err != nil || cred.AccessToken == "" {
		return keeper.Usage{}, fmt.Errorf("no access token in credential")
	}
	var resp hostHTTPResponse
	err := callHost("host.http.do", hostHTTPRequest{Method: "GET", URL: "https://api.anthropic.com/api/oauth/usage",
		Headers: http.Header{
			"Authorization":  {"Bearer " + cred.AccessToken},
			"Accept":         {"application/json"},
			"Anthropic-Beta": {"oauth-2025-04-20"},
			"User-Agent":     {"claude-cli/2.1.280 (external, cli)"},
		}}, &resp)
	if err != nil {
		return keeper.Usage{}, err
	}
	if resp.StatusCode != 200 {
		return keeper.Usage{}, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, resp.Body)
	}
	return keeper.ParseUsage(resp.Body)
}

// startWindow sends one tiny request through the account, via the host's normal Claude path
// (pinned to that credential), which opens a new 5-hour window.
func startWindow(authID, model string) error {
	body, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	var resp struct {
		StatusCode int `json:"status_code"`
	}
	err := callHost("host.model.execute", map[string]any{
		"entry_protocol": "claude", "exit_protocol": "claude", "model": model, "stream": false,
		"body": body, "headers": http.Header{"Content-Type": {"application/json"}},
		"forced_provider": "claude", "auth_id": authID,
	}, &resp)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// guarded runs background work, logging instead of crashing the proxy on panic.
func guarded(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			logf("error", "window-keeper: panic in %s: %v", what, r)
		}
	}()
	fn()
}

// logf logs through the host asynchronously, so it never re-enters the host while the host is
// calling into the plugin (e.g. during plugin.register).
func logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	go func() { _ = callHost("host.log", map[string]any{"level": level, "message": msg}, nil) }()
}
