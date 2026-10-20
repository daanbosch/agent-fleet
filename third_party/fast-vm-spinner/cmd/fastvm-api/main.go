package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/team-telnyx/fast-vm-spinner/internal/api"
	"github.com/team-telnyx/fast-vm-spinner/internal/enroller"
	"github.com/team-telnyx/fast-vm-spinner/internal/fastvm"
)

// Set at build time via -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	port := flag.Int("port", 8090, "HTTP listen port")
	backend := flag.String("backend", envOr("FASTVM_BACKEND", "firecracker"), "VM backend: proxmox or firecracker")
	stateDir := flag.String("state-dir", envOr("FASTVM_STATE_DIR", "/var/lib/fastvm"), "Path to fastvm state directory")
	sshHost := flag.String("ssh-host", envOr("FASTVM_SSH_HOST", ""), "Public hostname/IP for SSH proxy commands (default: machine hostname)")
	fcInstallDir := flag.String("firecracker-install-dir", envOr("FASTVM_FIRECRACKER_INSTALL_DIR", "/opt/fastvm-microvm"), "Firecracker install directory")
	fcBin := flag.String("firecracker-bin", envOr("FASTVM_FIRECRACKER_BIN", ""), "Path to firecrackerctl (default: <install-dir>/bin/firecrackerctl)")
	fcTemplate := flag.String("firecracker-template", envOr("FASTVM_FIRECRACKER_DEFAULT_TEMPLATE", "9013"), "Default Firecracker rootfs template name")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	engine, err := newBackend(*backend, *stateDir, *sshHost, *fcInstallDir, *fcBin, *fcTemplate)
	if err != nil {
		logger.Error("invalid backend configuration", "err", err)
		os.Exit(1)
	}
	enrollmentCfg := enrollmentConfigFromEnv()
	logEnrollmentConfig(logger, enrollmentCfg)
	router := api.NewRouterWithEnrollment(engine, logger, version, enrollmentCfg)

	addr := fmt.Sprintf(":%d", *port)
	readTimeout := durationFromEnvOr("FASTVM_API_READ_TIMEOUT", 30*time.Second)
	logger.Info("fastvm-api read timeout", "timeout", readTimeout.String(), "env", "FASTVM_API_READ_TIMEOUT")
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  readTimeout,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("fastvm-api starting",
			"addr", addr,
			"backend", strings.ToLower(strings.TrimSpace(*backend)),
			"state_dir", *stateDir,
			"ssh_host", engine.SSHHost(),
		)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-done
	logger.Info("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "err", err)
		os.Exit(1)
	}
	logger.Info("server stopped")
}

func newBackend(backend, stateDir, sshHost, fcInstallDir, fcBin, fcTemplate string) (fastvm.Backend, error) {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "proxmox":
		return fastvm.NewEngine(stateDir, sshHost), nil
	case "", "firecracker":
		if strings.TrimSpace(fcBin) == "" {
			fcBin = filepath.Join(fcInstallDir, "bin", "firecrackerctl")
		}
		return fastvm.NewFirecrackerEngine(fastvm.FirecrackerConfig{
			StateDir:        stateDir,
			SSHHost:         sshHost,
			InstallDir:      fcInstallDir,
			Bin:             fcBin,
			DefaultTemplate: fcTemplate,
			DefaultMemory:   intEnv("FASTVM_FIRECRACKER_DEFAULT_MEMORY", 0),
			DefaultCores:    intEnv("FASTVM_FIRECRACKER_DEFAULT_CORES", 0),
			Node:            envOr("FASTVM_FIRECRACKER_NODE", "atl1-pc1-027"),
			SSHUser:         envOr("FASTVM_FIRECRACKER_SSH_USER", "root"),
		}), nil
	default:
		return nil, fmt.Errorf("unsupported FASTVM_BACKEND %q", backend)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationFromEnvOr parses a Go duration (e.g. "5m") or bare seconds ("300");
// invalid values fall back to the default so a typo cannot brick the API.
func durationFromEnvOr(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	slog.Warn("invalid duration env var; using default", "key", key, "value", v, "default", fallback.String())
	return fallback
}

func intEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func enrollmentConfigFromEnv() api.EnrollmentConfig {
	url := strings.TrimSpace(os.Getenv("FASTVM_ENROLLER_URL"))
	mode := envOr("FASTVM_ENROLLER_MODE", "")
	if url == "" {
		mode = "disabled"
	}
	var client api.VMEnroller
	if url != "" {
		client = &enroller.Client{
			BaseURL:   url,
			Token:     os.Getenv("FASTVM_ENROLLER_TOKEN"),
			TokenFile: os.Getenv("FASTVM_ENROLLER_TOKEN_FILE"),
		}
	}
	return api.EnrollmentConfig{
		Mode:     mode,
		Client:   client,
		Features: splitCSV(os.Getenv("FASTVM_ENROLLER_FEATURES")),
		Timeout:  durationEnv("FASTVM_ENROLLER_TIMEOUT", 2*time.Minute),
	}
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func logEnrollmentConfig(logger *slog.Logger, cfg api.EnrollmentConfig) {
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		if cfg.Client == nil {
			mode = "disabled"
		} else {
			mode = "async"
		}
	}
	if cfg.Client == nil {
		mode = "disabled"
	}
	logger.Info("fastvm enrollment configuration", "mode", mode, "features", strings.Join(cfg.Features, ","))
}
