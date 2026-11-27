package rtsp

// Draining reports whether the client is reading its UDP sockets dry after
// its control connection ended; for the external tests.
func (c *Client) Draining() bool { return c.draining.Load() }
