package local

import "time"

// importInspectTimeout bounds one whole inspect pipeline: credential reveal,
// branch discovery, fetch and parse. The git client has its own per-call
// bounds; this is the ceiling over all of them together.
const importInspectTimeout = 3 * time.Minute
