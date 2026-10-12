package sessionx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The limit snapshot (ADR 0111 decision 6) rides GET /sessions only when the caller asks for
// it: the reaper does, the Console's list does not, and a caller that does not ask must not pay
// for the collectors' file reads or see a member it does not know.
func TestSessionsListingCarriesTheLimitSnapshotOnlyWhenAsked(t *testing.T) {
	isolateAgentState(t)
	srv := httptest.NewServer(http.HandlerFunc(HandleListSessions))
	defer srv.Close()
	get := func(q string) map[string]json.RawMessage {
		t.Helper()
		res, err := http.Get(srv.URL + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if _, ok := get("")["limits"]; ok {
		t.Error("the plain listing carries the limit snapshot")
	}
	raw, ok := get("?limits=1")["limits"]
	if !ok {
		t.Fatal("?limits=1 did not add the snapshot")
	}
	var snap struct {
		V     int            `json:"v"`
		Kinds map[string]any `json:"kinds"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil || snap.V != 1 || snap.Kinds == nil {
		t.Fatalf("snapshot = %s (err %v)", raw, err)
	}
}
