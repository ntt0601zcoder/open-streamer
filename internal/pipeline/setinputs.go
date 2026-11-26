package pipeline

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrNotRunning is SetInputs' error once Run has returned.
var ErrNotRunning = errors.New("pipeline: not running")

// lastGone is Pipeline.last once SetInputs removed the input that was active
// last: the next activation is a switch, whichever input it is.
const lastGone = -2

// edit is a SetInputs request; Run answers on ans, which has room for it.
type edit struct {
	urls []string
	ans  chan error
}

// SetInputs replaces the inputs, by priority, while Run runs, and returns
// once Run has applied the change (R-E3). An input whose URL stays keeps
// its reader and its retry state; one that goes is closed before SetInputs
// returns; the hub changes epoch only if the active input goes, once, when
// its successor delivers. Event.Input and Stats.Active index the new list
// from then on. On an error nothing changed. Safe from any goroutine; a
// call made before Run waits for it.
//
// Inputs are matched by URL text, duplicates in order. With no input active
// after the change, every added input opens at once; a kept input that
// waits out a backoff still waits. It must not be called from OnEvent,
// which runs in the Run goroutine.
func (p *Pipeline) SetInputs(ctx context.Context, urls []string) error {
	if len(urls) == 0 {
		return errors.New("pipeline: set inputs: no input")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e := edit{urls: slices.Clone(urls), ans: make(chan error, 1)}
	select {
	case <-p.done:
		return ErrNotRunning
	default:
	}
	select {
	case p.edits <- e:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return ErrNotRunning
	}
	// Run answers every request it takes before it does anything else, so
	// the answer is there even when Run has returned since.
	return <-e.ans
}

// Inputs returns the URLs in force, by priority. Safe from any goroutine.
func (p *Pipeline) Inputs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.opt.Inputs)
}

// setInputs applies a SetInputs request in the Run goroutine. The inputs
// that go are closed first, so an added input may reuse an address; schedule
// then opens what is due. Removing the active input flushes the Clock as a
// failure does, without an event or a backoff: the next activation's Break
// is the one epoch change.
func (p *Pipeline) setInputs(ctx context.Context, urls []string) error {
	passive := make([]bool, len(urls))
	for i, u := range urls {
		spec, err := p.opt.Parse(u)
		if err != nil {
			return fmt.Errorf("pipeline: set inputs: parse: %w", err)
		}
		passive[i] = spec.Passive
	}
	// Match each new URL, in order, to the first old entry of the same URL
	// that no earlier one took.
	old := p.opt.Inputs
	from := make([]int, len(urls)) // new index → old index, -1 for an added input
	to := make([]int, len(old))    // old index → new index, -1 for a removed input
	for j := range to {
		to[j] = -1
	}
	for i, u := range urls {
		from[i] = -1
		for j, o := range old {
			if o == u && to[j] < 0 {
				from[i], to[j] = j, i
				break
			}
		}
	}
	for j, in := range p.inputs {
		a := in.a
		if to[j] >= 0 || a == nil {
			continue
		}
		if a == p.act {
			p.deactivate(ctx, a, false)
		}
		p.stopAttempt(a)
	}
	inputs := make([]*input, len(urls))
	for i := range urls {
		if j := from[i]; j >= 0 {
			inputs[i] = p.inputs[j]
			if a := inputs[i].a; a != nil {
				a.in = i
			}
			continue
		}
		inputs[i] = &input{passive: passive[i]}
	}
	p.inputs = inputs
	if p.last >= 0 {
		if p.last = to[p.last]; p.last < 0 {
			p.last = lastGone
		}
	}
	p.mu.Lock()
	p.opt.Inputs = urls
	if p.act != nil {
		p.st.Active = p.act.in
	}
	p.mu.Unlock()
	if p.act == nil {
		// No search goes on over an edited list: every added input opens at
		// once, and the kept ones after their backoff.
		p.search = false
	} else {
		// As at an activation, for the inputs now below the active one.
		for _, in := range p.inputs[p.act.in+1:] {
			switch o := in.a; {
			case o == nil:
			case in.passive:
				o.deadline = time.Time{}
				o.resetProbe()
			default:
				p.stopAttempt(o)
			}
		}
	}
	p.rearm = true
	return nil
}
