package spec

import "reflect"

// Change is how an edit to a stream is applied (R-D6). The classes are
// ordered by how much of the stream an edit touches.
type Change uint8

// The ways an edit is applied.
const (
	ChangeNone    Change = iota // the documents are equal
	ChangeOutputs               // they differ only in outputs: applied in place
	ChangeLive                  // they differ in inputs or transcode.watermark, maybe in outputs, and nowhere else: applied to the running pipeline
	ChangeRebuild               // they differ elsewhere: the stream is rebuilt
)

// Diff returns how the edit from old to new is applied: ChangeNone when
// the documents are equal; ChangeOutputs when they differ only in outputs
// (a reordering of pushes included); ChangeLive when they differ in the
// inputs (their order included) or, when both have a ladder, in its
// watermark (added and removed included), and maybe in outputs, but
// nowhere else; and ChangeRebuild otherwise. Nil and empty slices are
// equal, and so are an empty layout and video then audio.
func Diff(old, new Stream) Change {
	o, n := old.normal(), new.normal()
	if reflect.DeepEqual(o, n) {
		return ChangeNone
	}
	o.Outputs, n.Outputs = Outputs{}, Outputs{}
	if reflect.DeepEqual(o, n) {
		return ChangeOutputs
	}
	// o and n are deep copies: clearing their fields leaves old and new as
	// they were.
	o.Inputs, n.Inputs = nil, nil
	if o.Transcode != nil && n.Transcode != nil {
		o.Transcode.Watermark, n.Transcode.Watermark = nil, nil
	}
	if reflect.DeepEqual(o, n) {
		return ChangeLive
	}
	return ChangeRebuild
}
