package memoryx

// The member's own routes into AF-owned agent memory (#1985): read one body, save, forget. The
// tools' routes need a calling session and the agentMemory switch; the Console has neither, so
// these name the scope and the project id directly, are not gated by the switch (the member can
// always tend what sessions wrote), and record the author as the Console, like a revert. The
// write path is the tools' own (agentMemSave, agentMemForget): the same revision check, the same
// secret scan.

import (
	"net/http"
	"os"
	"time"

	"github.com/k-k1/agent-fleet/workspace/agent/internal/httpx"
)

// agentMemMemberCaller is the Console as a caller for one scope. A project must already hold
// memory: the Console cannot start a project, because it has no working copy to take the
// project's root from.
func agentMemMemberCaller(scope, project string) (agentMemCaller, error) {
	c := agentMemMember
	switch scope {
	case agentMemScopeUser:
		return c, nil
	case agentMemScopeProject:
		if !agentMemValidProjectID(project) {
			return c, memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest, "project is not a project id")
		}
		if _, err := agentMemCheckDir("projects/"+project, false); err != nil {
			if os.IsNotExist(err) {
				return c, memoryErrf(http.StatusNotFound, errCodeMemoryNotFound, "no such project")
			}
			return c, err
		}
		c.Project = agentMemProjectInfo(project)
		return c, nil
	}
	return c, memoryErrf(http.StatusBadRequest, errCodeMemoryBadRequest, "scope must be %q or %q", agentMemScopeUser, agentMemScopeProject)
}

// agentMemMemberRead is one memory with its body for the member. Unlike the tools' read it is
// not a use: the count says what sessions found worth reading.
func agentMemMemberRead(scope, project, name string) (agentMemEntry, error) {
	if err := agentMemCheckName(name); err != nil {
		return agentMemEntry{}, err
	}
	c, err := agentMemMemberCaller(scope, project)
	if err != nil {
		return agentMemEntry{}, err
	}
	agentMemMu.RLock()
	defer agentMemMu.RUnlock()
	l, ok, err := agentMemLoadOne(c, scope, name)
	if err != nil {
		return agentMemEntry{}, err
	}
	if !ok {
		return agentMemEntry{}, memoryErrf(http.StatusNotFound, errCodeMemoryNotFound, "no memory by that name")
	}
	if l.Withheld {
		return agentMemEntry{}, memoryErrf(http.StatusUnprocessableEntity, errCodeMemorySecretDetected,
			"this memory is withheld: its file looks like it contains a secret or cannot be checked; fix or remove the file")
	}
	rel, _ := agentMemScopeDir(scope, c)
	es := []agentMemEntry{l.Entry}
	agentMemSetUses(es, rel)
	return es[0], nil
}

// agentMemMemberSaveReq is agentMemSaveReq plus the project id the Console has no session to
// resolve.
type agentMemMemberSaveReq struct {
	agentMemSaveReq
	Project string `json:"project"`
}

// agentMemMemberForgetReq is agentMemForgetReq plus the project id.
type agentMemMemberForgetReq struct {
	agentMemForgetReq
	Project string `json:"project"`
}

// HandleAgentMemoryMemberRead returns one memory with its body to the Console.
func HandleAgentMemoryMemberRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	e, err := agentMemMemberRead(q.Get("scope"), q.Get("project"), q.Get("name"))
	if err != nil {
		agentMemWriteErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, e)
}

// HandleAgentMemoryMemberSave creates or updates a memory as the member.
func HandleAgentMemoryMemberSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, agentMemSaveMaxRequest)
	var req agentMemMemberSaveReq
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	c, err := agentMemMemberCaller(req.Scope, req.Project)
	if err != nil {
		agentMemWriteErr(w, err)
		return
	}
	res, err := agentMemSave(c, req.agentMemSaveReq, time.Now())
	if err != nil {
		agentMemWriteErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// HandleAgentMemoryMemberForget removes a memory as the member; history keeps it and the
// changes list can bring it back.
func HandleAgentMemoryMemberForget(w http.ResponseWriter, r *http.Request) {
	var req agentMemMemberForgetReq
	if !httpx.DecodeJSON(w, r, &req) {
		return
	}
	c, err := agentMemMemberCaller(req.Scope, req.Project)
	if err != nil {
		agentMemWriteErr(w, err)
		return
	}
	res, err := agentMemForget(c, req.agentMemForgetReq, time.Now())
	if err != nil {
		agentMemWriteErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
