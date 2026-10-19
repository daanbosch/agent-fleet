package opencode

// Reading an opencode 2 conversation without its daemon.
//
// v2 keeps a conversation in session_v2 / session_message and never writes the v1 tables, and the
// state readers (v2state.go) go through the daemon's API. The daemon folds up when nothing depends
// on it, and a stopped session is not a dependent, so a stopped session's history would read as
// empty although every message is still in opencode.db. When the daemon cannot be reached the
// transcript is read from the store, read-only, and run through the same v2Turns as the API's
// answer: session_message.data is the API's message without its id and type, which are columns.
//
// This leans on a private schema, which is why it is the fallback and not the default: a live
// daemon is always asked first. Anything the store cannot answer (the form list, the inbox, the
// running set) degrades to "nothing" because those only exist while a daemon runs.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/session"
)

var errV2Offline = errors.New("opencode api: reading from the store, no daemon")

// v2Served reports whether conversations of this workspace live in the v2 store. A probed daemon
// says so. Before any probe (the Agent has just restarted and nothing has started a daemon yet)
// the installed opencode and the database together do: v1 was the default there, which read the
// tables v2 never fills, but an install of 2.x over a database v2 has not migrated yet still holds
// its history in the v1 tables, and only v2's first start moves it.
func v2Served() bool {
	if isV2() {
		return true
	}
	return apiMajor(detectedMajor.Load()) == 0 && installedMajor() >= 2 && storeIsV2()
}

// storeIsV2: the database has the v2 tables, i.e. opencode 2 has opened (and migrated) it.
func storeIsV2() bool {
	db, err := openV2Store()
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	return db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_v2'`).Scan(&n) == nil && n > 0
}

// daemonGone: err is a failure to reach the daemon, or a connection cut mid-answer, and not an
// answer from it. A refusal (statusError) and a body that does not parse (an HTML page, a changed
// contract) are answers: the store would show an older conversation than the one the daemon holds,
// or hide that its contract moved.
func daemonGone(err error) bool {
	if err == nil {
		return false
	}
	var ue *url.Error
	var ne net.Error
	return errors.Is(err, errV2Down) || errors.As(err, &ue) || errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// conversation resolves the slot's conversation and reads its messages through the daemon. When
// the daemon cannot be reached at either step, both are read again, whole, from the store: a
// partial answer from the API is never joined to the store's. The returned target is the one every
// later read of this conversation has to use.
func (t apiTarget) conversation(m session.Meta) (apiTarget, *v2SessionInfo, []v2Msg, error) {
	info, err := t.v2Session(m)
	var msgs []v2Msg
	if err == nil && info != nil {
		if msgs, err = t.messages(info.ID); err != nil && !daemonGone(err) {
			err = nil // best-effort like readSession: the turns gathered so far
		}
	}
	if !daemonGone(err) {
		return t, info, msgs, err
	}
	off := apiTarget{offline: true}
	info, err = off.v2Session(m)
	if err != nil || info == nil {
		return off, info, nil, err
	}
	msgs, _ = off.messages(info.ID)
	return off, info, msgs, nil
}

// openV2Store opens the database the v2 opencode of this workspace writes.
func openV2Store() (*sql.DB, error) {
	db, ok := openRODB(specDBPath(CurrentLaunch()))
	if !ok {
		return nil, errors.New("opencode store: not found")
	}
	return db, nil
}

const storeSessionCols = `SELECT id, COALESCE(parent_id, ''), COALESCE(agent, ''), COALESCE(model, ''),
	COALESCE(directory, ''), time_created, time_updated FROM session_v2`

func scanStoreSession(row interface{ Scan(...any) error }) (*v2SessionInfo, error) {
	var s v2SessionInfo
	var model string
	if err := row.Scan(&s.ID, &s.ParentID, &s.Agent, &model, &s.Location.Directory, &s.Time.Created, &s.Time.Updated); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(model), &s.Model) // best-effort: only the model's name is shown
	return &s, nil
}

// storeSessionByID is one conversation's record; the not-found answer is the API's 404.
func storeSessionByID(id string) (*v2SessionInfo, error) {
	db, err := openV2Store()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	s, err := scanStoreSession(db.QueryRow(storeSessionCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, statusError(http.StatusNotFound)
	}
	return s, err
}

// storeSession is v2Session on the store: the id recorded for the slot, else the newest root
// conversation in its directory that the slot can have opened. nil when there is none.
func storeSession(m session.Meta) (*v2SessionInfo, error) {
	if id := sids.Read(session.UUID(m.Dir, m.Name)); id != "" {
		switch s, err := storeSessionByID(id); {
		case err == nil:
			return s, nil
		case !errors.Is(err, statusError(http.StatusNotFound)):
			return nil, err
		}
	}
	db, err := openV2Store()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	s, err := scanStoreSession(db.QueryRow(storeSessionCols+
		` WHERE directory = ? AND (parent_id IS NULL OR parent_id = '') AND time_created >= ?
		 ORDER BY time_updated DESC, id DESC LIMIT 1`, m.CWD(), metaCreatedMs(m)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// storeMessages reads a whole conversation, oldest first.
func storeMessages(ses string) ([]v2Msg, error) {
	db, err := openV2Store()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, type, data FROM session_message WHERE session_id = ? ORDER BY seq`, ses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []v2Msg
	for rows.Next() {
		var id, typ, data string
		if err := rows.Scan(&id, &typ, &data); err != nil {
			return out, err
		}
		var msg v2Msg
		if json.Unmarshal([]byte(data), &msg) != nil {
			continue // a payload this reader does not parse is skipped, not a reason to show nothing
		}
		msg.ID, msg.Type = id, typ
		out = append(out, msg)
	}
	return out, rows.Err()
}
