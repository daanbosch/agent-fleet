// hyprmux-shim is the read-only window the agent-fleet Control Plane gets onto the host's
// hyprmux session registry. It serves the registry rows and headless session logs over
// loopback so the console can list and inspect host sessions without restarting them. It
// never writes the registry and never attaches to a session.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var requiredEnv = []string{"HYPRMUX_SHIM_ADDR", "HYPRMUX_SHIM_TOKEN", "HYPRMUX_REGISTRY", "HYPRMUX_AGENT_LOGS"}

// configFromEnv takes the shim's settings from the environment the Control Plane passes it.
// The token travels in the environment, never in argv, where any local user could read it.
func configFromEnv(get func(string) string) (config, string, error) {
	for _, k := range requiredEnv {
		if get(k) == "" {
			return config{}, "", fmt.Errorf("%s is required", k)
		}
	}
	addr := get("HYPRMUX_SHIM_ADDR")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return config{}, "", fmt.Errorf("HYPRMUX_SHIM_ADDR %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return config{}, "", fmt.Errorf("HYPRMUX_SHIM_ADDR %q must bind a loopback address", addr)
	}
	return config{
		Token:        get("HYPRMUX_SHIM_TOKEN"),
		RegistryPath: get("HYPRMUX_REGISTRY"),
		LogsDir:      get("HYPRMUX_AGENT_LOGS"),
	}, addr, nil
}

func main() {
	cfg, addr, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("hyprmux-shim: %v", err)
	}
	srv := &http.Server{Addr: addr, Handler: newHandler(cfg), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("hyprmux-shim serving %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("hyprmux-shim: %v", err)
	}
}
