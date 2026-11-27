//go:build race

package srtout

// raceEnabled reports a build with the race detector, which slows a gosrt
// receiver enough to need more latency for its repairs.
const raceEnabled = true
