package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const continueListFixture = `[
  {"id":"h-pi-0003","title":"Migrate","status":"running","agent":"pi","projectId":"proj-alpha"},
  {"id":"h-codex-0002","title":"Codex work","status":"exited","agent":"codex","projectId":"proj-alpha"},
  {"id":"h-claude-0001","title":"Imported","status":"running","agent":"claude","external":true}
]`

func continueCLI(t *testing.T) *fakeCLI {
	cli := defaultCLI(t)
	cli.answers["session list"] = []byte(continueListFixture)
	cli.answers["session continue-pi"] = []byte(`{"id":"h-cont-9","title":"Continue Migrate (Pi SOL)","agent":"pi-sol","prompt":"SECRET-PROMPT"}`)
	cli.answers["session resume"] = []byte(`{"prompt":true,"updated":true}`)
	return cli
}

// routeFailingCLI fails only the named route ("resume" or "continue-pi") and answers the rest.
type routeFailingCLI struct {
	*fakeCLI
	route string
}

func (c routeFailingCLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) > 1 && args[0] == "session" && args[1] == c.route {
		c.fakeCLI.calls = append(c.fakeCLI.calls, args)
		return nil, errors.New("boom")
	}
	return c.fakeCLI.Run(ctx, args...)
}

func lastCall(cli *fakeCLI) string {
	return strings.Join(cli.calls[len(cli.calls)-1], " ")
}

func spawnedContinuation(cli *fakeCLI) bool {
	for _, call := range cli.calls {
		if len(call) > 1 && call[0] == "session" && call[1] == "continue-pi" {
			return true
		}
	}
	return false
}

func TestContinueRunsCLIWithTrimmedPromptAndReturnsNewSession(t *testing.T) {
	cli := continueCLI(t)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/sessions/h-codex-0002/continue", `{"prompt":"  run the failing test  ","agent":"pi-sol","mode":"yolo"}`)
	if code != http.StatusOK || strings.TrimSpace(body) != `{"agent":"pi-sol","name":"h-cont-9","title":"Continue Migrate (Pi SOL)"}` {
		t.Fatalf("continue = %d %s", code, body)
	}
	want := "session continue-pi -json -agent=pi-sol -mode=yolo -prompt=run the failing test h-codex-0002"
	if got := lastCall(cli); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestContinueWithoutPromptRunsDefaultsAndOmitsPromptFlag(t *testing.T) {
	cli := continueCLI(t)
	srv := newAPIServer(t, cli, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/h-codex-0002/continue", `{}`); code != http.StatusOK {
		t.Fatalf("continue = %d %s", code, body)
	}
	want := "session continue-pi -json -agent=pi -mode=normal h-codex-0002"
	if got := lastCall(cli); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestContinueRefusesImportedSessionBeforeSpawning(t *testing.T) {
	cli := continueCLI(t)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/sessions/h-claude-0001/continue", `{}`)
	if code != http.StatusConflict {
		t.Fatalf("imported continue = %d %s, want 409", code, body)
	}
	if spawnedContinuation(cli) {
		t.Fatalf("continue-pi ran for an imported session: %v", cli.calls)
	}
}

func TestContinueRejectsBadInputBeforeCLI(t *testing.T) {
	long := strings.Repeat("x", maxContentBytes+1)
	for _, body := range []string{
		`{"agent":"bash"}`,
		`{"mode":"root"}`,
		`{"prompt":"   "}`,
		`{"prompt":"bad\u0000prompt"}`,
		`{"prompt":"` + long + `"}`,
		`{"bogus":1}`,
	} {
		cli := continueCLI(t)
		srv := newAPIServer(t, cli, "")
		if code, resp := apiCall(t, srv, "POST", "/sessions/h-pi-0003/continue", body); code != http.StatusBadRequest {
			t.Errorf("%.40s = %d %s, want 400", body, code, resp)
		}
		if len(cli.calls) != 0 {
			t.Errorf("CLI ran for refused body %.40s: %v", body, cli.calls)
		}
	}
}

func TestContinueUnknownSessionIsNotFoundBeforeCLI(t *testing.T) {
	cli := continueCLI(t)
	srv := newAPIServer(t, cli, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/h-missing-9/continue", `{}`); code != http.StatusNotFound {
		t.Fatalf("unknown continue = %d %s, want 404", code, body)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("CLI ran for an unknown session: %v", cli.calls)
	}
}

func TestContinueFailsClosedWithoutCLI(t *testing.T) {
	srv := newAPIServer(t, nil, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/h-pi-0003/continue", `{}`); code != http.StatusServiceUnavailable {
		t.Fatalf("continue without CLI = %d %s, want 503", code, body)
	}
}

func TestContinueCLIFailuresAreBadGateway(t *testing.T) {
	srv := newAPIServer(t, &fakeCLI{fail: errors.New("boom")}, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/h-pi-0003/continue", `{}`); code != http.StatusBadGateway {
		t.Fatalf("CLI failure = %d %s, want 502", code, body)
	}

	cli := continueCLI(t)
	cli.answers["session continue-pi"] = []byte(`{"title":"no id"}`)
	srv = newAPIServer(t, cli, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/h-codex-0002/continue", `{}`); code != http.StatusBadGateway {
		t.Fatalf("CLI answer without a session id = %d %s, want 502", code, body)
	}
}

func TestContinuePiSourceResumesInPlaceWithTrimmedPrompt(t *testing.T) {
	cli := continueCLI(t)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/sessions/h-pi-0003/continue", `{"prompt":"  go on  "}`)
	if code != http.StatusOK {
		t.Fatalf("pi resume = %d %s", code, body)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "h-pi-0003" || got["agent"] != "pi" || got["title"] != "Migrate" || got["route"] != "session resume" {
		t.Fatalf("pi resume body = %s", body)
	}
	if want := "session resume -json -prompt=go on h-pi-0003"; lastCall(cli) != want {
		t.Fatalf("argv = %q, want %q", lastCall(cli), want)
	}
	if spawnedContinuation(cli) {
		t.Fatalf("continue-pi ran for a Pi source: %v", cli.calls)
	}
}

func TestContinuePiSourceWithoutPromptResumesPlainly(t *testing.T) {
	cli := continueCLI(t)
	cli.answers["session resume"] = []byte(`{"updated":true}`)
	srv := newAPIServer(t, cli, "")
	if code, body := apiCall(t, srv, "POST", "/sessions/h-pi-0003/continue", `{}`); code != http.StatusOK {
		t.Fatalf("pi resume = %d %s", code, body)
	}
	if want := "session resume -json h-pi-0003"; lastCall(cli) != want {
		t.Fatalf("argv = %q, want %q", lastCall(cli), want)
	}
}

func TestContinuePiSourceRefusesUnconfirmedPromptAndNamesRoute(t *testing.T) {
	cli := continueCLI(t)
	cli.answers["session resume"] = []byte(`{"updated":true}`)
	srv := newAPIServer(t, cli, "")
	code, body := apiCall(t, srv, "POST", "/sessions/h-pi-0003/continue", `{"prompt":"go"}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "session resume") || !strings.Contains(body, "did not confirm") {
		t.Fatalf("unconfirmed prompt = %d %s, want 502 naming the session resume route", code, body)
	}
}

func TestContinueErrorsNameTheRouteTaken(t *testing.T) {
	for _, tc := range []struct{ source, route, want string }{
		{"h-pi-0003", "resume", "route session resume:"},
		{"h-codex-0002", "continue-pi", "route continue-pi:"},
	} {
		cli := routeFailingCLI{fakeCLI: continueCLI(t), route: tc.route}
		srv := newAPIServer(t, cli, "")
		code, body := apiCall(t, srv, "POST", "/sessions/"+tc.source+"/continue", `{}`)
		if code != http.StatusBadGateway || !strings.Contains(body, tc.want) {
			t.Fatalf("%s route failure = %d %s, want 502 naming %q", tc.route, code, body, tc.want)
		}
	}
}
