package memoryx

// The member's read of one memory body (#1985). The tools' read needs a calling session and the
// agentMemory switch; the Console has neither, so this names the scope and the project id
// directly and is not gated by the switch, like the changes list. It writes nothing and counts no
// use. There is deliberately no member save or forget route here: the Agent cannot tell the
// Console from a session holding AGENT_TOKEN, so a route that writes free text as "member" would
// let a session bypass the switch (#1997).

import (
	"net/http"
	"os"

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
