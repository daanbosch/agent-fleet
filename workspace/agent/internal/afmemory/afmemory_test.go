package afmemory

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeAgent(t *testing.T, applied *[]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-xyz" {
			w.WriteHeader(401)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /agents/memory/claude-import":
			io.WriteString(w, `{"sources":[{"slug":"-a","count":2,"project":{"id":"a-0123456789ab","display":"a"}},{"slug":"-b","count":1,"reason":"no_project"}]}`)
		case "GET /agents/memory/claude-import/preview":
			if r.URL.Query().Get("slug") == "-b" {
				io.WriteString(w, `{"reason":"no_project","items":[],"counts":{}}`)
				return
			}
			var items []string
			for i := 0; i < 120; i++ {
				items = append(items, `{"name":"n`+strings.Repeat("x", i%3)+string(rune('a'+i%26))+`","status":"new","sourceHash":"h"}`)
			}
			items = append(items, `{"name":"bad","status":"secret","findings":[{"path":"body","rule":"private-key-block","line":3,"hint":"-----BEG…"}]}`)
			io.WriteString(w, `{"project":{"id":"a-0123456789ab","display":"a"},"counts":{"new":120,"secret":1},"items":[`+strings.Join(items, ",")+`]}`)
		case "POST /agents/memory/claude-import":
			var req struct {
				Project string
				Items   []struct{ Name string }
			}
			json.NewDecoder(r.Body).Decode(&req)
			if len(req.Items) > 50 || req.Project != "a-0123456789ab" || r.URL.Query().Get("project") != req.Project {
				w.WriteHeader(400)
				return
			}
			*applied = append(*applied, r.URL.RawQuery)
			var rs []string
			for _, it := range req.Items {
				rs = append(rs, `{"name":"`+it.Name+`","result":"imported"}`)
			}
			io.WriteString(w, `{"results":[`+strings.Join(rs, ",")+`]}`)
		case "GET /agents/memory/entries/changes":
			io.WriteString(w, `{"changes":[{"at":"2026-10-05T00:00:00Z","op":"import","name":"x","authorKind":"member","authorSession":"console","project":{"display":"a"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Token: "tok-xyz", HTTP: srv.Client()}
}

func run(c *Client, args ...string) (int, string, string) {
	var o, e bytes.Buffer
	code := Main(c, args, &o, &e)
	return code, o.String(), e.String()
}

func TestAFMemoryCommands(t *testing.T) {
	var applied []string
	c := fakeAgent(t, &applied)

	if code, out, _ := run(c, "import-sources"); code != 0 || !strings.Contains(out, "-> a") || !strings.Contains(out, "no_project") {
		t.Errorf("sources: %d %q", code, out)
	}
	if code, out, _ := run(c, "changes", "--limit", "5"); code != 0 || !strings.Contains(out, "import") {
		t.Errorf("changes: %d %q", code, out)
	}
	code, out, _ := run(c, "import", "--project", "-a", "--dry-run")
	if code != 0 || len(applied) != 0 || !strings.Contains(out, "private-key-block in body, line 3") || !strings.Contains(out, "nothing was written") {
		t.Errorf("dry run: %d applied=%v\n%s", code, applied, out)
	}
	// 120 items go in batches of 50.
	code, out, _ = run(c, "import", "--project", "-a")
	if code != 0 || len(applied) != 3 || !strings.Contains(out, "done: 120 imported, 0 skipped") {
		t.Errorf("import: %d batches=%d\n%s", code, len(applied), out)
	}
	if code, _, e := run(c, "import", "--project", "-b"); code != 1 || !strings.Contains(e, "no_project") {
		t.Errorf("no_project: %d %q", code, e)
	}
}

func TestAFMemoryUsageAndTokenNeverPrinted(t *testing.T) {
	c := fakeAgent(t, new([]string))
	for _, args := range [][]string{nil, {"bogus"}, {"import"}, {"changes", "x"}} {
		if code, _, e := run(c, args...); code != 2 || !strings.Contains(e, "Usage") {
			t.Errorf("%v: code %d", args, code)
		}
	}
	if code, out, _ := run(c, "--help"); code != 0 || !strings.Contains(out, "import-sources") {
		t.Errorf("help: %d", code)
	}
	bad := &Client{Base: c.Base, Token: "wrong-token-value", HTTP: c.HTTP}
	code, o, e := run(bad, "import-sources")
	if code != 1 || strings.Contains(o+e, "wrong-token-value") {
		t.Errorf("bad token: %d %q %q", code, o, e)
	}
}

func TestAFMemoryMuseImport(t *testing.T) {
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /agents/memory/muse-import":
			io.WriteString(w, `{"sources":[{"dir":"demo","count":2,"shared":1,"project":{"id":"demo-0123456789ab","display":"demo"}}]}`)
		case "GET /agents/memory/muse-import/preview":
			if r.URL.Query().Get("dir") != "demo" {
				io.WriteString(w, `{"reason":"no_project","items":[],"counts":{}}`)
				return
			}
			io.WriteString(w, `{"project":{"id":"demo-0123456789ab","display":"demo"},"counts":{"new":1,"diverged":1},"items":[`+
				`{"name":"team","status":"new","sourceHash":"h","shared":true},`+
				`{"name":"mine","status":"diverged","reason":"af_changed"}]}`)
		case "POST /agents/memory/muse-import":
			var req struct {
				Project, Dir string
				Items        []struct{ Name string }
			}
			json.NewDecoder(r.Body).Decode(&req)
			if req.Dir != "demo" || len(req.Items) != 1 || req.Items[0].Name != "team" || r.URL.Query().Get("project") != req.Project {
				w.WriteHeader(400)
				return
			}
			posted = append(posted, req.Items[0].Name)
			io.WriteString(w, `{"results":[{"name":"team","result":"imported"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Token: "tok", HTTP: srv.Client()}

	if code, out, _ := run(c, "muse-import-sources"); code != 0 || !strings.Contains(out, "-> demo") || !strings.Contains(out, "may be shared") {
		t.Errorf("sources: %d\n%s", code, out)
	}
	code, out, _ := run(c, "muse-import", "--dir", "demo", "--dry-run")
	if code != 0 || len(posted) != 0 || !strings.Contains(out, "(shared: git tracks this file") ||
		!strings.Contains(out, "mine: af_changed") || !strings.Contains(out, "dry run: 1 would be imported") {
		t.Errorf("dry run: %d posted=%v\n%s", code, posted, out)
	}
	if code, out, _ = run(c, "muse-import", "--dir", "demo"); code != 0 || len(posted) != 1 || !strings.Contains(out, "done: 1 imported, 0 skipped") {
		t.Errorf("apply: %d posted=%v\n%s", code, posted, out)
	}
	if code, _, e := run(c, "muse-import", "--dir", "other"); code != 1 || !strings.Contains(e, "muse working copy cannot be imported") {
		t.Errorf("no project: %d %s", code, e)
	}
	if code, _, _ := run(c, "muse-import"); code != 2 {
		t.Errorf("missing --dir: %d", code)
	}
}

// The personal scope lands in the user scope and is never sent without --into-user.
func TestAFMemoryMusePersonalNeedsIntoUser(t *testing.T) {
	var posted []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /agents/memory/muse-import/preview":
			q := r.URL.Query()
			if q.Get("scope") != "personal" || q.Get("into") != "user" {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"scope":"personal","target":"user","counts":{"new":1},"items":[{"name":"likes-go","status":"new","sourceHash":"h","derived":true}]}`)
		case "POST /agents/memory/muse-import":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			posted = append(posted, req)
			io.WriteString(w, `{"results":[{"name":"likes-go","result":"imported"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, Token: "tok", HTTP: srv.Client()}

	if code, _, e := run(c, "muse-import", "--scope", "personal"); code != 2 || !strings.Contains(e, "--into-user") || len(posted) != 0 {
		t.Errorf("without --into-user: %d %q", code, e)
	}
	if code, _, _ := run(c, "muse-import", "--dir", "demo", "--into-user"); code != 2 {
		t.Errorf("--into-user on the project scope: %d", code)
	}
	code, out, _ := run(c, "muse-import", "--scope", "personal", "--into-user", "--dry-run")
	if code != 0 || len(posted) != 0 || !strings.Contains(out, "USER memory") || !strings.Contains(out, "no frontmatter") {
		t.Errorf("dry run: %d\n%s", code, out)
	}
	if code, out, _ = run(c, "muse-import", "--scope", "personal", "--into-user"); code != 0 || len(posted) != 1 ||
		posted[0]["into"] != "user" || posted[0]["scope"] != "personal" || posted[0]["project"] != "" {
		t.Errorf("apply: %d posted=%v\n%s", code, posted, out)
	}
}
