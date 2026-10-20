package enroller

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	AuthToken                 string
	SecretsDir                string
	SSHKeyPath                string
	LaceworkAccessTokenPath   string
	CrowdStrikeCustomerIDPath string
	DefaultTimeout            time.Duration
	AllowUnauth               bool
	EnableSystemctl           bool
	AllowedSSHHostSuff        string
}

type Service struct {
	cfg      Config
	runner   Runner
	log      *slog.Logger
	mu       sync.RWMutex
	statuses map[string]EnrollmentStatus
}

type Runner interface {
	RunScript(ctx context.Context, target SSHTarget, script string) (string, error)
}

type SSHTarget struct {
	Host    string
	Port    int
	User    string
	KeyPath string
	Timeout time.Duration
}

type ExecRunner struct{}

type EnrollRequest struct {
	VMID     string            `json:"vmid"`
	Name     string            `json:"name"`
	User     string            `json:"user"`
	Backend  string            `json:"backend"`
	Template string            `json:"template"`
	Node     string            `json:"node"`
	SSHHost  string            `json:"ssh_host"`
	SSHPort  int               `json:"ssh_port"`
	SSHUser  string            `json:"ssh_user"`
	Features []string          `json:"features"`
	Tags     map[string]string `json:"tags,omitempty"`
}

type EnrollResponse struct {
	VMID     string   `json:"vmid"`
	Status   string   `json:"status"`
	Applied  []string `json:"applied"`
	Skipped  []string `json:"skipped"`
	Duration string   `json:"duration"`
}

type EnrollmentStatus struct {
	VMID      string   `json:"vmid"`
	Status    string   `json:"status"`
	Applied   []string `json:"applied,omitempty"`
	Skipped   []string `json:"skipped,omitempty"`
	Error     string   `json:"error,omitempty"`
	StartedAt string   `json:"started_at,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	Duration  string   `json:"duration,omitempty"`
}

type httpResponse struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data,omitempty"`
	Error *string     `json:"error,omitempty"`
}

func NewService(cfg Config, runner Runner, logger *slog.Logger) *Service {
	if cfg.SecretsDir == "" {
		cfg.SecretsDir = "/run/secrets/fastvm-enroller"
	}
	if cfg.SSHKeyPath == "" {
		cfg.SSHKeyPath = filepath.Join(cfg.SecretsDir, "id_rsa")
	}
	if cfg.LaceworkAccessTokenPath == "" {
		cfg.LaceworkAccessTokenPath = filepath.Join(cfg.SecretsDir, "lacework_access_token")
	}
	if cfg.CrowdStrikeCustomerIDPath == "" {
		cfg.CrowdStrikeCustomerIDPath = filepath.Join(cfg.SecretsDir, "crowdstrike_customer_id")
	}
	if cfg.DefaultTimeout == 0 {
		cfg.DefaultTimeout = 2 * time.Minute
	}
	if cfg.AllowedSSHHostSuff == "" {
		cfg.AllowedSSHHostSuff = ".hosts.telnyx.io"
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{cfg: cfg, runner: runner, log: logger, statuses: make(map[string]EnrollmentStatus)}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /v1/enroll", s.enroll)
	mux.HandleFunc("GET /v1/enrollments/{vmid}", s.getEnrollment)
	return mux
}

func (s *Service) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) enroll(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var req EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := validateRequest(req, s.cfg.AllowedSSHHostSuff); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	start := time.Now()
	s.setStatus(req.VMID, EnrollmentStatus{
		VMID:      req.VMID,
		Status:    "running",
		StartedAt: start.UTC().Format(time.RFC3339),
		UpdatedAt: start.UTC().Format(time.RFC3339),
	})

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.DefaultTimeout)
	defer cancel()

	target := SSHTarget{
		Host:    req.SSHHost,
		Port:    req.SSHPort,
		User:    req.SSHUser,
		KeyPath: s.cfg.SSHKeyPath,
		Timeout: s.cfg.DefaultTimeout,
	}
	features := normalizeFeatures(req.Features)
	if len(features) == 0 {
		features = []string{"host-identity", "authorized-keys", "lacework", "crowdstrike", "auditd", "fluent-bit"}
	}

	applied := make([]string, 0, len(features))
	skipped := make([]string, 0)
	for _, feature := range features {
		script, ok, err := s.scriptForFeature(req, feature)
		if err != nil {
			s.markFailed(req.VMID, start, applied, skipped, fmt.Sprintf("%s: %v", feature, err))
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("%s: %v", feature, err))
			return
		}
		if !ok {
			skipped = append(skipped, feature)
			continue
		}
		if out, err := s.runner.RunScript(ctx, target, script); err != nil {
			s.log.Error("enrollment feature failed", "vmid", req.VMID, "feature", feature, "err", err, "output", strings.TrimSpace(out))
			s.markFailed(req.VMID, start, applied, skipped, fmt.Sprintf("%s failed: %v", feature, err))
			writeError(w, http.StatusBadGateway, fmt.Sprintf("%s failed: %v", feature, err))
			return
		}
		applied = append(applied, feature)
		s.setStatus(req.VMID, EnrollmentStatus{
			VMID:      req.VMID,
			Status:    "running",
			Applied:   append([]string(nil), applied...),
			Skipped:   append([]string(nil), skipped...),
			StartedAt: start.UTC().Format(time.RFC3339),
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}

	marker, err := enrollmentMarker(req, applied, skipped)
	if err != nil {
		s.markFailed(req.VMID, start, applied, skipped, "build enrollment marker: "+err.Error())
		writeError(w, http.StatusInternalServerError, "build enrollment marker: "+err.Error())
		return
	}
	if out, err := s.runner.RunScript(ctx, target, writeFileScript("/var/lib/fastvm/enrollment.json", "0600", marker)); err != nil {
		s.log.Error("write enrollment marker failed", "vmid", req.VMID, "err", err, "output", strings.TrimSpace(out))
		s.markFailed(req.VMID, start, applied, skipped, "write enrollment marker failed: "+err.Error())
		writeError(w, http.StatusBadGateway, "write enrollment marker failed: "+err.Error())
		return
	}

	duration := time.Since(start).Round(time.Millisecond).String()
	s.setStatus(req.VMID, EnrollmentStatus{
		VMID:      req.VMID,
		Status:    "enrolled",
		Applied:   applied,
		Skipped:   skipped,
		StartedAt: start.UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Duration:  duration,
	})

	writeJSON(w, http.StatusOK, EnrollResponse{
		VMID:     req.VMID,
		Status:   "enrolled",
		Applied:  applied,
		Skipped:  skipped,
		Duration: duration,
	})
}

func (s *Service) getEnrollment(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	vmid := r.PathValue("vmid")
	status, ok := s.getStatus(vmid)
	if !ok {
		writeError(w, http.StatusNotFound, "enrollment not found")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Service) authorized(r *http.Request) bool {
	if s.cfg.AllowUnauth {
		return true
	}
	token := strings.TrimSpace(s.cfg.AuthToken)
	if token == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func validateRequest(req EnrollRequest, allowedSuffix string) error {
	if req.VMID == "" {
		return errors.New("vmid is required")
	}
	if req.SSHHost == "" {
		return errors.New("ssh_host is required")
	}
	if allowedSuffix != "" && !strings.HasSuffix(req.SSHHost, allowedSuffix) {
		return fmt.Errorf("ssh_host must end with %s", allowedSuffix)
	}
	if req.SSHPort < 1 || req.SSHPort > 65535 {
		return errors.New("ssh_port must be 1-65535")
	}
	if req.SSHUser == "" {
		return errors.New("ssh_user is required")
	}
	return nil
}

func (s *Service) scriptForFeature(req EnrollRequest, feature string) (string, bool, error) {
	switch feature {
	case "host-identity", "identity":
		return hostIdentityScript(), true, nil
	case "authorized-keys":
		keys, err := s.readSecret("authorized_keys")
		if errors.Is(err, os.ErrNotExist) || strings.TrimSpace(keys) == "" {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return appendAuthorizedKeysScript(keys, req.SSHUser), true, nil
	case "lacework":
		token, err := s.readSecretFile(s.cfg.LaceworkAccessTokenPath)
		if errors.Is(err, os.ErrNotExist) || strings.TrimSpace(token) == "" {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		cfg := map[string]any{"tokens": map[string]string{"accesstoken": strings.TrimSpace(token)}}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return "", false, err
		}
		return writeFileScript("/etc/lacework/config.json", "0600", string(data)) + enableServiceScript(s.cfg.EnableSystemctl, "lacework"), true, nil
	case "crowdstrike":
		cid, err := s.readSecretFile(s.cfg.CrowdStrikeCustomerIDPath)
		if errors.Is(err, os.ErrNotExist) || strings.TrimSpace(cid) == "" {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		tags := falconTags(req)
		return falconScript(strings.TrimSpace(cid), tags, s.cfg.EnableSystemctl), true, nil
	case "auditd":
		return enableServiceScript(s.cfg.EnableSystemctl, "auditd"), true, nil
	case "fluent-bit":
		return enableServiceScript(s.cfg.EnableSystemctl, "fluent-bit"), true, nil
	default:
		return "", false, fmt.Errorf("unknown feature %q", feature)
	}
}

func (s *Service) readSecret(name string) (string, error) {
	return s.readSecretFile(filepath.Join(s.cfg.SecretsDir, name))
}

func (s *Service) readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *Service) setStatus(vmid string, status EnrollmentStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[vmid] = status
}

func (s *Service) getStatus(vmid string) (EnrollmentStatus, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	status, ok := s.statuses[vmid]
	return status, ok
}

func (s *Service) markFailed(vmid string, start time.Time, applied, skipped []string, message string) {
	s.setStatus(vmid, EnrollmentStatus{
		VMID:      vmid,
		Status:    "failed",
		Applied:   append([]string(nil), applied...),
		Skipped:   append([]string(nil), skipped...),
		Error:     message,
		StartedAt: start.UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Duration:  time.Since(start).Round(time.Millisecond).String(),
	})
}

func (ExecRunner) RunScript(ctx context.Context, target SSHTarget, script string) (string, error) {
	if target.User == "" {
		target.User = "root"
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-p", strconv.Itoa(target.Port),
	}
	if target.KeyPath != "" {
		args = append(args, "-i", target.KeyPath)
	}
	args = append(args, fmt.Sprintf("%s@%s", target.User, target.Host))
	if target.User == "root" {
		args = append(args, "sh -s")
	} else {
		args = append(args, "sudo -n sh -s")
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func normalizeFeatures(features []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(features))
	for _, feature := range features {
		feature = strings.ToLower(strings.TrimSpace(feature))
		if feature == "" || seen[feature] {
			continue
		}
		seen[feature] = true
		out = append(out, feature)
	}
	return out
}

func hostIdentityScript() string {
	return `set -eu
if [ ! -f /var/lib/fastvm/enrollment.json ]; then
  install -d -m 0755 /var/lib/fastvm
  if command -v systemd-machine-id-setup >/dev/null 2>&1; then
    : > /etc/machine-id
    rm -f /var/lib/dbus/machine-id
    systemd-machine-id-setup >/dev/null 2>&1 || true
  fi
  if command -v ssh-keygen >/dev/null 2>&1; then
    rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub
    ssh-keygen -A >/dev/null 2>&1 || true
  fi
fi
`
}

func appendAuthorizedKeysScript(keys, user string) string {
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	return fmt.Sprintf(`set -eu
target_user=%s
home_dir="$(getent passwd "$target_user" | cut -d: -f6 || true)"
[ -n "$home_dir" ] || home_dir=/root
install -d -m 0700 "$home_dir/.ssh"
touch "$home_dir/.ssh/authorized_keys"
chmod 0600 "$home_dir/.ssh/authorized_keys"
tmp="$(mktemp)"
base64 -d > "$tmp" <<'FASTVM_SECRET'
%s
FASTVM_SECRET
while IFS= read -r key; do
  [ -n "$key" ] || continue
  grep -qxF "$key" "$home_dir/.ssh/authorized_keys" || printf '%%s\n' "$key" >> "$home_dir/.ssh/authorized_keys"
done < "$tmp"
chown -R "$target_user" "$home_dir/.ssh" || true
rm -f "$tmp"
`, shellQuote(user), b64(keys))
}

func writeFileScript(path, mode, contents string) string {
	dir := filepath.Dir(path)
	return fmt.Sprintf(`set -eu
install -d -m 0755 %s
base64 -d > %s <<'FASTVM_SECRET'
%s
FASTVM_SECRET
chmod %s %s
`, shellQuote(dir), shellQuote(path), b64(contents), shellQuote(mode), shellQuote(path))
}

func enableServiceScript(enabled bool, name string) string {
	if !enabled {
		return "\n"
	}
	q := shellQuote(name)
	return fmt.Sprintf(`
if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files %s.service --no-legend 2>/dev/null | grep -q .; then
  systemctl enable --now %s
fi
`, q, q)
}

func falconScript(cid string, tags []string, enableSystemctl bool) string {
	return fmt.Sprintf(`set -eu
if [ -x /opt/CrowdStrike/falconctl ]; then
  /opt/CrowdStrike/falconctl -sf --cid=%s --tags=%s
fi
%s`, shellQuote(cid), shellQuote(strings.Join(tags, ",")), enableServiceScript(enableSystemctl, "falcon-sensor"))
}

func falconTags(req EnrollRequest) []string {
	tags := map[string]string{
		"backend":  req.Backend,
		"node":     req.Node,
		"template": req.Template,
		"user":     req.User,
		"vmid":     req.VMID,
	}
	for k, v := range req.Tags {
		tags[k] = v
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if tags[k] == "" {
			continue
		}
		out = append(out, sanitizeTag(k)+"_"+sanitizeTag(tags[k]))
	}
	return out
}

func sanitizeTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func enrollmentMarker(req EnrollRequest, applied, skipped []string) (string, error) {
	data := map[string]any{
		"vmid":        req.VMID,
		"name":        req.Name,
		"user":        req.User,
		"backend":     req.Backend,
		"template":    req.Template,
		"node":        req.Node,
		"applied":     applied,
		"skipped":     skipped,
		"enrolled_at": time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(httpResponse{OK: true, Data: data})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(httpResponse{OK: false, Error: &msg})
}
