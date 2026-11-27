package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ntt0601zcoder/open-streamer/internal/api"
	"github.com/ntt0601zcoder/open-streamer/internal/asset"
	"github.com/ntt0601zcoder/open-streamer/internal/logging"
	"github.com/ntt0601zcoder/open-streamer/internal/stream"
)

// The font probe (R-E8): `<transcoder> -probe font FILE`.
const (
	// exitProbeFailed is the transcoder's exit status of a probe that
	// judged the file bad (cmd/ostranscoder).
	exitProbeFailed = 3
	// probeStdoutMax is the most of the probe's stdout kept; the rest is
	// read and dropped.
	probeStdoutMax = 4 << 10
	// probeReasonMax is the longest refusal reason taken from the probe.
	probeReasonMax = 256
	// probePrefix starts the probe's verdict line.
	probePrefix = "font: "
	// probeNoReason is the reason of a refusal without a verdict line.
	probeNoReason = "the transcoder could not load the font"
	// probeCrashed is the reason of a probe killed by a signal.
	probeCrashed = "the transcoder crashed on the font"
	// probeWaitDelay bounds the wait for the probe's stdout once it has
	// exited or been killed: a process it left behind may hold the pipe.
	probeWaitDelay = time.Second
)

// fontProbeLimit is how long a font probe may run. It is a seam: the tests
// lower it. New reads it once.
var fontProbeLimit = 10 * time.Second

// fontProber returns the asset store's ProbeFont: it runs binary with
// -probe font and the temp file's path under limit, env added to the
// environment. Only the probe's own verdict refuses a font: exit status 3,
// with the reason of its stdout line "font: …", or death by a signal that
// is not the context's kill. Anything else (another status, the limit, an
// end of ctx, an exec error) says nothing about the font and is an error,
// whose text names the binary and the status but never the temp path. The
// end of the caller's own ctx is that ctx's error, and the limit is an
// error of its own: the server tells a client that hung up from a probe
// that ran out of time.
func fontProber(binary string, env []string, limit time.Duration) func(ctx context.Context, path string) (bool, string, error) {
	return func(parent context.Context, path string) (bool, string, error) {
		ctx, cancel := context.WithTimeout(parent, limit)
		defer cancel()
		out := &cappedBuffer{max: probeStdoutMax}
		cmd := exec.CommandContext(ctx, binary, "-probe", "font", path)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout = out // stderr stays nil, the null device: it carries temp paths
		cmd.WaitDelay = probeWaitDelay
		err := cmd.Run()
		if err == nil {
			return false, "", nil
		}
		// A context kill is a SIGKILL: it must not read as a crash on the
		// font, so the context is asked before the wait status.
		if perr := parent.Err(); perr != nil {
			return false, "", fmt.Errorf("server: probe font with %s: %w", binary, perr)
		}
		if ctx.Err() != nil {
			return false, "", fmt.Errorf("server: probe font with %s: no verdict within %s", binary, limit)
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return false, "", fmt.Errorf("server: probe font: %w", err) // the exec error names the binary
		}
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return true, probeCrashed, nil
		}
		if exit.ExitCode() == exitProbeFailed {
			return true, probeReason(out.Bytes()), nil
		}
		return false, "", fmt.Errorf("server: probe font with %s: %s", binary, exit.ProcessState)
	}
}

// probeReason returns the reason of the first line of stdout that starts
// with "font: ", without that prefix, as valid UTF-8 without control
// characters, and cut to probeReasonMax bytes; or probeNoReason when there
// is none.
func probeReason(stdout []byte) string {
	for line := range strings.SplitSeq(string(stdout), "\n") {
		rest, ok := strings.CutPrefix(line, probePrefix)
		if !ok {
			continue
		}
		rest = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, strings.ToValidUTF8(rest, ""))
		rest = strings.TrimSpace(rest)
		if len(rest) > probeReasonMax {
			rest = strings.ToValidUTF8(rest[:probeReasonMax], "")
		}
		if rest == "" {
			break
		}
		return rest
	}
	return probeNoReason
}

// cappedBuffer keeps the first max bytes written to it and drops the rest,
// so that a probe that writes without end neither blocks nor grows memory.
type cappedBuffer struct {
	max int
	b   bytes.Buffer
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// Bytes returns what was kept.
func (c *cappedBuffer) Bytes() []byte { return c.b.Bytes() }

// assetDoc is the API document of inf, used by the streams users.
func assetDoc(inf asset.Info, users []string) api.Asset {
	return api.Asset{
		Name: inf.Name, Kind: string(inf.Kind), Type: inf.Type, Size: inf.Size,
		Modified: inf.Modified, ETag: inf.ETag, UsedBy: users,
	}
}

// Assets returns every asset of the directory, by name, each with the
// streams that name it.
func (s *Server) Assets() ([]api.Asset, error) {
	list, err := s.assets.List()
	if err != nil {
		return nil, err
	}
	m := s.Manager()
	out := make([]api.Asset, len(list))
	for i, inf := range list {
		out[i] = assetDoc(inf, m.AssetUsers(inf.Name))
	}
	return out, nil
}

// Asset returns the asset as an open regular file: a replacement while it
// is read leaves it reading the old bytes.
func (s *Server) Asset(name string) (api.Asset, io.ReadSeekCloser, error) {
	f, inf, err := s.assets.Open(name)
	if err != nil {
		return api.Asset{}, nil, err
	}
	return assetDoc(inf, s.Manager().AssetUsers(name)), f, nil
}

// PutAsset stores the body under name, then has the streams that name it
// take the new bytes. The read, the checks and the font probe run without
// the apply mutex; the refresh runs under it, so a refresh and a stream
// edit are in one order. Still under it, when a stream left stopped names
// the asset, the text in force is applied again, as PutStream does for an
// unchanged document (nothing is written), which tries those streams
// again: their outcomes are Retried. An error of that apply is logged and
// leaves Retried empty: the asset is stored either way. Not while the text
// in force holds entries the start skipped: its strict parse would fail
// every time, for the errors load_errors already reports.
//
// An asset.ErrWrite can come after the rename, with the new bytes in place,
// so the streams are refreshed then too, and the error returned. A probe
// cut short by the end of ctx (the client went away) is that ctx's error,
// not asset.ErrProbe: it says nothing about the probe.
func (s *Server) PutAsset(ctx context.Context, name string, body io.Reader, ifMatch string, ifNoneMatch bool) (api.AssetPut, bool, error) {
	inf, created, err := s.assets.Put(ctx, name, body, ifMatch, ifNoneMatch)
	if err != nil && errors.Is(err, asset.ErrProbe) && ctx.Err() != nil {
		return api.AssetPut{}, false, fmt.Errorf("server: put asset: %w", ctx.Err())
	}
	if err != nil && !errors.Is(err, asset.ErrWrite) {
		return api.AssetPut{}, false, err
	}
	s.cfg.Lock()
	defer s.cfg.Unlock()
	m := s.Manager()
	refreshed := m.RefreshAsset(ctx, name)
	var retried []stream.Outcome
	switch names := s.stoppedNaming(name); {
	case len(names) == 0:
	case s.skipped:
		s.log.Debug("server: assets: the config holds entries the start skipped: the streams left stopped are not tried again",
			"asset", name)
	default:
		res, aerr := s.apply(ctx, s.applied, true)
		if aerr != nil {
			s.log.Error("server: assets: the streams left stopped were not tried again",
				"asset", name, "error", logging.Scrub(aerr.Error()))
		}
		for _, o := range res.Streams {
			if slices.Contains(names, o.Name) {
				retried = append(retried, o)
			}
		}
	}
	if err != nil {
		return api.AssetPut{}, false, err
	}
	return api.AssetPut{
		Asset:     assetDoc(inf, m.AssetUsers(name)),
		Refreshed: refreshed,
		Retried:   retried,
	}, created, nil
}

// stoppedNaming returns the streams left stopped whose spec names the
// asset. The caller holds s.cfg.
func (s *Server) stoppedNaming(name string) []string {
	m := s.Manager()
	var out []string
	for _, st := range m.Stopped() {
		if sp, ok := m.Get(st.Name); ok && slices.Contains(sp.Assets(), name) {
			out = append(out, st.Name)
		}
	}
	return out
}

// DeleteAsset removes the asset, under the apply mutex, so that it cannot
// cross an edit that starts using it. It is refused while a stream names
// it (enabled or not), and while the text in force holds an entry the
// start skipped: the manager cannot see what such an entry names.
func (s *Server) DeleteAsset(_ context.Context, name, ifMatch string) error {
	if !s.assetsOn {
		return asset.ErrOff
	}
	s.cfg.Lock()
	defer s.cfg.Unlock()
	if s.skipped {
		return api.ErrSkippedEntries
	}
	if users := s.Manager().AssetUsers(name); len(users) > 0 {
		return &api.AssetInUseError{Streams: users}
	}
	return s.assets.Delete(name, ifMatch)
}
