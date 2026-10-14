package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	efstypes "github.com/aws/aws-sdk-go-v2/service/efs/types"
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

// The local adapters can see a home but never prove one gone: a missing directory is also
// what a detached volume's empty mountpoint, or a WS_DATA aimed at the wrong place, looks like.
func TestProveLocalHomeGoneNeverGone(t *testing.T) {
	actualRoot, wrongRoot := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(actualRoot, "alice", "home", ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v, _ := proveLocalHomeGone(filepath.Join(actualRoot, "alice")); v != HomePresent {
		t.Errorf("present = %s", v)
	}
	// The same member judged under a root that is not the one holding the home.
	if v, _ := proveLocalHomeGone(filepath.Join(wrongRoot, "alice")); v != HomeUnknown {
		t.Errorf("absent under another existing root = %s", v)
	}
	for _, bad := range []string{"", "relative/alice"} {
		if v, _ := proveLocalHomeGone(bad); v != HomeUnknown {
			t.Errorf("%q = %s", bad, v)
		}
	}
	if v, _ := ProveHomeGone(context.Background(), &dockerRuntime{dataDir: filepath.Join(actualRoot, "alice")}); v != HomePresent {
		t.Errorf("docker adapter = %s", v)
	}
	if v, _ := ProveHomeGone(context.Background(), &nativeRuntime{dataDir: filepath.Join(wrongRoot, "alice")}); v != HomeUnknown {
		t.Errorf("native adapter = %s", v)
	}
}

// ecs: the home task exiting 0 proves only the directories it removes. An access point rooted
// anywhere else (ensureAccessPoint reuses one by tag without checking its root) may hold the
// real secrets.enc, so the key stays, and the access point stays too: deleting it would let a
// retry find nothing odd and prove the home gone.
func TestECSDestroyProvesTheHomeGoneOnlyForTheDirectoriesTheTaskRemoves(t *testing.T) {
	ap := func(id string, root *efstypes.RootDirectory) efstypes.AccessPointDescription {
		return efstypes.AccessPointDescription{AccessPointId: aws.String(id), RootDirectory: root,
			Tags: []efstypes.Tag{{Key: aws.String("af-membership"), Value: aws.String("M-1")}}}
	}
	dir := func(p string) *efstypes.RootDirectory { return &efstypes.RootDirectory{Path: aws.String(p)} }
	for _, tc := range []struct {
		name     string
		aps      []efstypes.AccessPointDescription
		exit     int32
		want     HomeVerdict
		wantLeft int // access points left after the first and the retry
	}{
		{"standard access points", []efstypes.AccessPointDescription{ap("a", dir("/home/M-1")), ap("b", dir("/claude-config/M-1"))}, 0, HomeGone, 0},
		{"no access point at all", nil, 0, HomeGone, 0},
		{"access point elsewhere", []efstypes.AccessPointDescription{ap("a", dir("/home/M-1")), ap("b", dir("/old-home/M-1"))}, 0, HomeUnknown, 1},
		{"access point with no root", []efstypes.AccessPointDescription{ap("a", nil)}, 0, HomeUnknown, 1},
		{"task failed", []efstypes.AccessPointDescription{ap("a", dir("/home/M-1"))}, HomeOpExitFailed, HomeUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff, ft := &fakeEFS{aps: tc.aps}, &fakeTasks{exitCode: exit(tc.exit)}
			rt := newHomeTaskECS(&fakeECS{}, ff, ft)
			_, _ = rt.Destroy(context.Background())
			if v, why := rt.ProveHomeGone(context.Background()); v != tc.want {
				t.Fatalf("after Destroy: %s (%s), want %s", v, why, tc.want)
			}
			if len(ff.aps) != tc.wantLeft {
				t.Fatalf("%d access points left, want %d", len(ff.aps), tc.wantLeft)
			}
			// A retry on a new runtime value (the CP restarted) sees what the first left.
			rt2 := newHomeTaskECS(&fakeECS{}, ff, &fakeTasks{exitCode: exit(0)})
			_, _ = rt2.Destroy(context.Background())
			want2 := tc.want
			if tc.name == "task failed" {
				want2 = HomeGone // the retry's task succeeded and there was nothing odd to forget
			}
			if v, why := rt2.ProveHomeGone(context.Background()); v != want2 {
				t.Fatalf("after the retry: %s (%s), want %s", v, why, want2)
			}
		})
	}
}
