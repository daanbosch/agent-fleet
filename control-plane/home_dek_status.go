package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/k-k1/agent-fleet/control-plane/internal/store"
)

// runHomeDEKStatus is `af-cp home-dek-status [--remigrate <id> | --orphans | --remove-orphan <id> --force]`:
// a read-only count of how far the homes are on their own credential-store key
// (AF_WORKSPACE_DEK=random, dek.go).
// --remigrate puts one confirmed home back to 'migrating', so its next start hands out the
// derived key beside the home's key again: the way back for a home restored from a copy made
// before its store moved, which the home's key alone does not open. It needs no custodian and
// opens nothing; exit 2 on usage or a store that cannot be read, 1 when --remigrate found no
// confirmed home by that id.
//
// --orphans lists the keys no workspace row names. --remove-orphan drops one of them and
// needs --force: this process has no runtime, so it cannot prove the home gone the way the
// destroy path does (runtime.ProveHomeGone), and --force is the operator's statement that
// they checked in the cloud console or on the host that nothing of the home is left. It
// still refuses a key a workspace row names, whatever the flag. Exit 1: nothing removed.
func runHomeDEKStatus(args []string) {
	fs := flag.NewFlagSet("home-dek-status", flag.ContinueOnError)
	remigrate := fs.String("remigrate", "", "membership id of a confirmed home to put back to migrating")
	orphans := fs.Bool("orphans", false, "list the keys no workspace row names")
	removeOrphan := fs.String("remove-orphan", "", "membership id of an orphaned key to remove (needs --force)")
	force := fs.Bool("force", false, "with --remove-orphan: the home is confirmed gone")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		usageHomeDEKStatus()
	}
	modes := 0
	for _, set := range []bool{*remigrate != "", *orphans, *removeOrphan != ""} {
		if set {
			modes++
		}
	}
	if modes > 1 || (*force && *removeOrphan == "") {
		usageHomeDEKStatus()
	}
	st, where, err := openRewrapStore()
	if err != nil {
		log.Printf("home-dek-status: open the metadata store: %v", err)
		os.Exit(2)
	}
	defer st.Close()
	log.Printf("home-dek-status: %s", where)
	ctx := context.Background()
	switch {
	case *remigrate != "":
		os.Exit(remigrateHomeDEK(ctx, st, *remigrate, os.Stdout))
	case *orphans:
		if err := printOrphanHomeDEKs(ctx, st, os.Stdout); err != nil {
			log.Printf("home-dek-status: %v", err)
			os.Exit(2)
		}
		return
	case *removeOrphan != "":
		os.Exit(removeOrphanHomeDEK(ctx, st, *removeOrphan, *force, os.Stdout))
	}
	if err := printHomeDEKStatus(ctx, st, os.Stdout); err != nil {
		log.Printf("home-dek-status: %v", err)
		os.Exit(2)
	}
}

func remigrateHomeDEK(ctx context.Context, st *store.SQL, membershipID string, w io.Writer) int {
	ok, err := st.RemigrateHomeDEK(ctx, membershipID)
	switch {
	case err != nil:
		log.Printf("home-dek-status: remigrate: %v", err)
		return 2
	case !ok:
		fmt.Fprintf(w, "no confirmed home key for membership %s; nothing changed\n", membershipID)
		return 1
	}
	fmt.Fprintf(w, "membership %s is migrating again; restart its workspace to re-seal the store\n", membershipID)
	return 0
}

func printHomeDEKStatus(ctx context.Context, st *store.SQL, w io.Writer) error {
	c, err := st.CountHomeDEKs(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "homes with their own key, migrating: %d\n", c.Migrating)
	fmt.Fprintf(w, "homes with their own key, confirmed: %d\n", c.Random)
	fmt.Fprintf(w, "workspaces whose home has no key of its own yet: %d\n", c.WithoutKey)
	return nil
}

func printOrphanHomeDEKs(ctx context.Context, st *store.SQL, w io.Writer) error {
	rows, err := st.ListOrphanHomeDEKs(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "no home key without a workspace")
		return nil
	}
	fmt.Fprintf(w, "%d home key(s) no workspace names. A home can outlive its workspace, so none of these is\n"+
		"proven gone: remove one only after checking its home is (--remove-orphan <id> --force).\n", len(rows))
	for _, o := range rows {
		member := "membership gone"
		if o.MembershipExists {
			member = "membership exists"
		}
		fmt.Fprintf(w, "  %s  %s  tenant %s  created %s  %s\n", o.MembershipID, o.Scheme, o.KeyRef, o.CreatedAt, member)
	}
	return nil
}

func removeOrphanHomeDEK(ctx context.Context, st *store.SQL, membershipID string, force bool, w io.Writer) int {
	if !force {
		fmt.Fprintf(w, "refusing to remove the key of membership %s without --force: this tool cannot prove its home gone, "+
			"and a key removed while any of the home survives makes its credential store unreadable for good\n", membershipID)
		return 1
	}
	ok, err := st.DeleteOrphanHomeDEK(ctx, membershipID)
	switch {
	case err != nil:
		log.Printf("home-dek-status: remove-orphan: %v", err)
		return 2
	case !ok:
		fmt.Fprintf(w, "no key without a workspace for membership %s; nothing changed\n", membershipID)
		return 1
	}
	fmt.Fprintf(w, "removed the key of membership %s\n", membershipID)
	return 0
}

func usageHomeDEKStatus() {
	fmt.Fprintln(os.Stderr, "usage: af-cp home-dek-status [--remigrate <membership-id> | --orphans | --remove-orphan <membership-id> --force]")
	os.Exit(2)
}
