package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/team-telnyx/fast-vm-spinner/internal/enroller"
)

func main() {
	port := flag.Int("port", intEnv("FASTVM_ENROLLER_PORT", 8080), "HTTP listen port")
	secretsDir := flag.String("secrets-dir", envOr("FASTVM_ENROLLER_SECRETS_DIR", "/run/secrets/fastvm-enroller"), "Directory containing Vault-synced secret files")
	sshKey := flag.String("ssh-key", envOr("FASTVM_ENROLLER_SSH_KEY", ""), "SSH private key path for guest enrollment")
	token := flag.String("token", envOr("FASTVM_ENROLLER_TOKEN", ""), "Bearer token for FastVM API calls")
	tokenFile := flag.String("token-file", envOr("FASTVM_ENROLLER_TOKEN_FILE", "/run/secrets/fastvm-enroller/token"), "File containing bearer token")
	laceworkTokenFile := flag.String("lacework-token-file", envOr("FASTVM_ENROLLER_LACEWORK_TOKEN_FILE", ""), "File containing the Lacework agent access token")
	crowdStrikeCustomerIDFile := flag.String("crowdstrike-customer-id-file", envOr("FASTVM_ENROLLER_CROWDSTRIKE_CUSTOMER_ID_FILE", ""), "File containing the CrowdStrike Falcon customer ID")
	timeout := flag.Duration("timeout", durationEnv("FASTVM_ENROLLER_TIMEOUT", 2*time.Minute), "Per enrollment timeout")
	allowUnauth := flag.Bool("allow-unauth", boolEnv("FASTVM_ENROLLER_ALLOW_UNAUTH", false), "Allow unauthenticated enrollment requests")
	enableSystemctl := flag.Bool("enable-systemctl", boolEnv("FASTVM_ENROLLER_ENABLE_SYSTEMCTL", true), "Start/enable guest services with systemctl")
	allowedSuffix := flag.String("allowed-ssh-host-suffix", envOr("FASTVM_ENROLLER_ALLOWED_SSH_HOST_SUFFIX", ".hosts.telnyx.io"), "Allowed suffix for SSH proxy hostnames")
	flag.Parse()

	if strings.TrimSpace(*token) == "" && strings.TrimSpace(*tokenFile) != "" {
		if data, err := os.ReadFile(*tokenFile); err == nil {
			*token = strings.TrimSpace(string(data))
		}
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	svc := enroller.NewService(enroller.Config{
		AuthToken:                 *token,
		SecretsDir:                *secretsDir,
		SSHKeyPath:                *sshKey,
		LaceworkAccessTokenPath:   *laceworkTokenFile,
		CrowdStrikeCustomerIDPath: *crowdStrikeCustomerIDFile,
		DefaultTimeout:            *timeout,
		AllowUnauth:               *allowUnauth,
		EnableSystemctl:           *enableSystemctl,
		AllowedSSHHostSuff:        *allowedSuffix,
	}, nil, logger)

	addr := fmt.Sprintf(":%d", *port)
	server := &http.Server{
		Addr:         addr,
		Handler:      svc.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: timeoutWithFloor(*timeout, 30*time.Second),
		IdleTimeout:  60 * time.Second,
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("fastvm-enroller starting", "addr", addr, "secrets_dir", *secretsDir, "allowed_ssh_host_suffix", *allowedSuffix)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-done
	logger.Info("shutting down fastvm-enroller")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", "err", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func intEnv(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func boolEnv(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
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

func timeoutWithFloor(value, floor time.Duration) time.Duration {
	if value < floor {
		return floor
	}
	return value + 5*time.Second
}
