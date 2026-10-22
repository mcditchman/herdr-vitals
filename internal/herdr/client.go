package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
)

// socketPathEnv is where Herdr names the socket it made for this session.
const socketPathEnv = "HERDR_SOCKET_PATH"

// Client is the subset of the Herdr socket API this plugin uses.
type Client interface {
	// Call issues one request and decodes its result into result, which may be
	// nil when the caller does not need it.
	Call(ctx context.Context, method string, params any, result any) error
	// Server identifies the server holding the socket, or is "" while none
	// does. It changes when another server binds the path.
	Server() string
}

// SocketClient speaks NDJSON to the Herdr socket. Herdr closes the connection
// after answering, so each call dials its own.
type SocketClient struct {
	path string
	seq  atomic.Uint64
}

var _ Client = (*SocketClient)(nil)

// New builds a client for the socket named by HERDR_SOCKET_PATH. It performs no
// I/O; the first connection is made by the first call.
func New() (*SocketClient, error) {
	path := os.Getenv(socketPathEnv)
	if path == "" {
		return nil, fmt.Errorf("%s is not set: the plugin must be started by Herdr", socketPathEnv)
	}

	return &SocketClient{path: path}, nil
}

// Server reads the socket's identity the way Herdr tells its own socket from a
// successor's. It performs no request.
func (c *SocketClient) Server() string {
	return serverIdentity(c.path)
}

// Call sends one request on a connection of its own and reads the single
// response Herdr answers with before closing.
func (c *SocketClient) Call(ctx context.Context, method string, params any, result any) error {
	if params == nil {
		params = emptyParams{}
	}

	conn, err := dial(ctx, c.path)
	if err != nil {
		return fmt.Errorf("connect to herdr socket %s: %w", c.path, err)
	}
	defer conn.Close()

	// Unblock a call whose context is cancelled while it waits on the socket.
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()

	req := request{
		ID:     fmt.Sprintf("vitals-%d", c.seq.Add(1)),
		Method: method,
		Params: params,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return withContextErr(ctx, fmt.Errorf("send %s: %w", method, err))
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return withContextErr(ctx, fmt.Errorf("read %s response: %w", method, err))
	}

	return decodeFrame(method, line, result)
}

// decodeFrame turns one response line into result or the error it carries.
func decodeFrame(method string, line []byte, result any) error {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}

	if f.Error != nil {
		return fmt.Errorf("%s: %w", method, f.Error)
	}

	if result != nil && len(f.Result) > 0 {
		if err := json.Unmarshal(f.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
	}

	return nil
}

// withContextErr reports cancellation as such rather than as the socket error
// that closing the connection produced.
func withContextErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

// SessionSnapshot fetches every pane in the session.
func SessionSnapshot(ctx context.Context, c Client) (Snapshot, error) {
	var res snapshotResult
	if err := c.Call(ctx, MethodSessionSnapshot, emptyParams{}, &res); err != nil {
		return Snapshot{}, err
	}

	return res.Snapshot, nil
}

// ReportMetadata sets or clears display tokens on a pane.
func ReportMetadata(ctx context.Context, c Client, params ReportMetadataParams) error {
	return c.Call(ctx, MethodPaneReportMetadata, params, nil)
}

// ReportWorkspaceMetadata sets or clears display tokens on a workspace.
func ReportWorkspaceMetadata(ctx context.Context, c Client, params WorkspaceReportMetadataParams) error {
	return c.Call(ctx, MethodWorkspaceReportMetadata, params, nil)
}
