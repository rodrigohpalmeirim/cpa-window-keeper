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
	c, err = parseConfig([]byte("codex: {enabled: true}\nclaude: {enabled: false, model: claude-haiku-4-5}\nstagger: false\n"))
	if err != nil || !*c.Codex.Enabled || *c.Claude.Enabled || c.Claude.Model != "claude-haiku-4-5" || *c.Stagger {
		t.Fatalf("toggles = %+v, %v", c, err)
	}
	if _, err := parseConfig([]byte("mode: sometimes\n")); err == nil {
		t.Fatal("bad mode accepted")
	}
}
