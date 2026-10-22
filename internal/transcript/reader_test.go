package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sessionID = "3f2b8c1e-5d4a-4c6b-9e7f-0a1b2c3d4e5f"

func assistant(model string, input, cacheCreate, cacheRead int64) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"message":{"model":%q,"usage":`+
		`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":99}}}`,
		model, input, cacheCreate, cacheRead)
}

func sidechain(model string, input int64) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":true,"message":{"model":%q,"usage":{"input_tokens":%d}}}`,
		model, input)
}

func compact(post int64) string {
	return fmt.Sprintf(`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto","preTokens":237651,"postTokens":%d}}`, post)
}

const userLine = `{"type":"user","message":{"role":"user","content":"hello"}}`

// fixture is a Claude config root with one project directory.
type fixture struct {
	t       *testing.T
	root    string
	project string
	path    string
}

func newFixture(t *testing.T, cwd string) *fixture {
	t.Helper()

	root := t.TempDir()
	project := filepath.Join(root, "projects", slugOf(cwd))
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	return &fixture{t: t, root: root, project: project, path: filepath.Join(project, sessionID+".jsonl")}
}

func (f *fixture) append(lines ...string) {
	f.t.Helper()

	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()

	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) appendRaw(s string) {
	f.t.Helper()

	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()

	if _, err := file.WriteString(s); err != nil {
		f.t.Fatal(err)
	}
}

func mustContext(t *testing.T, r *Reader, dir string, wantTokens int64, wantModel string) {
	t.Helper()

	got, ok := r.Context(sessionID, dir)
	if !ok {
		t.Fatalf("Context: not known, want %d tokens", wantTokens)
	}

	if got.Tokens != wantTokens || got.Model != wantModel {
		t.Fatalf("Context = %+v, want {Tokens:%d Model:%s}", got, wantTokens, wantModel)
	}
}

func mustUnknown(t *testing.T, r *Reader, dir string) {
	t.Helper()

	if got, ok := r.Context(sessionID, dir); ok {
		t.Fatalf("Context = %+v, want unknown", got)
	}
}

const cwd = `C:\Users\me\source\repos\my-app`

func TestSlugOf(t *testing.T) {
	tests := map[string]string{
		`C:\Users\Jane.Doe\AppData\Roaming\herdr`: "C--Users-Jane-Doe-AppData-Roaming-herdr",
		`/home/me/work`:             "-home-me-work",
		`C:\Users\me\source\repos\`: "C--Users-me-source-repos-",
		`C:\Users\민수\proj`:          "C--Users----proj",
	}

	for in, want := range tests {
		if got := slugOf(in); got != want {
			t.Errorf("slugOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContextSumsAllInputTokens(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(userLine, assistant("claude-opus-5-5", 2, 3030, 79095))

	mustContext(t, NewReader(f.root), cwd, 82127, "claude-opus-5-5")
}

func TestContextLastAssistantWins(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 10, 0, 0), userLine, assistant("claude-sonnet-5-5", 20, 0, 0))

	mustContext(t, NewReader(f.root), cwd, 20, "claude-sonnet-5-5")
}

func TestContextIgnoresSidechainSyntheticAndEmptyUsage(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(
		assistant("claude-opus-5-5", 500, 0, 0),
		sidechain("claude-haiku-5-5", 9999),
		assistant("<synthetic>", 0, 0, 0),
		assistant("claude-opus-5-5", 0, 0, 0),
		`{"type":"assistant","message":{"model":"claude-opus-5-5"}}`,
	)

	mustContext(t, NewReader(f.root), cwd, 500, "claude-opus-5-5")
}

func TestContextIgnoresMalformedLines(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 500, 0, 0), `{"type":"assistant", broken`)

	mustContext(t, NewReader(f.root), cwd, 500, "claude-opus-5-5")
}

func TestCompactionResetsTokensAndKeepsModel(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 0, 0, 237000), compact(16119))

	mustContext(t, NewReader(f.root), cwd, 16119, "claude-opus-5-5")
}

func TestCwdComesFromTheLatestAssistantLineAndSurvivesCompaction(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(
		`{"type":"assistant","cwd":"C:\\repo","message":{"model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"assistant","cwd":"C:\\repo\\.worktrees\\feature-login","message":{"model":"m","usage":{"input_tokens":2}}}`,
		compact(1),
	)

	got, ok := NewReader(f.root).Context(sessionID, cwd)
	if !ok || got.Cwd != `C:\repo\.worktrees\feature-login` {
		t.Fatalf("Context = %+v (known %v), want the worktree cwd", got, ok)
	}
}

func TestCompactionWithoutPostTokensIsIgnored(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 0, 0, 237000),
		`{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"auto"}}`)

	mustContext(t, NewReader(f.root), cwd, 237000, "claude-opus-5-5")
}

func TestAssistantAfterCompactionWins(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 0, 0, 237000), compact(16119), assistant("claude-opus-5-5", 0, 100, 20000))

	mustContext(t, NewReader(f.root), cwd, 20100, "claude-opus-5-5")
}

func TestNoUsageYetIsUnknown(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(userLine)

	mustUnknown(t, NewReader(f.root), cwd)
}

func TestIncrementalReadsOnlyNewLines(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 100, 0, 0))

	r := NewReader(f.root)
	mustContext(t, r, cwd, 100, "claude-opus-5-5")

	f.append(userLine)
	mustContext(t, r, cwd, 100, "claude-opus-5-5")

	f.append(assistant("claude-opus-5-5", 200, 0, 0))
	mustContext(t, r, cwd, 200, "claude-opus-5-5")
}

func TestPartialLineWaitsForItsNewline(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 100, 0, 0))

	r := NewReader(f.root)
	mustContext(t, r, cwd, 100, "claude-opus-5-5")

	line := assistant("claude-opus-5-5", 300, 0, 0)
	f.appendRaw(line[:len(line)/2])
	mustContext(t, r, cwd, 100, "claude-opus-5-5")

	f.appendRaw(line[len(line)/2:] + "\n")
	mustContext(t, r, cwd, 300, "claude-opus-5-5")
}

func TestTruncatedTranscriptIsReadAfresh(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(userLine, userLine, userLine, assistant("claude-opus-5-5", 100, 0, 0))

	r := NewReader(f.root)
	mustContext(t, r, cwd, 100, "claude-opus-5-5")

	if err := os.WriteFile(f.path, []byte(userLine+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustUnknown(t, r, cwd)
}

func TestLargeTranscriptIsCaughtUpFromItsTail(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 1, 0, 0))
	// Push the first line out of the tail window with a huge tool result.
	f.append(`{"type":"user","message":{"content":"` + strings.Repeat("x", maxScan) + `"}}`)
	f.append(assistant("claude-opus-5-5", 777, 0, 0))

	mustContext(t, NewReader(f.root), cwd, 777, "claude-opus-5-5")
}

func TestTranscriptFoundOutsideThePaneDirectory(t *testing.T) {
	f := newFixture(t, `C:\started\here`)
	f.append(assistant("claude-opus-5-5", 42, 0, 0))

	mustContext(t, NewReader(f.root), `C:\moved\elsewhere`, 42, "claude-opus-5-5")
}

func TestMissingTranscriptIsRetriedAfterBackoff(t *testing.T) {
	f := newFixture(t, cwd)

	now := time.Unix(1_000, 0)
	r := NewReader(f.root)
	r.now = func() time.Time { return now }

	mustUnknown(t, r, cwd)

	f.append(assistant("claude-opus-5-5", 42, 0, 0))
	mustUnknown(t, r, cwd) // still inside the backoff

	now = now.Add(locateRetry)
	mustContext(t, r, cwd, 42, "claude-opus-5-5")
}

func TestDeletedTranscriptKeepsLastReading(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 42, 0, 0))

	r := NewReader(f.root)
	mustContext(t, r, cwd, 42, "claude-opus-5-5")

	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}

	mustContext(t, r, cwd, 42, "claude-opus-5-5")
}

func TestInvalidSessionIDIsRefused(t *testing.T) {
	f := newFixture(t, cwd)
	r := NewReader(f.root)

	for _, id := range []string{"", "../../etc/passwd", "*", "not-a-uuid"} {
		if got, ok := r.Context(id, cwd); ok {
			t.Errorf("Context(%q) = %+v, want unknown", id, got)
		}
	}
}

func TestRetainForgetsOtherSessions(t *testing.T) {
	f := newFixture(t, cwd)
	f.append(assistant("claude-opus-5-5", 42, 0, 0))

	r := NewReader(f.root)
	mustContext(t, r, cwd, 42, "claude-opus-5-5")

	r.Retain(nil)
	if len(r.sessions) != 0 {
		t.Fatalf("sessions after Retain(nil) = %d, want 0", len(r.sessions))
	}

	r.Context(sessionID, cwd)
	r.Retain([]string{sessionID})
	if len(r.sessions) != 1 {
		t.Fatalf("sessions after Retain(id) = %d, want 1", len(r.sessions))
	}
}
