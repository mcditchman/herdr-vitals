package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"testing"
	"time"

	"github.com/mcditchman/herdr-vitals/internal/herdr"
	"github.com/mcditchman/herdr-vitals/internal/transcript"
)

// fakeClient serves a fixed snapshot and records every metadata report.
type fakeClient struct {
	panes      []herdr.PaneInfo
	workspaces []herdr.WorkspaceInfo
	snapErr    error
	reportErr  map[string]error // by pane or workspace id
	reports    []herdr.ReportMetadataParams
	wsReports  []herdr.WorkspaceReportMetadataParams
	server     string
}

func (f *fakeClient) Server() string { return f.server }

func (f *fakeClient) Call(_ context.Context, method string, params any, result any) error {
	switch method {
	case herdr.MethodSessionSnapshot:
		if f.snapErr != nil {
			return f.snapErr
		}

		raw, _ := json.Marshal(map[string]any{"snapshot": herdr.Snapshot{Workspaces: f.workspaces, Panes: f.panes}})
		return json.Unmarshal(raw, result)
	case herdr.MethodPaneReportMetadata:
		p := params.(herdr.ReportMetadataParams)
		if err := f.reportErr[p.PaneID]; err != nil {
			return err
		}

		f.reports = append(f.reports, p)
		return nil
	case herdr.MethodWorkspaceReportMetadata:
		p := params.(herdr.WorkspaceReportMetadataParams)
		if err := f.reportErr[p.WorkspaceID]; err != nil {
			return err
		}

		f.wsReports = append(f.wsReports, p)
		return nil
	default:
		return errors.New("unexpected method " + method)
	}
}

// fakeReader answers from a map of session id to context.
type fakeReader struct {
	contexts map[string]transcript.Context
	retained []string
}

func (f *fakeReader) Context(sessionID, _ string) (transcript.Context, bool) {
	c, ok := f.contexts[sessionID]
	return c, ok
}

func (f *fakeReader) Retain(ids []string) { f.retained = ids }

func claudePane(paneID, sessionID string) herdr.PaneInfo {
	return herdr.PaneInfo{
		PaneID:       paneID,
		Agent:        "claude",
		AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: sessionID},
	}
}

type harness struct {
	app      *App
	client   *fakeClient
	reader   *fakeReader
	branches map[string]string // dir -> branch
	now      time.Time
}

func newHarness(panes ...herdr.PaneInfo) *harness {
	h := &harness{
		client:   &fakeClient{panes: panes, reportErr: map[string]error{}},
		reader:   &fakeReader{contexts: map[string]transcript.Context{}},
		branches: map[string]string{},
		now:      time.Unix(1_000, 0),
	}

	h.app = New(DefaultConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.client, h.reader)
	h.app.now = func() time.Time { return h.now }
	h.app.branch = func(dir string) string { return h.branches[dir] }

	return h
}

// tokensOf flattens one report into name -> value, "<nil>" for a clear.
func tokensOf(p herdr.ReportMetadataParams) map[string]string {
	out := make(map[string]string, len(p.Tokens))
	for name, v := range p.Tokens {
		if v == nil {
			out[name] = "<nil>"
		} else {
			out[name] = *v
		}
	}

	return out
}

func mustReport(t *testing.T, reports []herdr.ReportMetadataParams, paneID string, want map[string]string) {
	t.Helper()

	if len(reports) != 1 || reports[0].PaneID != paneID {
		t.Fatalf("reports = %+v, want one for %s", reports, paneID)
	}

	if got := tokensOf(reports[0]); !maps.Equal(got, want) {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
}

func (h *harness) poll(t *testing.T) {
	t.Helper()

	if err := h.app.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

func (h *harness) takeReports() []herdr.ReportMetadataParams {
	r := h.client.reports
	h.client.reports = nil
	return r
}

func tokenOf(t *testing.T, p herdr.ReportMetadataParams) *string {
	t.Helper()

	v, ok := p.Tokens[DefaultToken]
	if !ok || len(p.Tokens) != 1 {
		t.Fatalf("report tokens = %v, want exactly %q", p.Tokens, DefaultToken)
	}

	return v
}

func mustSet(t *testing.T, reports []herdr.ReportMetadataParams, paneID, want string) {
	t.Helper()

	if len(reports) != 1 {
		t.Fatalf("reports = %+v, want one for %s", reports, paneID)
	}

	p := reports[0]
	if p.PaneID != paneID || p.Source != Source || p.TTLMs == 0 {
		t.Fatalf("report = %+v, want pane %s source %s with a TTL", p, paneID, Source)
	}

	if v := tokenOf(t, p); v == nil || *v != want {
		t.Fatalf("token = %v, want %q", v, want)
	}
}

func mustClear(t *testing.T, reports []herdr.ReportMetadataParams, paneID string) {
	t.Helper()

	if len(reports) != 1 || reports[0].PaneID != paneID {
		t.Fatalf("reports = %+v, want one clear for %s", reports, paneID)
	}

	if v := tokenOf(t, reports[0]); v != nil {
		t.Fatalf("token = %q, want nil (clear)", *v)
	}
}

const session1 = "11111111-1111-1111-1111-111111111111"

func TestPublishesBarForClaudePane(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)

	mustSet(t, h.takeReports(), "w1:p1", "▰▰▰▱▱▱▱▱ 41%")
}

func TestUsesLegacyWindowForOlderModels(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 100_000, Model: "claude-haiku-4-5"}

	h.poll(t)

	mustSet(t, h.takeReports(), "w1:p1", "▰▰▰▰▱▱▱▱ 50%")
}

func TestConfiguredWindowOverride(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.app.cfg.Windows = map[string]int64{"claude-opus-5-5": 200_000}
	h.reader.contexts[session1] = transcript.Context{Tokens: 100_000, Model: "claude-opus-5-5"}

	h.poll(t)

	mustSet(t, h.takeReports(), "w1:p1", "▰▰▰▰▱▱▱▱ 50%")
}

func TestIgnoresNonClaudePanes(t *testing.T) {
	shell := herdr.PaneInfo{PaneID: "w1:p1"}
	codex := herdr.PaneInfo{PaneID: "w1:p2", Agent: "codex",
		AgentSession: &herdr.AgentSessionInfo{Agent: "codex", Kind: "id", Value: session1}}
	// Claude has exited but Herdr still remembers its session.
	exited := herdr.PaneInfo{PaneID: "w1:p3",
		AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: session1}}
	noSession := herdr.PaneInfo{PaneID: "w1:p4", Agent: "claude"}

	h := newHarness(shell, codex, exited, noSession)
	h.reader.contexts[session1] = transcript.Context{Tokens: 1, Model: "claude-opus-5-5"}

	h.poll(t)

	if r := h.takeReports(); len(r) != 0 {
		t.Fatalf("reports = %+v, want none", r)
	}
}

func TestNothingPublishedUntilUsageIsKnown(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))

	h.poll(t)

	if r := h.takeReports(); len(r) != 0 {
		t.Fatalf("reports = %+v, want none", r)
	}
}

func TestUnchangedValueIsNotResentUntilHalfTTL(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	h.now = h.now.Add(h.app.ttl/2 - time.Millisecond)
	h.poll(t)
	if r := h.takeReports(); len(r) != 0 {
		t.Fatalf("reports before half TTL = %+v, want none", r)
	}

	h.now = h.now.Add(time.Millisecond)
	h.poll(t)
	mustSet(t, h.takeReports(), "w1:p1", "▰▰▰▱▱▱▱▱ 41%")
}

func TestChangedValueIsSentImmediately(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	h.reader.contexts[session1] = transcript.Context{Tokens: 420_000, Model: "claude-opus-5-5"}
	h.poll(t)

	mustSet(t, h.takeReports(), "w1:p1", "▰▰▰▱▱▱▱▱ 42%")
}

func TestClearsWhenClaudeLeavesThePane(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	h.client.panes = []herdr.PaneInfo{{PaneID: "w1:p1"}}
	h.poll(t)
	mustClear(t, h.takeReports(), "w1:p1")

	// Cleared once, not on every poll after.
	h.poll(t)
	if r := h.takeReports(); len(r) != 0 {
		t.Fatalf("reports = %+v, want none", r)
	}
}

func TestClosedPaneIsForgottenWithoutAReport(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	h.client.panes = nil
	h.poll(t)

	if r := h.takeReports(); len(r) != 0 {
		t.Fatalf("reports = %+v, want none", r)
	}

	if len(h.app.reported) != 0 {
		t.Fatalf("reported = %v, want empty", h.app.reported)
	}
}

func TestPaneNotFoundIsNotAnError(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}
	h.client.reportErr["w1:p1"] = &herdr.APIError{Code: herdr.CodePaneNotFound}

	h.poll(t)

	if len(h.app.reported) != 0 {
		t.Fatalf("reported = %v, want empty", h.app.reported)
	}
}

func TestReportFailureIsRetriedAndOtherPanesStillPublish(t *testing.T) {
	const session2 = "22222222-2222-2222-2222-222222222222"

	h := newHarness(claudePane("w1:p1", session1), claudePane("w1:p2", session2))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}
	h.reader.contexts[session2] = transcript.Context{Tokens: 100_000, Model: "claude-opus-5-5"}
	h.client.reportErr["w1:p1"] = errors.New("socket hiccup")

	if err := h.app.Poll(context.Background()); err == nil {
		t.Fatal("Poll: want the report error")
	}

	mustSet(t, h.takeReports(), "w1:p2", "▰▱▱▱▱▱▱▱ 10%")

	delete(h.client.reportErr, "w1:p1")
	h.poll(t)
	mustSet(t, h.takeReports(), "w1:p1", "▰▰▰▱▱▱▱▱ 41%")
}

func TestSnapshotFailureIsReturned(t *testing.T) {
	h := newHarness()
	h.client.snapErr = errors.New("down")

	if err := h.app.Poll(context.Background()); err == nil {
		t.Fatal("Poll: want an error")
	}
}

func TestRetainsLiveClaudeSessions(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1), herdr.PaneInfo{PaneID: "w1:p2"})

	h.poll(t)

	if len(h.reader.retained) != 1 || h.reader.retained[0] != session1 {
		t.Fatalf("retained = %v, want [%s]", h.reader.retained, session1)
	}
}

func TestCustomTokenName(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.app.cfg.Token = "context"
	h.reader.contexts[session1] = transcript.Context{Tokens: 1, Model: "claude-opus-5-5"}

	h.poll(t)

	r := h.takeReports()
	if len(r) != 1 || r[0].Tokens["context"] == nil {
		t.Fatalf("reports = %+v, want one under token \"context\"", r)
	}
}

func TestTTLScalesWithSlowPolls(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Poll = 20 * time.Second

	a := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeClient{}, &fakeReader{})
	if a.ttl != 100*time.Second {
		t.Fatalf("ttl = %v, want 100s", a.ttl)
	}

	if b := New(DefaultConfig(), a.log, &fakeClient{}, &fakeReader{}); b.ttl != minTTL {
		t.Fatalf("ttl = %v, want %v", b.ttl, minTTL)
	}
}

func TestSuperseded(t *testing.T) {
	h := newHarness()

	steps := []struct {
		server string
		want   bool
	}{
		{"", false},      // no server yet: nothing decided
		{"1:100", false}, // first server seen
		{"", false},      // server briefly gone
		{"1:100", false}, // same server
		{"2:200", true},  // a successor
	}

	for i, s := range steps {
		h.client.server = s.server
		if got := h.app.superseded(); got != s.want {
			t.Fatalf("step %d (%q): superseded = %v, want %v", i, s.server, got, s.want)
		}
	}
}

const (
	paneDir   = `C:\repos\my-app\`
	agentDir  = `C:\repos\my-app\.worktrees\feature-login`
	bar41     = "▰▰▰▱▱▱▱▱ 41%"
	ctxToken  = DefaultToken
	branchTok = DefaultBranch
)

func claudePaneIn(paneID, sessionID, dir string) herdr.PaneInfo {
	p := claudePane(paneID, sessionID)
	p.CWD = dir

	return p
}

func TestBranchComesFromWhereClaudeIsWorking(t *testing.T) {
	h := newHarness(claudePaneIn("w1:p1", session1, paneDir))
	h.branches[paneDir] = "dev"
	h.branches[agentDir] = "feature-login"
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Cwd: agentDir}

	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, branchTok: "feature-login"})
}

func TestBranchFallsBackToThePaneDirectory(t *testing.T) {
	h := newHarness(claudePaneIn("w1:p1", session1, paneDir))
	h.branches[paneDir] = "dev"

	t.Run("before any usage is known", func(t *testing.T) {
		h.poll(t)
		mustReport(t, h.takeReports(), "w1:p1", map[string]string{branchTok: "dev"})
	})

	t.Run("when the transcript names no cwd", func(t *testing.T) {
		h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}
		h.poll(t)
		mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, branchTok: "dev"})
	})
}

func TestOtherAgentsGetBranchButNoContext(t *testing.T) {
	codex := herdr.PaneInfo{PaneID: "w1:p2", Agent: "codex", CWD: paneDir}
	shell := herdr.PaneInfo{PaneID: "w1:p3", CWD: paneDir}

	h := newHarness(codex, shell)
	h.branches[paneDir] = "dev"

	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p2", map[string]string{branchTok: "dev"})
}

func TestBranchCanBeTurnedOff(t *testing.T) {
	h := newHarness(claudePaneIn("w1:p1", session1, paneDir))
	h.app.cfg.BranchToken = ""
	h.branches[paneDir] = "dev"
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41})
}

func TestBranchSwitchResendsTheWholeSet(t *testing.T) {
	h := newHarness(claudePaneIn("w1:p1", session1, paneDir))
	h.branches[paneDir] = "dev"
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	h.branches[paneDir] = "feature-login"
	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, branchTok: "feature-login"})
}

func TestLeavingTheRepositoryClearsOnlyTheBranch(t *testing.T) {
	h := newHarness(claudePaneIn("w1:p1", session1, paneDir))
	h.branches[paneDir] = "dev"
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	delete(h.branches, paneDir)
	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, branchTok: "<nil>"})
}

func TestClaudeLeavingClearsEveryToken(t *testing.T) {
	h := newHarness(claudePaneIn("w1:p1", session1, paneDir))
	h.branches[paneDir] = "dev"
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)
	h.takeReports()

	h.client.panes = []herdr.PaneInfo{{PaneID: "w1:p1", CWD: paneDir}}
	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: "<nil>", branchTok: "<nil>"})
}

func inWorkspace(p herdr.PaneInfo, workspaceID string) herdr.PaneInfo {
	p.WorkspaceID = workspaceID
	return p
}

func (h *harness) takeWorkspaceReports() map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, r := range h.client.wsReports {
		out[r.WorkspaceID] = tokensOf(herdr.ReportMetadataParams{Tokens: r.Tokens})
	}

	h.client.wsReports = nil

	return out
}

func mustWorkspaceReports(t *testing.T, h *harness, want map[string]map[string]string) {
	t.Helper()

	got := h.takeWorkspaceReports()
	if len(got) != len(want) {
		t.Fatalf("workspace reports = %v, want %v", got, want)
	}

	for id, tokens := range want {
		if !maps.Equal(got[id], tokens) {
			t.Fatalf("workspace %s tokens = %v, want %v", id, got[id], tokens)
		}
	}
}

func withStatus(p herdr.PaneInfo, status string) herdr.PaneInfo {
	p.AgentStatus = status
	return p
}

const (
	workingTok = DefaultAgents + workingSuffix
	blockedTok = DefaultAgents + blockedSuffix
)

func TestAgentCountPerSpace(t *testing.T) {
	h := newHarness(
		inWorkspace(withStatus(claudePane("wB:p1", session1), "working"), "wB"),
		inWorkspace(withStatus(claudePane("wB:p2", session1), "working"), "wB"),
		inWorkspace(withStatus(claudePane("wB:p3", session1), "blocked"), "wB"),
		inWorkspace(withStatus(herdr.PaneInfo{PaneID: "wB:p4", Agent: "codex"}, "idle"), "wB"),
		inWorkspace(withStatus(claudePane("wB:p5", session1), "done"), "wB"),
		inWorkspace(herdr.PaneInfo{PaneID: "wB:p6", AgentStatus: "unknown"}, "wB"), // a shell is not an agent
		inWorkspace(withStatus(claudePane("w6:p1", session1), "idle"), "w6"),
		inWorkspace(herdr.PaneInfo{PaneID: "wC:p1"}, "wC"),
	)
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wB"}, {WorkspaceID: "w6"}, {WorkspaceID: "wC"}}

	h.poll(t)

	mustWorkspaceReports(t, h, map[string]map[string]string{
		"wB": {DefaultAgents: "5 agents", workingTok: "2 working", blockedTok: "1 blocked"},
		"w6": {DefaultAgents: "1 agent"},
	})
}

func TestBreakdownFollowsStatusChanges(t *testing.T) {
	h := newHarness(inWorkspace(withStatus(claudePane("wB:p1", session1), "idle"), "wB"))
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wB"}}

	h.poll(t)
	mustWorkspaceReports(t, h, map[string]map[string]string{"wB": {DefaultAgents: "1 agent"}})

	steps := []struct {
		status string
		want   map[string]string
	}{
		{"working", map[string]string{DefaultAgents: "1 agent", workingTok: "1 working"}},
		{"blocked", map[string]string{DefaultAgents: "1 agent", workingTok: "<nil>", blockedTok: "1 blocked"}},
		{"done", map[string]string{DefaultAgents: "1 agent", blockedTok: "<nil>"}},
	}

	for _, s := range steps {
		h.client.panes[0].AgentStatus = s.status
		h.poll(t)
		mustWorkspaceReports(t, h, map[string]map[string]string{"wB": s.want})
	}
}

func TestBreakdownUsesTheConfiguredAgentsToken(t *testing.T) {
	h := newHarness(inWorkspace(withStatus(claudePane("wB:p1", session1), "blocked"), "wB"))
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wB"}}
	h.app.cfg.AgentsToken = "n"

	h.poll(t)

	mustWorkspaceReports(t, h, map[string]map[string]string{"wB": {"n": "1 agent", "n_blocked": "1 blocked"}})
}

func TestAgentCountUpdatesAndClears(t *testing.T) {
	h := newHarness(inWorkspace(claudePane("wB:p1", session1), "wB"), inWorkspace(claudePane("wB:p2", session1), "wB"))
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wB"}}

	h.poll(t)
	h.takeWorkspaceReports()

	t.Run("unchanged count is not resent", func(t *testing.T) {
		h.poll(t)
		mustWorkspaceReports(t, h, map[string]map[string]string{})
	})

	t.Run("an agent exits", func(t *testing.T) {
		h.client.panes[1].Agent = ""
		h.poll(t)
		mustWorkspaceReports(t, h, map[string]map[string]string{"wB": {DefaultAgents: "1 agent"}})
	})

	t.Run("the last agent exits", func(t *testing.T) {
		h.client.panes[0].Agent = ""
		h.poll(t)
		mustWorkspaceReports(t, h, map[string]map[string]string{"wB": {DefaultAgents: "<nil>"}})
	})
}

func TestAgentCountCanBeTurnedOff(t *testing.T) {
	h := newHarness(inWorkspace(claudePane("wB:p1", session1), "wB"))
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wB"}}
	h.app.cfg.AgentsToken = ""

	h.poll(t)

	mustWorkspaceReports(t, h, map[string]map[string]string{})
}

func TestClosedWorkspaceIsForgottenAndNotFoundIsNotAnError(t *testing.T) {
	h := newHarness(inWorkspace(claudePane("wB:p1", session1), "wB"))
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wB"}}

	h.poll(t)
	h.takeWorkspaceReports()

	if _, ok := h.app.reported[workspaceTarget("wB")]; !ok {
		t.Fatal("workspace not tracked after publishing")
	}

	h.client.panes, h.client.workspaces = nil, nil
	h.poll(t)

	if _, ok := h.app.reported[workspaceTarget("wB")]; ok {
		t.Fatal("closed workspace still tracked")
	}

	h.client.panes = []herdr.PaneInfo{inWorkspace(claudePane("wD:p1", session1), "wD")}
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "wD"}}
	h.client.reportErr["wD"] = &herdr.APIError{Code: herdr.CodeWorkspaceNotFound}

	h.poll(t) // must not return the not-found as an error
}

func TestPaneAndWorkspaceWithTheSameIDAreTrackedApart(t *testing.T) {
	// Herdr's ids never collide today, but the bookkeeping must not rely on it.
	h := newHarness(inWorkspace(claudePane("x", session1), "x"))
	h.client.workspaces = []herdr.WorkspaceInfo{{WorkspaceID: "x"}}
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}

	h.poll(t)

	if len(h.app.reported) != 2 {
		t.Fatalf("reported = %v, want a pane and a workspace entry", h.app.reported)
	}
}

func TestSubagentCountShowsOnlyWhileAnyRun(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Subagents: 2}

	h.poll(t)
	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, DefaultSubagent: "⧉ 2 subagents"})

	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Subagents: 1}
	h.poll(t)
	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, DefaultSubagent: "⧉ 1 subagent"})

	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}
	h.poll(t)
	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, DefaultSubagent: "<nil>"})
}

func TestSubagentCountCanBeTurnedOff(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.app.cfg.SubagentsToken = ""
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Subagents: 2}

	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41})
}

func TestMonitorCountShowsOnlyWhileAnyRun(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Monitors: 2}

	h.poll(t)
	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, DefaultMonitors: "⦿ 2 monitors"})

	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Monitors: 1}
	h.poll(t)
	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, DefaultMonitors: "⦿ 1 monitor"})

	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}
	h.poll(t)
	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41, DefaultMonitors: "<nil>"})
}

func TestMonitorAndSubagentCountsShowSideBySide(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Subagents: 1, Monitors: 1}

	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{
		ctxToken: bar41, DefaultSubagent: "⧉ 1 subagent", DefaultMonitors: "⦿ 1 monitor",
	})
}

func TestMonitorCountCanBeTurnedOff(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.app.cfg.MonitorsToken = ""
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5", Monitors: 2}

	h.poll(t)

	mustReport(t, h.takeReports(), "w1:p1", map[string]string{ctxToken: bar41})
}

func TestPlural(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{{0, "0 agents"}, {1, "1 agent"}, {2, "2 agents"}, {12, "12 agents"}}

	for _, tt := range tests {
		if got := plural(tt.n, "agent"); got != tt.want {
			t.Errorf("plural(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestRunClearsTokensOnExit(t *testing.T) {
	h := newHarness(claudePane("w1:p1", session1))
	h.reader.contexts[session1] = transcript.Context{Tokens: 410_000, Model: "claude-opus-5-5"}
	h.client.server = "1:100"

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run still makes its first pass before seeing the cancel.

	h.app.Run(ctx)

	r := h.takeReports()
	if len(r) != 2 {
		t.Fatalf("reports = %+v, want a set then a clear", r)
	}

	mustSet(t, r[:1], "w1:p1", "▰▰▰▱▱▱▱▱ 41%")
	mustClear(t, r[1:], "w1:p1")
}
