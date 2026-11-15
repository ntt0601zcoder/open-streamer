package rtmp

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
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
