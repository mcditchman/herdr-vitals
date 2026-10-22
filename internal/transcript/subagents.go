package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Subagents run inside the parent Claude process, so the parent transcript is
// the record of their lifecycle:
//
//   - start: an assistant tool_use named Agent or Workflow;
//   - foreground finish: that tool_use's tool_result;
//   - background finish: the tool_result only says it launched, and a
//     <task-notification> naming the tool_use id arrives when it ends.
//
// Each subagent writes its own transcript under <session>/subagents/, which is
// how a launch whose finish was never recorded (a crashed or killed parent) is
// told apart from one that is still working.

const (
	toolAgent    = "Agent"
	toolWorkflow = "Workflow"
)

// Launch results that mean "started in the background", not "finished".
var backgroundResults = []string{"Async agent launched", "Workflow launched in background"}

const (
	// staleAfter is how long a launch may go without its transcript changing
	// before it stops counting. Long enough for a subagent waiting on a slow
	// tool call (Bash allows up to 10 minutes), short enough that an orphan
	// from a crashed session clears itself.
	staleAfter = 10 * time.Minute
	// forgetAfter drops a stale launch entirely, so orphans are not re-checked
	// every poll for the life of the pane.
	forgetAfter = time.Hour
	// workflowActive is how recently a workflow agent must have written to
	// count as one of the workflow's running agents.
	workflowActive = time.Minute
)

// launch is a subagent or workflow that has started and not been seen to end.
type launch struct {
	workflow bool
	// dir is a workflow's transcript directory, named in its launch result.
	dir string
	// agentFile is an Agent subagent's transcript, found through the
	// toolUseId in its .meta.json.
	agentFile string
	// seen is when the launch was read, the activity floor before its
	// transcript exists.
	seen time.Time
}

// contentBlock is the part of a message content block lifecycle tracking reads.
type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// The notification is matched in the raw line, which Claude Code writes with
// literal angle brackets; the < / > escapes other JSON encoders
// emit are accepted too, so the match does not depend on the writer.
var (
	toolResultMarker   = []byte(`"tool_result"`)
	notificationMarker = []byte(`task-notification`)
	notifiedIDPattern  = regexp.MustCompile(`(?:<|\\u003c)tool-use-id(?:>|\\u003e)(toolu_[A-Za-z0-9_-]+)(?:<|\\u003c)/tool-use-id`)
	transcriptDirLine  = regexp.MustCompile(`(?m)^Transcript dir: (.+?)\s*$`)
)

// absorbLaunches records Agent and Workflow tool_use blocks as running.
func (s *session) absorbLaunches(content json.RawMessage, now time.Time) {
	var blocks []contentBlock
	if json.Unmarshal(content, &blocks) != nil {
		return
	}

	for _, b := range blocks {
		if b.Type != "tool_use" || b.ID == "" || (b.Name != toolAgent && b.Name != toolWorkflow) {
			continue
		}

		if s.running == nil {
			s.running = make(map[string]*launch)
		}

		s.running[b.ID] = &launch{workflow: b.Name == toolWorkflow, seen: now}
	}
}

// absorbLifecycle handles a line that is not an assistant line but may end a
// launch. Only lines that could matter are decoded: user lines carry tool
// results that can be megabytes.
func (s *session) absorbLifecycle(line []byte) {
	if len(s.running) == 0 {
		return
	}

	if bytes.Contains(line, notificationMarker) {
		for _, m := range notifiedIDPattern.FindAllSubmatch(line, -1) {
			delete(s.running, string(m[1]))
		}

		return
	}

	if !bytes.Contains(line, toolResultMarker) || !s.mentionsRunning(line) {
		return
	}

	var e struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "user" {
		return
	}

	var blocks []contentBlock
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return
	}

	for _, b := range blocks {
		l, ok := s.running[b.ToolUseID]
		if b.Type != "tool_result" || !ok {
			continue
		}

		text := textOf(b.Content)
		if !isBackgroundResult(text) {
			delete(s.running, b.ToolUseID) // finished, failed or interrupted
			continue
		}

		if m := transcriptDirLine.FindStringSubmatch(text); l.workflow && m != nil {
			l.dir = m[1]
		}
	}
}

func (s *session) mentionsRunning(line []byte) bool {
	for id := range s.running {
		if bytes.Contains(line, []byte(id)) {
			return true
		}
	}

	return false
}

func isBackgroundResult(text string) bool {
	for _, prefix := range backgroundResults {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}

	return false
}

// textOf flattens tool_result content, which is either a string or a list of
// text blocks.
func textOf(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}

	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}

	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}

	return b.String()
}

// subagentsDir is where a session's subagent transcripts live, beside its own.
func (s *session) subagentsDir() string {
	return filepath.Join(strings.TrimSuffix(s.path, ".jsonl"), "subagents")
}

// countSubagents is how many subagents are running now: one per live Agent
// launch, and per live workflow its recently active agents (at least one).
func (s *session) countSubagents(now time.Time) int {
	if len(s.running) == 0 {
		return 0
	}

	s.mapAgentFiles()

	count := 0

	for id, l := range s.running {
		last, active := l.seen, 1
		if l.workflow {
			newest, recent := activity(s.workflowDir(l), now)
			last, active = later(last, newest), max(1, recent)
		} else if mtime, ok := modTime(l.agentFile); ok {
			last = later(last, mtime)
		}

		switch idle := now.Sub(last); {
		case idle > forgetAfter:
			delete(s.running, id)
		case idle <= staleAfter:
			count += active
		}
	}

	return count
}

// workflowDir is the launch's transcript directory, or every workflow of the
// session when its launch result named none.
func (s *session) workflowDir(l *launch) string {
	if l.dir != "" {
		return l.dir
	}

	return filepath.Join(s.subagentsDir(), "workflows", "*")
}

// mapAgentFiles finds the transcript of each running Agent launch from the
// toolUseId its .meta.json records. It only reads when a launch is unmapped.
func (s *session) mapAgentFiles() {
	unmapped := false
	for _, l := range s.running {
		unmapped = unmapped || (!l.workflow && l.agentFile == "")
	}

	if !unmapped {
		return
	}

	metas, _ := filepath.Glob(filepath.Join(s.subagentsDir(), "agent-*.meta.json"))
	for _, meta := range metas {
		raw, err := os.ReadFile(meta)
		if err != nil {
			continue
		}

		var m struct {
			ToolUseID string `json:"toolUseId"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}

		if l, ok := s.running[m.ToolUseID]; ok && !l.workflow {
			l.agentFile = strings.TrimSuffix(meta, ".meta.json") + ".jsonl"
		}
	}
}

// activity reports the newest agent transcript change under dir (a glob
// pattern is allowed) and how many changed within workflowActive.
func activity(dir string, now time.Time) (newest time.Time, recent int) {
	files, _ := filepath.Glob(filepath.Join(dir, "agent-*.jsonl"))
	for _, f := range files {
		mtime, ok := modTime(f)
		if !ok {
			continue
		}

		newest = later(newest, mtime)
		if now.Sub(mtime) <= workflowActive {
			recent++
		}
	}

	return newest, recent
}

func modTime(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}

	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, false
	}

	return info.ModTime(), true
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}
