package media

import "time"

// MaxAnchorLead bounds how far past the source's own timing an epoch's wall
// is pushed to keep it from overlapping the previous epoch (see clock.Epoch).
const MaxAnchorLead = time.Second

// Anchor maps one epoch's stream time to wallclock: stream time Start (90 kHz
// ticks) is presented at Wall, the epoch's effective wall (see clock.Epoch).
type Anchor struct {
	Epoch uint32
	Start int64
	Wall  time.Time
}
