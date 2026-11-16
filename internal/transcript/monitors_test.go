package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// claudeJSON encodes v the way Claude Code writes transcript lines: angle
// brackets stay literal instead of becoming \u003c / \u003e.
func claudeJSON(v any) string {
	var b bytes.Buffer

	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)

	return strings.TrimSuffix(b.String(), "\n")
}

// callLine is a main-thread assistant line with one tool_use, written at at.
func callLine(id, name string, input map[string]any, at time.Time) string {
	return claudeJSON(map[string]any{
		"type":        "assistant",
		"isSidechain": false,
		"timestamp":   at.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"model":   "claude-opus-5-5",
			"content": []map[string]any{{"type": "tool_use", "id": id, "name": name, "input": input}},
			"usage":   map[string]any{"input_tokens": 100},
		},
	})
}

func monitorCall(id string, timeoutMs int, at time.Time) string {
	return callLine(id, toolMonitor, map[string]any{
		"command": "bash watch.sh", "description": "PR review comments", "timeout_ms": timeoutMs,
	}, at)
}

// toolResult is a user line with one tool_result and, when set, the
// structured toolUseResult Claude Code stores beside it.
func toolResult(id, text string, toolUseResult any) string {
	line := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text}},
		},
	}
	if toolUseResult != nil {
		line["toolUseResult"] = toolUseResult
	}

	return claudeJSON(line)
}

func startedText(taskID string) string {
	return fmt.Sprintf("Monitor started (task %s, expires in 30m unless the source ends first; "+
		"you get one notice at expiry — re-arm if you still need the watch). You will be notified on each event.", taskID)
}

func monitorStarted(id, taskID string, timeoutMs int64) string {
	return toolResult(id, startedText(taskID),
		map[string]any{"taskId": taskID, "timeoutMs": timeoutMs, "persistent": false})
}

// notification is a queued task notification, as Claude Code writes it.
func notification(body string) string {
	return claudeJSON(map[string]any{
		"type": "queue-operation", "operation": "enqueue",
		"content": "<task-notification>\n" + body + "\n</task-notification>",
	})
}

func monitorEvent(taskID, event string) string {
	return notification(fmt.Sprintf("<task-id>%s</task-id>\n<summary>Monitor event: \"PR review comments\"</summary>\n<event>%s</event>",
		taskID, event))
}

func streamEnded(toolUseID, taskID string) string {
	return notification(fmt.Sprintf("<task-id>%s</task-id>\n<tool-use-id>%s</tool-use-id>\n<output-file>C:\\tasks\\%s.output</output-file>\n"+
		"<status>completed</status>\n<summary>Monitor \"PR review comments\" stream ended</summary>\n<event>done</event>",
		taskID, toolUseID, taskID))
}

func stopCall(id, taskID string, at time.Time) string {
	return callLine(id, toolTaskStop, map[string]any{"task_id": taskID}, at)
}

func stopSucceeded(id, taskID string) string {
	text := claudeJSON(map[string]any{
		"message": "Successfully stopped task: " + taskID + " (bash watch.sh)", "task_id": taskID, "task_type": "local_bash",
	})

	return toolResult(id, text, nil)
}

func mustMonitors(t *testing.T, r *Reader, want int) {
	t.Helper()

	got, ok := r.Context(sessionID, cwd)
	if !ok {
		t.Fatalf("Context unknown, want %d monitors", want)
	}

	if got.Monitors != want {
		t.Fatalf("Monitors = %d, want %d", got.Monitors, want)
	}
}

const thirtyMinutesMs = 1_800_000

func TestMonitorCountsFromItsCallUntilItsStreamEnds(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now))
	mustMonitors(t, r, 1) // counted while awaiting its start result

	f.append(monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	mustMonitors(t, r, 1)

	f.append(monitorEvent("bmon1", "NEW comment thread=1"), monitorEvent("bmon1", "NEW comment thread=2"))
	mustMonitors(t, r, 1) // events do not end it

	f.append(streamEnded("toolu_M", "bmon1"))
	mustMonitors(t, r, 0)
}

func TestMonitorExpiryNoticeEndsIt(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(monitorEvent("bmon1", "[Monitor expired after 30m with 2 events delivered. Re-arm it if you still need the watch.]"))

	mustMonitors(t, r, 0)
}

func TestMonitorEndsAtItsDeadlineWithoutAnyMarker(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", 900_000, c.now), monitorStarted("toolu_M", "bmon1", 900_000))

	c.advance(15*time.Minute - time.Second)
	mustMonitors(t, r, 1)

	c.advance(time.Second)
	mustMonitors(t, r, 0)

	if len(r.sessions[sessionID].monitors) != 0 {
		t.Fatal("an expired monitor should be dropped, not kept")
	}
}

func TestMonitorDeadlineRunsFromTheCallsTimestampNotFromWhenItWasRead(t *testing.T) {
	// A transcript caught up on at startup holds monitors that ended long ago.
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	started := c.now.Add(-40 * time.Minute)
	f.append(monitorCall("toolu_OLD", thirtyMinutesMs, started), monitorStarted("toolu_OLD", "bold", thirtyMinutesMs))
	f.append(monitorCall("toolu_NEW", thirtyMinutesMs, c.now.Add(-10*time.Minute)), monitorStarted("toolu_NEW", "bnew", thirtyMinutesMs))

	mustMonitors(t, r, 1)
}

func TestMonitorWithoutATimestampIsTimedFromWhenItWasRead(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	line := strings.Replace(monitorCall("toolu_M", 60_000, c.now), `"timestamp"`, `"_timestamp"`, 1)
	f.append(line, monitorStarted("toolu_M", "bmon1", 60_000))
	mustMonitors(t, r, 1)

	c.advance(time.Minute)
	mustMonitors(t, r, 0)
}

func TestMonitorTimeoutComesFromItsCallWhenTheResultOmitsIt(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", 120_000, c.now), toolResult("toolu_M", startedText("bmon1"), nil))

	c.advance(2*time.Minute - time.Second)
	mustMonitors(t, r, 1)

	c.advance(time.Second)
	mustMonitors(t, r, 0)
}

func TestRequestedMonitorTimeout(t *testing.T) {
	tests := map[string]time.Duration{
		`{"timeout_ms":120000}`:  2 * time.Minute,
		`{"timeout_ms":3600000}`: maxMonitorTimeout, // the tool caps it
		`{"timeout_ms":0}`:       defaultMonitorTimeout,
		`{"command":"x"}`:        defaultMonitorTimeout,
		`not json`:               defaultMonitorTimeout,
	}

	for input, want := range tests {
		if got := requestedMonitorTimeout(json.RawMessage(input)); got != want {
			t.Errorf("requestedMonitorTimeout(%s) = %v, want %v", input, got, want)
		}
	}
}

func TestPersistentMonitorOutlivesItsTimeout(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now))
	f.append(toolResult("toolu_M", startedText("bmon1"), map[string]any{"taskId": "bmon1", "timeoutMs": thirtyMinutesMs, "persistent": true}))

	c.advance(3 * time.Hour)
	mustMonitors(t, r, 1)

	f.append(streamEnded("toolu_M", "bmon1"))
	mustMonitors(t, r, 0)
}

func TestMonitorTaskIDIsReadFromTheResultTextWhenNotStructured(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), toolResult("toolu_M", startedText("bmon1"), "not an object"))
	mustMonitors(t, r, 1)

	f.append(monitorEvent("bmon1", "[Monitor expired after 30m with 0 events delivered.]"))
	mustMonitors(t, r, 0)
}

func TestMonitorThatFailedToStartIsNotCounted(t *testing.T) {
	for name, text := range map[string]string{
		"error":       "<tool_use_error>Error: command is required</tool_use_error>",
		"interrupted": "[Request interrupted by user for tool use]",
		"denied":      "The user doesn't want to proceed with this tool use.",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, cwd)
			r, c := newClockedReader(f)

			f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), toolResult("toolu_M", text, nil))

			mustMonitors(t, r, 0)
		})
	}
}

func TestTaskStopEndsTheMonitorItStopped(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M1", thirtyMinutesMs, c.now), monitorStarted("toolu_M1", "bmon1", thirtyMinutesMs))
	f.append(monitorCall("toolu_M2", thirtyMinutesMs, c.now), monitorStarted("toolu_M2", "bmon2", thirtyMinutesMs))
	mustMonitors(t, r, 2)

	f.append(stopCall("toolu_S", "bmon1", c.now))
	mustMonitors(t, r, 2) // not until the stop succeeds

	f.append(stopSucceeded("toolu_S", "bmon1"))
	mustMonitors(t, r, 1)

	if len(r.sessions[sessionID].stops) != 0 {
		t.Fatal("a stop should be forgotten once its result is read")
	}
}

func TestTaskStopByDeprecatedShellID(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(callLine("toolu_S", toolTaskStop, map[string]any{"shell_id": "bmon1"}, c.now), stopSucceeded("toolu_S", "bmon1"))

	mustMonitors(t, r, 0)
}

func TestFailedTaskStopLeavesTheMonitorRunning(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(stopCall("toolu_S", "bmon1", c.now), toolResult("toolu_S", "<tool_use_error>No task found with ID: bmon1</tool_use_error>", nil))

	mustMonitors(t, r, 1)

	if len(r.sessions[sessionID].stops) != 0 {
		t.Fatal("a failed stop should be forgotten once its result is read")
	}
}

func TestTaskStopOfAnotherTaskIsIgnored(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(stopCall("toolu_S", "bshell", c.now), stopSucceeded("toolu_S", "bshell"))

	mustMonitors(t, r, 1)

	if len(r.sessions[sessionID].stops) != 0 {
		t.Fatal("a stop aimed at no monitor should not be tracked")
	}
}

func TestStopWhoseResultNeverCameIsDroppedWithItsMonitor(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", 60_000, c.now), monitorStarted("toolu_M", "bmon1", 60_000), stopCall("toolu_S", "bmon1", c.now))
	mustMonitors(t, r, 1)

	c.advance(time.Minute)
	mustMonitors(t, r, 0)

	if len(r.sessions[sessionID].stops) != 0 {
		t.Fatal("a stop should be dropped once its monitor is gone")
	}
}

func TestOrphanSummaryEndsEveryTaskItNames(t *testing.T) {
	// A resumed session marks the previous process's background tasks stopped.
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M1", thirtyMinutesMs, c.now), monitorStarted("toolu_M1", "bmon1", thirtyMinutesMs))
	f.append(monitorCall("toolu_M2", thirtyMinutesMs, c.now), monitorStarted("toolu_M2", "bmon2", thirtyMinutesMs))
	f.append(monitorCall("toolu_M3", thirtyMinutesMs, c.now), monitorStarted("toolu_M3", "bmon3", thirtyMinutesMs))
	f.append(notification("<task-id>bmon1</task-id>\n<task-id>bmon2</task-id>\n<task-id>__orphan_summary__:shell</task-id>\n" +
		"<status>stopped</status>\n<summary>2 background shell command tasks didn't finish before the previous session ended.</summary>"))

	mustMonitors(t, r, 1)
}

func TestFinalNotificationOfAnotherTaskLeavesMonitorsAlone(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(streamEnded("toolu_OTHER", "bother"))

	mustMonitors(t, r, 1)
}

func TestMonitorEventQuotingAStatusTagDoesNotEndIt(t *testing.T) {
	// The event is the script's own output and can hold any text.
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(monitorEvent("bmon1", "build: <status>completed</status> <task-id>bmon1</task-id>"))

	mustMonitors(t, r, 1)
}

func TestMonitorNotificationsWithEscapedAngleBrackets(t *testing.T) {
	// Other JSON encoders write < and > as \u003c and \u003e.
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))

	line, _ := json.Marshal(map[string]any{
		"type": "queue-operation", "operation": "enqueue",
		"content": "<task-notification>\n<task-id>bmon1</task-id>\n<status>completed</status>\n</task-notification>",
	})
	if !bytes.Contains(line, []byte(`\u003c`)) {
		t.Fatal("test line should hold escaped angle brackets")
	}

	f.append(string(line))
	mustMonitors(t, r, 0)
}

func TestNotificationInsideAUserMessageEndsTheMonitorToo(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	f.append(claudeJSON(map[string]any{
		"type": "user",
		"message": map[string]any{"role": "user", "content": "<task-notification>\n<task-id>bmon1</task-id>\n<tool-use-id>toolu_M</tool-use-id>\n" +
			"<status>completed</status>\n<summary>Monitor \"x\" stream ended</summary>\n</task-notification>"},
	}))

	mustMonitors(t, r, 0)
}

func TestSidechainMonitorsAreNotCounted(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	line := strings.Replace(monitorCall("toolu_M", thirtyMinutesMs, c.now), `"isSidechain":false`, `"isSidechain":true`, 1)
	f.append(assistant("claude-opus-5-5", 1, 0, 0), line)

	mustMonitors(t, r, 0)
}

func TestMonitorsAndSubagentsAreCountedApart(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")), resultLine("toolu_A", "Async agent launched successfully.", true))
	f.append(monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))

	got, _ := r.Context(sessionID, cwd)
	if got.Subagents != 1 || got.Monitors != 1 {
		t.Fatalf("Subagents, Monitors = %d, %d, want 1, 1", got.Subagents, got.Monitors)
	}

	f.append(notificationLine("toolu_A"))
	mustSubagents(t, r, 0)
	mustMonitors(t, r, 1)

	f.append(streamEnded("toolu_M", "bmon1"))
	mustMonitors(t, r, 0)
}

func TestAgentResultInALineQuotingANotificationIsStillRead(t *testing.T) {
	// A tool result that prints notification text must not hide the result
	// itself, as when an agent's report quotes one.
	f := newFixture(t, cwd)
	r, _ := newClockedReader(f)

	f.append(launchLine(false, agentCall("toolu_A")))
	f.append(resultLine("toolu_A", "Found this in the log: <task-notification><task-id>x</task-id></task-notification>", false))

	mustSubagents(t, r, 0)
}

func TestTruncatedTranscriptForgetsMonitors(t *testing.T) {
	f := newFixture(t, cwd)
	r, c := newClockedReader(f)

	f.append(userLine, userLine, monitorCall("toolu_M", thirtyMinutesMs, c.now), monitorStarted("toolu_M", "bmon1", thirtyMinutesMs))
	mustMonitors(t, r, 1)

	if err := os.WriteFile(f.path, []byte(assistant("claude-opus-5-5", 5, 0, 0)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mustMonitors(t, r, 0)
}

func TestWrittenAt(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	if got := writtenAt("2026-10-07T20:02:23.676Z", now); !got.Equal(time.Date(2026, 10, 7, 20, 2, 23, 676_000_000, time.UTC)) {
		t.Errorf("writtenAt(valid) = %v", got)
	}

	for _, bad := range []string{"", "yesterday"} {
		if got := writtenAt(bad, now); !got.Equal(now) {
			t.Errorf("writtenAt(%q) = %v, want now", bad, got)
		}
	}
}
