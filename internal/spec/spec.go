package spec

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/ntt0601zcoder/open-streamer/internal/redact"
)

// Limits on one document and on one config.
const (
	MaxInputs     = 8
	MaxRenditions = 8
	MaxPushes     = 8
	MaxURLBytes   = 2048
	MaxSpecBytes  = 1 << 20
	MaxStreams    = 1000 // streams in one config
)

// Stream is one stream's document.
type Stream struct {
	Name      string     `json:"name" yaml:"name"`
	Disabled  bool       `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	Inputs    []Input    `json:"inputs" yaml:"inputs"`                     // by priority, first highest
	Layout    []string   `json:"layout,omitempty" yaml:"layout,omitempty"` // "video", "audio"; empty means both
	Transcode *Transcode `json:"transcode,omitempty" yaml:"transcode,omitempty"`
	Thumbnail *Thumbnail `json:"thumbnail,omitempty" yaml:"thumbnail,omitempty"`
	Outputs   Outputs    `json:"outputs" yaml:"outputs"`
}

// Input is one source of a stream; the ingest URL forms apply.
type Input struct {
	URL string `json:"url" yaml:"url"`
}

// Transcode is a stream's ladder.
type Transcode struct {
	Renditions []Rendition `json:"renditions" yaml:"renditions"`
	Audio      Audio       `json:"audio" yaml:"audio"`
	Watermark  *Watermark  `json:"watermark,omitempty" yaml:"watermark,omitempty"`
}

// Rendition is one rung of a ladder: a copy of the source, or an encode.
type Rendition struct {
	Label          string `json:"label" yaml:"label"`
	Copy           bool   `json:"copy,omitempty" yaml:"copy,omitempty"`
	Height         int    `json:"height,omitempty" yaml:"height,omitempty"`
	BitrateKbps    int    `json:"bitrate_kbps,omitempty" yaml:"bitrate_kbps,omitempty"`
	MaxBitrateKbps int    `json:"max_bitrate_kbps,omitempty" yaml:"max_bitrate_kbps,omitempty"`
	BFrames        int    `json:"b_frames,omitempty" yaml:"b_frames,omitempty"`
	Preset         string `json:"preset,omitempty" yaml:"preset,omitempty"`
	Encoder        string `json:"encoder,omitempty" yaml:"encoder,omitempty"` // "", "x264", "nvenc"
}

// Audio is the audio of every rung of a ladder.
type Audio struct {
	Copy        bool `json:"copy,omitempty" yaml:"copy,omitempty"`
	BitrateKbps int  `json:"bitrate_kbps,omitempty" yaml:"bitrate_kbps,omitempty"`
	SampleRate  int  `json:"sample_rate,omitempty" yaml:"sample_rate,omitempty"`
	Channels    int  `json:"channels,omitempty" yaml:"channels,omitempty"`
}

// Watermark is a text or an image drawn on every encoded rung.
type Watermark struct {
	Text       string `json:"text,omitempty" yaml:"text,omitempty"`
	Image      string `json:"image,omitempty" yaml:"image,omitempty"`       // an asset name (R-C8)
	Position   string `json:"position,omitempty" yaml:"position,omitempty"` // "tl" (default), "tr", "bl", "br", "c"
	OpacityPct int    `json:"opacity_pct,omitempty" yaml:"opacity_pct,omitempty"`
	SizePct    int    `json:"size_pct,omitempty" yaml:"size_pct,omitempty"`
	TimeZone   string `json:"time_zone,omitempty" yaml:"time_zone,omitempty"`
}

// Thumbnail is the operator preview of the source.
type Thumbnail struct {
	IntervalSec int `json:"interval_sec,omitempty" yaml:"interval_sec,omitempty"`
	Height      int `json:"height,omitempty" yaml:"height,omitempty"`
	Quality     int `json:"quality,omitempty" yaml:"quality,omitempty"`
}

// Outputs are the outputs a stream is served on, and where it is pushed.
type Outputs struct {
	HLS    bool   `json:"hls,omitempty" yaml:"hls,omitempty"`   // hls/* and cmaf/*.m3u8
	DASH   bool   `json:"dash,omitempty" yaml:"dash,omitempty"` // cmaf/*.mpd
	HTTPTS bool   `json:"httpts,omitempty" yaml:"httpts,omitempty"`
	RTMP   bool   `json:"rtmp,omitempty" yaml:"rtmp,omitempty"`
	RTSP   bool   `json:"rtsp,omitempty" yaml:"rtsp,omitempty"`
	SRT    bool   `json:"srt,omitempty" yaml:"srt,omitempty"`
	Push   []Push `json:"push,omitempty" yaml:"push,omitempty"`
}

// Push is one target the stream is pushed to.
type Push struct {
	URL string `json:"url" yaml:"url"`
}

// nameRE is the shape of a stream name and a rendition label (R-D3).
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// deviceNames are the Windows device names, which are not safe file names.
var deviceNames = []string{
	"con", "prn", "aux", "nul",
	"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
	"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9",
}

// ValidName reports whether s may name a stream or label a rendition: it
// matches ^[a-z0-9][a-z0-9-]{0,62}$ and is not a Windows device name.
func ValidName(s string) bool {
	return nameRE.MatchString(s) && !slices.Contains(deviceNames, s)
}

// Decode reads one JSON document of at most MaxSpecBytes. Unknown fields
// and data after the document are errors. It does not validate.
func Decode(r io.Reader) (Stream, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxSpecBytes+1))
	if err != nil {
		return Stream{}, fmt.Errorf("spec: decode: %w", err)
	}
	if len(body) > MaxSpecBytes {
		return Stream{}, fmt.Errorf("spec: decode: the document is larger than %d bytes", MaxSpecBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var s Stream
	if err := dec.Decode(&s); err != nil {
		return Stream{}, fmt.Errorf("spec: decode: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Stream{}, errors.New("spec: decode: data after the document")
	}
	return s, nil
}

// Redacted returns a deep copy of s with every input and push URL passed
// through redact.URL, for responses and logs.
func (s Stream) Redacted() Stream {
	c := s.clone()
	for i := range c.Inputs {
		c.Inputs[i].URL = redact.URL(c.Inputs[i].URL)
	}
	for i := range c.Outputs.Push {
		c.Outputs.Push[i].URL = redact.URL(c.Outputs.Push[i].URL)
	}
	return c
}

// ETag is the document's entity tag: `"` + the first 16 hex digits of
// redact.Tag of its JSON + `"`. Nil and empty slices are encoded alike, so
// documents that Diff finds equal have equal ETags. The tag is keyed with
// a key that is random per process, because a viewer gets it beside the
// redacted document and must not be able to test guesses of the secrets
// against it; so it is the same for the same document within one process,
// and changes when the server restarts.
func (s Stream) ETag() string {
	b, err := json.Marshal(s.normal())
	if err != nil {
		// A Stream holds only strings, numbers, booleans, slices and
		// pointers to structs of those, which always encode.
		panic(fmt.Sprintf("spec: etag: %v", err))
	}
	return `"` + hex.EncodeToString(redact.Tag(b)[:8]) + `"`
}

// Served returns the names the stream is served under (R-D3): its name,
// then for a ladder <name>_<label> for each rendition in order.
func (s Stream) Served() []string {
	names := []string{s.Name}
	if s.Transcode != nil {
		for _, r := range s.Transcode.Renditions {
			names = append(names, s.Name+"_"+r.Label)
		}
	}
	return names
}

// clone returns a deep copy of s; an empty slice stays empty.
func (s Stream) clone() Stream {
	s.Inputs = slices.Clone(s.Inputs)
	s.Layout = slices.Clone(s.Layout)
	if s.Transcode != nil {
		t := *s.Transcode
		t.Renditions = slices.Clone(t.Renditions)
		if t.Watermark != nil {
			w := *t.Watermark
			t.Watermark = &w
		}
		s.Transcode = &t
	}
	if s.Thumbnail != nil {
		th := *s.Thumbnail
		s.Thumbnail = &th
	}
	s.Outputs.Push = slices.Clone(s.Outputs.Push)
	return s
}

// normal returns a deep copy of s with every empty slice nil.
func (s Stream) normal() Stream {
	c := s.clone()
	c.Inputs = nilIfEmpty(c.Inputs)
	c.Layout = nilIfEmpty(c.Layout)
	if c.Transcode != nil {
		c.Transcode.Renditions = nilIfEmpty(c.Transcode.Renditions)
	}
	c.Outputs.Push = nilIfEmpty(c.Outputs.Push)
	return c
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}
