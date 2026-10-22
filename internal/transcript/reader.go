// Package transcript reads how much context a Claude Code session is holding,
// and how many subagents it has running, from the transcript it appends to
// (~/.claude/projects/<slug>/<id>.jsonl).
//
// Every assistant line carries the API usage of the request that produced it,
// and input + cache read + cache creation tokens is the context the model saw.
// A compaction resets that, and its boundary line says by how much.
//
// Locating and tailing transcripts is adapted from kryptamine/herdr-auto-title
// (MIT).
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// maxScan bounds one read. A transcript is append-only, so this bounds the
// catch-up on a session first seen mid-flight, not the steady state.
const maxScan = 2 << 20

// locateRetry is how long a session whose transcript was not found is left
// alone. Herdr can name a session before the agent has written its first
// line, so the search is repeated, just not every poll.
const locateRetry = 10 * time.Second

// syntheticModel marks lines Claude Code writes itself (errors, interrupts)
// with zeroed usage that says nothing about the context.
const syntheticModel = "<synthetic>"

// Context is what a session's latest request sent to the model.
type Context struct {
	// Tokens is input + cache read + cache creation tokens.
	Tokens int64
	// Model is the model id of the latest assistant line, kept across a
	// compaction so the window can still be looked up.
	Model string
	// Cwd is where the agent itself is working, which can differ from the
	// pane's directory (a worktree it entered, a `cd` it ran).
	Cwd string
	// Subagents is how many subagents are running now; see subagents.go.
	Subagents int
}

// Reader reads session transcripts, remembering how far it has read each.
type Reader struct {
	mu       sync.Mutex
	root     string
	sessions map[string]*session
	now      func() time.Time
}

// session is one transcript and what has been read out of it. An empty path
// is a session whose transcript was looked for and not found at searchedAt.
type session struct {
	path       string
	offset     int64
	context    Context
	known      bool
	searchedAt time.Time
	// running is every subagent launch not yet seen to end, by tool_use id.
	running map[string]*launch
}

// NewReader builds a reader over Claude Code's configuration directory: root
// when given, else CLAUDE_CONFIG_DIR, else ~/.claude.
func NewReader(root string) *Reader {
	if root == "" {
		root = defaultRoot()
	}

	return &Reader{root: root, sessions: make(map[string]*session), now: time.Now}
}

func defaultRoot() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".claude")
}

// Context reports the session's latest context size, and false until one has
// been read. dir is the pane's directory, where the transcript is tried first.
func (r *Reader) Context(sessionID, dir string) (Context, bool) {
	if r.root == "" || !isSessionID(sessionID) {
		return Context{}, false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[sessionID]
	if !ok {
		s = &session{}
		r.sessions[sessionID] = s
	}

	if s.path == "" && !r.find(s, sessionID, dir) {
		return s.context, s.known
	}

	now := r.now()
	s.readNew(now)
	// Counted every call, not only when the transcript grew: a subagent's
	// liveness also depends on its own transcript and on time passing.
	s.context.Subagents = s.countSubagents(now)

	return s.context, s.known
}

// Retain forgets every session but these, so a closed pane's transcript state
// goes with it.
func (r *Reader) Retain(sessionIDs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	kept := make(map[string]*session, len(sessionIDs))
	for _, id := range sessionIDs {
		if s, ok := r.sessions[id]; ok {
			kept[id] = s
		}
	}

	r.sessions = kept
}

// find fills in the transcript path and reports whether there is one to read.
// A failed search is not repeated until locateRetry has passed, because it
// globs every project directory.
func (r *Reader) find(s *session, sessionID, dir string) bool {
	now := r.now()
	if !s.searchedAt.IsZero() && now.Sub(s.searchedAt) < locateRetry {
		return false
	}

	s.searchedAt = now

	path, ok := r.locate(sessionID, dir)
	if !ok {
		return false
	}

	s.path = path

	return true
}

// sessionIDPattern is the shape of a session id. The id arrives over the
// socket and becomes part of a path, so anything else is refused.
var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

func isSessionID(value string) bool {
	return sessionIDPattern.MatchString(value)
}

// locate finds the transcript. Claude Code files a session under the
// directory it was started in, usually the pane's, so that is tried before
// the glob across every project.
func (r *Reader) locate(sessionID, dir string) (string, bool) {
	name := sessionID + ".jsonl"
	projects := filepath.Join(r.root, "projects")

	if dir != "" {
		candidate := filepath.Join(projects, slugOf(dir), name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}

	matches, err := filepath.Glob(filepath.Join(projects, "*", name))
	if err != nil || len(matches) == 0 {
		return "", false
	}

	return matches[0], true
}

// slugOf is how Claude Code names a project directory: every character that is
// not an ASCII letter or digit becomes a dash.
func slugOf(dir string) string {
	var slug strings.Builder
	slug.Grow(len(dir))

	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			slug.WriteRune(r)
		default:
			slug.WriteByte('-')
		}
	}

	return slug.String()
}

// readNew reads what has been appended since the last read. A transcript only
// grows, so one shorter than what was read is a different file.
func (s *session) readNew(now time.Time) {
	info, err := os.Stat(s.path)
	if err != nil {
		// Deleted or refiled: search again, but keep the last reading, which
		// still describes the session better than nothing.
		s.path, s.offset = "", 0
		return
	}

	size := info.Size()
	switch {
	case size == s.offset:
		return
	case size < s.offset:
		s.offset, s.context, s.known, s.running = 0, Context{}, false, nil
	}

	file, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer file.Close()

	from, fromTail := s.offset, size-s.offset > maxScan
	if fromTail {
		from = size - maxScan
	}

	if _, err := file.Seek(from, io.SeekStart); err != nil {
		return
	}

	content, err := io.ReadAll(io.LimitReader(file, maxScan))
	if err != nil {
		return
	}

	// Only whole lines are read: the agent may be mid-write, and the rest of
	// the line arrives next poll.
	end := bytes.LastIndexByte(content, '\n')
	if end < 0 {
		return
	}

	s.offset = from + int64(end) + 1

	complete := content[:end]
	if fromTail {
		// A tail read starts mid-line, and that fragment is not JSON.
		if i := bytes.IndexByte(complete, '\n'); i >= 0 {
			complete = complete[i+1:]
		} else {
			complete = nil
		}
	}

	s.absorb(complete, now)
}

// entry is the part of a transcript line this package reads.
type entry struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	IsSidechain bool   `json:"isSidechain"`
	Cwd         string `json:"cwd"`
	Message     struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	CompactMetadata *struct {
		PostTokens int64 `json:"postTokens"`
	} `json:"compactMetadata"`
}

// Lines are only decoded when they could matter. Tool results can be
// megabytes, and decoding each one to discard it is the cost this avoids.
var (
	assistantMarker = []byte(`"type":"assistant"`)
	compactMarker   = []byte(`"compact_boundary"`)
)

// absorb updates the context and subagent launches from a run of lines; the
// last relevant line wins.
func (s *session) absorb(lines []byte, now time.Time) {
	for line := range bytes.SplitSeq(lines, []byte("\n")) {
		if !bytes.Contains(line, assistantMarker) && !bytes.Contains(line, compactMarker) {
			s.absorbLifecycle(line)
			continue
		}

		var e entry
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			continue
		}

		switch {
		case e.Type == "assistant":
			s.absorbAssistant(e)
			s.absorbLaunches(e.Message.Content, now)
		case e.Type == "system" && e.Subtype == "compact_boundary":
			if e.CompactMetadata != nil && e.CompactMetadata.PostTokens > 0 {
				s.context.Tokens, s.known = e.CompactMetadata.PostTokens, true
			}
		default:
			s.absorbLifecycle(line)
		}
	}
}

func (s *session) absorbAssistant(e entry) {
	u := e.Message.Usage
	if u == nil || e.Message.Model == syntheticModel {
		return
	}

	tokens := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if tokens <= 0 {
		return
	}

	s.context, s.known = Context{Tokens: tokens, Model: e.Message.Model, Cwd: e.Cwd}, true
}
