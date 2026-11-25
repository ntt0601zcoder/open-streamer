package config

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// fixtureLibrary is a config already in the library's own formatting:
// 2-space indent, no blank lines, one space before a line comment.
func fixtureLibrary() string {
	return fixtureLibraryHead() + `  # stream b
  - name: b
    inputs:
      - url: rtmp://src.example/live/b
    outputs:
      hls: true
`
}

// fixtureLibraryHead is fixtureLibrary up to stream b's entry.
func fixtureLibraryHead() string {
	return `# header
version: 1
# above server
server:
  rtmp:
    listen: ":1935"
users:
  - name: ops # the operator
    role: editor
    password: "` + hashAt(1000) + `"
streams:
  # stream a
  - name: a
    # inside a
    inputs:
      - url: rtmp://src.example/live/a
`
}

// fixtureHand is a hand-formatted config: blank lines between sections,
// 4-space sequences, aligned line comments, a line comment on b's name.
func fixtureHand() string {
	return `# header
version: 1

# above server
server:
    rtmp:
        listen: ":1935"      # rtmp port

users:
    -   name: ops            # the operator
        role: editor
        password: "` + hashAt(1000) + `"

# the streams
streams:
    # stream a
    -   name: a
        # inside a
        inputs:
            -   url: rtmp://src.example/live/a

    # stream b
    -   name: b              # b's name
        # inside b
        inputs:
            -   url: rtmp://src.example/live/b
`
}

// newB is the replacement of stream b in the edit tests.
var newB = spec.Stream{
	Name:    "b",
	Inputs:  []spec.Input{{URL: "rtmp://src.example/live/b2"}},
	Outputs: spec.Outputs{HLS: true, DASH: true},
}

// newBYAML is newB's entry as SetStream writes it.
const newBYAML = `  - name: b
    inputs:
      - url: rtmp://src.example/live/b2
    outputs:
      hls: true
      dash: true
`

// reencode is text after one node round trip with the encoder's settings.
func reencode(t *testing.T, text string) string {
	t.Helper()
	var n yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(text), &n))
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	require.NoError(t, e.Encode(&n))
	require.NoError(t, e.Close())
	return b.String()
}

// streamNames parses text and returns its stream names in order.
func streamNames(t *testing.T, text []byte) []string {
	t.Helper()
	p, err := Parse(text, testOpt)
	require.NoError(t, err, string(text))
	names := make([]string, 0, len(p.Doc.Streams))
	for _, s := range p.Doc.Streams {
		names = append(names, s.Name)
	}
	return names
}

func TestSetStreamKeepsComments(t *testing.T) {
	t.Run("library formatting", func(t *testing.T) {
		in := fixtureLibrary()
		require.Equal(t, in, reencode(t, in), "the fixture must be in the library's formatting")
		out, err := SetStream([]byte(in), newB)
		require.NoError(t, err)
		assert.Equal(t, fixtureLibraryHead()+"  # stream b\n"+newBYAML, string(out))
		p, err := Parse(out, testOpt)
		require.NoError(t, err)
		assert.Equal(t, newB, p.Doc.Streams[1])
	})

	t.Run("hand formatting", func(t *testing.T) {
		in := fixtureHand()
		_, err := Parse([]byte(in), testOpt)
		require.NoError(t, err)
		out, err := SetStream([]byte(in), newB)
		require.NoError(t, err)
		s := string(out)
		p, err := Parse(out, testOpt)
		require.NoError(t, err)
		assert.Equal(t, newB, p.Doc.Streams[1])

		// Every head comment survives, in order, with the keys and streams.
		order := []string{
			"# header", "version: 1", "# above server", "server:", `listen: ":1935" # rtmp port`,
			"users:", "- name: ops # the operator", "# the streams", "streams:",
			"# stream a", "- name: a", "# inside a", "# stream b", "- name: b",
		}
		last := -1
		for _, o := range order {
			i := strings.Index(s, o)
			require.Greater(t, i, last, "%q out of order or missing in\n%s", o, s)
			last = i
		}
		// The losses, pinned so that a library upgrade changing them is noticed.
		assert.NotContains(t, s, "\n\n", "blank lines are dropped")
		assert.Contains(t, s, "\n  rtmp:\n    listen:", "mappings are re-indented to 2")
		assert.Contains(t, s, "\nusers:\n  - name: ops", "sequences are re-indented to 2")
		assert.NotContains(t, s, "# inside b", "comments inside the replaced entry are lost")
		assert.NotContains(t, s, "# b's name", "a line comment on the replaced name is lost")
		assert.Contains(t, s, "  # stream b\n"+newBYAML)
	})

	t.Run("append", func(t *testing.T) {
		c := spec.Stream{Name: "c", Inputs: []spec.Input{{URL: "rtmp://src.example/live/c"}}}
		out, err := SetStream([]byte(fixtureLibrary()), c)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"}, streamNames(t, out))
		assert.True(t, strings.HasPrefix(string(out), fixtureLibrary()), string(out))
	})

	t.Run("no streams key", func(t *testing.T) {
		out, err := SetStream([]byte(head()), newB)
		require.NoError(t, err)
		assert.Equal(t, []string{"b"}, streamNames(t, out))
		for _, empty := range []string{"streams:\n", "streams: []\n", "streams: null\n"} {
			out, err := SetStream([]byte(head()+empty), newB)
			require.NoError(t, err)
			assert.Equal(t, []string{"b"}, streamNames(t, out))
			assert.Contains(t, string(out), "streams:\n"+newBYAML)
		}
	})

	t.Run("a name two entries have", func(t *testing.T) {
		in := head() + "streams:\n" + streamYAML("b") + streamYAML("a") + streamYAML("b", "    disabled: true")
		out, err := SetStream([]byte(in), newB)
		require.NoError(t, err)
		assert.Equal(t, []string{"b", "a"}, streamNames(t, out))
		p, _ := Parse(out, testOpt)
		assert.Equal(t, newB, p.Doc.Streams[0])
	})

	t.Run("number-like text is quoted", func(t *testing.T) {
		s := spec.Stream{
			Name:   "007",
			Inputs: []spec.Input{{URL: "rtmp://src.example/live/007"}},
			Transcode: &spec.Transcode{
				Renditions: []spec.Rendition{{Label: "720", Height: 720, BitrateKbps: 800}},
				Watermark:  &spec.Watermark{Text: "24.7"},
			},
		}
		out, err := SetStream([]byte(head()), s)
		require.NoError(t, err)
		p, err := Parse(out, testOpt)
		require.NoError(t, err, string(out))
		assert.Equal(t, s, p.Doc.Streams[0])
	})

	t.Run("bad text", func(t *testing.T) {
		_, err := SetStream([]byte("version: [\n"), newB)
		var se *SyntaxError
		require.ErrorAs(t, err, &se)
		_, err = SetStream(nil, newB)
		require.ErrorAs(t, err, &se)
		_, err = SetStream([]byte("- 1\n"), newB)
		require.ErrorContains(t, err, "config: set stream: the document is not a mapping")
		_, err = SetStream([]byte(head()+"streams: 5\n"), newB)
		require.ErrorContains(t, err, "config: set stream: streams is not a list")
		_, err = DeleteStream([]byte(head()+"streams: {a: 1}\n"), "a")
		require.ErrorContains(t, err, "config: delete stream: streams is not a list")
		_, err = DeleteStream([]byte("version: [\n"), "a")
		require.ErrorAs(t, err, &se)
	})
}

func TestSetStreamRepairsSkippedEntry(t *testing.T) {
	in := head() + "streams:\n" + streamYAML("cam", "    colour: red") + streamYAML("other")
	p, skipped, err := ParseLenient([]byte(in), testOpt)
	require.NoError(t, err)
	require.Len(t, skipped, 1)
	require.Len(t, p.Doc.Streams, 1)

	cam := spec.Stream{Name: "cam", Inputs: []spec.Input{{URL: "rtmp://src.example/live/cam"}}}
	out, err := SetStream([]byte(in), cam)
	require.NoError(t, err)
	assert.Equal(t, []string{"cam", "other"}, streamNames(t, out))

	out, err = DeleteStream([]byte(in), "cam")
	require.NoError(t, err)
	assert.Equal(t, []string{"other"}, streamNames(t, out))

	nameless := head() + "streams:\n  - inputs: [{url: rtmp://a/live/x}]\n  - name: [x]\n"
	_, err = DeleteStream([]byte(nameless), "")
	require.ErrorIs(t, err, ErrNoStream)
	_, err = DeleteStream([]byte(nameless), "x")
	require.ErrorIs(t, err, ErrNoStream)
	out, err = SetStream([]byte(nameless), cam)
	require.NoError(t, err)
	var doc struct {
		Streams []yaml.Node `yaml:"streams"`
	}
	require.NoError(t, yaml.Unmarshal(out, &doc))
	assert.Len(t, doc.Streams, 3, "the nameless entries stay and cam is appended")
}

func TestNodeEditsRefuseAnchors(t *testing.T) {
	merged := head() + "streams:\n" +
		"  - &src\n" + // 7
		"    name: src\n" +
		"    inputs:\n" +
		"      - url: rtmp://a/live/src\n" +
		"  - {<<: *src, name: ali}\n" + // 11
		streamYAML("other")
	user := "version: 1\nusers:\n  - &u\n    name: ops\n    role: editor\n    password: \"" + hashAt(1000) + "\"\n" +
		"streams:\n" + streamYAML("src") + streamYAML("ali") + streamYAML("other")
	for name, c := range map[string]struct {
		text  string
		lines []string
	}{
		"merge":  {merged, []string{"line 7: anchors, aliases and merge keys are not supported", "line 11: anchors, aliases and merge keys are not supported"}},
		"a user": {user, []string{"line 3: anchors, aliases and merge keys are not supported"}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, s := range []string{"src", "ali", "other"} {
				sp := spec.Stream{Name: s, Inputs: []spec.Input{{URL: "rtmp://a/live/" + s}}}
				out, err := SetStream([]byte(c.text), sp)
				var es Errors
				require.ErrorAs(t, err, &es, s)
				assert.Nil(t, out)
				assert.Len(t, es, len(c.lines))
				for _, l := range c.lines {
					assert.Contains(t, err.Error(), l)
				}
				out, err = DeleteStream([]byte(c.text), s)
				require.ErrorAs(t, err, &es, s)
				assert.Nil(t, out)
				assert.Len(t, es, len(c.lines))
				for _, l := range c.lines {
					assert.Contains(t, err.Error(), l)
				}
			}
		})
	}
}

func TestDeleteStream(t *testing.T) {
	out, err := DeleteStream([]byte(fixtureLibrary()), "b")
	require.NoError(t, err)
	assert.Equal(t, fixtureLibraryHead(), string(out))

	_, err = DeleteStream([]byte(fixtureLibrary()), "nope")
	require.ErrorIs(t, err, ErrNoStream)
	_, err = DeleteStream([]byte(head()), "b")
	require.ErrorIs(t, err, ErrNoStream)

	out, err = DeleteStream(out, "a")
	require.NoError(t, err)
	assert.Contains(t, string(out), "\nstreams: []\n")
	assert.Empty(t, streamNames(t, out))

	dup := head() + "streams:\n" + streamYAML("b") + streamYAML("a") + streamYAML("b")
	out, err = DeleteStream([]byte(dup), "b")
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, streamNames(t, out))
}
