package config

import (
	"bytes"
	"errors"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// ErrNoStream is DeleteStream's error for a name no entry has.
var ErrNoStream = errors.New("config: no such stream")

// SetStream returns text with the stream of sp's name replaced, or sp
// appended to streams (the key is added when it is missing). It edits the
// YAML node tree, so the comments above keys and entries and the order of
// everything else stay; see the package documentation for what the round
// trip loses. The entry is found by its name scalar alone, so an entry the
// lenient start skipped can be repaired; when several entries have the
// name, the first is replaced and the others removed. A text that holds
// any anchor, alias or merge key is refused with Errors, one per place,
// and is not edited: the edit could leave an alias with nothing to point
// at. SetStream does not validate sp or the result; Parse does.
func SetStream(text []byte, sp spec.Stream) ([]byte, error) {
	root, err := editable(text)
	if err != nil {
		return nil, err
	}
	m := topMapping(root)
	if m == nil {
		return nil, errors.New("config: set stream: the document is not a mapping")
	}
	seq, err := findStreams(m, true)
	if err != nil {
		return nil, fmt.Errorf("config: set stream: %w", err)
	}
	var n yaml.Node
	if err := n.Encode(sp); err != nil {
		return nil, fmt.Errorf("config: set stream: %w", err)
	}
	kept := make([]*yaml.Node, 0, len(seq.Content)+1)
	replaced := false
	for _, e := range seq.Content {
		if name, ok := entryName(e); ok && name == sp.Name {
			if replaced {
				continue
			}
			n.HeadComment, n.FootComment = e.HeadComment, e.FootComment
			*e = n
			replaced = true
		}
		kept = append(kept, e)
	}
	if !replaced {
		kept = append(kept, &n)
	}
	seq.Content = kept
	seq.Style &^= yaml.FlowStyle
	return encode(root, "set stream")
}

// DeleteStream returns text without the entries of the stream name, and
// their comments; ErrNoStream when there is none. It edits the node tree
// as SetStream does, with the same refusal of anchors, aliases and merge
// keys. Removing the last stream leaves "streams: []".
func DeleteStream(text []byte, name string) ([]byte, error) {
	root, err := editable(text)
	if err != nil {
		return nil, err
	}
	m := topMapping(root)
	if m == nil {
		return nil, ErrNoStream
	}
	seq, err := findStreams(m, false)
	if err != nil {
		return nil, fmt.Errorf("config: delete stream: %w", err)
	}
	if seq == nil {
		return nil, ErrNoStream
	}
	kept := make([]*yaml.Node, 0, len(seq.Content))
	for _, e := range seq.Content {
		if n, ok := entryName(e); !ok || n != name {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(seq.Content) {
		return nil, ErrNoStream
	}
	seq.Content = kept
	if len(kept) == 0 {
		seq.Style = yaml.FlowStyle
	}
	return encode(root, "delete stream")
}

// editable decodes text for an edit, refusing a *SyntaxError and any
// anchor, alias or merge key.
func editable(text []byte) (*yaml.Node, error) {
	if len(text) > MaxBytes {
		return nil, &SyntaxError{Err: fmt.Errorf("the text is larger than %d bytes", MaxBytes)}
	}
	root, err := decodeNode(text)
	if err != nil {
		return nil, &SyntaxError{Err: err}
	}
	if vs := walk(root, nil, false); len(vs) > 0 {
		errs := make(Errors, len(vs))
		for i, v := range vs {
			errs[i] = errors.New(v.String())
		}
		return nil, errs
	}
	return root, nil
}

// findStreams returns the top-level streams sequence of the mapping m.
// With create, a missing key is added and a null value made a sequence;
// without, either is nil. Any other value is an error.
func findStreams(m *yaml.Node, create bool) (*yaml.Node, error) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind != yaml.ScalarNode || k.Value != "streams" {
			continue
		}
		switch {
		case v.Kind == yaml.SequenceNode:
			return v, nil
		case v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null":
			if !create {
				return nil, nil
			}
			*v = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", HeadComment: v.HeadComment, LineComment: v.LineComment, FootComment: v.FootComment}
			return v, nil
		default:
			return nil, errors.New("streams is not a list")
		}
	}
	if !create {
		return nil, nil
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "streams"}, seq)
	return seq, nil
}

// encode writes the node tree back to text, indented by 2.
func encode(root *yaml.Node, op string) ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(root); err != nil {
		return nil, fmt.Errorf("config: %s: %w", op, err)
	}
	if err := e.Close(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", op, err)
	}
	return b.Bytes(), nil
}
