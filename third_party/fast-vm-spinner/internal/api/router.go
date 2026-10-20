package api

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/team-telnyx/fast-vm-spinner/internal/fastvm"
)

// NewRouter builds the HTTP mux with all API routes.
func NewRouter(engine fastvm.Backend, logger *slog.Logger, version string) http.Handler {
	return NewRouterWithEnrollment(engine, logger, version, EnrollmentConfig{})
}

// NewRouterWithEnrollment builds the HTTP mux with optional post-create VM enrollment.
func NewRouterWithEnrollment(engine fastvm.Backend, logger *slog.Logger, version string, enrollment EnrollmentConfig) http.Handler {
	h := &Handler{
		engine:     engine,
		logger:     logger,
		version:    version,
		enrollment: normalizeEnrollmentConfig(enrollment),
	}

	mux := http.NewServeMux()

	// Health, version & status
	mux.HandleFunc("GET /api/v1/health", h.Health)
	mux.HandleFunc("GET /api/v1/version", h.Version)
	mux.HandleFunc("GET /api/v1/status", h.Status)

	// VMs
	mux.HandleFunc("POST /api/v1/vms", h.CreateVMs)
	mux.HandleFunc("GET /api/v1/vms", h.ListVMs)
	mux.HandleFunc("GET /api/v1/vms/by-name/{name}", h.GetVMByName)
	mux.HandleFunc("GET /api/v1/vms/resolve/{ref}", h.ResolveVM)
	mux.HandleFunc("GET /api/v1/vms/{vmid}", h.GetVM)
	mux.HandleFunc("DELETE /api/v1/vms/{vmid}", h.DestroyVM)

	// User operations
	mux.HandleFunc("DELETE /api/v1/users/{user}/vms", h.DestroyUserVMs)

	// Snapshot (convert VM to template)
	mux.HandleFunc("POST /api/v1/vms/{vmid}/snapshot", h.SnapshotVM)
	mux.HandleFunc("POST /api/v1/vms/{vmid}/warmsnap", h.WarmSnapshot)
	mux.HandleFunc("POST /api/v1/warm-launch", h.WarmLaunch)
	mux.HandleFunc("GET /api/v1/warm-templates", h.WarmTemplates)
	mux.HandleFunc("POST /api/v1/warm-templates/{name}/acl", h.WarmTemplateACL)

	// Converge
	mux.HandleFunc("POST /api/v1/converge", h.Converge)

	// sandbox-parity guest operations
	mux.HandleFunc("POST /api/v1/vms/{vmid}/pause", h.PauseVM)
	mux.HandleFunc("POST /api/v1/vms/{vmid}/resume", h.ResumeVM)
	mux.HandleFunc("POST /api/v1/vms/{vmid}/exec", h.ExecVM)
	mux.HandleFunc("POST /api/v1/vms/{vmid}/expose", h.ExposeVM)
	mux.HandleFunc("POST /api/v1/vms/{vmid}/unexpose", h.UnexposeVM)

	// TTL expiry loop: destroy VMs past their ExpiresAt once a minute
	h.StartTTLSweeper(time.Minute)

	// Profiling telemetry (per-stage latency breakdowns of the last flows)
	mux.HandleFunc("GET /api/v1/profile", h.Profiles)

	// Self-update & rollback
	mux.HandleFunc("POST /api/v1/update", h.SelfUpdate)
	mux.HandleFunc("POST /api/v1/rollback", h.Rollback)

	// FASTVM_PPROF=1 mounts Go's net/http/pprof endpoints for deep daemon
	// profiling (CPU/heap/block/goroutine) — off by default in production.
	if strings.TrimSpace(os.Getenv("FASTVM_PPROF")) == "1" {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		logger.Info("pprof endpoints enabled at /debug/pprof")
	}

	// Wrap with logging middleware
	return loggingMiddleware(logger, mux)
}

func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: 200}

		next.ServeHTTP(rw, r)

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", clientIP(r),
		)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.Split(xff, ",")[0]
	}
	return r.RemoteAddr
}
