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

// providers the keeper knows, in display order.
var providers = []string{"claude", "codex"}

// ---- wire types (CPA plugin RPC schema v9) ----

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// configField describes one setting for the management panel. The host HTML-escapes
// descriptions, so they avoid apostrophes and <>&.
type configField struct {
	Name        string
	Type        string
	Default     any      `json:",omitempty"`
	EnumValues  []string `json:",omitempty"`
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

type providerConfig struct {
	Enabled *bool `json:"enabled"`
	// Model is the one tiny request that starts a window.
	Model string `json:"model"`
}

// config is flat (plain toggles and text fields) so the management panel renders it as a form.
type config struct {
	// Mode "active" starts windows; "observe" only plans and reports.
	Mode    string `yaml:"mode"`
	Stagger *bool  `yaml:"stagger"`
	// Claude and Codex switch each provider on or off; the models are for the starting request.
	ClaudeOn    *bool  `yaml:"claude"`
	ClaudeModel string `yaml:"claude_model"`
	CodexOn     *bool  `yaml:"codex"`
	CodexModel  string `yaml:"codex_model"`
	// Accounts limits the keeper to these auth IDs or emails; empty means every account.
	Accounts      stringList `yaml:"accounts"`
	CheckInterval string     `yaml:"check_interval"`
	Tolerance     string     `yaml:"tolerance"`

	Claude, Codex providerConfig `yaml:"-"`
	checkEvery    time.Duration
	tolerance     time.Duration
}

// stringList accepts a YAML list or one comma-separated string.
type stringList []string

func (l *stringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		var v []string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*l = v
		return nil
	}
	var v string
	if err := n.Decode(&v); err != nil {
		return err
	}
	*l = nil
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

func (c config) provider(name string) providerConfig {
	if name == "codex" {
		return c.Codex
	}
	return c.Claude
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
	on, off := true, false
	if c.Stagger == nil {
		c.Stagger = &on
	}
	// Claude is on unless switched off; Codex is off unless switched on.
	if c.ClaudeOn == nil {
		c.ClaudeOn = &on
	}
	if c.CodexOn == nil {
		c.CodexOn = &off
	}
	c.Claude = providerConfig{Enabled: c.ClaudeOn, Model: firstNonEmpty(strings.TrimSpace(c.ClaudeModel), "claude-haiku-5-5")}
	c.Codex = providerConfig{Enabled: c.CodexOn, Model: firstNonEmpty(strings.TrimSpace(c.CodexModel), "gpt-6-luna")}
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
	Provider  string    `json:"provider"`
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
	// headerResets is the 5-hour window end read from the starting request's response headers,
	// for accounts whose usage endpoint doesn't list the window (some Codex plans).
	headerResets time.Time
}

type usageOut struct {
	Session   bool      `json:"has_5h_limit"`
	ResetsAt  time.Time `json:"5h_resets_at,omitzero"`
	WeeklyPct float64   `json:"longer_limit_pct"`
}

// sessionResets is when the account's running 5-hour window ends, from the usage endpoint or,
// failing that, from the last starting request's headers.
func (a *account) sessionResets() time.Time {
	if !a.usage.SessionResets.IsZero() {
		return a.usage.SessionResets
	}
	return a.headerResets
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
				Description: "The 5-hour window of each account, the planned start of idle ones and the last start."},
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
				{Name: "mode", Type: "enum", EnumValues: []string{"active", "observe"}, Default: "active",
					Description: "Active starts windows. Observe only plans them and shows the plan in the status."},
				{Name: "stagger", Type: "boolean", Default: true,
					Description: "Space the 5-hour windows of each provider evenly, so resets are spread out."},
				{Name: "claude", Type: "boolean", Default: true, Description: "Keep the 5-hour windows of Claude accounts running."},
				{Name: "claude_model", Type: "string", Default: "claude-haiku-5-5",
					Description: "Model for the one-token request that starts a Claude window (default claude-haiku-5-5)."},
				{Name: "codex", Type: "boolean", Default: false, Description: "Keep the 5-hour windows of Codex accounts running."},
				{Name: "codex_model", Type: "string", Default: "gpt-6-luna",
					Description: "Model for the request that starts a Codex window (default gpt-6-luna)."},
				{Name: "accounts", Type: "string",
					Description: "Comma-separated auth IDs or emails to manage. Leave empty for every account."},
				{Name: "check_interval", Type: "string", Default: "5m", Description: "How often idle accounts are re-read (default 5m)."},
				{Name: "tolerance", Type: "string", Default: "15m",
					Description: "How far from the ideal spacing a start may land (default 15m)."},
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
	logf("info", "window-keeper %s configured: mode=%s stagger=%v claude=%v codex=%v", pluginVersion, c.Mode,
		*c.Stagger, *c.Claude.Enabled, *c.Codex.Enabled)
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
	slices.SortFunc(list, func(a, b *account) int {
		return strings.Compare(a.Provider+a.ID, b.Provider+b.ID)
	})
	body, err := json.MarshalIndent(map[string]any{
		"now": time.Now().UTC(), "mode": cfg.Mode, "stagger": *cfg.Stagger,
		"claude": cfg.Claude, "codex": cfg.Codex, "accounts": list,
	}, "", " ")
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	return okEnvelope(managementResponse{StatusCode: 200,
		Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body})
}

// manualStart starts one account's window now, whether or not its provider is switched on.
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
	err := start(target, c.provider(target.Provider).Model)
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
	files, err := listAccounts(c.Accounts)
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
		a.Provider, a.AuthIndex, a.Label = f.provider(), f.AuthIndex, f.Label
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
		u, err := fetchUsage(a.Provider, a.AuthIndex)
		mu.Lock()
		a.FetchedAt = time.Now()
		if err != nil {
			a.FetchErr = err.Error()
			mu.Unlock()
			logf("warn", "window-keeper: usage for %s: %v", a.Label, err)
			continue
		}
		a.FetchErr = ""
		a.usage = u
		a.Usage = &usageOut{Session: u.HasSession, ResetsAt: a.sessionResets(), WeeklyPct: u.WeeklyPct}
		checkStarted(a, a.FetchedAt)
		mu.Unlock()
	}

	var start []*account
	mu.Lock()
	now = time.Now()
	for _, p := range providers {
		start = append(start, planProvider(p, c, now)...)
	}
	mu.Unlock()

	for _, a := range start {
		if c.Mode != "active" {
			mu.Lock()
			a.Note = "observe mode: would start now"
			mu.Unlock()
			continue
		}
		if err := start1(a, c); err != nil {
			logf("warn", "window-keeper: starting %s's window failed: %v", a.Label, err)
		} else {
			logf("info", "window-keeper: started %s's 5-hour window", a.Label)
		}
	}
}

func start1(a *account, c config) error {
	err := start(a, c.provider(a.Provider).Model)
	if err != nil {
		mu.Lock()
		a.PausedUntil = time.Now().Add(15 * time.Minute)
		mu.Unlock()
	}
	return err
}

// planProvider plans one provider's accounts (they are staggered among themselves) and returns
// those to start now. The caller holds mu.
func planProvider(provider string, c config, now time.Time) []*account {
	enabled := *c.provider(provider).Enabled
	var in []keeper.Account
	for _, a := range accounts {
		if a.Provider != provider || a.Usage == nil {
			continue // never read: unknown, leave it alone
		}
		resets := a.sessionResets()
		active := resets.After(now)
		skip := a.usage.WeeklyPct >= 100 || now.Before(a.PausedUntil) || a.FetchErr != ""
		in = append(in, keeper.Account{ID: a.ID, Label: a.Label, Active: active, ResetsAt: resets, Skip: skip})
		if active || skip || !enabled {
			a.PlannedAt = time.Time{}
		}
		switch {
		case !enabled:
			a.Note = provider + " is switched off"
		case a.usage.WeeklyPct >= 100:
			a.Note = "longer limit reached"
		case active, a.Note == provider+" is switched off":
			a.Note = ""
		}
	}
	if !enabled {
		return nil
	}
	var out []*account
	for _, d := range keeper.Plan(in, now, windowLength, *c.Stagger, c.tolerance) {
		a := accounts[d.ID]
		if d.StartNow {
			a.PlannedAt = time.Time{}
			out = append(out, a)
			continue
		}
		if !a.PlannedAt.Equal(d.At) {
			logf("info", "window-keeper: %s idle; staggered start planned at %s", a.Label, d.At.UTC().Format(time.RFC3339))
		}
		a.PlannedAt = d.At
	}
	return out
}

// needsFetch: never read, periodic refresh, the running window just ended, a planned start is
// due, or a start needs confirming.
func needsFetch(a *account, now time.Time, every time.Duration) bool {
	end := a.sessionResets()
	switch {
	case a.FetchedAt.IsZero(), now.Sub(a.FetchedAt) >= every:
		return true
	case !end.IsZero() && now.After(end.Add(20*time.Second)) && a.FetchedAt.Before(end.Add(20*time.Second)):
		return true
	case !a.PlannedAt.IsZero() && !now.Before(a.PlannedAt):
		return true
	case !a.StartedAt.IsZero() && a.FetchedAt.Before(a.StartedAt) && now.Sub(a.StartedAt) >= 30*time.Second:
		return true
	}
	return false
}

// checkStarted looks at the first readings after a start: if no window is running, pause the
// account (a day when it reports no 5-hour limit at all) instead of retrying every minute.
func checkStarted(a *account, now time.Time) {
	if a.StartedAt.IsZero() || a.StartErr != "" || now.Sub(a.StartedAt) > 10*time.Minute || a.sessionResets().After(now) {
		return
	}
	if !a.usage.HasSession {
		a.PausedUntil, a.Note = now.Add(24*time.Hour), "no 5-hour limit reported"
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

func (f authFile) provider() string {
	if f.Provider != "" {
		return strings.ToLower(f.Provider)
	}
	return strings.ToLower(f.Type)
}

func listAccounts(only []string) ([]authFile, error) {
	var list struct {
		Files []authFile `json:"files"`
	}
	if err := callHost("host.auth.list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	var out []authFile
	for _, f := range list.Files {
		if !slices.Contains(providers, f.provider()) || f.Disabled {
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

type credential struct {
	AccessToken      string `json:"access_token"`
	AccountID        string `json:"account_id"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
}

func fetchUsage(provider, authIndex string) (keeper.Usage, error) {
	var got struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := callHost("host.auth.get", map[string]any{"auth_index": authIndex}, &got); err != nil {
		return keeper.Usage{}, err
	}
	var cred credential
	if err := json.Unmarshal(got.JSON, &cred); err != nil || cred.AccessToken == "" {
		return keeper.Usage{}, fmt.Errorf("no access token in credential")
	}
	headers := http.Header{"Authorization": {"Bearer " + cred.AccessToken}, "Accept": {"application/json"}}
	url := "https://api.anthropic.com/api/oauth/usage"
	if provider == "codex" {
		url = "https://chatgpt.com/backend-api/wham/usage"
		headers["User-Agent"] = []string{"codex-tui/0.149.1"}
		if id := firstNonEmpty(cred.ChatGPTAccountID, cred.AccountID); id != "" {
			headers["Chatgpt-Account-Id"] = []string{id}
		}
	} else {
		headers["Anthropic-Beta"] = []string{"oauth-2025-04-20"}
		headers["User-Agent"] = []string{"claude-cli/2.1.280 (external, cli)"}
	}
	var resp hostHTTPResponse
	if err := callHost("host.http.do", hostHTTPRequest{Method: "GET", URL: url, Headers: headers}, &resp); err != nil {
		return keeper.Usage{}, err
	}
	if resp.StatusCode != 200 {
		return keeper.Usage{}, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, resp.Body)
	}
	if provider == "codex" {
		return keeper.ParseCodexUsage(resp.Body, time.Now())
	}
	return keeper.ParseUsage(resp.Body)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// start sends one tiny request through the account, via the host's normal path for its provider
// (pinned to that credential), which opens a new 5-hour window, and records the outcome.
func start(a *account, model string) error {
	mu.Lock()
	id, provider := a.ID, a.Provider
	mu.Unlock()
	req := map[string]any{"model": model, "stream": false, "auth_id": id, "forced_provider": provider,
		"headers": http.Header{"Content-Type": {"application/json"}}}
	var body []byte
	if provider == "codex" {
		req["entry_protocol"], req["exit_protocol"] = "openai-response", "openai-response"
		body, _ = json.Marshal(map[string]any{"model": model, "input": "hi", "max_output_tokens": 16})
	} else {
		req["entry_protocol"], req["exit_protocol"] = "claude", "claude"
		body, _ = json.Marshal(map[string]any{"model": model, "max_tokens": 1,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}})
	}
	req["body"] = body
	var resp struct {
		StatusCode int         `json:"status_code"`
		Headers    http.Header `json:"headers"`
	}
	err := callHost("host.model.execute", req, &resp)
	if err == nil && resp.StatusCode != 200 {
		err = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	now := time.Now()
	mu.Lock()
	defer mu.Unlock()
	a.StartedAt, a.StartErr = now, ""
	if err != nil {
		a.StartErr = err.Error()
		return err
	}
	if provider == "codex" {
		a.headerResets = keeper.CodexSessionFromHeaders(resp.Headers, now)
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
