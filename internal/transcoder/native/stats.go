package native

import "slices"

// RenditionStats holds one rendition's running totals.
type RenditionStats struct {
	FramesIn    uint64 // frames handed to the encoder
	EncodeNanos uint64 // time spent in encode calls
}

// PipelineStats is a snapshot of a pipeline's running totals, cumulative for
// the pipeline's lifetime, plus its current queue depths.
type PipelineStats struct {
	FramesDecoded           uint64
	DroppedVideoPreKeyframe uint64 // video waiting for the first IDR
	DroppedAudioPreKeyframe uint64 // audio waiting for the first video IDR
	DroppedAudioSwitch      uint64 // held audio discarded by an input switch
	PendingAudioFrames      int    // audio held for A/V alignment
	DemuxQueueChunks        int    // raw-TS chunks waiting for the demuxer
	Renditions              []RenditionStats
}

// Stats returns a snapshot of the pipeline's counters. Like ProcessPacket it
// must be called from the goroutine driving the pipeline.
func (p *StreamPipeline) Stats() PipelineStats {
	st := p.stats
	st.Renditions = slices.Clone(p.stats.Renditions)
	st.PendingAudioFrames = len(p.pendingAudio)
	if p.tsInput != nil {
		st.DemuxQueueChunks = p.tsInput.QueuedChunks()
	}
	return st
}

// dropHeldAudioOnSwitch discards audio held from the previous input, which
// the new input can't be aligned with.
func (p *StreamPipeline) dropHeldAudioOnSwitch() {
	p.stats.DroppedAudioSwitch += uint64(len(p.pendingAudio))
	p.pendingAudio = nil
}
