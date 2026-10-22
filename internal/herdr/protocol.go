// Package herdr is a client for the Herdr local socket API: NDJSON over the
// socket named by HERDR_SOCKET_PATH (a named pipe on Windows), one request per
// connection.
//
// Adapted from kryptamine/herdr-auto-title (MIT), whose docs/architecture/
// herdr-socket-api.md records the protocol facts this package relies on.
package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
)

// request is one outbound line. Herdr requires "params" on every method, so
// methods without parameters send an empty object rather than omitting it.
type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

// frame is one inbound line: a result or an error.
type frame struct {
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr api error %s: %s", e.Code, e.Message)
}

// Returned when a pane or workspace closed between the snapshot that listed it
// and the report that followed.
const (
	CodePaneNotFound      = "pane_not_found"
	CodeWorkspaceNotFound = "workspace_not_found"
)

// ErrorCode returns the Herdr error code carried by err, or "" if err is not a
// Herdr API error.
func ErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}

	return ""
}

const (
	MethodSessionSnapshot         = "session.snapshot"
	MethodPaneReportMetadata      = "pane.report_metadata"
	MethodWorkspaceReportMetadata = "workspace.report_metadata"
)

type emptyParams struct{}

// ReportMetadataParams is pane.report_metadata's request. A nil token value is
// sent as JSON null, which clears that token.
type ReportMetadataParams struct {
	PaneID string             `json:"pane_id"`
	Source string             `json:"source"`
	Tokens map[string]*string `json:"tokens,omitempty"`
	TTLMs  uint64             `json:"ttl_ms,omitempty"`
}

// WorkspaceReportMetadataParams is workspace.report_metadata's request; tokens
// are shown in space rows as $name. Tokens is required, so never omitted.
type WorkspaceReportMetadataParams struct {
	WorkspaceID string             `json:"workspace_id"`
	Source      string             `json:"source"`
	Tokens      map[string]*string `json:"tokens"`
	TTLMs       uint64             `json:"ttl_ms,omitempty"`
}

// WorkspaceInfo is the subset of a snapshot workspace this plugin reads.
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
}

// PaneInfo is the subset of a snapshot pane this plugin reads. Optional fields
// decode null to their zero value.
type PaneInfo struct {
	PaneID       string            `json:"pane_id"`
	WorkspaceID  string            `json:"workspace_id"`
	CWD          string            `json:"cwd"`
	Agent        string            `json:"agent"`
	AgentStatus  string            `json:"agent_status"`
	AgentSession *AgentSessionInfo `json:"agent_session"`
}

// Agent statuses the space breakdown counts. Herdr also reports idle, done
// and unknown, which are not broken out.
const (
	AgentStatusWorking = "working"
	AgentStatusBlocked = "blocked"
)

// SessionRefID is the only reference kind Herdr answers with: it keeps the id
// even when an integration reported a transcript path too.
const SessionRefID = "id"

// AgentSessionInfo identifies the conversation a pane's agent holds, as its
// integration hook reported it. It is null until an integration reports one.
type AgentSessionInfo struct {
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// IDFor reports the session id this reference holds, and false unless it names
// a session of the given agent by id.
func (s *AgentSessionInfo) IDFor(agent string) (string, bool) {
	if s == nil || s.Agent != agent || s.Kind != SessionRefID || s.Value == "" {
		return "", false
	}

	return s.Value, true
}

// Snapshot is the part of session.snapshot this plugin reads.
type Snapshot struct {
	Workspaces []WorkspaceInfo `json:"workspaces"`
	Panes      []PaneInfo      `json:"panes"`
}

type snapshotResult struct {
	Snapshot Snapshot `json:"snapshot"`
}
