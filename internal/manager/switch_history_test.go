package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/domain"
)

// recordSwitch keeps newest at index 0 and caps at maxSwitchHistory. This
// is the contract RuntimeStatus.Switches exposes — frontend reads
// Switches[0] for the most recent switch.
func TestRecordSwitch_OrderingAndCap(t *testing.T) {
	t.Parallel()
	state := &streamState{}
	base := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 7; i++ {
		recordSwitch(state, SwitchEvent{
			At:     base.Add(time.Duration(i) * time.Second),
			From:   i,
			To:     i + 1,
			Reason: SwitchReasonError,
			Detail: switchDetail(i),
		})
	}

	require.Len(t, state.switchHistory, maxSwitchHistory,
		"ring buffer must cap at %d", maxSwitchHistory)
	assert.Equal(t, 6, state.switchHistory[0].From, "newest entry at index 0")
	assert.Equal(t, "switch-6", state.switchHistory[0].Detail)
	assert.Equal(t, 2, state.switchHistory[maxSwitchHistory-1].From,
		"oldest survivor at end")
	assert.Equal(t, base.Add(6*time.Second), state.switchHistory[0].At)
}

// ──────────────────────────────────────────────────────────────────────────
// End-to-end coverage — every callsite of tryFailover must produce a switch
// entry with the right reason. The cases below pin one path each.
// ──────────────────────────────────────────────────────────────────────────

// Error path: ingestor reports a failure on the active input → degrade →
// failover with reason=error and detail=err.Error().
func TestSwitchHistory_RecordsErrorReason(t *testing.T) {
	t.Parallel()
	svc, ing := newSvc(t)
	require.NoError(t, svc.Register(context.Background(),
		streamWithInputs("s_err", "rtmp://primary", "rtmp://backup"), ""))

	svc.ReportInputError("s_err", 0, errors.New("rtmp connection reset"))

	require.Eventually(t, func() bool {
		return len(ing.startCallsCopy()) >= 2
	}, 2*time.Second, 20*time.Millisecond)

	rt, _ := svc.RuntimeStatus("s_err")
	require.Len(t, rt.Switches, 1)
	assert.Equal(t, 0, rt.Switches[0].From)
	assert.Equal(t, 1, rt.Switches[0].To)
	assert.Equal(t, SwitchReasonError, rt.Switches[0].Reason)
	assert.Equal(t, "rtmp connection reset", rt.Switches[0].Detail)
}

// Manual path: SwitchInput API call → failover with reason=manual and no
// detail (operator action has no implicit context).
func TestSwitchHistory_RecordsManualReason(t *testing.T) {
	t.Parallel()
	svc, ing := newSvc(t)
	require.NoError(t, svc.Register(context.Background(),
		streamWithInputs("s_man", "rtmp://primary", "rtmp://backup"), ""))

	require.NoError(t, svc.SwitchInput("s_man", 1))
	require.Eventually(t, func() bool {
		return len(ing.startCallsCopy()) >= 2
	}, 2*time.Second, 20*time.Millisecond)

	rt, _ := svc.RuntimeStatus("s_man")
	require.Len(t, rt.Switches, 1)
	assert.Equal(t, SwitchReasonManual, rt.Switches[0].Reason)
	assert.Empty(t, rt.Switches[0].Detail)
	assert.Equal(t, 1, rt.Switches[0].To)
}

// input_removed path: UpdateInputs deletes the active input → failover
// with reason=input_removed (takes precedence over input_added if both
// happen in the same call).
func TestSwitchHistory_RecordsInputRemovedReason(t *testing.T) {
	t.Parallel()
	svc, ing := newSvc(t)
	require.NoError(t, svc.Register(context.Background(),
		streamWithInputs("s_rm", "rtmp://primary", "rtmp://backup"), ""))

	// Remove the active priority 0 → backup at priority 1 must take over.
	svc.UpdateInputs("s_rm", nil, []domain.Input{{Priority: 0, URL: "rtmp://primary"}}, nil)

	require.Eventually(t, func() bool {
		return len(ing.startCallsCopy()) >= 2
	}, 2*time.Second, 20*time.Millisecond)

	rt, _ := svc.RuntimeStatus("s_rm")
	require.Len(t, rt.Switches, 1)
	assert.Equal(t, SwitchReasonInputRemoved, rt.Switches[0].Reason)
	assert.Equal(t, 1, rt.Switches[0].To)
}

// input_added path: UpdateInputs adds a higher-priority input while a
// lower-priority is active → failover with reason=input_added.
func TestSwitchHistory_RecordsInputAddedReason(t *testing.T) {
	t.Parallel()
	svc, ing := newSvc(t)
	// Start with a single backup input at priority 5.
	stream := &domain.Stream{
		Code:   "s_add",
		Inputs: []domain.Input{{Priority: 5, URL: "rtmp://backup"}},
	}
	require.NoError(t, svc.Register(context.Background(), stream, ""))

	// Now add a higher-priority (lower number) input.
	svc.UpdateInputs("s_add", []domain.Input{{Priority: 1, URL: "rtmp://primary"}}, nil, nil)

	require.Eventually(t, func() bool {
		return len(ing.startCallsCopy()) >= 2
	}, 2*time.Second, 20*time.Millisecond)

	rt, _ := svc.RuntimeStatus("s_add")
	require.Len(t, rt.Switches, 1)
	assert.Equal(t, SwitchReasonInputAdded, rt.Switches[0].Reason)
	assert.Equal(t, 5, rt.Switches[0].From)
	assert.Equal(t, 1, rt.Switches[0].To)
}

// Switches snapshot must be a defensive copy — caller mutating the slice
// must not affect future RuntimeStatus reads.
func TestSwitchHistory_DefensiveCopy(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t)
	require.NoError(t, svc.Register(context.Background(),
		streamWithInputs("s_def", "rtmp://primary", "rtmp://backup"), ""))
	svc.ReportInputError("s_def", 0, errors.New("first"))

	require.Eventually(t, func() bool {
		rt, _ := svc.RuntimeStatus("s_def")
		return len(rt.Switches) > 0
	}, 2*time.Second, 20*time.Millisecond)

	first, _ := svc.RuntimeStatus("s_def")
	require.Len(t, first.Switches, 1)
	first.Switches[0].Reason = "tampered"

	second, _ := svc.RuntimeStatus("s_def")
	require.Len(t, second.Switches, 1)
	assert.Equal(t, SwitchReasonError, second.Switches[0].Reason,
		"caller mutation of the returned slice must not leak back into state")
}

// Switches must not be emitted for a freshly-registered stream that
// hasn't switched yet — Switches[0] otherwise would be the initial
// activation, which the user explicitly excluded from the history.
func TestSwitchHistory_NoEntryForInitialActivation(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t)
	require.NoError(t, svc.Register(context.Background(),
		streamWithInputs("s_init", "rtmp://primary"), ""))
	rt, _ := svc.RuntimeStatus("s_init")
	assert.Empty(t, rt.Switches, "first activation must not appear in switch history")
}

func switchDetail(i int) string {
	return "switch-" + string(rune('0'+i))
}
