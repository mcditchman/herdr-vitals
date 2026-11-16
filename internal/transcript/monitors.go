package transcript

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Monitors are watch scripts the session starts with the Monitor tool. Like
// background subagents they run inside the parent process, so the parent
// transcript is the record of their lifecycle:
//
//   - start: an assistant tool_use named Monitor. Its tool_result
//     ("Monitor started (task <id>, ...)") names the task id that every
//     later message uses instead of the tool_use id;
//   - events: task notifications naming the task id with no <status>. They
//     do not end the monitor;
//   - end: a task notification naming the task id with a <status> (the
//     script exited, or a resumed session marked it stopped), the
//     "[Monitor expired ...]" event, or a TaskStop that succeeded.
//
// Claude Code kills every monitor at its timeout, which can leave no
// transcript marker, so the deadline ends it too.

const (
	toolMonitor  = "Monitor"
	toolTaskStop = "TaskStop"
	// The Monitor tool's timeout_ms default and cap, used when its result
	// does not state the timeout.
	defaultMonitorTimeout = 5 * time.Minute
	maxMonitorTimeout     = 30 * time.Minute
)

const (
	monitorStartedPrefix = "Monitor started"
	monitorExpiredMarker = "[Monitor expired after"
	taskStoppedPrefix    = "Successfully stopped task"
)

var (
	startedTaskPattern  = regexp.MustCompile(`^Monitor started \(task ([A-Za-z0-9_-]+)`)
	notificationBlock   = regexp.MustCompile(`(?:<|\\u003c)task-notification(?:>|\\u003e)(.*?)(?:(?:<|\\u003c)/task-notification|$)`)
	notifiedTaskPattern = regexp.MustCompile(`(?:<|\\u003c)task-id(?:>|\\u003e)([A-Za-z0-9_-]+)(?:<|\\u003c)/task-id`)
	statusTagPattern    = regexp.MustCompile(`(?:<|\\u003c)status(?:>|\\u003e)`)
	eventTagPattern     = regexp.MustCompile(`(?:<|\\u003c)event(?:>|\\u003e)`)
)

// monitor is a Monitor call that has not been seen to end.
type monitor struct {
	// awaitingResult is true until the tool_result of the Monitor call.
	awaitingResult bool
	// taskID names the monitor in notifications and TaskStop. It is empty
	// until the start result, and stays empty if that result did not name it.
	taskID string
	// started is when the Monitor call was made, from the line's timestamp.
	started time.Time
	timeout time.Duration
	// persistent monitors have no deadline.
	persistent bool
}

func (m *monitor) expired(now time.Time) bool {
	return !m.persistent && now.Sub(m.started) >= m.timeout
}

// monitorInput is the part of a Monitor call's input that is read.
type monitorInput struct {
	TimeoutMs float64 `json:"timeout_ms"`
}

// monitorStart is the toolUseResult of a successful Monitor call.
type monitorStart struct {
	TaskID     string `json:"taskId"`
	TimeoutMs  int64  `json:"timeoutMs"`
	Persistent bool   `json:"persistent"`
}

// stopInput is the part of a TaskStop call's input that is read. shell_id is
// the deprecated name of task_id.
type stopInput struct {
	TaskID  string `json:"task_id"`
	ShellID string `json:"shell_id"`
}

// requestedMonitorTimeout is the timeout a Monitor call asked for, as the tool
// applies it: the default when unset, capped at the maximum.
func requestedMonitorTimeout(input json.RawMessage) time.Duration {
	var in monitorInput
	if json.Unmarshal(input, &in) != nil || in.TimeoutMs <= 0 {
		return defaultMonitorTimeout
	}

	return min(time.Duration(in.TimeoutMs)*time.Millisecond, maxMonitorTimeout)
}

// startMonitor records a Monitor call as running.
func (s *session) startMonitor(b contentBlock, at time.Time) {
	if s.monitors == nil {
		s.monitors = make(map[string]*monitor)
	}

	s.monitors[b.ID] = &monitor{awaitingResult: true, started: at, timeout: requestedMonitorTimeout(b.Input)}
}

// trackStop records a TaskStop call aimed at a running monitor, so its result
// can end that monitor.
func (s *session) trackStop(b contentBlock) {
	var in stopInput
	if json.Unmarshal(b.Input, &in) != nil {
		return
	}

	taskID := in.TaskID
	if taskID == "" {
		taskID = in.ShellID
	}

	if s.monitorFor(taskID) == "" {
		return
	}

	if s.stops == nil {
		s.stops = make(map[string]string)
	}

	s.stops[b.ID] = taskID
}

// absorbMonitorResult handles the tool_result of a Monitor call: a start
// names its task, anything else (an error, a denial, an interrupt) means it
// never ran.
func (s *session) absorbMonitorResult(toolUseID, text string, toolUseResult json.RawMessage) {
	m := s.monitors[toolUseID]
	if !strings.HasPrefix(text, monitorStartedPrefix) {
		delete(s.monitors, toolUseID)
		return
	}

	m.awaitingResult = false

	var start monitorStart
	_ = json.Unmarshal(toolUseResult, &start) // a string or absent result leaves it zero

	m.taskID = start.TaskID
	if m.taskID == "" {
		if match := startedTaskPattern.FindStringSubmatch(text); match != nil {
			m.taskID = match[1]
		}
	}

	if start.TimeoutMs > 0 {
		m.timeout = time.Duration(start.TimeoutMs) * time.Millisecond
	}

	m.persistent = start.Persistent
}

// absorbStopResult ends the monitor a TaskStop was aimed at once it reports
// success. A failed stop leaves the monitor to its other endings.
func (s *session) absorbStopResult(toolUseID, text string) {
	taskID := s.stops[toolUseID]
	delete(s.stops, toolUseID)

	if strings.Contains(text, taskStoppedPrefix) {
		s.endMonitorTask(taskID)
	}
}

// endNotifiedMonitors ends each monitor that a notification in line reports
// as finished: one with a <status> ahead of its <event>, or the expiry
// notice. A monitor's events carry no status of their own; only the header is
// checked for one, since the event is the script's output and can hold any
// text.
func (s *session) endNotifiedMonitors(line []byte) {
	if len(s.monitors) == 0 {
		return
	}

	for _, block := range notificationBlock.FindAllSubmatch(line, -1) {
		header, event := block[1], []byte(nil)
		if loc := eventTagPattern.FindIndex(header); loc != nil {
			header, event = block[1][:loc[0]], block[1][loc[1]:]
		}

		if !statusTagPattern.Match(header) && !bytes.HasPrefix(event, []byte(monitorExpiredMarker)) {
			continue
		}

		for _, id := range notifiedTaskPattern.FindAllSubmatch(header, -1) {
			s.endMonitorTask(string(id[1]))
		}
	}
}

func (s *session) endMonitorTask(taskID string) {
	if id := s.monitorFor(taskID); id != "" {
		delete(s.monitors, id)
	}
}

// monitorFor is the tool_use id of the monitor running as taskID, or "".
func (s *session) monitorFor(taskID string) string {
	if taskID == "" {
		return ""
	}

	for id, m := range s.monitors {
		if m.taskID == taskID {
			return id
		}
	}

	return ""
}

// countMonitors is how many monitors are running now, dropping those past
// their deadline.
func (s *session) countMonitors(now time.Time) int {
	for id, m := range s.monitors {
		if m.expired(now) {
			delete(s.monitors, id)
		}
	}

	// A stop whose result never came is moot once its monitor is gone.
	for id, taskID := range s.stops {
		if s.monitorFor(taskID) == "" {
			delete(s.stops, id)
		}
	}

	return len(s.monitors)
}
