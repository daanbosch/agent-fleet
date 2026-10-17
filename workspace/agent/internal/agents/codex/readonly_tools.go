package codex

// ReadOnlyTools lists the tool names of this kind's transcript that only read: the cross-agent
// move (ADR 0115 decision 7) drops a call to one of them outside the last turns, because
// running it again on the target costs nothing. A name that is missing here is kept, so a
// renamed or new tool errs toward keeping a line, never toward hiding a side effect.
// Names are matched exactly.
//
// Source: the one non-shell, non-edit tool name transcript.go emits; exec_command is a shell and is never listed.
var ReadOnlyTools = []string{"view_image"}
