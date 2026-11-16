package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/mcditchman/herdr-vitals/internal/usage"
)

// ConfigFile is the file read from the plugin's config directory
// (HERDR_PLUGIN_CONFIG_DIR, see `herdr plugin config-dir mcditchman.vitals`).
const ConfigFile = "config.json"

const (
	DefaultPoll     = 2 * time.Second
	minPoll         = 250 * time.Millisecond
	DefaultBarWidth = 8
	maxBarWidth     = 20
	DefaultToken    = "ctx"
	DefaultBranch   = "branch"
	DefaultAgents   = "agents"
	DefaultSubagent = "subagents"
	DefaultMonitors = "monitors"
	// maxAgentsToken leaves room for "_working"/"_blocked" within Herdr's
	// 32-character token names.
	maxAgentsToken = 32 - max(len(workingSuffix), len(blockedSuffix))
)

type Config struct {
	// Poll is how often the session is read.
	Poll time.Duration
	// BarWidth is how many cells the bar has; 0 shows the percentage alone.
	BarWidth int
	// Token is the pane metadata token the value is published under, shown
	// in the sidebar wherever "$<Token>" appears in a row.
	Token string
	// BranchToken is the token the agent's git branch is published under;
	// empty turns branch publishing off.
	BranchToken string
	// SubagentsToken is the token a Claude agent's running subagent count is
	// published under, only while it has any; empty turns it off.
	SubagentsToken string
	// MonitorsToken is the token a Claude agent's running Monitor count is
	// published under, only while it has any; empty turns it off.
	MonitorsToken string
	// AgentsToken is the workspace token each space's agent count is
	// published under, shown in space rows as "$<AgentsToken>"; empty turns
	// it off.
	AgentsToken string
	// DefaultWindow is the context window of any model not otherwise known.
	DefaultWindow int64
	// Windows overrides the context window by model id prefix.
	Windows map[string]int64
	// ClaudeDir is Claude Code's config directory; empty means
	// CLAUDE_CONFIG_DIR, else ~/.claude.
	ClaudeDir string
}

func DefaultConfig() Config {
	return Config{
		Poll:           DefaultPoll,
		BarWidth:       DefaultBarWidth,
		Token:          DefaultToken,
		BranchToken:    DefaultBranch,
		SubagentsToken: DefaultSubagent,
		MonitorsToken:  DefaultMonitors,
		AgentsToken:    DefaultAgents,
		DefaultWindow:  usage.OneMillion,
	}
}

// fileConfig is config.json. Pointers tell "absent" from "zero".
type fileConfig struct {
	PollMs         *int64           `json:"poll_ms"`
	BarWidth       *int             `json:"bar_width"`
	Token          *string          `json:"token"`
	BranchToken    *string          `json:"branch_token"`
	SubagentsToken *string          `json:"subagents_token"`
	MonitorsToken  *string          `json:"monitors_token"`
	AgentsToken    *string          `json:"agents_token"`
	DefaultWindow  *int64           `json:"default_window"`
	Windows        map[string]int64 `json:"windows"`
	ClaudeDir      *string          `json:"claude_dir"`
}

// tokenPattern is what Herdr accepts as a metadata token name.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// LoadConfig reads dir/config.json over the defaults. Nothing here is fatal:
// a missing file is the defaults, and each unusable value is reported as a
// warning with its default kept, so a typo never stops the plugin.
func LoadConfig(dir string) (Config, []string) {
	cfg := DefaultConfig()
	if dir == "" {
		return cfg, nil
	}

	path := filepath.Join(dir, ConfigFile)

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}

	if err != nil {
		return cfg, []string{fmt.Sprintf("read %s: %v; using defaults", path, err)}
	}

	var file fileConfig
	if err := json.Unmarshal(raw, &file); err != nil {
		return cfg, []string{fmt.Sprintf("parse %s: %v; using defaults", path, err)}
	}

	return apply(cfg, file)
}

func apply(cfg Config, file fileConfig) (Config, []string) {
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	if file.PollMs != nil {
		if poll := time.Duration(*file.PollMs) * time.Millisecond; poll >= minPoll {
			cfg.Poll = poll
		} else {
			warn("poll_ms %d is below %d; using %d", *file.PollMs, minPoll.Milliseconds(), cfg.Poll.Milliseconds())
		}
	}

	if file.BarWidth != nil {
		if w := *file.BarWidth; w >= 0 && w <= maxBarWidth {
			cfg.BarWidth = w
		} else {
			warn("bar_width %d is outside 0..%d; using %d", w, maxBarWidth, cfg.BarWidth)
		}
	}

	if file.Token != nil {
		if tokenPattern.MatchString(*file.Token) {
			cfg.Token = *file.Token
		} else {
			warn("token %q must match %s; using %q", *file.Token, tokenPattern, cfg.Token)
		}
	}

	// Pane tokens share one namespace, so none may reuse another's name. The
	// agents token lives on workspaces and cannot collide with them.
	cfg.BranchToken = optionalToken(warn, "branch_token", file.BranchToken, cfg.BranchToken, cfg.Token)
	cfg.SubagentsToken = optionalToken(warn, "subagents_token", file.SubagentsToken, cfg.SubagentsToken,
		cfg.Token, cfg.BranchToken)
	cfg.MonitorsToken = optionalToken(warn, "monitors_token", file.MonitorsToken, cfg.MonitorsToken,
		cfg.Token, cfg.BranchToken, cfg.SubagentsToken)
	if agents := optionalToken(warn, "agents_token", file.AgentsToken, cfg.AgentsToken); len(agents) <= maxAgentsToken {
		cfg.AgentsToken = agents
	} else {
		warn("agents_token %q is longer than %d, leaving no room for its _working/_blocked tokens; using %q",
			agents, maxAgentsToken, cfg.AgentsToken)
	}

	if file.DefaultWindow != nil {
		if *file.DefaultWindow > 0 {
			cfg.DefaultWindow = *file.DefaultWindow
		} else {
			warn("default_window %d must be positive; using %d", *file.DefaultWindow, cfg.DefaultWindow)
		}
	}

	for prefix, window := range file.Windows {
		if window <= 0 {
			warn("windows[%q] = %d must be positive; ignored", prefix, window)
			continue
		}

		if cfg.Windows == nil {
			cfg.Windows = make(map[string]int64, len(file.Windows))
		}

		cfg.Windows[prefix] = window
	}

	if file.ClaudeDir != nil {
		cfg.ClaudeDir = *file.ClaudeDir
	}

	return cfg, warnings
}

// optionalToken validates a token name that may be "" (feature off). An
// invalid name, or one already taken by another token, keeps current with a
// warning.
func optionalToken(warn func(string, ...any), key string, raw *string, current string, taken ...string) string {
	if raw == nil {
		return current
	}

	switch name := *raw; {
	case name == "":
		return ""
	case !tokenPattern.MatchString(name):
		warn("%s %q must match %s or be empty; using %q", key, name, tokenPattern, current)
	case slices.Contains(taken, name):
		warn("%s %q collides with another token; using %q", key, name, current)
	default:
		return name
	}

	return current
}
