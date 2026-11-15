package rtmp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Handshake layout. C1, S1, C2 and S2 are each handshakeSize bytes:
// time(4) + version(4) + 1528 bytes. In the digest handshake the 1528 bytes
// are a 764-byte key block and a 764-byte digest block; schema 0 puts the key
// block first (digest block at 8+764), schema 1 the digest block first (at 8).
// A digest block starts with four offset bytes whose sum mod 728 locates the
// 32-byte digest after them.
const (
	rtmpVersion     = 3
	handshakeSize   = 1536
	digestLen       = sha256.Size
	blockSize       = 764
	digestOffsetMod = blockSize - digestLen - 4 // 728
	clientVersion   = 0x80000702
	serverVersion   = 0x0D0E0A0D
)

// keyConstant is the public 32-byte tail shared by both handshake keys.
var keyConstant = []byte{
	0xF0, 0xEE, 0xC2, 0x4A, 0x80, 0x68, 0xBE, 0xE8,
	0x2E, 0x00, 0xD0, 0xD1, 0x02, 0x9E, 0x7E, 0x57,
	0x6E, 0xEC, 0x5D, 0x2D, 0x29, 0x80, 0x6F, 0xAB,
	0x93, 0xB8, 0xE6, 0x36, 0xCF, 0xEB, 0x31, 0xAE,
}

// clientKey is the 62-byte client key; its first 30 bytes sign C1.
var clientKey = append([]byte("Genuine Adobe Flash Player 001"), keyConstant...)

// serverKey is the 68-byte server key; its first 36 bytes sign S1 and the
// whole key derives the S2 signing key.
var serverKey = append([]byte("Genuine Adobe Flash Media Server 001"), keyConstant...)

// ServerHandshake answers an RTMP handshake. It reads exactly C0+C1 and C2
// from rw with io.ReadFull and never reads past C2, so a buffered reader
// passed as rw's read side keeps the bytes that follow. A C1 carrying a valid
// digest gets a digest S1/S2 in the same schema; any other C1 gets the simple
// handshake (random S1 with zero version, S2 echoing C1). C2 is not
// validated. Deadlines belong to the caller.
func ServerHandshake(rw io.ReadWriter, rnd io.Reader) error {
	c0c1 := make([]byte, 1+handshakeSize)
	if _, err := io.ReadFull(rw, c0c1); err != nil {
		return fmt.Errorf("rtmp: handshake: read C0+C1: %w", err)
	}
	if c0c1[0] != rtmpVersion {
		return fmt.Errorf("rtmp: handshake: unsupported version %d", c0c1[0])
	}
	c1 := c0c1[1:]

	out := make([]byte, 1+2*handshakeSize)
	out[0] = rtmpVersion
	s1 := out[1 : 1+handshakeSize]
	s2 := out[1+handshakeSize:]
	if _, err := io.ReadFull(rnd, s1[8:]); err != nil {
		return fmt.Errorf("rtmp: handshake: random: %w", err)
	}

	if schema, ok := findDigest(c1, clientKey[:30]); ok {
		binary.BigEndian.PutUint32(s1[4:8], serverVersion)
		signDigest(s1, schema, serverKey[:36])
		if _, err := io.ReadFull(rnd, s2[:handshakeSize-digestLen]); err != nil {
			return fmt.Errorf("rtmp: handshake: random: %w", err)
		}
		pos := digestOffset(c1, schema)
		key := hmacSHA256(serverKey, c1[pos:pos+digestLen])
		copy(s2[handshakeSize-digestLen:], hmacSHA256(key, s2[:handshakeSize-digestLen]))
	} else {
		copy(s2, c1)
	}

	if _, err := rw.Write(out); err != nil {
		return fmt.Errorf("rtmp: handshake: write S0+S1+S2: %w", err)
	}
	c2 := make([]byte, handshakeSize)
	if _, err := io.ReadFull(rw, c2); err != nil {
		return fmt.Errorf("rtmp: handshake: read C2: %w", err)
	}
	return nil
}

// ClientHandshake performs an RTMP handshake as the client. It sends a digest
// C1 (schema 1) and reads exactly S0+S1+S2. S2 is verified only when S1
// carries a valid digest. Deadlines belong to the caller.
func ClientHandshake(rw io.ReadWriter, rnd io.Reader) error {
	return clientHandshake(rw, rnd, 1)
}

// clientHandshake is ClientHandshake with a chosen C1 digest schema.
func clientHandshake(rw io.ReadWriter, rnd io.Reader, schema int) error {
	c0c1 := make([]byte, 1+handshakeSize)
	c0c1[0] = rtmpVersion
	c1 := c0c1[1:]
	if _, err := io.ReadFull(rnd, c1[8:]); err != nil {
		return fmt.Errorf("rtmp: handshake: random: %w", err)
	}
	binary.BigEndian.PutUint32(c1[4:8], clientVersion)
	signDigest(c1, schema, clientKey[:30])
	if _, err := rw.Write(c0c1); err != nil {
		return fmt.Errorf("rtmp: handshake: write C0+C1: %w", err)
	}

	resp := make([]byte, 1+2*handshakeSize)
	if _, err := io.ReadFull(rw, resp); err != nil {
		return fmt.Errorf("rtmp: handshake: read S0+S1+S2: %w", err)
	}
	if resp[0] != rtmpVersion {
		return fmt.Errorf("rtmp: handshake: unsupported version %d", resp[0])
	}
	s1 := resp[1 : 1+handshakeSize]
	s2 := resp[1+handshakeSize:]

	c2 := make([]byte, handshakeSize)
	if s1Schema, ok := findDigest(s1, serverKey[:36]); ok {
		pos := digestOffset(c1, schema)
		key := hmacSHA256(serverKey, c1[pos:pos+digestLen])
		want := hmacSHA256(key, s2[:handshakeSize-digestLen])
		if !hmac.Equal(want, s2[handshakeSize-digestLen:]) {
			return errors.New("rtmp: handshake: S2 digest mismatch")
		}
		if _, err := io.ReadFull(rnd, c2[:handshakeSize-digestLen]); err != nil {
			return fmt.Errorf("rtmp: handshake: random: %w", err)
		}
		sPos := digestOffset(s1, s1Schema)
		key = hmacSHA256(clientKey, s1[sPos:sPos+digestLen])
		copy(c2[handshakeSize-digestLen:], hmacSHA256(key, c2[:handshakeSize-digestLen]))
	} else {
		copy(c2, s1)
	}
	if _, err := rw.Write(c2); err != nil {
		return fmt.Errorf("rtmp: handshake: write C2: %w", err)
	}
	return nil
}

// digestOffset returns the absolute position of the 32-byte digest in a
// 1536-byte C1/S1 for the given schema: the digest block starts at 8 (schema
// 1) or 8+764 (schema 0), and the digest sits after its four offset bytes at
// (sum of the offset bytes) mod 728.
func digestOffset(b []byte, schema int) int {
	base := 8
	if schema == 0 {
		base = 8 + blockSize
	}
	sum := int(b[base]) + int(b[base+1]) + int(b[base+2]) + int(b[base+3])
	return base + 4 + sum%digestOffsetMod
}

// digestFor computes the HMAC-SHA256 of b with the 32 digest bytes at pos
// left out.
func digestFor(b []byte, pos int, key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(b[:pos])
	m.Write(b[pos+digestLen:])
	return m.Sum(nil)
}

// signDigest writes the digest of b into the schema's digest position.
func signDigest(b []byte, schema int, key []byte) {
	pos := digestOffset(b, schema)
	copy(b[pos:], digestFor(b, pos, key))
}

// findDigest reports which schema, if any, carries a valid digest in b.
func findDigest(b []byte, key []byte) (int, bool) {
	for _, schema := range []int{0, 1} {
		pos := digestOffset(b, schema)
		if hmac.Equal(b[pos:pos+digestLen], digestFor(b, pos, key)) {
			return schema, true
		}
	}
	return 0, false
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}
