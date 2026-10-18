package memreview

// Releasing the rejected hashes of a forgotten memory (ADR 0117 decision 5, owner answer 6): a
// rejection is remembered only as long as the memory it concerned. The forget happens in the memory
// store (memoryx), which tells this package twice: before it commits, and after. The first call
// writes down that a release is owed (an intent), the second does it and strikes the intent. A
// release that fails, or a stop between the commit and the second call, leaves the intent, and
// ReconcileReleases (every wake-up of the loop and at start) finishes it. An intent for a memory
// that still exists means the forget never committed, and is dropped.
//
// An intent carries the time it was written, and only rejections made up to then are released: one
// made after the forget (a review proposing the forgotten memory again, rejected by the member) is
// about the memory as it is now and stays.

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/memoryx"
)

type releaseIntent struct {
	Scope     string    `json:"scope"`
	Name      string    `json:"name"`
	ProjectID string    `json:"projectId,omitempty"`
	At        time.Time `json:"at"`
}

var releaseStore = store[releaseIntent]{"releasing"}

func releaseKey(scope, projectID, name string) string {
	h := sha256.Sum256([]byte(scope + "\x00" + projectID + "\x00" + name))
	return hex.EncodeToString(h[:])[:24]
}

// ForgetHooks are the hooks package main installs on memoryx.
func ForgetHooks() *memoryx.ForgetHooks {
	return &memoryx.ForgetHooks{Intent: noteForgetIntent, Done: noteForgotten}
}

func hasRejectionsLocked(scope, projectID, name string) bool {
	for _, h := range rejectedStore.keys() {
		if r, ok := rejectedStore.Read(h); ok && r.Scope == scope && r.Name == name && r.ProjectID == projectID {
			return true
		}
	}
	return false
}

// noteForgetIntent runs before a forget commits. With nothing remembered about the memory there is
// nothing owed and nothing is written, so a forget does not depend on this package's directory.
func noteForgetIntent(scope, projectID, name string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if !hasRejectionsLocked(scope, projectID, name) {
		return
	}
	in := releaseIntent{Scope: scope, Name: name, ProjectID: projectID, At: time.Now()}
	if err := releaseStore.Write(releaseKey(scope, projectID, name), in); err != nil {
		log.Printf("memory review: could not record a release of rejected proposals")
	}
}

// noteForgotten runs after a forget committed.
func noteForgotten(scope, projectID, name string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	finishReleaseLocked(scope, projectID, name)
}

// finishReleaseLocked releases what the memory's intent covers (everything remembered so far when
// there is none) and strikes the intent only when that worked.
func finishReleaseLocked(scope, projectID, name string) {
	key := releaseKey(scope, projectID, name)
	in, owed := releaseStore.Read(key)
	var cutoff time.Time // zero: every rejection
	if owed {
		cutoff = in.At
	}
	if _, err := releaseLocked(scope, name, projectID, cutoff); err != nil {
		log.Printf("memory review: releasing rejected proposals failed; it is retried")
		return
	}
	if owed {
		if err := releaseStore.Remove(key); err != nil {
			log.Printf("memory review: could not clear a finished release")
		}
	}
}

// ReconcileReleases finishes the releases that are still owed. It reads the store to tell a forget
// that committed from one that did not.
func ReconcileReleases() {
	stateMu.Lock()
	keys := releaseStore.keys()
	var owed []releaseIntent
	for _, k := range keys {
		if in, ok := releaseStore.Read(k); ok {
			owed = append(owed, in)
		}
	}
	stateMu.Unlock()
	for _, in := range owed {
		st, err := memoryx.ReviewTarget(in.Scope, in.ProjectID, in.Name)
		if err != nil {
			continue // cannot tell; the intent stays
		}
		stateMu.Lock()
		if st.Exists {
			if err := releaseStore.Remove(releaseKey(in.Scope, in.ProjectID, in.Name)); err != nil {
				log.Printf("memory review: could not drop an obsolete release")
			}
		} else {
			finishReleaseLocked(in.Scope, in.ProjectID, in.Name)
		}
		stateMu.Unlock()
	}
}

// ReleasePending is how many releases are owed and not done: a failure the member can see.
func ReleasePending() int {
	stateMu.Lock()
	defer stateMu.Unlock()
	return len(releaseStore.keys())
}
