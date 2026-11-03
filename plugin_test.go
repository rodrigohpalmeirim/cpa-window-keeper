package main

import "testing"

func TestConfigDefaults(t *testing.T) {
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "active" || !*c.Stagger || !*c.Claude.Enabled || *c.Codex.Enabled ||
		c.Claude.Model != "claude-haiku-5-5" || c.Codex.Model != "gpt-6-luna" {
		t.Fatalf("defaults = %+v", c)
	}
	c, err = parseConfig([]byte("codex: true\nclaude: false\nclaude_model: claude-haiku-4-5\nstagger: false\naccounts: \"a@x, b\"\n"))
	if err != nil || !*c.Codex.Enabled || *c.Claude.Enabled || c.Claude.Model != "claude-haiku-4-5" || *c.Stagger ||
		len(c.Accounts) != 2 || c.Accounts[1] != "b" {
		t.Fatalf("toggles = %+v, %v", c, err)
	}
	if c, err := parseConfig([]byte("accounts: [a, b, c]\n")); err != nil || len(c.Accounts) != 3 {
		t.Fatalf("list accounts = %+v, %v", c.Accounts, err)
	}
	if _, err := parseConfig([]byte("mode: sometimes\n")); err == nil {
		t.Fatal("bad mode accepted")
	}
}
