package opencode

// `workspace-agent opencode-attach`: the pane program of a shared Terminal session. It PUTs this
// pane's environment as the session's shell environment and only then replaces itself with the
// TUI, so the environment is in place before the TUI can submit anything and a fresh session
// never runs a shell with the server's environment. The pane's toolchain exports (JAVA_HOME,
// PATH, …) are part of what it reads, so the shell matches what a v1 pane's shell sees.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// attachRetries bounds the wait for the server to take the environment (a restart in progress).
var attachRetries = 40

// RunAttach is the subcommand body: opencode-attach --server <addr> --session <id> -- <tui flags>.
func RunAttach(args []string) {
	if err := runAttach(args, os.Environ(), execTUI); err != nil {
		fmt.Fprintf(os.Stderr, "agent-fleet: opencode: %v\n", err)
		time.Sleep(5 * time.Second) // let the pane show why before it closes
		os.Exit(1)
	}
}

// execTUI replaces the process with opencode; a var so a test can observe the argv.
var execTUI = func(argv, environ []string) error {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		return err
	}
	return syscall.Exec(bin, append([]string{"opencode"}, argv...), environ)
}

func runAttach(args, environ []string, start func(argv, environ []string) error) error {
	var server, ses string
	var flags []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--server":
			i++
			if i < len(args) {
				server = args[i]
			}
		case "--session":
			i++
			if i < len(args) {
				ses = args[i]
			}
		case "--":
			flags = args[i+1:]
			i = len(args)
		default:
			return fmt.Errorf("unknown argument %q", args[i])
		}
	}
	if server == "" || ses == "" {
		return fmt.Errorf("usage: opencode-attach --server <url> --session <id> -- <flags>")
	}
	password := ""
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, "OPENCODE_SERVER_PASSWORD="); ok {
			password = v
		}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: basicAuth{password: password}}
	vars := shellEnvironment(environ)
	var err error
	for try := 0; try < attachRetries; try++ {
		if err = apiJSONWith(client, "PUT", sessionPathURL(server, ses)+"/environment", map[string]any{"variables": vars}, nil); err == nil {
			break
		}
		if try < attachRetries-1 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if err != nil {
		return fmt.Errorf("could not set the session environment: %w", err)
	}
	return start(append([]string{"--server", server, "--session", ses}, flags...), environ)
}

// basicAuth signs every request with the serve user and a fixed password. The attach process
// holds one server's password only, so unlike authTransport it needs no host check.
type basicAuth struct{ password string }

func (b basicAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(serveUser, b.password)
	return http.DefaultTransport.RoundTrip(req)
}
