package fastvm

// Backend is the VM lifecycle contract exposed by fastvm-api.
//
// Implementations must preserve the existing HTTP/client semantics: IDs are
// returned as VMID strings, names can be resolved through the by-name route,
// and SSHCommand is directly usable by the fastvm client.
type Backend interface {
	CreateVMs(user string, count int, opts CreateOpts) ([]VMInfo, error)
	// WarmSnapshot converts a running VM into a memory-preserving warm
	// template (firecracker backend only; others return a not-supported error).
	WarmSnapshot(vmid, name string) (*WarmTemplate, error)
	// WarmLaunch restores a clone from a warm template.
	WarmLaunch(user, template, name, sshPubKey string) (VMInfo, error)
	// ListWarmTemplates returns available warm templates.
	ListWarmTemplates() ([]WarmTemplate, error)
	// ListWarmTemplatesForUser returns templates readable by user; empty
	// user means the operator view (everything).
	ListWarmTemplatesForUser(user string) ([]WarmTemplate, error)
	// UpdateWarmTemplateACL sets owner/public/readers on a warm template
	// (permissive defaults; owner-only changes once claimed).
	UpdateWarmTemplateACL(user, name string, public bool, readers []string) (*WarmTemplate, error)
	// PauseVM freezes the guest with memory resident (VMM-level pause).
	PauseVM(vmid string) error
	// ResumeVM resumes a paused guest.
	ResumeVM(vmid string) error
	// ExecVM runs a non-interactive command in the guest over the engine's
	// channel and returns combined output and exit code.
	ExecVM(vmid, command string, timeoutSec int) (*ExecResult, error)
	// ExposePort maps a guest TCP port to a host port (socat forward).
	ExposePort(vmid string, guestPort int) (*ExposedPortInfo, error)
	// UnexposePort removes a host-port exposure.
	UnexposePort(vmid string, hostPort int) error
	ListVMs(user string) ([]VMInfo, error)
	GetVM(vmid string) (*VMInfo, error)
	GetVMByName(name string) (*VMInfo, error)
	ResolveVMID(nameOrID string) (string, error)
	DestroyVM(vmid string) error
	DestroyUserVMs(user string) error
	Converge(user, keepVMID string) error
	SnapshotVM(vmid string) error
	Status() (*StatusInfo, error)
	SSHHost() string
}
