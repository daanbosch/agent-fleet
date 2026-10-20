// Package fastvm is the official Go SDK for the fastvm REST API
// (e.g. http://10.242.128.198:8090 on the atl1 lab node).
//
// It is deliberately a thin, stdlib-only client: every call maps 1:1 onto a
// REST route, errors surface verbatim, and forward-compatible option fields
// (TTL on warm launch, per-VM egress allowlist, fork) are already part of the
// request bodies. Servers before the feat/agent-substrate line ignore the
// unknown fields silently; servers on it enforce them server-side.
package fastvm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one fastvm API server as one user.
type Client struct {
	BaseURL string
	User    string
	HTTP    *http.Client
}

func New(baseURL, user string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		User:    user,
		HTTP:    &http.Client{Timeout: 180 * time.Second},
	}
}

type apiResponse struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *string         `json:"error"`
}

func (c *Client) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var ar apiResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return fmt.Errorf("fastvm %s %s: bad JSON from HTTP %d: %w", method, path, resp.StatusCode, err)
	}
	if resp.StatusCode >= 400 || !ar.OK {
		msg := http.StatusText(resp.StatusCode)
		if ar.Error != nil {
			msg = *ar.Error
		}
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(ar.Data, out)
}

// APIError is a non-OK fastvm API response.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("fastvm %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports a not-found / unknown-VM style API error.
func (e *APIError) IsNotFound() bool {
	return strings.Contains(strings.ToLower(e.Message), "not found") ||
		strings.Contains(strings.ToLower(e.Message), "unknown vm")
}

// VMInfo mirrors the API's VM record.
type VMInfo struct {
	VMID         string         `json:"vmid"`
	Name         string         `json:"name"`
	User         string         `json:"user"`
	TemplateName string         `json:"template_name,omitempty"`
	IP           string         `json:"ip,omitempty"`
	Created      string         `json:"created"`
	SSHPort      int            `json:"ssh_port"`
	Paused       bool           `json:"paused,omitempty"`
	ExpiresAt    string         `json:"expires_at,omitempty"`
	ExposedPorts map[string]int `json:"exposed_ports,omitempty"` // daemon shape: guest->host
}

// ExecResult is the outcome of a guest command.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// LaunchOptions describe a warm clone launch. TTLMinutes and Egress are
// forward-compatible: enforced on the feat/agent-substrate server line,
// carried (and ignored) by older daemons.
type LaunchOptions struct {
	Template   string   // warm template name (default warm-ubuntu-dev)
	Name       string   // VM name; empty -> AgpocAutoName()
	SSHKey     string   // public key appended in-guest (warm path: async kick)
	TTLMinutes int      // forward-compat: server-side sweeper expiry
	Egress     []string // forward-compat: per-VM egress allowlist entries (host/CIDR)
}

// NamePrefix scopes VMs created by SDK/MCP callers so cleanup tooling can
// never touch another tenant's VMs.
const NamePrefix = "agpoc-"

// AgpocAutoName builds the default leak-guard-scoped VM name.
func AgpocAutoName() string {
	return NamePrefix + time.Now().UTC().Format("20060102T150405") + fmt.Sprintf("-%d", time.Now().UnixNano()%1e6)
}

// WarmLaunch restores a clone from a warm template.
func (c *Client) WarmLaunch(opts LaunchOptions) (VMInfo, error) {
	if opts.Name == "" {
		opts.Name = AgpocAutoName()
	}
	if opts.Template == "" {
		opts.Template = "warm-ubuntu-dev"
	}
	body := map[string]any{
		"template": opts.Template,
		"user":     c.User,
		"name":     opts.Name,
	}
	if opts.SSHKey != "" {
		body["ssh_key"] = opts.SSHKey
	}
	if opts.TTLMinutes > 0 {
		body["ttl_minutes"] = opts.TTLMinutes
	}
	if len(opts.Egress) > 0 {
		body["egress"] = opts.Egress
	}
	var data struct {
		VMs []VMInfo `json:"vms"`
	}
	if err := c.do("POST", "/api/v1/warm-launch", body, &data); err != nil {
		return VMInfo{}, err
	}
	if len(data.VMs) != 1 {
		return VMInfo{}, fmt.Errorf("warm-launch: expected 1 VM, got %d", len(data.VMs))
	}
	return data.VMs[0], nil
}

// ForkRequest clones a live VM (feat/agent-substrate server line; older
// daemons return 404/not-supported).
type ForkRequest struct {
	VM         string   `json:"vm"`
	Count      int      `json:"count,omitempty"`
	Name       string   `json:"name,omitempty"`
	TTLMinutes int      `json:"ttl_minutes,omitempty"`
	Egress     []string `json:"egress,omitempty"`
}

// Fork requests N live forks of a VM. The source keeps running untouched.
// Feat/agent-substrate servers implement the route; older daemons fail with
// a clear not-found error.
func (c *Client) Fork(req ForkRequest) ([]VMInfo, error) {
	if req.Count <= 0 {
		req.Count = 1
	}
	body := map[string]any{
		"vm":    req.VM,
		"count": req.Count,
		"user":  c.User,
	}
	if req.Name != "" {
		body["name"] = req.Name
	}
	if req.TTLMinutes > 0 {
		body["ttl_minutes"] = req.TTLMinutes
	}
	if len(req.Egress) > 0 {
		body["egress"] = req.Egress
	}
	var data struct {
		VMs []VMInfo `json:"vms"`
	}
	if err := c.do("POST", "/api/v1/vms/"+c.Resolve(req.VM)+"/fork", body, &data); err != nil {
		return nil, err
	}
	return data.VMs, nil
}

// Exec runs a command inside the VM.
func (c *Client) Exec(vm, command string, timeoutSec int) (ExecResult, error) {
	body := map[string]any{"command": command}
	if timeoutSec > 0 {
		body["timeout_s"] = timeoutSec
	}
	var res ExecResult
	err := c.do("POST", "/api/v1/vms/"+c.Resolve(vm)+"/exec", body, &res)
	return res, err
}

// Pause freezes the guest (memory resident).
func (c *Client) Pause(vm string) error {
	return c.do("POST", "/api/v1/vms/"+c.Resolve(vm)+"/pause", map[string]any{}, nil)
}

// Resume resumes a paused guest.
func (c *Client) Resume(vm string) error {
	return c.do("POST", "/api/v1/vms/"+c.Resolve(vm)+"/resume", map[string]any{}, nil)
}

// ExposedPort is a socat-forwarded guest TCP port on the API host.
type ExposedPort struct {
	VMID      string `json:"vmid"`
	GuestPort int    `json:"guest_port"`
	HostPort  int    `json:"host_port"`
}

// ExposePort maps a guest TCP port to a host port (socat forward on the
// fastvm node; host ports come from the 61000-64999 range).
func (c *Client) ExposePort(vm string, guestPort int) (ExposedPort, error) {
	var ep ExposedPort
	body := map[string]any{"guest_port": guestPort}
	err := c.do("POST", "/api/v1/vms/"+c.Resolve(vm)+"/expose", body, &ep)
	return ep, err
}

// Destroy removes the VM.
func (c *Client) Destroy(vm string) error {
	return c.do("DELETE", "/api/v1/vms/"+c.Resolve(vm), nil, nil)
}

// Get resolves a name-or-ID and returns its record.
func (c *Client) Get(vm string) (*VMInfo, error) {
	var info VMInfo
	if err := c.do("GET", "/api/v1/vms/resolve/"+vm, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// List returns this user's VMs.
func (c *Client) List() ([]VMInfo, error) {
	var data struct {
		VMs []VMInfo `json:"vms"`
	}
	if err := c.do("GET", "/api/v1/vms?user="+c.User, nil, &data); err != nil {
		return nil, err
	}
	return data.VMs, nil
}

// Resolve maps a VM name to its ID (names pass through, IDs pass through after
// a lookup so the caller can always hand around names).
func (c *Client) Resolve(vm string) string {
	id, err := func() (string, error) {
		var data struct {
			VMID string `json:"vmid"`
		}
		if err := c.do("GET", "/api/v1/vms/resolve/"+vm, nil, &data); err != nil {
			return "", err
		}
		return data.VMID, nil
	}()
	if err != nil || id == "" {
		return vm
	}
	return id
}
