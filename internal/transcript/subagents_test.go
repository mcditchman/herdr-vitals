package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func launchLine(sidechain bool, calls ...[2]string) string {
	blocks := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": c[0], "name": c[1], "input": map[string]any{}})
	}

	line, _ := json.Marshal(map[string]any{
		"type":        "assistant",
		"isSidechain": sidechain,
		"message": map[string]any{
			"model":   "claude-opus-5-5",
			"content": blocks,
			"usage":   map[string]any{"input_tokens": 100},
		},
	})

	return string(line)
}

func agentCall(id string) [2]string    { return [2]string{id, toolAgent} }
func workflowCall(id string) [2]string { return [2]string{id, toolWorkflow} }

// resultLine is a user line carrying a tool_result; content is either a
// string or text blocks, as Claude Code writes both.
func resultLine(id, text string, asBlocks bool) string {
	var content any = text
	if asBlocks {
		content = []map[string]any{{"type": "text", "text": text}}
	}

	line, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": content}},
		},
	})

	return string(line)
}

func notificationLine(id string) string {
	text := fmt.Sprintf("<task-notification>\n<task-id>abc</task-id>\n<tool-use-id>%s</tool-use-id>\n<status>completed</status>\n</task-notification>", id)
	line, _ := json.Marshal(map[string]any{"type": "queue-operation", "operation": "enqueue", "content": text})

	return string(line)
}

// clock drives a reader's time.
type clock struct{ now time.Time }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newClockedReader(f *fixture) (*Reader, *clock) {
	c := &clock{now: time.Now()}
	r := NewReader(f.root)
	r.now = func() time.Time { return c.now }

	return r, c
}

func mustSubagents(t *testing.T, r *Reader, want int) {
	t.Helper()

	got, ok := r.Context(sessionID, cwd)
	if !ok {
		t.Fatalf("Context unknown, want %d subagents", want)
	}

	if got.Subagents != want {
		t.Fatalf("Subagents = %d, want %d", got.Subagents, want)
	}
}

// touch writes a subagent transcript (and its meta file when toolUseID is
// set) with the given modification time.
func (f *fixture) touch(t *testing.T, rel, toolUseID string, mtime time.Time) {
	t.Helper()

	path := filepath.Join(strings.TrimSuffix(f.path, ".jsonl"), "subagents", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	if toolUseID != "" {
		meta := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
		body := fmt.Sprintf(`{"agentType":"general-purpose","toolUseId":%q}`, toolUseID)
		if err := os.WriteFile(meta, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestForegroundSubagentCountsUntilItsResult(t *testing.T) {
	for _, asBlocks := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocks=%v", asBlocks), func(t *testing.T) {
			f := newFixture(t, cwd)
			r, _ := newClockedReader(f)

			f.append(launchLine(false, agentCall("toolu_A")))
			mustSubagents(t, r, 1)

			f.append(resultLine("toolu_A", "Here is what I found...", asBlocks))
			mustSubagents(t, r, 0)
		})
	}
}

func TestInterruptedForegroundSubagentEnds(t *testing.T) {
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_A", "[Request interrupted by user for tool use]", true))

	mustSubagents(t, r, 0)
}

func TestBackgroundSubagentCountsUntilItsNotification(t *testing.T) {
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_A", "Async agent launched successfully.\nagentId: a1", true))
	mustSubagents(t, r, 1)

	f.append(notificationLine("toolu_A"))
	mustSubagents(t, r, 0)
}

func TestNotificationInsideAUserMessageEndsTheLaunchToo(t *testing.T) {
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_A", "Async agent launched successfully.", false))

	line, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": "<task-notification>\n<tool-use-id>toolu_A</tool-use-id>\n</task-notification>"},
	})
	f.append(string(line))

	mustSubagents(t, r, 0)
}

func TestParallelLaunchesInOneMessage(t *testing.T) {
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A"), agentCall("toolu_B"), [2]string{"toolu_C", "Bash"}))
	mustSubagents(t, r, 2)

	f.append(resultLine("toolu_B", "done", false))
	mustSubagents(t, r, 1)
}

func TestUnrelatedResultsAndNotificationsAreIgnored(t *testing.T) {
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_OTHER", "file contents", false), notificationLine("toolu_BASH"))

	mustSubagents(t, r, 1)
}

func TestSidechainLaunchesAreNotCounted(t *testing.T) {
	// A subagent's own nested calls are its business, not the parent's count.
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(assistant("claude-opus-5-5", 1, 0, 0), launchLine(true, agentCall("toolu_A")))

	mustSubagents(t, r, 0)
}

func TestWorkflowCountsItsRecentlyActiveAgents(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	dir := filepath.Join(strings.TrimSuffix(f.path, ".jsonl"), "subagents", "workflows", "wf_1")
	f.append(launchLine(false, workflowCall("toolu_W")))
	f.append(resultLine("toolu_W", "Workflow launched in background. Task ID: w1\nSummary: s\nTranscript dir: "+dir+"\nScript file: x", false))

	t.Run("before any agent writes it counts as one", func(t *testing.T) {
		mustSubagents(t, r, 1)
	})

	t.Run("recently active agents each count", func(t *testing.T) {
		f.touch(t, "workflows/wf_1/agent-1.jsonl", "", c.now)
		f.touch(t, "workflows/wf_1/agent-2.jsonl", "", c.now.Add(-30*time.Second))
		f.touch(t, "workflows/wf_1/agent-3.jsonl", "", c.now.Add(-5*time.Minute)) // finished earlier
		mustSubagents(t, r, 2)
	})

	t.Run("a quiet but live workflow still counts one", func(t *testing.T) {
		c.advance(3 * time.Minute)
		mustSubagents(t, r, 1)
	})

	t.Run("its notification ends it", func(t *testing.T) {
		f.append(notificationLine("toolu_W"))
		mustSubagents(t, r, 0)
	})
}

func TestWorkflowWithoutATranscriptDirFallsBackToAllWorkflows(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(launchLine(false, workflowCall("toolu_W")))
	f.append(resultLine("toolu_W", "Workflow launched in background. Task ID: w1", false))
	f.touch(t, "workflows/wf_x/agent-1.jsonl", "", c.now)
	f.touch(t, "workflows/wf_x/agent-2.jsonl", "", c.now)
	f.touch(t, "workflows/wf_y/agent-3.jsonl", "", c.now)

	mustSubagents(t, r, 3)
}

func TestOrphanedLaunchGoesStaleThenIsForgotten(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	mustSubagents(t, r, 1)

	c.advance(staleAfter - time.Second)
	mustSubagents(t, r, 1)

	c.advance(2 * time.Second)
	mustSubagents(t, r, 0)

	if len(r.sessions[sessionID].running) != 1 {
		t.Fatal("a stale launch should be kept until forgetAfter, in case it writes again")
	}

	c.advance(forgetAfter)
	mustSubagents(t, r, 0)

	if len(r.sessions[sessionID].running) != 0 {
		t.Fatal("launch not forgotten after forgetAfter")
	}
}

func TestSubagentTranscriptActivityKeepsALaunchAlive(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_A", "Async agent launched successfully.", true))

	mustSubagents(t, r, 1) // launch seen now

	c.advance(30 * time.Minute)
	f.touch(t, "agent-a1.jsonl", "toolu_A", c.now.Add(-time.Minute))
	mustSubagents(t, r, 1) // the launch is old, but its transcript is fresh

	c.advance(staleAfter)
	mustSubagents(t, r, 0) // transcript now quiet for staleAfter + 1m
}

func TestNotificationWithLiteralAngleBrackets(t *testing.T) {
	// What Claude Code actually writes: json.Marshal would escape the < and >.
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_A", "Async agent launched successfully.", true))
	f.append(`{"type":"queue-operation","operation":"enqueue","content":"<task-notification>\n<task-id>a1</task-id>\n<tool-use-id>toolu_A</tool-use-id>\n<status>completed</status>"}`)

	mustSubagents(t, r, 0)
}

func TestTruncatedTranscriptForgetsLaunches(t *testing.T) {
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(userLine, userLine, launchLine(false, agentCall("toolu_A")))
	mustSubagents(t, r, 1)

	if err := os.WriteFile(f.path, []byte(assistant("claude-opus-5-5", 5, 0, 0)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustSubagents(t, r, 0)
}

func TestTextOf(t *testing.T) {
	tests := map[string]string{
		`"plain"`: "plain",
		`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`: "ab",
		`{"not":"either"}`: "",
		`null`:             "",
	}

	for raw, want := range tests {
		if got := textOf(json.RawMessage(raw)); got != want {
			t.Errorf("textOf(%s) = %q, want %q", raw, got, want)
		}
	}
}
