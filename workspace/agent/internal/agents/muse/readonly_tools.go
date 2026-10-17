package muse

// ReadOnlyTools is empty on purpose: the host names its tools (msp Item.Tool), and none of
// those names has been measured as read-only yet. Until one is, every call is kept as a line
// (ADR 0115 decision 7: when the data cannot show that a part is safe to drop, keep it).
var ReadOnlyTools []string
