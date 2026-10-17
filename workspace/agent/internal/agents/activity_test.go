package agents

import (
	"testing"
	"time"
)

func TestActivityProgress(t *testing.T) {
	var fresh Activity
	fresh.Touch()
	at, _ := fresh.At()
	later := at.Add(2 * time.Hour) // the same stamp, two hours on: stale
	soon := at.Add(10 * time.Second)

	yes := func() bool { return true }
	no := func() bool { return false }
	boom := func() bool { t.Fatal("tool probe ran for a fresh stamp"); return false }

	t.Run("never heard from, no tool: cannot tell", func(t *testing.T) {
		var a Activity
		if _, ok := a.Progress(later, no); ok {
			t.Fatal("answered for a runtime never heard from")
		}
	})
	t.Run("never heard from, tool alive: now", func(t *testing.T) {
		var a Activity
		got, ok := a.Progress(later, yes)
		if !ok || !got.Equal(later) {
			t.Fatalf("got %v, %v; want now", got, ok)
		}
	})
	t.Run("stale stamp and a silent tool keeps holding", func(t *testing.T) {
		got, ok := fresh.Progress(later, yes)
		if !ok || !got.Equal(later) {
			t.Fatalf("got %v, %v; want now", got, ok)
		}
	})
	t.Run("stale stamp, no tool: the stamp, so the bound can lapse", func(t *testing.T) {
		got, ok := fresh.Progress(later, no)
		if !ok || !got.Equal(at) {
			t.Fatalf("got %v, %v; want the stamp %v", got, ok, at)
		}
	})
	t.Run("fresh stamp skips the probe", func(t *testing.T) {
		got, ok := fresh.Progress(soon, boom)
		if !ok || !got.Equal(at) {
			t.Fatalf("got %v, %v; want the stamp", got, ok)
		}
	})
	t.Run("nil probe: stamp only", func(t *testing.T) {
		got, ok := fresh.Progress(later, nil)
		if !ok || !got.Equal(at) {
			t.Fatalf("got %v, %v; want the stamp", got, ok)
		}
	})
}
