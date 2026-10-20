package fastvm

import (
	"fmt"
	"log/slog"
	"sort"
	"strconv"
)

type cpuPlacementMode string

const (
	cpuPlacementDedicated cpuPlacementMode = "dedicated"
	cpuPlacementShared    cpuPlacementMode = "shared"
)

type cpuRequest struct {
	vmid  int
	cores int
}

type cpuPlacement struct {
	VMID           int
	Cores          int
	HostCPUs       []int
	Mode           cpuPlacementMode
	TotalRequested int
	AllowedCount   int
}

func (p cpuPlacement) Affinity() string {
	return formatCPUList(p.HostCPUs)
}

func calculateCPUPlacements(requests []cpuRequest, allowed []int) map[int]cpuPlacement {
	placements := make(map[int]cpuPlacement, len(requests))
	if len(requests) == 0 || len(allowed) == 0 {
		return placements
	}

	reqs := append([]cpuRequest(nil), requests...)
	sort.Slice(reqs, func(i, j int) bool {
		return reqs[i].vmid < reqs[j].vmid
	})

	totalRequested := 0
	for i := range reqs {
		if reqs[i].cores <= 0 {
			reqs[i].cores = defaultCores
		}
		totalRequested += reqs[i].cores
	}

	mode := cpuPlacementDedicated
	if totalRequested > len(allowed) {
		mode = cpuPlacementShared
	}

	cursor := 0
	for _, req := range reqs {
		count := req.cores
		if count > len(allowed) {
			count = len(allowed)
		}

		hostCPUs := cpuWindow(allowed, cursor, count)
		placements[req.vmid] = cpuPlacement{
			VMID:           req.vmid,
			Cores:          req.cores,
			HostCPUs:       hostCPUs,
			Mode:           mode,
			TotalRequested: totalRequested,
			AllowedCount:   len(allowed),
		}

		cursor += count
		if mode == cpuPlacementShared {
			cursor %= len(allowed)
		}
	}

	return placements
}

func cpuWindow(allowed []int, start, count int) []int {
	if len(allowed) == 0 || count <= 0 {
		return nil
	}
	if count > len(allowed) {
		count = len(allowed)
	}

	out := make([]int, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, allowed[(start+i)%len(allowed)])
	}
	return out
}

func (e *Engine) cpuPlacementsForCurrentState() (map[int]cpuPlacement, error) {
	allowed, err := readQemuSliceCPUs()
	if err != nil {
		return nil, err
	}

	vms, err := e.state.ReadVMs()
	if err != nil {
		return nil, err
	}

	requests := make([]cpuRequest, 0, len(vms))
	for vmidStr, vm := range vms {
		vmid, err := strconv.Atoi(vmidStr)
		if err != nil {
			continue
		}
		cores := vm.Cores
		if cores <= 0 {
			if configured, err := qmGetConfigInt(vmid, "cores"); err == nil {
				cores = configured
			} else {
				cores = defaultCores
			}
		}
		requests = append(requests, cpuRequest{vmid: vmid, cores: cores})
	}

	return calculateCPUPlacements(requests, allowed), nil
}

func (e *Engine) reconcileCPUPlacement(reason string) {
	placements, err := e.cpuPlacementsForCurrentState()
	if err != nil {
		slog.Warn("failed to calculate CPU placements", "reason", reason, "err", err)
		return
	}
	if len(placements) == 0 {
		return
	}

	vmids := make([]int, 0, len(placements))
	for vmid := range placements {
		vmids = append(vmids, vmid)
	}
	sort.Ints(vmids)

	for _, vmid := range vmids {
		placement := placements[vmid]
		affinity := placement.Affinity()
		if affinity == "" {
			continue
		}
		log := slog.With(
			"vmid", vmid,
			"reason", reason,
			"mode", placement.Mode,
			"host_cpus", affinity,
			"requested_vcpus", placement.TotalRequested,
			"available_host_cpus", placement.AllowedCount,
		)

		if err := qmSet(vmid, map[string]string{"affinity": affinity}); err != nil {
			log.Warn("failed to set VM CPU affinity", "err", err)
			continue
		}
		if err := pinVMCPUs(vmid, placement); err != nil {
			log.Warn("failed to pin VM CPU threads", "err", err)
			continue
		}
		log.Info("reconciled VM CPU placement")
	}
}

func cpuPlacementSummary(placement cpuPlacement) string {
	return fmt.Sprintf("%s:%s", placement.Mode, placement.Affinity())
}
