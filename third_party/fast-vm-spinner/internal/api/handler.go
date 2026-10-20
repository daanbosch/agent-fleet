package api

import (
	"encoding/json"
	"fmt"
	"github.com/team-telnyx/fast-vm-spinner/internal/fastvm"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Handler holds dependencies for HTTP handlers.
type Handler struct {
	engine     fastvm.Backend
	logger     *slog.Logger
	version    string
	enrollment EnrollmentConfig
}

// --- Response helpers ---

type Response struct {
	OK    bool        `json:"ok"`
	Data  interface{} `json:"data"`
	Error *string     `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{OK: true, Data: data})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{OK: false, Error: &msg})
}

// --- Health & Status ---

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": h.version})
}

func (h *Handler) Version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": h.version})
}

func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	status, err := h.engine.Status()
	if err != nil {
		h.logger.Error("failed to read status", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read state: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// --- VM CRUD ---

type CreateRequest struct {
	User          string `json:"user"`
	Count         int    `json:"count"`
	Template      int    `json:"template,omitempty"`
	Memory        int    `json:"memory,omitempty"`
	Cores         int    `json:"cores,omitempty"`
	SSHKey        string `json:"ssh_key,omitempty"`         // Path on Proxmox host
	SSHKeyContent string `json:"ssh_key_content,omitempty"` // Actual pubkey content (preferred)
	Name          string `json:"name,omitempty"`            // Custom VM name (optional)
	TTLMinutes    int    `json:"ttl_minutes,omitempty"`     // Auto-destroy after N minutes
}

func (h *Handler) CreateVMs(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Validate
	if req.User == "" {
		writeError(w, http.StatusBadRequest, "user is required")
		return
	}
	if req.Count < 1 || req.Count > 50 {
		writeError(w, http.StatusBadRequest, "count must be 1-50")
		return
	}
	if !isValidUsername(req.User) {
		writeError(w, http.StatusBadRequest, "user must be alphanumeric (dashes/underscores allowed)")
		return
	}

	h.logger.Info("creating VMs", "user", req.User, "count", req.Count)

	opts := fastvm.CreateOpts{
		Template:      req.Template,
		Memory:        req.Memory,
		Cores:         req.Cores,
		SSHKey:        req.SSHKey,
		SSHKeyContent: req.SSHKeyContent,
		Name:          req.Name,
		TTLMinutes:    req.TTLMinutes,
	}

	vms, err := h.engine.CreateVMs(req.User, req.Count, opts)
	if err != nil {
		h.logger.Error("create VMs failed", "err", err)
		if fastvm.IsInvalidRequest(err) {
			writeError(w, http.StatusBadRequest, "create failed: "+err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "create failed: "+err.Error())
		return
	}

	resp := map[string]interface{}{
		"vms":     vms,
		"count":   len(vms),
		"message": fmt.Sprintf("Created %d VM(s) for user '%s'", len(vms), req.User),
	}
	enrollmentSummary, err := h.enrollCreatedVMs(r.Context(), vms)
	if err != nil {
		h.logger.Error("VM enrollment failed", "err", err)
		writeError(w, http.StatusBadGateway, "enrollment failed: "+err.Error())
		return
	}
	if enrollmentSummary != nil {
		resp["enrollment"] = enrollmentSummary
	}

	writeJSON(w, http.StatusCreated, resp)
}

func (h *Handler) ListVMs(w http.ResponseWriter, r *http.Request) {
	userFilter := r.URL.Query().Get("user")

	vms, err := h.engine.ListVMs(userFilter)
	if err != nil {
		h.logger.Error("failed to list VMs", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list VMs: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"vms":   vms,
		"count": len(vms),
	})
}

func (h *Handler) GetVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	if vmid == "" {
		writeError(w, http.StatusBadRequest, "vmid is required")
		return
	}

	vm, err := h.engine.GetVM(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, vm)
}

func (h *Handler) DestroyVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	if vmid == "" {
		writeError(w, http.StatusBadRequest, "vmid is required")
		return
	}

	h.logger.Info("destroying VM", "vmid", vmid)

	if err := h.engine.DestroyVM(vmid); err != nil {
		h.logger.Error("destroy VM failed", "vmid", vmid, "err", err)
		writeError(w, http.StatusInternalServerError, "destroy failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": fmt.Sprintf("VM %s destroy queued", vmid),
	})
}

func (h *Handler) DestroyUserVMs(w http.ResponseWriter, r *http.Request) {
	user := r.PathValue("user")
	if user == "" {
		writeError(w, http.StatusBadRequest, "user is required")
		return
	}

	h.logger.Info("destroying all VMs for user", "user", user)

	if err := h.engine.DestroyUserVMs(user); err != nil {
		h.logger.Error("destroy user VMs failed", "user", user, "err", err)
		writeError(w, http.StatusInternalServerError, "destroy failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": fmt.Sprintf("Destroy queued for all VMs for user '%s'", user),
	})
}

// --- Converge ---

type ConvergeRequest struct {
	User     string `json:"user"`
	KeepVMID string `json:"keep_vmid"`
}

func (h *Handler) Converge(w http.ResponseWriter, r *http.Request) {
	var req ConvergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if req.User == "" {
		writeError(w, http.StatusBadRequest, "user is required")
		return
	}
	if req.KeepVMID == "" {
		writeError(w, http.StatusBadRequest, "keep_vmid is required")
		return
	}

	h.logger.Info("converging", "user", req.User, "keep_vmid", req.KeepVMID)

	if err := h.engine.Converge(req.User, req.KeepVMID); err != nil {
		h.logger.Error("converge failed", "err", err)
		writeError(w, http.StatusInternalServerError, "converge failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"message": fmt.Sprintf("Converge queued for user '%s', keeping VMID %s", req.User, req.KeepVMID),
		"kept":    req.KeepVMID,
	})
}

// --- Snapshot ---

func (h *Handler) SnapshotVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	if vmid == "" {
		writeError(w, http.StatusBadRequest, "vmid is required")
		return
	}

	h.logger.Info("snapshotting VM to template", "vmid", vmid)

	if err := h.engine.SnapshotVM(vmid); err != nil {
		h.logger.Error("snapshot VM failed", "vmid", vmid, "err", err)
		writeError(w, http.StatusInternalServerError, "snapshot failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"vmid":    vmid,
		"message": fmt.Sprintf("VM %s converted to template. Use -t %s to clone from it.", vmid, vmid),
	})
}

// --- VM by name ---

func (h *Handler) GetVMByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	vm, err := h.engine.GetVMByName(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, vm)
}

// ResolveVM accepts a name or VMID in the path and returns VM info.
func (h *Handler) ResolveVM(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if ref == "" {
		writeError(w, http.StatusBadRequest, "ref (name or vmid) is required")
		return
	}

	vmid, err := h.engine.ResolveVMID(ref)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	vm, err := h.engine.GetVM(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, vm)
}

// --- Helpers ---

func isValidUsername(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// --- Warm snapshot templates ---

// WarmSnapshotRequest names the target warm template.
type WarmACLRequest struct {
	User    string   `json:"user"`
	Public  bool     `json:"public"`
	Readers []string `json:"readers"`
}

type WarmSnapshotRequest struct {
	Name string `json:"name"`
}

// WarmLaunchRequest restores a warm-template clone.
type WarmLaunchRequest struct {
	Template string `json:"template"`
	User     string `json:"user"`
	Name     string `json:"name"`
	SSHKey   string `json:"ssh_key,omitempty"` // public key appended to root's authorized_keys post-restore
}

// WarmSnapshot converts a running VM into a memory-preserving warm template.
func (h *Handler) WarmSnapshot(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	if vmid == "" {
		writeError(w, http.StatusBadRequest, "vmid is required")
		return
	}
	var req WarmSnapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	h.logger.Info("creating warm snapshot", "vmid", vmid, "warm_template", req.Name)
	meta, err := h.engine.WarmSnapshot(vmid, req.Name)
	if err != nil {
		h.logger.Error("warm snapshot failed", "vmid", vmid, "err", err)
		writeError(w, http.StatusInternalServerError, "warm snapshot failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"vmid":     vmid,
		"message":  fmt.Sprintf("VM %s frozen as warm template '%s'. No boot on restore — clones in ~0.5s, N concurrently. Launch with: fastvm warm-launch -t %s", vmid, req.Name, req.Name),
		"template": meta,
	})
}

// WarmLaunch restores a clone from a warm template.
func (h *Handler) WarmLaunch(w http.ResponseWriter, r *http.Request) {
	var req WarmLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Template == "" {
		writeError(w, http.StatusBadRequest, "template is required")
		return
	}
	if req.User == "" {
		writeError(w, http.StatusBadRequest, "user is required")
		return
	}
	if !isValidUsername(req.User) {
		writeError(w, http.StatusBadRequest, "user must be alphanumeric (dashes/underscores allowed)")
		return
	}
	h.logger.Info("restoring warm clone", "user", req.User, "warm_template", req.Template)
	vm, err := h.engine.WarmLaunch(req.User, req.Template, req.Name, req.SSHKey)
	if err != nil {
		h.logger.Error("warm launch failed", "warm_template", req.Template, "err", err)
		writeError(w, http.StatusInternalServerError, "warm launch failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"vms":     []fastvm.VMInfo{vm},
		"count":   1,
		"message": fmt.Sprintf("Warm-restored 1 VM from template '%s' for user '%s'", req.Template, req.User),
	})
}

// WarmTemplates lists available warm templates.
func (h *Handler) WarmTemplates(w http.ResponseWriter, r *http.Request) {
	user := r.URL.Query().Get("user")
	templates, err := h.engine.ListWarmTemplatesForUser(user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list warm templates failed: "+err.Error())
		return
	}
	if templates == nil {
		templates = []fastvm.WarmTemplate{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"templates": templates,
		"count":     len(templates),
	})
}

// Profiles returns recent stage profiles for latency triage.
//
// Query params: limit (default 20, max 128), event (filter, e.g.
// warm_launch), vmid (filter).
func (h *Handler) Profiles(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	event := r.URL.Query().Get("event")
	vmidFilter := r.URL.Query().Get("vmid")

	items := []any{}
	for _, p := range fastvm.RecentProfiles() {
		if len(items) >= limit {
			break
		}
		if event != "" && p.Event != event {
			continue
		}
		if vmidFilter != "" && p.VMID != vmidFilter {
			continue
		}
		items = append(items, p)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count":    len(items),
		"profiles": items,
	})
}

// --- pause / resume / exec / expose (sandbox parity operations) ---

func (h *Handler) PauseVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	resolved, err := h.engine.ResolveVMID(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := h.engine.PauseVM(resolved); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vmid": resolved, "status": "paused"})
}

func (h *Handler) ResumeVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	resolved, err := h.engine.ResolveVMID(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := h.engine.ResumeVM(resolved); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vmid": resolved, "status": "resumed"})
}

type ExecRequest struct {
	Command  string `json:"command"`
	TimeoutS int    `json:"timeout_s,omitempty"`
}

func (h *Handler) ExecVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	resolved, err := h.engine.ResolveVMID(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Command == "" {
		writeError(w, http.StatusBadRequest, "command required")
		return
	}
	res, err := h.engine.ExecVM(resolved, req.Command, req.TimeoutS)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type ExposeRequest struct {
	GuestPort int `json:"guest_port"`
}

func (h *Handler) ExposeVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	resolved, err := h.engine.ResolveVMID(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var req ExposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.GuestPort <= 0 {
		writeError(w, http.StatusBadRequest, "guest_port required")
		return
	}
	info, err := h.engine.ExposePort(resolved, req.GuestPort)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type UnexposeRequest struct {
	HostPort int `json:"host_port"`
}

func (h *Handler) UnexposeVM(w http.ResponseWriter, r *http.Request) {
	vmid := r.PathValue("vmid")
	resolved, err := h.engine.ResolveVMID(vmid)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var req UnexposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.HostPort <= 0 {
		writeError(w, http.StatusBadRequest, "host_port required")
		return
	}
	if err := h.engine.UnexposePort(resolved, req.HostPort); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vmid": resolved, "host_port": req.HostPort, "status": "unexposed"})
}

// StartTTLSweeper destroys VMs whose ExpiresAt has passed, once per interval.
func (h *Handler) StartTTLSweeper(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			h.sweepTTL()
		}
	}()
}

func (h *Handler) sweepTTL() {
	vms, err := h.engine.ListVMs("")
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, vm := range vms {
		if vm.ExpiresAt == "" {
			continue
		}
		exp, err := time.Parse(time.RFC3339, vm.ExpiresAt)
		if err != nil {
			continue
		}
		if now.After(exp) {
			h.logger.Info("TTL expired, destroying VM", "vmid", vm.VMID, "user", vm.User)
			if err := h.engine.DestroyVM(vm.VMID); err != nil {
				h.logger.Warn("TTL destroy failed", "vmid", vm.VMID, "err", err)
			}
		}
	}
}


func (h *Handler) WarmTemplateACL(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	var req WarmACLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.User == "" {
		writeError(w, http.StatusBadRequest, "user is required (whose identity you act as)")
		return
	}
	meta, err := h.engine.UpdateWarmTemplateACL(req.User, name, req.Public, req.Readers)
	if err != nil {
		writeError(w, http.StatusForbidden, "acl update failed: "+err.Error())
		return
	}
	vis := "readable by everyone"
	if !req.Public {
		vis = fmt.Sprintf("restricted to owner %q and readers %v", req.User, req.Readers)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"template": meta,
		"message":  fmt.Sprintf("warm template %q: %s", name, vis),
	})
}
