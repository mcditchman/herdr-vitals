package herdr

import (
	"context"
	"io"
	"os"
)

// pipePrefix is the named pipe namespace. On Windows HERDR_SOCKET_PATH names a
// plain file, and the pipe Herdr listens on carries that whole path as its
// name, drive letter and backslashes included.
const pipePrefix = `\\.\pipe\`

// dial opens one connection to the pipe behind path. A pipe takes no deadline,
// but closing it unblocks a pending read, which is all cancellation needs.
func dial(_ context.Context, path string) (io.ReadWriteCloser, error) {
	return os.OpenFile(pipePrefix+path, os.O_RDWR, 0)
}

// serverIdentity is the marker Herdr writes into the socket file when it binds
// the pipe (its pid and start time), so a successor's differs from its
// predecessor's; "" says no server holds the path.
func serverIdentity(path string) string {
	marker, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	return string(marker)
}
