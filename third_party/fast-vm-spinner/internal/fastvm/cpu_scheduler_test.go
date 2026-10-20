package fastvm

import (
	"reflect"
	"testing"
)

func TestCalculateCPUPlacementsDedicated(t *testing.T) {
	placements := calculateCPUPlacements([]cpuRequest{
		{vmid: 60001, cores: 2},
		{vmid: 60000, cores: 4},
		{vmid: 60002, cores: 2},
	}, []int{4, 5, 6, 7, 8, 9, 10, 11})

	tests := map[int][]int{
		60000: {4, 5, 6, 7},
		60001: {8, 9},
		60002: {10, 11},
	}

	for vmid, want := range tests {
		got := placements[vmid]
		if got.Mode != cpuPlacementDedicated {
			t.Fatalf("placement mode for %d = %s, want %s", vmid, got.Mode, cpuPlacementDedicated)
		}
		if !reflect.DeepEqual(got.HostCPUs, want) {
			t.Fatalf("placement cpus for %d = %v, want %v", vmid, got.HostCPUs, want)
		}
	}
}

func TestCalculateCPUPlacementsSharedWraps(t *testing.T) {
	placements := calculateCPUPlacements([]cpuRequest{
		{vmid: 60000, cores: 4},
		{vmid: 60001, cores: 4},
		{vmid: 60002, cores: 4},
	}, []int{4, 5, 6, 7, 8, 9, 10, 11})

	tests := map[int][]int{
		60000: {4, 5, 6, 7},
		60001: {8, 9, 10, 11},
		60002: {4, 5, 6, 7},
	}

	for vmid, want := range tests {
		got := placements[vmid]
		if got.Mode != cpuPlacementShared {
			t.Fatalf("placement mode for %d = %s, want %s", vmid, got.Mode, cpuPlacementShared)
		}
		if !reflect.DeepEqual(got.HostCPUs, want) {
			t.Fatalf("placement cpus for %d = %v, want %v", vmid, got.HostCPUs, want)
		}
	}
}

func TestCPUWindowCapsAtAllowedCount(t *testing.T) {
	got := cpuWindow([]int{4, 5}, 0, 4)
	want := []int{4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cpuWindow() = %v, want %v", got, want)
	}
}
