package media

import "time"

// Anchor maps one epoch's stream time to wallclock: the epoch's first frame
// has stream time Start (90 kHz ticks) and was seen at Wall.
type Anchor struct {
	Epoch uint32
	Start int64
	Wall  time.Time
}
