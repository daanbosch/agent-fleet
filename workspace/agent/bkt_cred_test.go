package main

import (
	"bytes"
	"testing"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/secrets"
)

func seedBktStore(t *testing.T, fn func(s *secrets.Data)) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AF_SECRET_KEY", "")
	if err := secrets.Update(func(s *secrets.Data) error { fn(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func bktCredOut() string {
	var out bytes.Buffer
	bktCredGet(&out)
	return out.String()
}

// The pasted API token is the one connection git cannot describe to the REST API.
func TestBktCredServesTheEmailAndTokenOfAnAPITokenConnection(t *testing.T) {
	seedBktStore(t, func(s *secrets.Data) {
		s.Git["bitbucket.org"] = secrets.GitEntry{User: "me@example.com", Token: "fake-api-token"}
	})
	if got, want := bktCredOut(), "username=me@example.com\npassword=fake-api-token\n"; got != want {
		t.Fatalf("answered %q, want %q", got, want)
	}
}

// Nothing is served for what `git credential fill` already covers or for another host: the
// subcommand must not widen what a caller can pull out of the store.
func TestBktCredServesNothingElse(t *testing.T) {
	cases := map[string]func(s *secrets.Data){
		"oauth connection": func(s *secrets.Data) {
			s.Bitbucket = &secrets.BitbucketCreds{AccessToken: "fake-oauth"}
			s.Git["bitbucket.org"] = secrets.GitEntry{User: "me@example.com", Token: "fake-api-token"}
		},
		"app password (account name, not an email)": func(s *secrets.Data) {
			s.Git["bitbucket.org"] = secrets.GitEntry{User: "handle", Token: "fake-app-pw"}
		},
		"another host only": func(s *secrets.Data) {
			s.Git["github.com"] = secrets.GitEntry{User: "me@example.com", Token: "fake-gh"}
		},
		"no connection": func(s *secrets.Data) {},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			seedBktStore(t, fn)
			if got := bktCredOut(); got != "" {
				t.Fatalf("answered %q, want nothing", got)
			}
		})
	}
}
