package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func telegramCLI(t *testing.T) *fakeCLI {
	return &fakeCLI{answers: map[string][]byte{"session list": fixture(t, "session-list.json")}}
}

func TestTelegramStatusReportsCountsAndSupervisorsOnly(t *testing.T) {
	cli := telegramCLI(t)
	srv := newAPIServer(t, cli, "testdata/telegram-checkpoint.json")
	code, body := apiCall(t, srv, "GET", "/api/telegram/status", "")
	if code != http.StatusOK {
		t.Fatalf("telegram status = %d %s", code, body)
	}
	for _, leak := range []string{"-100123456", "gen-1", "fixture-hub.sock", "SECRET-PROMPT-MARKER", "/work/alpha", `"h-work-2"`} {
		if strings.Contains(body, leak) {
			t.Errorf("telegram status leaked %q: %s", leak, body)
		}
	}
	var out struct {
		Checkpoint struct {
			Present               bool `json:"present"`
			Version               int  `json:"version"`
			Projects              int  `json:"projects"`
			Sources               int  `json:"sources"`
			DestinationConfigured bool `json:"destinationConfigured"`
		} `json:"checkpoint"`
		Supervisors []sessionWireRow `json:"supervisors"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	c := out.Checkpoint
	if !c.Present || c.Version != 3 || c.Projects != 2 || c.Sources != 1 || !c.DestinationConfigured {
		t.Fatalf("checkpoint = %+v", c)
	}
	if len(out.Supervisors) != 1 || out.Supervisors[0].ID != "h-sup-1" {
		t.Fatalf("supervisors = %+v, want only the supervisor row", out.Supervisors)
	}
}

func TestTelegramStatusWithoutCheckpointIsAbsentNotAnError(t *testing.T) {
	srv := newAPIServer(t, telegramCLI(t), filepath.Join(t.TempDir(), "none.json"))
	code, body := apiCall(t, srv, "GET", "/api/telegram/status", "")
	if code != http.StatusOK || !strings.Contains(body, `"present":false`) {
		t.Fatalf("telegram status with no checkpoint = %d %s, want present:false", code, body)
	}
}

func TestTelegramStatusWithoutHomeIsUnavailable(t *testing.T) {
	srv := newAPIServer(t, telegramCLI(t), "")
	if code, body := apiCall(t, srv, "GET", "/api/telegram/status", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("telegram status with no HOME = %d %s, want 503", code, body)
	}
	if code, body := apiCall(t, srv, "GET", "/api/telegram/topics", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("telegram topics with no HOME = %d %s, want 503", code, body)
	}
}

func TestTelegramStatusCLIFailureIsBadGateway(t *testing.T) {
	cli := telegramCLI(t)
	cli.fail = errString("hyprmux session failed: state unavailable")
	srv := newAPIServer(t, cli, "testdata/telegram-checkpoint.json")
	code, body := apiCall(t, srv, "GET", "/api/telegram/status", "")
	if code != http.StatusBadGateway || !strings.Contains(body, "state unavailable") {
		t.Fatalf("telegram status with CLI failure = %d %s", code, body)
	}
}

func TestTelegramTopicsListProjectsWithTopicFlag(t *testing.T) {
	srv := newAPIServer(t, nil, "testdata/telegram-checkpoint.json")
	code, body := apiCall(t, srv, "GET", "/api/telegram/topics", "")
	if code != http.StatusOK {
		t.Fatalf("telegram topics = %d %s", code, body)
	}
	want := `{"present":true,"topics":[{"hasTopic":true,"projectId":"proj-alpha"},{"hasTopic":false,"projectId":"proj-beta"}]}`
	if strings.TrimSpace(body) != want {
		t.Fatalf("telegram topics = %s, want %s", body, want)
	}
}

func TestTelegramTopicMessageIsNotSent(t *testing.T) {
	cli := telegramCLI(t)
	srv := newAPIServer(t, cli, "testdata/telegram-checkpoint.json")
	code, body := apiCall(t, srv, "POST", "/api/telegram/topic-message", `{"topic":"8","text":"hello"}`)
	if code != http.StatusNotImplemented || !strings.Contains(body, "not sent") {
		t.Fatalf("topic-message = %d %s, want 501 saying nothing was sent", code, body)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("topic-message touched the CLI: %v", cli.calls)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
