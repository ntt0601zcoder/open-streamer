package media

// Frame is one access unit (video) or one AAC frame (audio).
//
// PTS and DTS are in 90 kHz ticks. Audio frames always have Key == true and
// RASL == false. Data holds the NAL units (video, without start codes) or the
// raw AAC frame (audio, one element).
type Frame struct {
	Track    int
	Config   *TrackConfig
	PTS, DTS int64
	Key      bool
	RASL     bool
	Data     [][]byte
	Epoch    uint32
}

// Kind returns the media kind of the frame's track.
func (f Frame) Kind() Kind {
	if f.Config == nil {
		return 0
	}
	return f.Config.Codec.Kind()
}
