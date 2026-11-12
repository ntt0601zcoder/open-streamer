package media

// ConfigCache remembers the last TrackConfig of each track, so that a reader
// that rebuilds an equal config hands out the previous pointer. Consumers
// compare config pointers to detect a change, so a rebuild of an unchanged
// config must not look like one. It is not safe for concurrent use.
type ConfigCache struct {
	last []*TrackConfig
}

// Stable returns the previous config of the track when cfg is Equal to it,
// and otherwise remembers and returns cfg. A nil cfg is returned as is and
// leaves the entry unchanged.
func (c *ConfigCache) Stable(track int, cfg *TrackConfig) *TrackConfig {
	if cfg == nil || track < 0 {
		return cfg
	}
	for len(c.last) <= track {
		c.last = append(c.last, nil)
	}
	if prev := c.last[track]; prev != nil && prev.Equal(cfg) {
		return prev
	}
	c.last[track] = cfg
	return cfg
}

// Reset forgets the track's config: the next one is taken as new even if it
// equals the old one.
func (c *ConfigCache) Reset(track int) {
	if track >= 0 && track < len(c.last) {
		c.last[track] = nil
	}
}
