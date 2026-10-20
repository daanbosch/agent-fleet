package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/team-telnyx/fast-vm-spinner/internal/enroller"
	"github.com/team-telnyx/fast-vm-spinner/internal/fastvm"
)

const (
	enrollmentModeDisabled = "disabled"
	enrollmentModeAsync    = "async"
	enrollmentModeRequired = "required"
)

type VMEnroller interface {
	Enroll(ctx context.Context, req enroller.EnrollRequest) error
}

type EnrollmentConfig struct {
	Mode     string
	Client   VMEnroller
	Features []string
	Timeout  time.Duration
}

type EnrollmentSummary struct {
	Mode   string   `json:"mode"`
	Status string   `json:"status"`
	Count  int      `json:"count"`
	Errors []string `json:"errors,omitempty"`
}

func normalizeEnrollmentConfig(cfg EnrollmentConfig) EnrollmentConfig {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		if cfg.Client == nil {
			cfg.Mode = enrollmentModeDisabled
		} else {
			cfg.Mode = enrollmentModeAsync
		}
	}
	if cfg.Client == nil || cfg.Mode == "off" || cfg.Mode == "false" {
		cfg.Mode = enrollmentModeDisabled
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Minute
	}
	return cfg
}

func (h *Handler) enrollCreatedVMs(ctx context.Context, vms []fastvm.VMInfo) (*EnrollmentSummary, error) {
	cfg := normalizeEnrollmentConfig(h.enrollment)
	if cfg.Mode == enrollmentModeDisabled || len(vms) == 0 {
		return nil, nil
	}

	requests := make([]enroller.EnrollRequest, 0, len(vms))
	for _, vm := range vms {
		requests = append(requests, enrollRequestForVM(vm, h.engine.SSHHost(), cfg.Features))
	}

	switch cfg.Mode {
	case enrollmentModeAsync:
		go h.runAsyncEnrollment(requests, cfg)
		return &EnrollmentSummary{Mode: cfg.Mode, Status: "queued", Count: len(requests)}, nil
	case enrollmentModeRequired:
		var errs []string
		for _, req := range requests {
			vmCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			err := cfg.Client.Enroll(vmCtx, req)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", req.VMID, err))
			}
		}
		if len(errs) > 0 {
			return &EnrollmentSummary{Mode: cfg.Mode, Status: "failed", Count: len(requests), Errors: errs}, fmt.Errorf("enrollment failed: %s", strings.Join(errs, "; "))
		}
		return &EnrollmentSummary{Mode: cfg.Mode, Status: "completed", Count: len(requests)}, nil
	default:
		h.logger.Warn("unknown enrollment mode; skipping enrollment", "mode", cfg.Mode)
		return nil, nil
	}
}

func (h *Handler) runAsyncEnrollment(requests []enroller.EnrollRequest, cfg EnrollmentConfig) {
	for _, req := range requests {
		vmCtx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
		if err := cfg.Client.Enroll(vmCtx, req); err != nil {
			h.logger.Error("async VM enrollment failed", "vmid", req.VMID, "err", err)
		} else {
			h.logger.Info("async VM enrollment completed", "vmid", req.VMID)
		}
		cancel()
	}
}

func enrollRequestForVM(vm fastvm.VMInfo, sshHost string, features []string) enroller.EnrollRequest {
	backend := vm.Backend
	if backend == "" {
		backend = "proxmox"
	}
	template := vm.TemplateName
	if template == "" && vm.Template > 0 {
		template = strconv.Itoa(vm.Template)
	}
	sshUser := vm.SSHUser
	if sshUser == "" {
		sshUser = "ubuntu"
	}
	return enroller.EnrollRequest{
		VMID:     vm.VMID,
		Name:     vm.Name,
		User:     vm.User,
		Backend:  backend,
		Template: template,
		Node:     vm.Node,
		SSHHost:  sshHost,
		SSHPort:  vm.SSHPort,
		SSHUser:  sshUser,
		Features: append([]string(nil), features...),
		Tags: map[string]string{
			"fastvm_backend": backend,
			"fastvm_node":    vm.Node,
		},
	}
}
