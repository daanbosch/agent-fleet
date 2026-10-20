package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const (
	ghRepo       = "team-telnyx/fast-vm-spinner"
	binaryName   = "fastvm-api"
	binaryPath   = "/usr/local/bin/fastvm-api"
	backupPath   = "/usr/local/bin/fastvm-api.prev"
	serviceName  = "fastvm-api"
	updateSecret = "FASTVM_UPDATE_SECRET"

	healthCheckRetries = 5
	healthCheckDelay   = 2 * time.Second
)

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	URL                string `json:"url"`
}

// SelfUpdate handles POST /api/v1/update — downloads the latest release
// binary from GitHub and restarts the service via systemd.
func (h *Handler) SelfUpdate(w http.ResponseWriter, r *http.Request) {
	// Validate shared secret
	secret := os.Getenv(updateSecret)
	if secret == "" {
		h.logger.Error("FASTVM_UPDATE_SECRET not set, update endpoint disabled")
		writeError(w, http.StatusServiceUnavailable, "update endpoint not configured")
		return
	}

	token := r.Header.Get("Authorization")
	if token == "" || token != "Bearer "+secret {
		writeError(w, http.StatusUnauthorized, "invalid or missing authorization")
		return
	}

	h.logger.Info("self-update triggered")

	// Fetch latest release metadata from GitHub
	ghToken := os.Getenv("GITHUB_TOKEN")
	release, err := fetchLatestRelease(ghToken)
	if err != nil {
		h.logger.Error("failed to fetch latest release", "err", err)
		writeError(w, http.StatusBadGateway, "failed to fetch release: "+err.Error())
		return
	}

	// Find the linux-amd64 binary asset
	assetName := fmt.Sprintf("%s-linux-%s", binaryName, runtime.GOARCH)
	var downloadURL, apiURL string
	for _, a := range release.Assets {
		if a.Name == assetName {
			downloadURL = a.BrowserDownloadURL
			apiURL = a.URL
			break
		}
	}
	if downloadURL == "" {
		h.logger.Error("no matching asset in release", "tag", release.TagName, "want", assetName)
		writeError(w, http.StatusNotFound,
			fmt.Sprintf("release %s has no asset %q", release.TagName, assetName))
		return
	}

	h.logger.Info("downloading release", "tag", release.TagName, "asset", assetName)

	// Download to a temp file
	tmpPath := binaryPath + ".new"
	if err := downloadAsset(apiURL, ghToken, tmpPath); err != nil {
		h.logger.Error("failed to download asset", "err", err)
		writeError(w, http.StatusBadGateway, "download failed: "+err.Error())
		return
	}

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "chmod failed: "+err.Error())
		return
	}

	// Back up current binary before replacing
	if err := copyFile(binaryPath, backupPath); err != nil {
		h.logger.Warn("could not back up current binary", "err", err)
		// Continue anyway — first deploy won't have an existing binary to back up
	} else {
		h.logger.Info("backed up current binary", "path", backupPath)
	}

	// Atomic replace
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "replace binary failed: "+err.Error())
		return
	}

	h.logger.Info("binary replaced, restarting service", "tag", release.TagName)

	// Respond before restarting
	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("Updated to %s, restarting with health check...", release.TagName),
		"version": release.TagName,
	})

	// Flush response, then spawn a detached health-check process that
	// survives the systemctl restart. It will verify the new binary is
	// healthy and roll back if not.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go h.spawnHealthCheckAndRestart(release.TagName)
}

// Rollback handles POST /api/v1/rollback — restores the previous binary and restarts.
func (h *Handler) Rollback(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv(updateSecret)
	if secret == "" {
		writeError(w, http.StatusServiceUnavailable, "update endpoint not configured")
		return
	}

	token := r.Header.Get("Authorization")
	if token == "" || token != "Bearer "+secret {
		writeError(w, http.StatusUnauthorized, "invalid or missing authorization")
		return
	}

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		writeError(w, http.StatusNotFound, "no backup binary found at "+backupPath)
		return
	}

	h.logger.Info("manual rollback triggered")

	if err := os.Rename(backupPath, binaryPath); err != nil {
		writeError(w, http.StatusInternalServerError, "rollback failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Rolled back to previous version, restarting...",
	})

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		cmd := exec.Command("sudo", "systemctl", "restart", serviceName)
		if out, err := cmd.CombinedOutput(); err != nil {
			h.logger.Error("rollback restart failed", "err", err, "output", string(out))
		}
	}()
}

// spawnHealthCheckAndRestart launches a detached shell script that:
//  1. Restarts the service
//  2. Health-checks the new binary
//  3. Rolls back to the backup if unhealthy
//
// The script runs as a separate process so it survives the systemctl restart
// that kills this (the old) process.
func (h *Handler) spawnHealthCheckAndRestart(tag string) {
	time.Sleep(500 * time.Millisecond)

	script := fmt.Sprintf(`#!/bin/bash
# Auto-rollback health checker — runs detached from the API process.
# Do NOT use set -e: systemctl restart may return non-zero if the new
# binary crashes immediately, and we need to keep running to rollback.

log() { echo "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ) fastvm-update: $*" | systemd-cat -t fastvm-update 2>/dev/null || echo "$*"; }

log "restarting %s after update to %s"
sudo systemctl restart %s || true

# Wait for new process to start (or fail)
sleep 3

# Health check loop
for i in $(seq 1 %d); do
    if curl -sf -o /dev/null http://localhost:8090/api/v1/health 2>/dev/null; then
        log "health check passed on attempt $i"
        rm -f "$0"
        exit 0
    fi
    log "health check failed (attempt $i/%d)"
    sleep %d
done

log "all health checks failed, rolling back"
if [ -f %s ]; then
    sudo mv %s %s
    sudo chmod +x %s
    sudo systemctl restart %s || true
    sleep 2
    if curl -sf -o /dev/null http://localhost:8090/api/v1/health 2>/dev/null; then
        log "rollback successful, service is healthy"
    else
        log "WARNING: rollback completed but service still unhealthy"
    fi
else
    log "no backup binary found, cannot roll back"
fi
rm -f "$0"
`, serviceName, tag, serviceName,
		healthCheckRetries, healthCheckRetries, int(healthCheckDelay.Seconds()),
		backupPath, backupPath, binaryPath, binaryPath, serviceName)

	tmpScript := "/tmp/fastvm-update.sh"
	if err := os.WriteFile(tmpScript, []byte(script), 0755); err != nil {
		h.logger.Error("failed to write health check script", "err", err)
		return
	}

	// Use systemd-run to launch as a transient service unit, so the script
	// runs in its own cgroup and survives when systemctl restart kills the
	// fastvm-api cgroup.
	cmd := exec.Command("sudo", "systemd-run",
		"--unit=fastvm-update",
		"--description=fastvm-api health check and rollback",
		"bash", tmpScript)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		h.logger.Error("failed to start health check script", "err", err)
		return
	}

	h.logger.Info("health check unit launched", "tag", tag)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}

	// Preserve permissions
	info, err := os.Stat(src)
	if err == nil {
		os.Chmod(dst, info.Mode())
	}
	return nil
}

func fetchLatestRelease(ghToken string) (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", ghRepo)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if ghToken != "" {
		req.Header.Set("Authorization", "Bearer "+ghToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

func downloadAsset(apiURL, ghToken, dest string) error {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return err
	}
	// Use the GitHub API URL with octet-stream accept to get the binary
	req.Header.Set("Accept", "application/octet-stream")
	if ghToken != "" {
		req.Header.Set("Authorization", "Bearer "+ghToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(dest)
		return err
	}
	return nil
}
