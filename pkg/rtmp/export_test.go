package rtmp

// SplitPlayURL exposes the play URL split to the external tests.
func SplitPlayURL(rawURL string) (addr, app, stream, tcURL string, err error) {
	t, err := splitPlayURL(rawURL)
	return t.addr, t.app, t.stream, t.tcURL, err
}

// OutStream is the last message type and message stream id a ServerConn
// wrote on one chunk stream.
type OutStream struct {
	Type     uint8
	StreamID uint32
}

// OutStreams returns, per chunk stream, what sc last wrote on it.
func OutStreams(sc *ServerConn) map[uint32]OutStream {
	return outStreams(sc.s)
}

func outStreams(s *session) map[uint32]OutStream {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	out := make(map[uint32]OutStream, len(s.cw.streams))
	for csid, st := range s.cw.streams {
		out[csid] = OutStream{Type: st.typ, StreamID: st.streamID}
	}
	return out
}

// PublishOutStreams returns, per chunk stream, what p last wrote on it.
func PublishOutStreams(p *PublishConn) map[uint32]OutStream {
	return outStreams(p.s)
}
