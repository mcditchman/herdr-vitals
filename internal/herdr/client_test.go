package herdr

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDecodeFrame(t *testing.T) {
	t.Run("result is decoded", func(t *testing.T) {
		var res snapshotResult
		line := []byte(`{"id":"x","result":{"snapshot":{"panes":[{"pane_id":"w1:p1","agent":"claude",` +
			`"agent_session":{"agent":"claude","kind":"id","value":"abc"}}]}}}`)

		if err := decodeFrame("m", line, &res); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(res.Snapshot.Panes) != 1 || res.Snapshot.Panes[0].AgentSession.Value != "abc" {
			t.Fatalf("snapshot not decoded: %+v", res)
		}
	})

	t.Run("error frame carries its code", func(t *testing.T) {
		err := decodeFrame("m", []byte(`{"id":"x","error":{"code":"pane_not_found","message":"gone"}}`), nil)

		if ErrorCode(err) != CodePaneNotFound {
			t.Fatalf("ErrorCode = %q, want %q (err %v)", ErrorCode(err), CodePaneNotFound, err)
		}
	})

	t.Run("malformed line is an error", func(t *testing.T) {
		if err := decodeFrame("m", []byte(`not json`), nil); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("nil result ignores the payload", func(t *testing.T) {
		if err := decodeFrame("m", []byte(`{"id":"x","result":{"anything":1}}`), nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestErrorCodeOfOtherErrors(t *testing.T) {
	if got := ErrorCode(errors.New("plain")); got != "" {
		t.Fatalf("ErrorCode = %q, want empty", got)
	}

	if got := ErrorCode(nil); got != "" {
		t.Fatalf("ErrorCode(nil) = %q, want empty", got)
	}
}

func TestAgentSessionIDFor(t *testing.T) {
	tests := []struct {
		name    string
		session *AgentSessionInfo
		wantID  string
		wantOK  bool
	}{
		{"nil", nil, "", false},
		{"other agent", &AgentSessionInfo{Agent: "codex", Kind: "id", Value: "a"}, "", false},
		{"path kind", &AgentSessionInfo{Agent: "claude", Kind: "path", Value: "a"}, "", false},
		{"empty value", &AgentSessionInfo{Agent: "claude", Kind: "id", Value: ""}, "", false},
		{"claude id", &AgentSessionInfo{Agent: "claude", Kind: "id", Value: "a"}, "a", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := tt.session.IDFor("claude")
			if id != tt.wantID || ok != tt.wantOK {
				t.Fatalf("IDFor = (%q, %v), want (%q, %v)", id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

// A nil token must reach the wire as null, which is how Herdr clears it.
func TestReportMetadataParamsEncodesClearAsNull(t *testing.T) {
	value := "x"
	raw, err := json.Marshal(ReportMetadataParams{
		PaneID: "w1:p1",
		Source: "s",
		Tokens: map[string]*string{"set": &value, "clear": nil},
		TTLMs:  1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"pane_id":"w1:p1","source":"s","tokens":{"clear":null,"set":"x"},"ttl_ms":1000}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}

func TestWorkspaceReportMetadataParamsEncoding(t *testing.T) {
	value := "3 agents"
	raw, err := json.Marshal(WorkspaceReportMetadataParams{
		WorkspaceID: "w1",
		Source:      "s",
		Tokens:      map[string]*string{"agents": &value, "gone": nil},
		TTLMs:       1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"workspace_id":"w1","source":"s","tokens":{"agents":"3 agents","gone":null},"ttl_ms":1000}`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}
