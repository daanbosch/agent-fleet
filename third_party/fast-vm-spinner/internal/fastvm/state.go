package fastvm

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// StateManager provides fast in-memory reads with file-backed persistence.
// All reads come from Go maps (zero I/O). Writes go to memory and flush to disk.
// A background goroutine reloads from disk periodically to pick up external changes.
type StateManager struct {
	dir string

	mu        sync.RWMutex
	vms       map[string]VMState
	users     map[string]UserState
	templates map[string]TemplateState
	claims    map[int]bool // in-flight VMID reservations for concurrent launches

	lastVMsMod       time.Time
	lastUsersMod     time.Time
	lastTemplatesMod time.Time
}

// VMState represents a single VM entry from vms.json.
type VMState struct {
	User             string `json:"user"`
	Name             string `json:"name"`
	Template         int    `json:"template"`
	TemplateName     string `json:"template_name,omitempty"`
	Node             string `json:"node"`
	Memory           int    `json:"memory,omitempty"`
	Cores            int    `json:"cores,omitempty"`
	IP               string `json:"ip,omitempty"`
	Backend          string `json:"backend,omitempty"`
	SSHUser          string `json:"ssh_user,omitempty"`
	SecurityTemplate bool   `json:"security_template,omitempty"`
	WarmTemplate     string `json:"warm_template,omitempty"`
	Created          string `json:"created"`
	// Paused marks a VMM-paused guest (memory resident, no CPU).
	Paused bool `json:"paused,omitempty"`
	// ExpiresAt (RFC3339) drives the TTL sweeper: VMs past it are destroyed.
	ExpiresAt string `json:"expires_at,omitempty"`
	// ExposedPorts maps guest port -> host port (socat forwards).
	ExposedPorts map[int]int `json:"exposed_ports,omitempty"`
}

// UserState represents a user entry from users.json.
type UserState struct {
	Block   int    `json:"block"`
	Created string `json:"created"`
}

// TemplateState records user-created templates that FastVM can safely expose
// through the existing numeric -t/--template flow.
type TemplateState struct {
	User               string `json:"user,omitempty"`
	SourceVMID         string `json:"source_vmid,omitempty"`
	SourceTemplate     int    `json:"source_template,omitempty"`
	SourceTemplateName string `json:"source_template_name,omitempty"`
	Backend            string `json:"backend,omitempty"`
	SecurityTemplate   bool   `json:"security_template,omitempty"`
	Created            string `json:"created"`
}

// StatusInfo is the aggregated status response.
type StatusInfo struct {
	TotalVMs  int                    `json:"total_vms"`
	Users     int                    `json:"users"`
	VMIDRange string                 `json:"vmid_range"`
	Backend   string                 `json:"backend,omitempty"`
	PerUser   map[string]UserSummary `json:"per_user"`
}

// UserSummary is per-user VM summary in status.
type UserSummary struct {
	Count   int    `json:"count"`
	Network string `json:"network"`
	Block   int    `json:"block"`
}

const (
	vmidMin = 60000
	vmidMax = 60999
)

// NewStateManager creates a state manager and loads initial state from disk.
// It starts a background goroutine that reloads when files change.
func NewStateManager(dir string) *StateManager {
	s := &StateManager{
		dir:       dir,
		vms:       make(map[string]VMState),
		claims:    make(map[int]bool),
		users:     make(map[string]UserState),
		templates: make(map[string]TemplateState),
	}

	// Ensure state directory exists
	os.MkdirAll(dir, 0755)

	// Create files if they don't exist
	vmsFile := filepath.Join(dir, "vms.json")
	usersFile := filepath.Join(dir, "users.json")
	templatesFile := filepath.Join(dir, "templates.json")
	if _, err := os.Stat(vmsFile); os.IsNotExist(err) {
		os.WriteFile(vmsFile, []byte("{}"), 0644)
	}
	if _, err := os.Stat(usersFile); os.IsNotExist(err) {
		os.WriteFile(usersFile, []byte("{}"), 0644)
	}
	if _, err := os.Stat(templatesFile); os.IsNotExist(err) {
		os.WriteFile(templatesFile, []byte("{}"), 0644)
	}

	// Initial load
	s.reload()

	// Background reload: check file mtime every 500ms, reload if changed.
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			s.reloadIfChanged()
		}
	}()

	return s
}

// reload reads both state files from disk into memory.
func (s *StateManager) reload() {
	vms, vmsMod := loadJSON[map[string]VMState](filepath.Join(s.dir, "vms.json"))
	users, usersMod := loadJSON[map[string]UserState](filepath.Join(s.dir, "users.json"))
	templates, templatesMod := loadJSON[map[string]TemplateState](filepath.Join(s.dir, "templates.json"))

	s.mu.Lock()
	defer s.mu.Unlock()

	if vms != nil {
		s.vms = *vms
		s.lastVMsMod = vmsMod
	}
	if users != nil {
		s.users = *users
		s.lastUsersMod = usersMod
	}
	if templates != nil {
		s.templates = *templates
		s.lastTemplatesMod = templatesMod
	}
}

// reloadIfChanged checks file mtimes and reloads only if changed.
func (s *StateManager) reloadIfChanged() {
	vmsPath := filepath.Join(s.dir, "vms.json")
	usersPath := filepath.Join(s.dir, "users.json")
	templatesPath := filepath.Join(s.dir, "templates.json")

	s.mu.RLock()
	lastVMs := s.lastVMsMod
	lastUsers := s.lastUsersMod
	lastTemplates := s.lastTemplatesMod
	s.mu.RUnlock()

	vmsChanged := fileModifiedSince(vmsPath, lastVMs)
	usersChanged := fileModifiedSince(usersPath, lastUsers)
	templatesChanged := fileModifiedSince(templatesPath, lastTemplates)

	if !vmsChanged && !usersChanged && !templatesChanged {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if vmsChanged {
		if vms, mod := loadJSON[map[string]VMState](vmsPath); vms != nil {
			s.vms = *vms
			s.lastVMsMod = mod
		}
	}
	if usersChanged {
		if users, mod := loadJSON[map[string]UserState](usersPath); users != nil {
			s.users = *users
			s.lastUsersMod = mod
		}
	}
	if templatesChanged {
		if templates, mod := loadJSON[map[string]TemplateState](templatesPath); templates != nil {
			s.templates = *templates
			s.lastTemplatesMod = mod
		}
	}
}

// --- Read operations (from memory) ---

// ReadVMs returns a copy of the current VM state.
func (s *StateManager) ReadVMs() (map[string]VMState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cp := make(map[string]VMState, len(s.vms))
	for k, v := range s.vms {
		cp[k] = v
	}
	return cp, nil
}

// ReadUsers returns a copy of the current user state.
func (s *StateManager) ReadUsers() (map[string]UserState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cp := make(map[string]UserState, len(s.users))
	for k, v := range s.users {
		cp[k] = v
	}
	return cp, nil
}

// ReadTemplates returns a copy of the current user-created template registry.
func (s *StateManager) ReadTemplates() (map[string]TemplateState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cp := make(map[string]TemplateState, len(s.templates))
	for k, v := range s.templates {
		cp[k] = v
	}
	return cp, nil
}

// ReadStatus builds the overall status from in-memory state.
func (s *StateManager) ReadStatus() (*StatusInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	perUser := make(map[string]UserSummary)
	for _, vm := range s.vms {
		su := perUser[vm.User]
		su.Count++
		if userInfo, ok := s.users[vm.User]; ok {
			su.Block = userInfo.Block
			octet3 := userInfo.Block / 4
			octet4Base := (userInfo.Block % 4) * blockSize
			su.Network = fmt.Sprintf("192.168.%d.%d/26", octet3, octet4Base)
		}
		perUser[vm.User] = su
	}

	return &StatusInfo{
		TotalVMs:  len(s.vms),
		Users:     len(s.users),
		VMIDRange: fmt.Sprintf("%d-%d", vmidMin, vmidMax),
		PerUser:   perUser,
	}, nil
}

// GetVM returns a single VM's state.
func (s *StateManager) GetVM(vmid string) (*VMState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return nil, false
	}
	return &vm, true
}

// GetUser returns a single user's state.
func (s *StateManager) GetUser(user string) (*UserState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[user]
	if !ok {
		return nil, false
	}
	return &u, true
}

// GetTemplate returns a single registered template.
func (s *StateManager) GetTemplate(template string) (*TemplateState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tpl, ok := s.templates[template]
	if !ok {
		return nil, false
	}
	return &tpl, true
}

// UserVMIDs returns all VMID strings belonging to a user.
func (s *StateManager) UserVMIDs(user string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ids []string
	for vmid, vm := range s.vms {
		if vm.User == user {
			ids = append(ids, vmid)
		}
	}
	sort.Strings(ids)
	return ids
}

// UserHasVMs returns true if the user has any VMs registered.
func (s *StateManager) UserHasVMs(user string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, vm := range s.vms {
		if vm.User == user {
			return true
		}
	}
	return false
}

// --- Write operations (memory + flush to disk) ---

// AllocateBlock assigns a /26 block to a user. If the user already has a block,
// returns the existing one. Block is derived by hashing the username (1-1023)
// with linear probing for collisions.
func (s *StateManager) AllocateBlock(user string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if already assigned
	if u, ok := s.users[user]; ok {
		return u.Block, nil
	}

	// Hash username to block 1-1023
	h := md5.Sum([]byte(user))
	hash := fmt.Sprintf("%x", h[:3])
	num64, _ := strconv.ParseInt(hash, 16, 64)
	block := int(num64%1023) + 1

	// Build set of taken blocks
	taken := make(map[int]bool)
	for _, u := range s.users {
		taken[u.Block] = true
	}

	// Linear probe for collisions
	for taken[block] {
		block = (block % 1023) + 1
	}

	// Store
	s.users[user] = UserState{
		Block:   block,
		Created: time.Now().UTC().Format(time.RFC3339),
	}

	if err := s.saveUsersLocked(); err != nil {
		return 0, fmt.Errorf("save users: %w", err)
	}

	slog.Info("allocated block", "user", user, "block", block)
	return block, nil
}

// NextVMID finds the next available VMID in the range 60000-60999.
// It checks both in-memory state AND Proxmox (via qm list) to avoid
// collisions with templates or VMs created outside fastvm.
func (s *StateManager) NextVMID() (int, error) {
	reserved := make(map[int]bool)
	for _, id := range listProxmoxVMIDs() {
		reserved[id] = true
	}
	return s.NextVMIDExcluding(reserved)
}

// ClaimVMID finds the next available VMID (like NextVMIDExcluding) and
// atomically claims it, so concurrent flows (warm launches fire N at once)
// cannot both take the same slot between allocation and RegisterVM. Release
// with ReleaseVMID; RegisterVM clears the claim. Claims live in memory only —
// they vanish on restart, so a crash mid-flow frees the slot.
func (s *StateManager) ClaimVMID(reserved map[int]bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := vmidMin; id <= vmidMax; id++ {
		if _, used := s.vms[strconv.Itoa(id)]; !used && !reserved[id] && !s.claims[id] {
			s.claims[id] = true
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free VMIDs in range %d-%d", vmidMin, vmidMax)
}

// ReleaseVMID frees a VMID previously taken with ClaimVMID.
func (s *StateManager) ReleaseVMID(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claims, id)
}

// NextVMIDExcluding finds the next available VMID while respecting additional
// backend-specific reservations such as Firecracker numeric template names.
func (s *StateManager) NextVMIDExcluding(reserved map[int]bool) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	used := make(map[int]bool, len(s.vms))
	for k := range s.vms {
		if id, err := strconv.Atoi(k); err == nil {
			used[id] = true
		}
	}

	for id := range reserved {
		used[id] = true
	}

	for id := vmidMin; id <= vmidMax; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free VMIDs in range %d-%d", vmidMin, vmidMax)
}

// NameExists checks if a VM name is already in use.
func (s *StateManager) NameExists(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, vm := range s.vms {
		if vm.Name == name {
			return true
		}
	}
	return false
}

// FindVMByName searches for a VM by name and returns its VMID, state, and whether it was found.
func (s *StateManager) FindVMByName(name string) (string, *VMState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for vmid, vm := range s.vms {
		if vm.Name == name {
			v := vm // copy
			return vmid, &v, true
		}
	}
	return "", nil, false
}

// RegisterVM adds a VM to state and flushes to disk.
func (s *StateManager) RegisterVM(vmid string, info VMState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// the VM is real now — any in-flight ClaimVMID for this slot is resolved
	delete(s.claims, parseIntSafe(vmid))
	s.vms[vmid] = info
	return s.saveVMsLocked()
}

// parseIntSafe is a tolerant Atoi for map cleanup paths.
func parseIntSafe(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

// SetVMPaused flips the paused flag in place and flushes.
func (s *StateManager) SetVMPaused(vmid string, paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	vm.Paused = paused
	s.vms[vmid] = vm
	return s.saveVMsLocked()
}

// SetVMExposedPort records a guest->host exposure; hostPort 0 removes the
// entry for that guest port.
func (s *StateManager) SetVMExposedPort(vmid string, guestPort, hostPort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	if vm.ExposedPorts == nil {
		vm.ExposedPorts = map[int]int{}
	}
	if hostPort == 0 {
		delete(vm.ExposedPorts, guestPort)
	} else {
		vm.ExposedPorts[guestPort] = hostPort
	}
	if len(vm.ExposedPorts) == 0 {
		vm.ExposedPorts = nil
	}
	s.vms[vmid] = vm
	return s.saveVMsLocked()
}

// ClearVMExposedPorts removes every exposure entry of a VM.
func (s *StateManager) ClearVMExposedPorts(vmid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return fmt.Errorf("VM %s not found", vmid)
	}
	vm.ExposedPorts = nil
	s.vms[vmid] = vm
	return s.saveVMsLocked()
}

// RegisterTemplate adds a user-created template to state and flushes to disk.
func (s *StateManager) RegisterTemplate(template string, info TemplateState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.templates[template] = info
	return s.saveTemplatesLocked()
}

// UnregisterVM removes a VM from state and flushes to disk.
func (s *StateManager) UnregisterVM(vmid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.vms, vmid)
	return s.saveVMsLocked()
}

// RemoveUser removes a user from the users state and flushes to disk.
func (s *StateManager) RemoveUser(user string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.users, user)
	return s.saveUsersLocked()
}

// --- Atomic file writes ---

// saveVMsLocked writes the VMs map to vms.json atomically. Must hold s.mu.
func (s *StateManager) saveVMsLocked() error {
	return atomicWriteJSON(filepath.Join(s.dir, "vms.json"), s.vms)
}

// saveUsersLocked writes the users map to users.json atomically. Must hold s.mu.
func (s *StateManager) saveUsersLocked() error {
	return atomicWriteJSON(filepath.Join(s.dir, "users.json"), s.users)
}

// saveTemplatesLocked writes the templates map to templates.json atomically. Must hold s.mu.
func (s *StateManager) saveTemplatesLocked() error {
	return atomicWriteJSON(filepath.Join(s.dir, "templates.json"), s.templates)
}

// atomicWriteJSON marshals data to JSON and writes atomically via temp file + rename.
func atomicWriteJSON(path string, data any) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return atomicWrite(path, b)
}

// atomicWrite writes bytes atomically via temp file + rename.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// --- helpers ---

// loadJSON reads and unmarshals a JSON file. Returns nil on any error.
func loadJSON[T any](path string) (*T, time.Time) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, time.Time{}
	}

	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, time.Time{}
	}

	return &result, info.ModTime()
}

// fileModifiedSince returns true if the file's mtime is after the given time.
func fileModifiedSince(path string, since time.Time) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.ModTime().After(since)
}
