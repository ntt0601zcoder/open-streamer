package publisher

import "bytes"

// alignedFeed appends b to *carry and dispatches each complete 188-byte MPEG-TS
// packet (sync-byte aligned) to onPacket.  onPacket returns false to abort
// processing early (e.g. on a write error).
//
// Alignment rules:
//   - Drop leading bytes until a 0x47 sync byte is found.
//   - Double-sync guard: if the byte at offset +188 is not 0x47, the current
//     byte is not a valid sync — advance by one and retry.
//   - If no sync is found, retain up to the last 187 bytes as carry so that a
//     sync that straddles two callback invocations is not lost.
func alignedFeed(b []byte, carry *[]byte, onPacket func([]byte) bool) {
	*carry = append(*carry, b...)
	for len(*carry) >= 188 {
		if (*carry)[0] != 0x47 {
			idx := bytes.IndexByte(*carry, 0x47)
			if idx < 0 {
				if len(*carry) > 187 {
					*carry = (*carry)[len(*carry)-187:]
				}
				return
			}
			*carry = (*carry)[idx:]
			if len(*carry) < 188 {
				return
			}
		}
		// Double-sync guard: next packet must also start with 0x47.
		if len(*carry) >= 376 && (*carry)[188] != 0x47 {
			*carry = (*carry)[1:]
			continue
		}
		if !onPacket((*carry)[:188]) {
			return
		}
		*carry = (*carry)[188:]
	}
}
