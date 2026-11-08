package publisher

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	mediaSeqRe = regexp.MustCompile(`#EXT-X-MEDIA-SEQUENCE:(\d+)`)
	segNameRe  = regexp.MustCompile(`(?m)^seg_(\d+)\.ts$`)
)

// parseManifestSeq returns EXT-X-MEDIA-SEQUENCE and the numeric suffix of the
// first listed segment.
func parseManifestSeq(t *testing.T, manifest string) (mediaSeq, firstSeg int) {
	t.Helper()
	m := mediaSeqRe.FindStringSubmatch(manifest)
	require.NotNil(t, m, "manifest has no EXT-X-MEDIA-SEQUENCE")
	s := segNameRe.FindStringSubmatch(manifest)
	require.NotNil(t, s, "manifest lists no segment")
	mediaSeq, err := strconv.Atoi(m[1])
	require.NoError(t, err)
	firstSeg, err = strconv.Atoi(s[1])
	require.NoError(t, err)
	return mediaSeq, firstSeg
}

func segEntries(from, to int) []hlsSegEntry {
	var out []hlsSegEntry
	for i := from; i <= to; i++ {
		out = append(out, hlsSegEntry{name: fmt.Sprintf("seg_%06d.ts", i), dur: 2.0})
	}
	return out
}

// The playlist's EXT-X-MEDIA-SEQUENCE must equal the number of the first
// segment it lists, otherwise players map sequence numbers to the wrong URIs.
func TestHLSSegmenter_MediaSequenceMatchesFirstListedSegment(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		segN   uint64
		window int
		onDisk []hlsSegEntry
		want   int
	}{
		{name: "full window", segN: 10, window: 3, onDisk: segEntries(8, 10), want: 8},
		{name: "window shorter than on-disk history", segN: 20, window: 3, onDisk: segEntries(12, 20), want: 18},
		{name: "fewer segments than window", segN: 2, window: 5, onDisk: segEntries(1, 2), want: 1},
		{name: "first segment only", segN: 1, window: 5, onDisk: segEntries(1, 1), want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			p := &hlsSegmenter{
				streamID:     "test",
				streamDir:    dir,
				manifestPath: filepath.Join(dir, "index.m3u8"),
				segSec:       2,
				window:       tc.window,
				segN:         tc.segN,
				onDisk:       tc.onDisk,
			}
			require.NoError(t, p.writeManifestLocked())
			data, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
			require.NoError(t, err)
			seq, first := parseManifestSeq(t, string(data))
			require.Equal(t, first, seq, "media sequence must equal first listed segment number")
			require.Equal(t, tc.want, seq)
		})
	}
}

// A failed segment write must not consume a segment number: the next
// published segment follows the last listed one without a gap, so the media
// sequence keeps matching the listed names.
func TestHLSSegmenter_FailedWriteDoesNotSkipSegmentNumber(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "missing")
	p := &hlsSegmenter{
		streamID:     "test",
		streamDir:    dir,
		manifestPath: filepath.Join(root, "index.m3u8"),
		segSec:       2,
		window:       5,
	}

	p.segBuf = append(p.segBuf, 0x47, 0x00)
	p.flushLocked() // fails: directory does not exist
	require.Empty(t, p.onDisk)

	require.NoError(t, os.MkdirAll(dir, 0o755))
	p.segBuf = append(p.segBuf, 0x47, 0x00)
	p.flushLocked()
	p.segBuf = append(p.segBuf, 0x47, 0x00)
	p.flushLocked()

	require.Len(t, p.onDisk, 2)
	require.Equal(t, "seg_000001.ts", p.onDisk[0].name)
	require.Equal(t, "seg_000002.ts", p.onDisk[1].name)

	data, err := os.ReadFile(filepath.Join(root, "index.m3u8"))
	require.NoError(t, err)
	seq, first := parseManifestSeq(t, string(data))
	require.Equal(t, 1, first)
	require.Equal(t, 1, seq)
}
