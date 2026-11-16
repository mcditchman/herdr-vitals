// Package app is the poll loop: read the session, work out what each agent
// pane should show (context usage, git branch) and what each space should
// show (agent count), and publish them as metadata tokens.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/mcditchman/herdr-vitals/internal/git"
	"github.com/mcditchman/herdr-vitals/internal/herdr"
	"github.com/mcditchman/herdr-vitals/internal/transcript"
	"github.com/mcditchman/herdr-vitals/internal/usage"
)

// Source names this plugin's metadata to Herdr.
const Source = "vitals"

// claudeAgent is the label Herdr gives Claude Code panes.
const claudeAgent = "claude"

// minTTL keeps tokens alive across a few slow polls. The TTL exists so a
// plugin that dies leaves no stale value behind, not to expire live ones.
const minTTL = 30 * time.Second

// pollTimeout bounds one poll so a wedged socket cannot stall the loop.
const pollTimeout = 10 * time.Second

// ContextReader is what the app needs from transcript.Reader.
type ContextReader interface {
	Context(sessionID, dir string) (transcript.Context, bool)
	Retain(sessionIDs []string)
}

// BranchReader reports the branch checked out at dir, "" for none.
type BranchReader func(dir string) string

// target is what a token set is published on: a pane (agent rows) or a
// workspace (space rows).
type target struct {
	workspace bool
	id        string
}

func paneTarget(id string) target      { return target{id: id} }
func workspaceTarget(id string) target { return target{workspace: true, id: id} }

// reported is the token set last published on a target, and when.
type reported struct {
	tokens map[string]string
	at     time.Time
}

type App struct {
	cfg      Config
	log      *slog.Logger
	client   herdr.Client
	reader   ContextReader
	branch   BranchReader
	reported map[target]reported
	server   string
	ttl      time.Duration
	now      func() time.Time
}

func New(cfg Config, log *slog.Logger, client herdr.Client, reader ContextReader) *App {
	return &App{
		cfg:      cfg,
		log:      log,
		client:   client,
		reader:   reader,
		branch:   git.Branch,
		reported: make(map[target]reported),
		ttl:      max(minTTL, 5*cfg.Poll),
		now:      time.Now,
	}
}

// Run polls until ctx is cancelled or another Herdr server takes the socket
// (that server runs its own copy of this plugin), then clears what it set.
func (a *App) Run(ctx context.Context) {
	defer a.clearAll()

	ticker := time.NewTicker(a.cfg.Poll)
	defer ticker.Stop()

	for {
		if a.superseded() {
			a.log.Info("another herdr server holds the socket, leaving")
			return
		}

		if err := a.Poll(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("poll failed", "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// superseded reports whether the socket has passed to a server other than the
// first one seen. While none can be read nothing is decided: Herdr comes and
// goes, and only a successor is a reason to leave.
func (a *App) superseded() bool {
	current := a.client.Server()

	switch {
	case current == "":
		return false
	case a.server == "":
		a.server = current
		return false
	default:
		return current != a.server
	}
}

// Poll runs one read-and-report pass. A failure on one pane does not stop the
// others; the first such error is returned.
func (a *App) Poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	snap, err := herdr.SessionSnapshot(ctx, a.client)
	if err != nil {
		return err
	}

	live := make(map[target]bool, len(snap.Panes)+len(snap.Workspaces))
	spaces := make(map[string]spaceCount, len(snap.Workspaces))
	var sessions []string
	var errs []error

	for _, pane := range snap.Panes {
		t := paneTarget(pane.PaneID)
		live[t] = true

		if pane.Agent != "" {
			spaces[pane.WorkspaceID] = spaces[pane.WorkspaceID].add(pane.AgentStatus)
		}

		tokens, sessionID := a.tokensFor(pane)
		if sessionID != "" {
			sessions = append(sessions, sessionID)
		}

		errs = append(errs, a.publish(ctx, t, tokens))
	}

	for _, ws := range snap.Workspaces {
		t := workspaceTarget(ws.WorkspaceID)
		live[t] = true

		errs = append(errs, a.publish(ctx, t, a.workspaceTokens(spaces[ws.WorkspaceID])))
	}

	// A closed pane or workspace takes its metadata with it; only the
	// bookkeeping is left.
	for t := range a.reported {
		if !live[t] {
			delete(a.reported, t)
		}
	}

	a.reader.Retain(sessions)

	return firstOf(errs)
}

// firstOf is the first non-nil error: one target failing does not stop the
// others, and one line in the log is enough to say the socket is unhappy.
func firstOf(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}

	return nil
}

// spaceCount tallies a space's agent panes by the statuses broken out.
type spaceCount struct {
	agents, working, blocked int
}

func (c spaceCount) add(status string) spaceCount {
	c.agents++

	switch status {
	case herdr.AgentStatusWorking:
		c.working++
	case herdr.AgentStatusBlocked:
		c.blocked++
	}

	return c
}

// Suffixes naming the breakdown tokens after the agents token, so one
// setting names all three: agents, agents_working, agents_blocked.
const (
	workingSuffix = "_working"
	blockedSuffix = "_blocked"
)

// workspaceTokens is what a space shows: its agent count, plus how many are
// working and blocked when any are. Nothing when it has no agents or the
// count is turned off.
func (a *App) workspaceTokens(c spaceCount) map[string]string {
	name := a.cfg.AgentsToken
	if name == "" || c.agents == 0 {
		return map[string]string{}
	}

	tokens := map[string]string{name: agentCount(c.agents)}
	if c.working > 0 {
		tokens[name+workingSuffix] = fmt.Sprintf("%d working", c.working)
	}

	if c.blocked > 0 {
		tokens[name+blockedSuffix] = fmt.Sprintf("%d blocked", c.blocked)
	}

	return tokens
}

func agentCount(n int) string {
	return plural(n, "agent")
}

// The marks set a subagent or monitor count apart from the context bar
// beside it.
const (
	subagentMark = "⧉ "
	monitorMark  = "⦿ "
)

func subagentCount(n int) string {
	return subagentMark + plural(n, "subagent")
}

func monitorCount(n int) string {
	return monitorMark + plural(n, "monitor")
}

// setCount publishes a running count under token, only while it is above
// zero and the token is on.
func setCount(tokens map[string]string, token string, n int, label func(int) string) {
	if token != "" && n > 0 {
		tokens[token] = label(n)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// tokensFor is what pane should show, keyed by token name, and the Claude
// session it holds if any. Panes without an agent show nothing.
func (a *App) tokensFor(pane herdr.PaneInfo) (map[string]string, string) {
	tokens := map[string]string{}
	if pane.Agent == "" {
		return tokens, ""
	}

	dir := pane.CWD

	var sessionID string
	if pane.Agent == claudeAgent {
		var read transcript.Context
		var known bool

		if id, ok := pane.AgentSession.IDFor(claudeAgent); ok {
			sessionID = id
			read, known = a.reader.Context(id, pane.CWD)
		}

		if known {
			window := usage.WindowFor(read.Model, a.cfg.Windows, a.cfg.DefaultWindow)
			tokens[a.cfg.Token] = usage.Bar(usage.Percent(read.Tokens, window), a.cfg.BarWidth)

			setCount(tokens, a.cfg.SubagentsToken, read.Subagents, subagentCount)
			setCount(tokens, a.cfg.MonitorsToken, read.Monitors, monitorCount)

			// Where Claude itself works beats the pane's shell directory.
			if read.Cwd != "" {
				dir = read.Cwd
			}
		}
	}

	if a.cfg.BranchToken != "" {
		if branch := a.branch(dir); branch != "" {
			tokens[a.cfg.BranchToken] = branch
		}
	}

	return tokens, sessionID
}

// publish brings the target's tokens in line with want. Any change re-sends
// the whole set (and clears tokens no longer wanted), so every token shares
// one TTL; an unchanged set is only re-sent to keep it ahead of that TTL.
func (a *App) publish(ctx context.Context, t target, want map[string]string) error {
	prev, had := a.reported[t]
	now := a.now()

	switch {
	case !had && len(want) == 0:
		return nil
	case had && maps.Equal(prev.tokens, want) && now.Sub(prev.at) < a.ttl/2:
		return nil
	}

	changes := make(map[string]*string, len(want)+len(prev.tokens))
	for name := range prev.tokens {
		changes[name] = nil
	}

	for name, value := range want {
		changes[name] = &value
	}

	return a.send(ctx, t, changes, func() {
		if len(want) == 0 {
			delete(a.reported, t)
			return
		}

		a.reported[t] = reported{tokens: want, at: now}
	})
}

// send reports token changes (nil clears a token) and runs onSuccess if Herdr
// took them. A target that closed meanwhile is forgotten rather than retried.
func (a *App) send(ctx context.Context, t target, tokens map[string]*string, onSuccess func()) error {
	ttl := uint64(a.ttl.Milliseconds())

	var err error
	if t.workspace {
		err = herdr.ReportWorkspaceMetadata(ctx, a.client, herdr.WorkspaceReportMetadataParams{
			WorkspaceID: t.id, Source: Source, Tokens: tokens, TTLMs: ttl,
		})
	} else {
		err = herdr.ReportMetadata(ctx, a.client, herdr.ReportMetadataParams{
			PaneID: t.id, Source: Source, Tokens: tokens, TTLMs: ttl,
		})
	}

	switch code := herdr.ErrorCode(err); {
	case err == nil:
		onSuccess()
		return nil
	case code == herdr.CodePaneNotFound, code == herdr.CodeWorkspaceNotFound:
		delete(a.reported, t)
		return nil
	default:
		return err
	}
}

// clearAll best-effort clears every token this run set, so stopping the
// plugin does not leave values to linger until their TTL.
func (a *App) clearAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var errs []error
	for t := range a.reported {
		errs = append(errs, a.publish(ctx, t, map[string]string{}))
	}

	if err := errors.Join(errs...); err != nil {
		a.log.Warn("could not clear every token on exit", "error", err)
	}
}
