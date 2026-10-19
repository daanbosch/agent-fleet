package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func fastvmLaunchCLI(t *testing.T) *fakeCLI {
	cli := defaultCLI(t)
	cli.answers["session launch"] = []byte(`{"id":"fv1","title":"Reply with exactly the single word W1-OK."}`)
	return cli
}

func TestLaunchFastVMRunsCLIWithFastVMFlagAndTrimmedPrompt(t *testing.T) {
	cli := fastvmLaunchCLI(t)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/sessions/launch-fastvm",
		`{"project":"proj-alpha","mode":"yolo","prompt":"  Reply with exactly the single word W1-OK.  "}`)
	if code != http.StatusOK || !strings.Contains(body, `"id":"fv1"`) {
		t.Fatalf("launch-fastvm = %d %s", code, body)
	}
	want := "session launch -project proj-alpha -agent pi -mode yolo -fastvm -json -- Reply with exactly the single word W1-OK."
	if got := lastCall(cli); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestLaunchFastVMDefaultsToNormalMode(t *testing.T) {
	cli := fastvmLaunchCLI(t)
	srv := newAPIServer(t, cli, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/launch-fastvm", `{"project":"proj-alpha","prompt":"hi"}`); code != http.StatusOK {
		t.Fatalf("launch-fastvm = %d %s", code, body)
	}
	if got := lastCall(cli); !strings.Contains(got, "-mode normal -fastvm") {
		t.Fatalf("argv = %q, want normal mode", got)
	}
}

func TestLaunchFastVMRefusesBadInputBeforeAnyLaunch(t *testing.T) {
	bodies := []string{
		`{"project":"proj-alpha","prompt":"   "}`,
		`{"project":"proj-alpha"}`,
		`{"project":"proj-alpha","prompt":"` + strings.Repeat("a", 8193) + `"}`,
		`{"project":"bad project","prompt":"x"}`,
		`{"project":"proj-alpha","prompt":"x","mode":"fast"}`,
		`{"project":"proj-alpha","prompt":"x","agent":"codex"}`,
		`{"project":"proj-alpha","prompt":"x","headless":true}`,
	}
	for _, body := range bodies {
		t.Run(body[:min(len(body), 60)], func(t *testing.T) {
			cli := fastvmLaunchCLI(t)
			srv := newAPIServer(t, cli, "")
			if code, out := apiCall(t, srv, "POST", "/sessions/launch-fastvm", body); code != http.StatusBadRequest {
				t.Fatalf("launch-fastvm = %d %s, want 400", code, out)
			}
			if len(cli.calls) != 0 {
				t.Fatalf("CLI was called for a refused request: %v", cli.calls)
			}
		})
	}
}

func TestLaunchFastVMReportsCLIFailureAsBadGateway(t *testing.T) {
	cli := fastvmLaunchCLI(t)
	cli.fail = errors.New("fastvm API unreachable")
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/sessions/launch-fastvm", `{"project":"proj-alpha","prompt":"hi"}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "fastvm API unreachable") {
		t.Fatalf("launch-fastvm = %d %s, want 502 with the CLI error", code, body)
	}
}
