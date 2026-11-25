package spec

import "reflect"

// Change is how an edit to a stream is applied (R-D6).
type Change uint8

// The ways an edit is applied.
const (
	ChangeNone    Change = iota // the documents are equal
	ChangeOutputs               // they differ only in outputs: applied in place
	ChangeRebuild               // they differ elsewhere: the stream is rebuilt
)

// Diff returns how the edit from old to new is applied: ChangeNone when
// the documents are equal, ChangeOutputs when they differ only in outputs
// (a reordering of pushes included), and ChangeRebuild otherwise. Nil and
// empty slices are equal.
func Diff(old, new Stream) Change {
	o, n := old.normal(), new.normal()
	oo, no := o.Outputs, n.Outputs
	o.Outputs, n.Outputs = Outputs{}, Outputs{}
	switch {
	case !reflect.DeepEqual(o, n):
		return ChangeRebuild
	case !reflect.DeepEqual(oo, no):
		return ChangeOutputs
	default:
		return ChangeNone
	}
}
