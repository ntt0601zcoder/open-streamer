package rtsp

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rtpPkt(seq uint16, ssrc uint32) Packet {
	return Packet{RTP: &rtp.Packet{Header: rtp.Header{SequenceNumber: seq, SSRC: ssrc}}}
}

// drain pops every packet releasable at now.
func drain(r *reorder, now time.Time, out []Packet) []uint16 {
	var seqs []uint16
	for _, p := range out {
		seqs = append(seqs, p.RTP.SequenceNumber)
	}
	for {
		p, ok := r.pop(now)
		if !ok {
			return seqs
		}
		seqs = append(seqs, p.RTP.SequenceNumber)
	}
}

func TestReorder(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t.Run("in order passes", func(t *testing.T) {
		var r reorder
		var out []Packet
		for s := uint16(65533); s != 3; s++ { // wraps
			assert.Equal(t, pushQueued, r.push(rtpPkt(s, 1), t0, &out))
		}
		assert.Equal(t, []uint16{65533, 65534, 65535, 0, 1, 2}, drain(&r, t0, out))
	})
	t.Run("swap reordered", func(t *testing.T) {
		var r reorder
		var out []Packet
		r.push(rtpPkt(10, 1), t0, &out)
		assert.Equal(t, []uint16{10}, drain(&r, t0, nil))
		assert.Equal(t, pushQueued, r.push(rtpPkt(12, 1), t0, &out))
		assert.Empty(t, drain(&r, t0, nil), "held behind 11")
		assert.Equal(t, pushReordered, r.push(rtpPkt(11, 1), t0, &out))
		assert.Equal(t, []uint16{11, 12}, drain(&r, t0, out))
	})
	t.Run("duplicates", func(t *testing.T) {
		var r reorder
		var out []Packet
		r.push(rtpPkt(10, 1), t0, &out)
		r.push(rtpPkt(12, 1), t0, &out)
		assert.Equal(t, pushDup, r.push(rtpPkt(12, 1), t0, &out), "buffered")
		assert.Equal(t, []uint16{10}, drain(&r, t0, nil))
		assert.Equal(t, pushDup, r.push(rtpPkt(10, 1), t0, &out), "released")
	})
	t.Run("time bound skips a hole", func(t *testing.T) {
		var r reorder
		var out []Packet
		r.push(rtpPkt(10, 1), t0, &out)
		r.push(rtpPkt(13, 1), t0.Add(10*time.Millisecond), &out)
		r.push(rtpPkt(15, 1), t0.Add(20*time.Millisecond), &out)
		assert.Equal(t, []uint16{10}, drain(&r, t0.Add(100*time.Millisecond), nil))
		d, ok := r.deadline()
		require.True(t, ok)
		assert.Equal(t, t0.Add(210*time.Millisecond), d)
		assert.Equal(t, []uint16{13}, drain(&r, d, nil), "15 has waited 190 ms")
		d, ok = r.deadline()
		require.True(t, ok)
		assert.Equal(t, t0.Add(220*time.Millisecond), d)
		assert.Equal(t, []uint16{15}, drain(&r, d, nil))
		assert.Equal(t, pushLate, r.push(rtpPkt(11, 1), d, &out), "skipped")
		_, ok = r.deadline()
		assert.False(t, ok)
	})
	t.Run("span bound skips a hole", func(t *testing.T) {
		var r reorder
		var out []Packet
		r.push(rtpPkt(0, 1), t0, &out)
		drain(&r, t0, nil)
		for s := uint16(2); s <= reorderSpan; s++ {
			r.push(rtpPkt(s, 1), t0, &out)
		}
		assert.Empty(t, drain(&r, t0, out), "held behind 1")
		r.push(rtpPkt(reorderSpan+1, 1), t0, &out) // 1 falls out of the span
		assert.Empty(t, out)
		seqs := drain(&r, t0, nil)
		require.Len(t, seqs, reorderSpan)
		assert.Equal(t, uint16(2), seqs[0])
		assert.Equal(t, uint16(reorderSpan+1), seqs[len(seqs)-1])
	})
	t.Run("jump ahead within the window", func(t *testing.T) {
		var r reorder
		var out []Packet
		r.push(rtpPkt(0, 1), t0, &out)
		drain(&r, t0, nil)
		r.push(rtpPkt(3, 1), t0, &out)
		r.push(rtpPkt(1000, 1), t0, &out)
		assert.Equal(t, []uint16{3}, drain(&r, t0, out), "3 released, 1000 waits for 937..999")
		assert.Equal(t, []uint16{1000}, drain(&r, t0.Add(reorderWait), nil))
	})
	t.Run("restart on SSRC change and big jumps", func(t *testing.T) {
		var r reorder
		var out []Packet
		r.push(rtpPkt(100, 1), t0, &out)
		r.push(rtpPkt(102, 1), t0, &out)
		r.push(rtpPkt(500, 2), t0, &out)
		assert.Equal(t, []uint16{100, 102, 500}, drain(&r, t0, out), "old SSRC flushed first")
		out = nil
		r.push(rtpPkt(502, 2), t0, &out)
		r.push(rtpPkt(500+seqWindow+10, 2), t0, &out)
		assert.Equal(t, []uint16{502, 500 + seqWindow + 10}, drain(&r, t0, out))
		out = nil
		r.push(rtpPkt(10, 2), t0, &out) // far behind: a restart too
		assert.Equal(t, []uint16{10}, drain(&r, t0, out))
	})
	t.Run("RTP-Info hint", func(t *testing.T) {
		r := reorder{hint: 50, hasHint: true}
		var out []Packet
		r.push(rtpPkt(51, 1), t0, &out)
		assert.Empty(t, drain(&r, t0, nil), "waits for 50")
		assert.Equal(t, pushReordered, r.push(rtpPkt(50, 1), t0, &out))
		assert.Equal(t, []uint16{50, 51}, drain(&r, t0, nil))

		far := reorder{hint: 50, hasHint: true}
		far.push(rtpPkt(5000, 1), t0, &out)
		assert.Equal(t, []uint16{5000}, drain(&far, t0, nil), "a wrong hint is ignored")
	})
}

// udpClient is a UDP Client with one track and no connection, for driving
// ReadPacket directly.
func udpClient() *Client {
	c := &Client{udp: true, pkts: make(chan Packet, packetQueue), done: make(chan struct{}), tracks: []*clientTrack{{index: 0}}}
	c.ro = map[int]*reorder{0: {}}
	return c
}

func TestReadPacketReleasesOnTime(t *testing.T) {
	c := udpClient()
	c.pkts <- rtpPkt(1, 7)
	c.pkts <- rtpPkt(3, 7)
	c.pkts <- Packet{RTCP: []byte{0x80, 201}}
	p, err := c.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, uint16(1), p.RTP.SequenceNumber)
	start := time.Now()
	p, err = c.ReadPacket()
	require.NoError(t, err)
	assert.NotNil(t, p.RTCP, "RTCP is not held")
	p, err = c.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, uint16(3), p.RTP.SequenceNumber)
	waited := time.Since(start)
	assert.GreaterOrEqual(t, waited, reorderWait-10*time.Millisecond)
	assert.Less(t, waited, reorderWait+150*time.Millisecond, "released with no new packet")
}

func TestReadPacketAfterEnd(t *testing.T) {
	c := udpClient()
	c.pkts <- rtpPkt(1, 7)
	c.pkts <- rtpPkt(3, 7)
	c.pkts <- rtpPkt(2, 7)
	c.err = ErrIdle
	close(c.done)
	var seqs []uint16
	for {
		p, err := c.ReadPacket()
		if err != nil {
			require.ErrorIs(t, err, ErrIdle)
			break
		}
		seqs = append(seqs, p.RTP.SequenceNumber)
	}
	assert.Equal(t, []uint16{1, 2, 3}, seqs)
	assert.Equal(t, uint64(1), c.Stats().Reordered)
}

func TestQueueUDPDropsNewest(t *testing.T) {
	c := udpClient()
	for i := 0; i < packetQueue+5; i++ {
		c.queueUDP(rtpPkt(uint16(i), 1))
	}
	assert.Equal(t, uint64(5), c.Stats().Dropped)
	p := <-c.pkts
	assert.Equal(t, uint16(0), p.RTP.SequenceNumber, "the oldest is kept")
}

func TestUDPLinkAccepts(t *testing.T) {
	l := &udpLink{srvIP: net.IPv4(127, 0, 0, 1), srvPorts: [2]int{5000, 5001}}
	assert.True(t, l.accepts(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5000}))
	assert.True(t, l.accepts(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5001}))
	assert.False(t, l.accepts(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5002}))
	assert.False(t, l.accepts(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 5000}))
	assert.False(t, l.accepts(nil))
	anyPort := &udpLink{srvIP: net.IPv4(127, 0, 0, 1)}
	assert.True(t, anyPort.accepts(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}))
}
