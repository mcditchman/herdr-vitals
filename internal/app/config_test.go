package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mcditchman/herdr-vitals/internal/usage"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestLoadConfigDefaults(t *testing.T) {
	for name, dir := range map[string]string{"no dir": "", "no file": t.TempDir()} {
		t.Run(name, func(t *testing.T) {
			cfg, warnings := LoadConfig(dir)
			if len(warnings) != 0 {
				t.Fatalf("warnings = %v", warnings)
			}

			want := DefaultConfig()
			if cfg.Poll != want.Poll || cfg.BarWidth != want.BarWidth || cfg.Token != want.Token ||
				cfg.DefaultWindow != usage.OneMillion || cfg.Windows != nil || cfg.ClaudeDir != "" {
				t.Fatalf("cfg = %+v, want defaults %+v", cfg, want)
			}
		})
	}
}

func TestLoadConfigReadsEveryField(t *testing.T) {
	dir := writeConfig(t, `{
		"poll_ms": 5000,
		"bar_width": 0,
		"token": "context_pct",
		"default_window": 200000,
		"windows": {"claude-opus-5-5": 500000},
		"claude_dir": "D:\\claude"
	}`)

	cfg, warnings := LoadConfig(dir)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}

	if cfg.Poll != 5*time.Second || cfg.BarWidth != 0 || cfg.Token != "context_pct" ||
		cfg.DefaultWindow != 200_000 || cfg.Windows["claude-opus-5-5"] != 500_000 || cfg.ClaudeDir != `D:\claude` {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadConfigRejectsBadValuesIndividually(t *testing.T) {
	dir := writeConfig(t, `{
		"poll_ms": 10,
		"bar_width": 99,
		"token": "has space",
		"default_window": -1,
		"windows": {"good": 1000, "bad": 0}
	}`)

	cfg, warnings := LoadConfig(dir)

	if len(warnings) != 5 {
		t.Fatalf("warnings = %d (%v), want 5", len(warnings), warnings)
	}

	want := DefaultConfig()
	if cfg.Poll != want.Poll || cfg.BarWidth != want.BarWidth || cfg.Token != want.Token || cfg.DefaultWindow != want.DefaultWindow {
		t.Fatalf("bad values were not replaced by defaults: %+v", cfg)
	}

	if len(cfg.Windows) != 1 || cfg.Windows["good"] != 1000 {
		t.Fatalf("windows = %v, want only the good entry", cfg.Windows)
	}
}

func TestLoadConfigBranchToken(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		warnings int
	}{
		{"default", `{}`, DefaultBranch, 0},
		{"renamed", `{"branch_token": "git_branch"}`, "git_branch", 0},
		{"empty turns it off", `{"branch_token": ""}`, "", 0},
		{"invalid name keeps default", `{"branch_token": "has space"}`, DefaultBranch, 1},
		{"collision with token keeps default", `{"branch_token": "ctx"}`, DefaultBranch, 1},
		{"collision with a renamed token", `{"token": "x", "branch_token": "x"}`, DefaultBranch, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, warnings := LoadConfig(writeConfig(t, tt.body))
			if cfg.BranchToken != tt.want || len(warnings) != tt.warnings {
				t.Fatalf("BranchToken = %q with %v, want %q with %d warnings", cfg.BranchToken, warnings, tt.want, tt.warnings)
			}
		})
	}
}

func TestLoadConfigSubagentsToken(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		warnings int
	}{
		{"default", `{}`, DefaultSubagent, 0},
		{"renamed", `{"subagents_token": "subs"}`, "subs", 0},
		{"empty turns it off", `{"subagents_token": ""}`, "", 0},
		{"collides with the usage token", `{"subagents_token": "ctx"}`, DefaultSubagent, 1},
		{"collides with the branch token", `{"subagents_token": "branch"}`, DefaultSubagent, 1},
		{"collides with a renamed branch token", `{"branch_token": "b", "subagents_token": "b"}`, DefaultSubagent, 1},
		{"free once the branch token moves", `{"branch_token": "b", "subagents_token": "branch"}`, "branch", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, warnings := LoadConfig(writeConfig(t, tt.body))
			if cfg.SubagentsToken != tt.want || len(warnings) != tt.warnings {
				t.Fatalf("SubagentsToken = %q with %v, want %q with %d warnings", cfg.SubagentsToken, warnings, tt.want, tt.warnings)
			}
		})
	}
}

func TestLoadConfigMonitorsToken(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		warnings int
	}{
		{"default", `{}`, DefaultMonitors, 0},
		{"renamed", `{"monitors_token": "watch"}`, "watch", 0},
		{"empty turns it off", `{"monitors_token": ""}`, "", 0},
		{"invalid name", `{"monitors_token": "has space"}`, DefaultMonitors, 1},
		{"collides with the usage token", `{"monitors_token": "ctx"}`, DefaultMonitors, 1},
		{"collides with the branch token", `{"monitors_token": "branch"}`, DefaultMonitors, 1},
		{"collides with the subagents token", `{"monitors_token": "subagents"}`, DefaultMonitors, 1},
		{"collides with a renamed subagents token", `{"subagents_token": "s", "monitors_token": "s"}`, DefaultMonitors, 1},
		{"free once the subagents token moves", `{"subagents_token": "s", "monitors_token": "subagents"}`, "subagents", 0},
		{"free once the subagents token is off", `{"subagents_token": "", "monitors_token": "subagents"}`, "subagents", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, warnings := LoadConfig(writeConfig(t, tt.body))
			if cfg.MonitorsToken != tt.want || len(warnings) != tt.warnings {
				t.Fatalf("MonitorsToken = %q with %v, want %q with %d warnings", cfg.MonitorsToken, warnings, tt.want, tt.warnings)
			}
		})
	}
}

func TestLoadConfigAgentsToken(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     string
		warnings int
	}{
		{"default", `{}`, DefaultAgents, 0},
		{"renamed", `{"agents_token": "agent_count"}`, "agent_count", 0},
		{"empty turns it off", `{"agents_token": ""}`, "", 0},
		{"invalid name keeps default", `{"agents_token": "a/b"}`, DefaultAgents, 1},
		// Workspace tokens are their own namespace, so sharing a pane token's name is fine.
		{"may share a pane token name", `{"agents_token": "ctx"}`, "ctx", 0},
		{"24 characters fits its suffixes", `{"agents_token": "` + strings.Repeat("a", 24) + `"}`, strings.Repeat("a", 24), 0},
		{"25 characters does not", `{"agents_token": "` + strings.Repeat("a", 25) + `"}`, DefaultAgents, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, warnings := LoadConfig(writeConfig(t, tt.body))
			if cfg.AgentsToken != tt.want || len(warnings) != tt.warnings {
				t.Fatalf("AgentsToken = %q with %v, want %q with %d warnings", cfg.AgentsToken, warnings, tt.want, tt.warnings)
			}
		})
	}
}

func TestLoadConfigMalformedFileWarnsAndUsesDefaults(t *testing.T) {
	cfg, warnings := LoadConfig(writeConfig(t, `{"poll_ms": `))

	if len(warnings) != 1 || !strings.Contains(warnings[0], "parse") {
		t.Fatalf("warnings = %v, want one parse warning", warnings)
	}

	if cfg.Poll != DefaultPoll {
		t.Fatalf("poll = %v, want default", cfg.Poll)
	}
}
