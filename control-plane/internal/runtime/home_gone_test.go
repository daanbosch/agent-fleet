package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A Runtime that does not implement the proof, and every adapter that cannot observe the
// home, answer unknown: the key stays.
func TestProveHomeGoneDefaultsToUnknown(t *testing.T) {
	ctx := context.Background()
	if v, _ := ProveHomeGone(ctx, &fakeNoProofRuntime{}); v != HomeUnknown {
		t.Errorf("a runtime without the method = %s", v)
	}
	var zero HomeVerdict
	if zero != HomeUnknown {
		t.Error("the zero verdict must be unknown")
	}
	// ecs: Destroy with no leftovers is not a proof (access points only, then a retry);
	// only the home task's exit 0 of this run is.
	e := &ecsRuntime{}
	if v, _ := e.ProveHomeGone(ctx); v != HomeUnknown {
		t.Errorf("a fresh ecs runtime = %s", v)
	}
	e.homeDestroyed = true
	if v, _ := ProveHomeGone(ctx, e); v != HomeGone {
		t.Errorf("ecs after its home task = %s", v)
	}
	if v, _ := ProveHomeGone(ctx, &ecsEC2Runtime{base: e}); v != HomeGone {
		t.Errorf("ecs-ec2 after the base's home task = %s", v)
	}
	if v, _ := ProveHomeGone(ctx, &ecsEC2Runtime{base: &ecsRuntime{}}); v != HomeUnknown {
		t.Errorf("a fresh ecs-ec2 runtime = %s", v)
	}
	if v, _ := ProveHomeGone(ctx, &kubeRuntime{homeDestroyed: true}); v != HomeGone {
		t.Errorf("kubernetes after Destroy emptied the inventory = %s", v)
	}
}

type fakeNoProofRuntime struct{ Runtime }

func TestProveLocalHomeGone(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "alice")
	if v, _ := proveLocalHomeGone(home); v != HomeGone {
		t.Errorf("absent under a present root = %s", v)
	}
	if err := os.MkdirAll(filepath.Join(home, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v, _ := proveLocalHomeGone(home); v != HomePresent {
		t.Errorf("present = %s", v)
	}
	// An unmounted or misconfigured root says nothing about the home.
	if v, _ := proveLocalHomeGone(filepath.Join(root, "no-such-root", "alice")); v != HomeUnknown {
		t.Errorf("absent root = %s", v)
	}
	for _, bad := range []string{"", "relative/alice"} {
		if v, _ := proveLocalHomeGone(bad); v != HomeUnknown {
			t.Errorf("%q = %s", bad, v)
		}
	}
	d := &dockerRuntime{dataDir: home}
	if v, _ := ProveHomeGone(context.Background(), d); v != HomePresent {
		t.Errorf("docker adapter = %s", v)
	}
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	n := &nativeRuntime{dataDir: home}
	if v, _ := ProveHomeGone(context.Background(), n); v != HomeGone {
		t.Errorf("native adapter = %s", v)
	}
}
