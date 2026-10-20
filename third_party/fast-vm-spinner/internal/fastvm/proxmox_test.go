package fastvm

import (
	"reflect"
	"testing"
)

func TestParseCPUList(t *testing.T) {
	got, err := parseCPUList("4-6,10,12-13")
	if err != nil {
		t.Fatalf("parseCPUList: %v", err)
	}

	want := []int{4, 5, 6, 10, 12, 13}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCPUList() = %v, want %v", got, want)
	}
}

func TestParseCPUListRejectsDescendingRange(t *testing.T) {
	if _, err := parseCPUList("8-4"); err == nil {
		t.Fatal("parseCPUList() err = nil, want error")
	}
}

func TestFormatCPUList(t *testing.T) {
	cpus := []int{4, 5, 6, 10, 12, 13}
	if got, want := formatCPUList(cpus), "4-6,10,12-13"; got != want {
		t.Fatalf("formatCPUList() = %q, want %q", got, want)
	}
}
