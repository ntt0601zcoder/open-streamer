package mpegts

// crcPoly is the CRC-32/MPEG-2 generator polynomial (ISO/IEC 13818-1 Annex
// B), processed most significant bit first, without reflection.
const crcPoly = 0x04C11DB7

// crcTable holds the CRC of every byte value shifted into the top of the
// register.
var crcTable = makeCRCTable()

func makeCRCTable() *[256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ crcPoly
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return &t
}

// crc32MPEG2 returns the CRC-32/MPEG-2 of b: initial value 0xFFFFFFFF, no
// reflection and no final XOR. Over a whole PSI section including its
// CRC_32 field the result is zero.
func crc32MPEG2(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, v := range b {
		c = c<<8 ^ crcTable[byte(c>>24)^v]
	}
	return c
}
