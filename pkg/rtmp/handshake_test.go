package rtmp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// splitRW lets a test use one bufio.Reader as the read side of a conn, the
// way the session does.
type splitRW struct {
	io.Reader
	io.Writer
}

// recordingReader copies every byte read through it.
type recordingReader struct {
	r   io.Reader
	buf bytes.Buffer
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	rr.buf.Write(p[:n])
	return n, err
}

// recordingWriter copies every byte written through it.
type recordingWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

func (rw *recordingWriter) Write(p []byte) (int, error) {
	rw.buf.Write(p)
	return rw.w.Write(p)
}

// fixedRandom is a deterministic stand-in for the random source; the known
// answers in this file were computed for exactly these bytes.
func fixedRandom(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*31 + seed
	}
	return b
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pipe(t *testing.T) (client, server net.Conn) {
	t.Helper()
	c, s := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = c.SetDeadline(deadline)
	_ = s.SetDeadline(deadline)
	t.Cleanup(func() {
		_ = c.Close()
		_ = s.Close()
	})
	return c, s
}

func runServer(server net.Conn) <-chan error {
	done := make(chan error, 1)
	go func() { done <- ServerHandshake(server, rand.Reader) }()
	return done
}

func TestDigestOffsetFormula(t *testing.T) {
	b := make([]byte, handshakeSize)
	// Schema 1: digest block at 8, offset bytes 8..11.
	copy(b[8:12], []byte{1, 2, 3, 4})
	if got := digestOffset(b, 1); got != 8+4+10 {
		t.Fatalf("schema 1 offset = %d, want %d", got, 8+4+10)
	}
	// Schema 0: digest block at 772, offset bytes 772..775; 800 mod 728 = 72.
	copy(b[772:776], []byte{200, 200, 200, 200})
	if got := digestOffset(b, 0); got != 772+4+72 {
		t.Fatalf("schema 0 offset = %d, want %d", got, 772+4+72)
	}
	// Largest sum keeps the 32-byte digest inside the 764-byte block.
	copy(b[8:12], []byte{255, 255, 255, 255})
	if got := digestOffset(b, 1); got != 8+4+(1020%728) || got+32 > 8+764 {
		t.Fatalf("schema 1 max offset = %d", got)
	}
	copy(b[772:776], []byte{255, 255, 255, 255})
	if got := digestOffset(b, 0); got+32 > handshakeSize {
		t.Fatalf("schema 0 max offset = %d overruns C1", got)
	}
}

func TestHandshakeKeys(t *testing.T) {
	if len(clientKey) != 62 || string(clientKey[:30]) != "Genuine Adobe Flash Player 001" {
		t.Fatalf("client key: len %d, prefix %q", len(clientKey), clientKey[:30])
	}
	if len(serverKey) != 68 || string(serverKey[:36]) != "Genuine Adobe Flash Media Server 001" {
		t.Fatalf("server key: len %d, prefix %q", len(serverKey), serverKey[:36])
	}
	if !bytes.Equal(clientKey[30:], serverKey[36:]) {
		t.Fatal("client and server keys must share the 32-byte constant")
	}
}

func TestHandshakeOurClientOurServer(t *testing.T) {
	c, s := pipe(t)
	done := runServer(s)
	if err := ClientHandshake(c, rand.Reader); err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestHandshakeSimpleClient(t *testing.T) {
	c, s := pipe(t)
	done := runServer(s)

	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = 3
	if _, err := rand.Read(c0c1[9:]); err != nil {
		t.Fatal(err)
	}
	// c0c1[5:9] stays zero: a simple (non-digest) C1.
	if _, err := c.Write(c0c1); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 3 {
		t.Fatalf("S0 = %d, want 3", resp[0])
	}
	s1 := resp[1 : 1+handshakeSize]
	if v := binary.BigEndian.Uint32(s1[4:8]); v != 0 {
		t.Fatalf("simple S1 version = %#x, want 0", v)
	}
	if _, ok := findDigest(s1, serverKey[:36]); ok {
		t.Fatal("simple S1 must not carry a digest")
	}
	if s2 := resp[1+handshakeSize:]; !bytes.Equal(s2, c0c1[1:]) {
		t.Fatal("simple S2 must echo C1")
	}
	if _, err := c.Write(s1); err != nil { // C2 = S1 echo
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestHandshakeDigestBothSchemas(t *testing.T) {
	for _, schema := range []int{0, 1} {
		c, s := pipe(t)
		done := runServer(s)

		rec := &recordingReader{r: c}
		if err := clientHandshake(splitRW{rec, c}, rand.Reader, schema); err != nil {
			t.Fatalf("schema %d: client: %v", schema, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("schema %d: server: %v", schema, err)
		}
		got := rec.buf.Bytes()
		if len(got) != 1+2*handshakeSize {
			t.Fatalf("schema %d: client read %d bytes, want %d", schema, len(got), 1+2*handshakeSize)
		}
		s1 := got[1 : 1+handshakeSize]
		if v := binary.BigEndian.Uint32(s1[4:8]); v != serverVersion {
			t.Fatalf("schema %d: S1 version = %#x", schema, v)
		}
		sch, ok := findDigest(s1, serverKey[:36])
		if !ok || sch != schema {
			t.Fatalf("schema %d: S1 digest schema = %d (ok %v)", schema, sch, ok)
		}
	}
}

func TestHandshakeClientRejectsBadS2(t *testing.T) {
	c, s := pipe(t)
	go func() {
		c0c1 := make([]byte, 1+handshakeSize)
		if _, err := io.ReadFull(s, c0c1); err != nil {
			return
		}
		s1 := make([]byte, handshakeSize)
		_, _ = rand.Read(s1)
		binary.BigEndian.PutUint32(s1[4:8], serverVersion)
		signDigest(s1, 1, serverKey[:36])
		s2 := make([]byte, handshakeSize) // digest S1, unsigned S2
		_, _ = s.Write(append(append([]byte{3}, s1...), s2...))
	}()
	if err := ClientHandshake(c, rand.Reader); err == nil {
		t.Fatal("client accepted an S2 with a bad digest")
	}
}

func TestHandshakeBadVersion(t *testing.T) {
	in := make([]byte, 1+handshakeSize)
	in[0] = 6
	err := ServerHandshake(splitRW{bytes.NewReader(in), io.Discard}, rand.Reader)
	if err == nil {
		t.Fatal("server accepted C0 = 6")
	}

	resp := make([]byte, 1+2*handshakeSize)
	resp[0] = 6
	err = ClientHandshake(splitRW{bytes.NewReader(resp), io.Discard}, rand.Reader)
	if err == nil {
		t.Fatal("client accepted S0 = 6")
	}
}

func TestHandshakeTruncated(t *testing.T) {
	full := make([]byte, 1+2*handshakeSize) // C0 + C1 + C2
	full[0] = 3
	for _, n := range []int{0, 1, 100, 1 + handshakeSize, 1 + handshakeSize + 1535} {
		err := ServerHandshake(splitRW{bytes.NewReader(full[:n]), io.Discard}, rand.Reader)
		if err == nil {
			t.Fatalf("server: %d bytes accepted", n)
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("server: %d bytes: err = %v, want EOF", n, err)
		}
	}
	for _, n := range []int{0, 1, 1 + handshakeSize, 2 * handshakeSize} {
		err := ClientHandshake(splitRW{bytes.NewReader(full[:n]), io.Discard}, rand.Reader)
		if err == nil {
			t.Fatalf("client: %d bytes accepted", n)
		}
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("client: %d bytes: err = %v, want EOF", n, err)
		}
	}
	// A short random source is an error, not a panic.
	err := ServerHandshake(splitRW{bytes.NewReader(full), io.Discard}, bytes.NewReader(nil))
	if err == nil {
		t.Fatal("server: empty random source accepted")
	}
}

func TestHandshakeC2AndConnectOneWrite(t *testing.T) {
	c, s := pipe(t)
	br := bufio.NewReader(s)
	done := make(chan error, 1)
	go func() { done <- ServerHandshake(splitRW{br, s}, rand.Reader) }()

	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = 3
	if _, err := c.Write(c0c1); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(c, resp); err != nil {
		t.Fatal(err)
	}
	connect := make([]byte, 200)
	for i := range connect {
		connect[i] = byte(i + 1)
	}
	c2 := append(append([]byte{}, resp[1:1+handshakeSize]...), connect...)
	writeErr := make(chan error, 1)
	go func() {
		_, err := c.Write(c2) // C2 and the next 200 bytes in one Write
		writeErr <- err
	}()
	if err := <-done; err != nil {
		t.Fatalf("server: %v", err)
	}
	got := make([]byte, len(connect))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, connect) {
		t.Fatal("bytes after C2 were lost or changed")
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if br.Buffered() != 0 {
		t.Fatalf("%d unexpected bytes left buffered", br.Buffered())
	}
}

// The known answers below come from testdata/handshake_known_answers.py, run
// as
//
//	python3 -I testdata/handshake_known_answers.py testdata/ffmpeg_play_handshake.bin
//
// It reads both keys from FFmpeg's libavformat.so.62 (not from this package)
// and computes every digest with Python's hmac/hashlib.

func TestHandshakeKeysKnownAnswer(t *testing.T) {
	const (
		wantClient = "47656e75696e652041646f626520466c61736820506c6179657220303031" +
			"f0eec24a8068bee82e00d0d1029e7e576eec5d2d29806fab93b8e636cfeb31ae"
		wantServer = "47656e75696e652041646f626520466c617368204d656469612053657276657220303031" +
			"f0eec24a8068bee82e00d0d1029e7e576eec5d2d29806fab93b8e636cfeb31ae"
	)
	if got := hex.EncodeToString(clientKey); got != wantClient {
		t.Fatalf("clientKey = %s\nwant        %s", got, wantClient)
	}
	if got := hex.EncodeToString(serverKey); got != wantServer {
		t.Fatalf("serverKey = %s\nwant        %s", got, wantServer)
	}
}

// TestHandshakeFFmpegFixture replays a handshake captured from ffmpeg 8
// playing from our server (random source fixedRandom(3072, 2)). ffmpeg
// verified that S2 and sent connect after it.
func TestHandshakeFFmpegFixture(t *testing.T) {
	f, err := os.ReadFile("testdata/ffmpeg_play_handshake.bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 1+handshakeSize+1+2*handshakeSize+handshakeSize {
		t.Fatalf("fixture is %d bytes", len(f))
	}
	c0c1 := f[:1+handshakeSize]
	s0s1s2 := f[1+handshakeSize : 2+3*handshakeSize]
	c2 := f[2+3*handshakeSize:]
	c1 := c0c1[1:]
	s1 := s0s1s2[1 : 1+handshakeSize]

	// ffmpeg's C1 carries a schema 1 digest under the 30-byte client key.
	schema, ok := findDigest(c1, clientKey[:30])
	if !ok || schema != 1 {
		t.Fatalf("ffmpeg C1 digest: schema %d ok %v", schema, ok)
	}
	pos := digestOffset(c1, 1)
	if pos != 494 {
		t.Fatalf("ffmpeg C1 digest offset = %d, want 494", pos)
	}
	if got := c1[pos : pos+digestLen]; !bytes.Equal(got, mustHex(t, "650bfb9b65f953d848e3456965d0d76b627a84e35e9ddabd8b43f0fadaee458c")) {
		t.Fatalf("ffmpeg C1 digest = %x", got)
	}

	// Our server reproduces, byte for byte, the S0+S1+S2 ffmpeg accepted.
	var out bytes.Buffer
	in := append(append([]byte{}, c0c1...), c2...)
	if err := ServerHandshake(splitRW{bytes.NewReader(in), &out}, bytes.NewReader(fixedRandom(2*handshakeSize, 2))); err != nil {
		t.Fatalf("server: %v", err)
	}
	if !bytes.Equal(out.Bytes(), s0s1s2) {
		t.Fatal("server output differs from the S0+S1+S2 ffmpeg accepted")
	}
	if got := s0s1s2[1+2*handshakeSize-digestLen:]; !bytes.Equal(got, mustHex(t, "508b839994a05df5c920d49d6d41459845bf1aef1b2550a583c1a35cd2033a0a")) {
		t.Fatalf("S2 digest = %x", got)
	}

	// ffmpeg signed C2 with HMAC(full 62-byte client key, S1 digest).
	sPos := digestOffset(s1, 1)
	want := hmacSHA256(hmacSHA256(clientKey, s1[sPos:sPos+digestLen]), c2[:handshakeSize-digestLen])
	if !bytes.Equal(c2[handshakeSize-digestLen:], want) {
		t.Fatal("ffmpeg's C2 does not verify under the full client key")
	}
}

func TestHandshakeKnownAnswerBothSchemas(t *testing.T) {
	type answer struct {
		c1Pos, s1Pos                   int
		c1Digest, s1Digest, s2, c2Hash string
	}
	answers := map[int]answer{
		0: {
			c1Pos: 1494, s1Pos: 1498,
			c1Digest: "bdd2b15524961c0c4fb5442c3f34e1cc0162605ecabc216e7017490d4328b8e6",
			s1Digest: "e28a228eaf56f7c861ac9f05dea64ff425d98cf82e7d82ff552424a65de66c36",
			s2:       "71c212fc17b96e4603ab08bfc08d29593d837bee646b7c94e0847ca31bbf0286",
			c2Hash:   "bfcd7557f27de6c9ad420595afda6a37b9e814e60cc28936aa51af29921d8c73",
		},
		1: {
			c1Pos: 202, s1Pos: 206,
			c1Digest: "f5235b4285f815513fbb72a05292eac3020f507c2301f7deb0329a0f3baffcb3",
			s1Digest: "3277ac44a2a36b9da1976b77ac07569ab3d5b70fc97839a2c93b94c07e33d8b0",
			s2:       "6392666fd48227b5708ad171e5fe582138d5089ab31f75286ef808e817463c2c",
			c2Hash:   "9080ff7c4ebb5c0d24a815a370d5ea49648e0a2bdffce9ed61c8fae635111979",
		},
	}
	for _, schema := range []int{0, 1} {
		want := answers[schema]
		c, s := pipe(t)
		done := make(chan error, 1)
		go func() {
			done <- ServerHandshake(s, bytes.NewReader(fixedRandom(handshakeSize-8+handshakeSize-digestLen, 2)))
		}()
		rr := &recordingReader{r: c}
		rw := &recordingWriter{w: c}
		if err := clientHandshake(splitRW{rr, rw}, bytes.NewReader(fixedRandom(handshakeSize-8+handshakeSize-digestLen, 1)), schema); err != nil {
			t.Fatalf("schema %d: client: %v", schema, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("schema %d: server: %v", schema, err)
		}
		sent, got := rw.buf.Bytes(), rr.buf.Bytes()
		c1 := sent[1 : 1+handshakeSize]
		c2 := sent[1+handshakeSize:]
		s1 := got[1 : 1+handshakeSize]
		s2 := got[1+handshakeSize:]

		if p := digestOffset(c1, schema); p != want.c1Pos || !bytes.Equal(c1[p:p+digestLen], mustHex(t, want.c1Digest)) {
			t.Fatalf("schema %d: C1 digest at %d = %x", schema, p, c1[p:p+digestLen])
		}
		if p := digestOffset(s1, schema); p != want.s1Pos || !bytes.Equal(s1[p:p+digestLen], mustHex(t, want.s1Digest)) {
			t.Fatalf("schema %d: S1 digest at %d = %x", schema, p, s1[p:p+digestLen])
		}
		if tail := s2[handshakeSize-digestLen:]; !bytes.Equal(tail, mustHex(t, want.s2)) {
			t.Fatalf("schema %d: S2 digest = %x", schema, tail)
		}
		if tail := c2[handshakeSize-digestLen:]; !bytes.Equal(tail, mustHex(t, want.c2Hash)) {
			t.Fatalf("schema %d: C2 digest = %x", schema, tail)
		}
	}
}
