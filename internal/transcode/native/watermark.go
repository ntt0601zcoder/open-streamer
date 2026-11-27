//go:build libav

package native

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/asticode/go-astiav"

	"github.com/ntt0601zcoder/open-streamer/internal/transcode/picture"
)

// Watermark sizes in pixels.
const (
	minFontPx     = 8 // smallest text glyph height
	minMarkW      = 2 // narrowest image mark
	borderDivisor = 16
)

// safePath is what a path the child writes into a filter-graph string may
// hold: nothing a graph parser treats as syntax.
var safePath = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// graphKey is what a watermark graph is built for, besides the watermark.
type graphKey struct {
	w, h int
	pf   astiav.PixelFormat
	sar  astiav.Rational
	cr   astiav.ColorRange
	cs   astiav.ColorSpace
}

// watermarker draws one watermark on decoded pictures through a libavfilter
// graph, built for one watermark and one picture format and rebuilt when
// either changes. Text goes through drawtext from files the child writes,
// the text and the font, so neither enters the graph string, which is the
// same whatever the font.
type watermarker struct {
	now func() time.Time
	// dir holds font.ttf and text; "" until a text watermark needs it.
	dir string
	// font is what <dir>/font.ttf holds as last written: the built-in
	// dejaVuSans, or the operator's font, the slice the watermark carried.
	// It is the one reference the child keeps to that font (R-E8): the
	// resolved wm has none, and the callers drop theirs once set returns.
	// nil until the directory has a font.
	font []byte

	wm  *picture.Watermark // resolved, without Font; nil when off
	loc *time.Location
	img *image.NRGBA // the decoded image of an image watermark

	written    string // the text in <dir>/text
	hasWritten bool

	graph *astiav.FilterGraph // nil until the next picture
	key   graphKey
	src   *astiav.BuffersrcFilterContext
	sink  *astiav.BuffersinkFilterContext
	out   *astiav.Frame
	desc  string // the graph string, for tests
}

func newWatermarker(now func() time.Time) *watermarker {
	if now == nil {
		now = time.Now
	}
	return &watermarker{now: now}
}

// active reports whether a watermark is set.
func (m *watermarker) active() bool { return m.wm != nil }

// set validates w and makes it the watermark from the next picture; nil
// removes it. A text watermark's font (the built-in one when w.Font is
// empty) replaces <dir>/font.ttf when it differs from the one there; nil
// and an image watermark drop the font and remove font.ttf (dropFont). An
// error leaves the current watermark in place. The watermarker keeps
// w.Font, which nobody may change afterwards; the caller may drop its own
// reference.
func (m *watermarker) set(w *picture.Watermark) error {
	if w == nil {
		m.wm, m.loc, m.img = nil, nil, nil
		m.freeGraph()
		m.dropFont()
		return nil
	}
	if err := w.Validate(); err != nil {
		return err
	}
	r := w.Resolved()
	loc, err := r.Location()
	if err != nil {
		return err
	}
	var img *image.NRGBA
	if len(r.Image) > 0 {
		if img, err = picture.DecodeImage(r.Image); err != nil {
			return fmt.Errorf("watermark: %w", err)
		}
	} else {
		if err := m.ensureDir(); err != nil {
			return err
		}
		if err := m.syncFont(r.Font); err != nil {
			return err
		}
	}
	r.Font = nil
	m.wm, m.loc, m.img = &r, loc, img
	m.freeGraph()
	if img != nil {
		m.dropFont()
	}
	return nil
}

// dropFont forgets the font, so that an operator's font is not held while
// no text watermark needs it, and removes <dir>/font.ttf, best effort: the
// next text watermark writes its font again, since m.font is nil. Call it
// after freeGraph.
func (m *watermarker) dropFont() {
	m.font = nil
	if m.dir != "" {
		_ = os.Remove(filepath.Join(m.dir, "font.ttf"))
	}
}

// syncFont makes <dir>/font.ttf hold font, the built-in one when empty,
// unless it already does.
func (m *watermarker) syncFont(font []byte) error {
	if len(font) == 0 {
		font = dejaVuSans
	}
	if m.font != nil && bytes.Equal(font, m.font) {
		return nil
	}
	if err := m.writeFont(font); err != nil {
		return err
	}
	m.font = font
	return nil
}

// writeFont replaces <dir>/font.ttf with font atomically, through
// font.tmp, making the directory again if it is gone. A graph that has the
// old file open keeps reading it.
func (m *watermarker) writeFont(font []byte) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("watermark: temp dir: %w", err)
	}
	tmp := filepath.Join(m.dir, "font.tmp")
	if err := os.WriteFile(tmp, font, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("watermark: write font: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, "font.ttf")); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("watermark: write font: %w", err)
	}
	return nil
}

// wmDirPrefix starts the name of a watermarker's private directory in the
// temp dir: ostx-wm-<pid>-<random>, pid being the child's.
const wmDirPrefix = "ostx-wm-"

// wmDirName matches a private directory's name, capturing the PID: at
// most 7 digits, since Linux's pid_max is at most 4194304. A longer one
// is no child's, and kill takes a 32-bit pid, so 2147483648 would reach
// the kernel as a process group and 4294967297 as pid 1.
var wmDirName = regexp.MustCompile(`^` + wmDirPrefix + `([1-9][0-9]{0,6})-[0-9]+$`)

// ensureDir creates the private directory, after sweeping the directories
// that dead children left behind (sweepStaleDirs): a child killed or
// crashed never runs close. set writes the font into it.
func (m *watermarker) ensureDir() error {
	if m.dir != "" {
		return nil
	}
	sweepStaleDirs(os.TempDir())
	dir, err := os.MkdirTemp("", fmt.Sprintf("%s%d-", wmDirPrefix, os.Getpid()))
	if err != nil {
		return fmt.Errorf("watermark: temp dir: %w", err)
	}
	if !safePath.MatchString(dir) {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("watermark: temp dir %q has a character outside [A-Za-z0-9_./-]", dir)
	}
	m.dir = dir
	return nil
}

// sweepStaleDirs removes the private directories in tmp whose child is
// gone: a directory (not a symlink) owned by this user, named
// ostx-wm-<pid>-<random>, whose PID no process has (pidGone). Nothing else
// is touched, and a failure only leaves the directory for a later sweep.
func sweepStaleDirs(tmp string) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	uid := os.Getuid()
	for _, e := range entries {
		sub := wmDirName.FindStringSubmatch(e.Name())
		if sub == nil || !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(sub[1])
		if err != nil || pid == os.Getpid() || !pidGone(pid) {
			continue
		}
		path := filepath.Join(tmp, e.Name())
		fi, err := os.Lstat(path)
		if err != nil || !fi.IsDir() {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != uid {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("watermark: removing a dead child's temp dir", "dir", path, "err", err)
		}
	}
}

// pidGone reports whether no process has pid: kill(pid, 0) fails with
// ESRCH. Any other answer (success, or EPERM for another user's process)
// means it is alive. pid must be positive: 0 and negatives name groups.
func pidGone(pid int) bool {
	if pid <= 0 {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// apply draws the watermark on fr and returns the result, valid until the
// next call. fr is not changed. Exactly one picture comes out, with fr's
// PTS.
func (m *watermarker) apply(fr *astiav.Frame) (*astiav.Frame, error) {
	if m.wm.Text != "" {
		if err := m.writeText(); err != nil {
			return nil, err
		}
	}
	k := graphKey{
		w: fr.Width(), h: fr.Height(), pf: fr.PixelFormat(), sar: fr.SampleAspectRatio(),
		cr: fr.ColorRange(), cs: fr.ColorSpace(),
	}
	if m.graph == nil || k != m.key {
		m.freeGraph()
		if err := m.build(k, fr.Pts()); err != nil {
			m.freeGraph()
			return nil, err
		}
		m.key = k
	}
	m.out.Unref()
	if err := m.src.AddFrame(fr, astiav.NewBuffersrcFlags(astiav.BuffersrcFlagKeepRef)); err != nil {
		return nil, fmt.Errorf("watermark: add frame: %w", err)
	}
	err := m.sink.GetFrame(m.out, astiav.NewBuffersinkFlags())
	switch {
	case errors.Is(err, astiav.ErrEagain):
		return nil, errors.New("watermark: graph held a frame")
	case err != nil:
		return nil, fmt.Errorf("watermark: get frame: %w", err)
	case m.out.Pts() != fr.Pts():
		return nil, fmt.Errorf("watermark: graph returned PTS %d for %d", m.out.Pts(), fr.Pts())
	}
	return m.out, nil
}

// writeText formats the text for now and, when it changed, replaces
// <dir>/text atomically.
func (m *watermarker) writeText() error {
	s := picture.FormatText(m.wm.Text, m.now().In(m.loc))
	if m.hasWritten && s == m.written {
		return nil
	}
	if err := m.writeTextFile(s); err != nil {
		return err
	}
	m.written, m.hasWritten = s, true
	return nil
}

// writeTextFile replaces <dir>/text with s atomically, making the directory
// again if it is gone.
func (m *watermarker) writeTextFile(s string) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("watermark: temp dir: %w", err)
	}
	tmp := filepath.Join(m.dir, "text.tmp")
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return fmt.Errorf("watermark: write text: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(m.dir, "text")); err != nil {
		return fmt.Errorf("watermark: write text: %w", err)
	}
	return nil
}

// restoreFiles writes the font and the text again if they are gone, as when
// a temp cleaner ages the directory on a long stream: drawtext would
// otherwise fall back to fontconfig for the font (against R-C7) and fail on
// the text. The font written is the current one, the operator's included.
func (m *watermarker) restoreFiles() error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("watermark: temp dir: %w", err)
	}
	switch _, err := os.Stat(filepath.Join(m.dir, "font.ttf")); {
	case errors.Is(err, fs.ErrNotExist):
		font := m.font
		if font == nil {
			font = dejaVuSans
		}
		if err := m.writeFont(font); err != nil {
			return err
		}
		m.font = font
	case err != nil:
		return fmt.Errorf("watermark: font: %w", err)
	}
	switch _, err := os.Stat(filepath.Join(m.dir, "text")); {
	case errors.Is(err, fs.ErrNotExist) && m.hasWritten:
		return m.writeTextFile(m.written)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("watermark: text: %w", err)
	}
	return nil
}

// drawtextError is the error of a text graph drawtext could not be
// created for, with libav's error. drawtext loads the font as it is
// created: a font FreeType refuses fails here (fontconfig knows no font to
// fall back to, noSystemFonts; libav's error is then fontconfig's ENOENT),
// as does an unreadable text file or any other failure of drawtext's init.
type drawtextError struct{ libav error }

func (e *drawtextError) Error() string {
	return "drawtext could not load the font or read the text: " + e.libav.Error()
}

func (e *drawtextError) Unwrap() error { return e.libav }

// opacity is OpacityPct as a libavfilter alpha with two decimals.
func opacity(pct int) string { return fmt.Sprintf("%.2f", float64(pct)/100) }

// textGraph is the drawtext graph string for a picture of height h: only
// numbers and the child's own paths.
func (m *watermarker) textGraph(h int) string {
	w := m.wm
	px := max(minFontPx, int(math.Round(float64(h)*float64(w.SizePct)/100)))
	a := opacity(w.OpacityPct)
	x, y := picture.PositionExpr(w.Position, "w", "h", "tw", "th", picture.Margin(h))
	reload := ""
	if picture.HasTime(w.Text) {
		reload = ":reload=1"
	}
	return fmt.Sprintf("drawtext=fontfile=%s:textfile=%s:expansion=none%s:fontsize=%d"+
		":fontcolor=white@%s:borderw=%d:bordercolor=black@%s:x=%s:y=%s,format=yuv420p",
		filepath.Join(m.dir, "font.ttf"), filepath.Join(m.dir, "text"), reload, px,
		a, max(1, px/borderDivisor), a, x, y)
}

// imageGraph is the overlay graph string for a picture of width w and
// height h.
func (m *watermarker) imageGraph(w, h int) string {
	wm := m.wm
	mw := max(minMarkW, int(math.Round(float64(w)*float64(wm.SizePct)/100)))
	x, y := picture.PositionExpr(wm.Position, "W", "H", "w", "h", picture.Margin(h))
	return fmt.Sprintf("[wm]scale=%d:-1,format=yuva420p,colorchannelmixer=aa=%s[o];"+
		"[in][o]overlay=%s:%s:format=auto,format=yuv420p", mw, opacity(wm.OpacityPct), x, y)
}

// build makes the graph for pictures of k, the first of which is at pts.
// On an error the caller frees what was built.
func (m *watermarker) build(k graphKey, pts int64) error {
	if m.img == nil {
		if err := m.restoreFiles(); err != nil {
			return err
		}
	}
	g := astiav.AllocFilterGraph()
	if g == nil {
		return errors.New("watermark: alloc filter graph")
	}
	m.graph = g
	src, err := newSource(g, "in", k)
	if err != nil {
		return err
	}
	m.src = src
	sink, err := g.NewBuffersinkFilterContext(astiav.FindFilterByName("buffersink"), "out")
	if err != nil {
		return fmt.Errorf("watermark: buffersink: %w", err)
	}
	m.sink = sink

	outputs := astiav.AllocFilterInOut()
	defer outputs.Free()
	outputs.SetName("in")
	outputs.SetFilterContext(src.FilterContext())
	outputs.SetPadIdx(0)
	outputs.SetNext(nil)
	inputs := astiav.AllocFilterInOut()
	defer inputs.Free()
	inputs.SetName("out")
	inputs.SetFilterContext(sink.FilterContext())
	inputs.SetPadIdx(0)
	inputs.SetNext(nil)

	var mark *astiav.BuffersrcFilterContext
	if m.img != nil {
		b := m.img.Bounds()
		mark, err = newSource(g, "wm", graphKey{
			w: b.Dx(), h: b.Dy(), pf: astiav.PixelFormatRgba, sar: astiav.NewRational(1, 1),
		})
		if err != nil {
			return err
		}
		wmOut := astiav.AllocFilterInOut()
		wmOut.SetName("wm")
		wmOut.SetFilterContext(mark.FilterContext())
		wmOut.SetPadIdx(0)
		wmOut.SetNext(nil)
		outputs.SetNext(wmOut) // freed with outputs
		m.desc = m.imageGraph(k.w, k.h)
	} else {
		m.desc = m.textGraph(k.h)
	}
	if err := g.Parse(m.desc, inputs, outputs); err != nil {
		if m.img == nil {
			return fmt.Errorf("watermark: parse graph: %w", &drawtextError{err})
		}
		return fmt.Errorf("watermark: parse graph: %w", err)
	}
	if err := g.Configure(); err != nil {
		return fmt.Errorf("watermark: configure graph: %w", err)
	}
	if mark != nil {
		if err := m.addImage(mark, pts); err != nil {
			return err
		}
	}
	if m.out == nil {
		m.out = astiav.AllocFrame()
	}
	return nil
}

// newSource adds a buffer source for pictures of k, in 90 kHz time.
func newSource(g *astiav.FilterGraph, name string, k graphKey) (*astiav.BuffersrcFilterContext, error) {
	src, err := g.NewBuffersrcFilterContext(astiav.FindFilterByName("buffer"), name)
	if err != nil {
		return nil, fmt.Errorf("watermark: buffer %s: %w", name, err)
	}
	par := astiav.AllocBuffersrcFilterContextParameters()
	defer par.Free()
	par.SetWidth(k.w)
	par.SetHeight(k.h)
	par.SetPixelFormat(k.pf)
	par.SetSampleAspectRatio(k.sar)
	par.SetTimeBase(timeBase)
	par.SetColorRange(k.cr)
	par.SetColorSpace(k.cs)
	if err := src.SetParameters(par); err != nil {
		return nil, fmt.Errorf("watermark: buffer %s parameters: %w", name, err)
	}
	if err := src.Initialize(nil); err != nil {
		return nil, fmt.Errorf("watermark: buffer %s init: %w", name, err)
	}
	return src, nil
}

// addImage adds the image once, at pts, the PTS of the graph's first
// picture, and ends the mark's input, so that overlay repeats it on that
// picture and every later one, whatever their sign.
func (m *watermarker) addImage(mark *astiav.BuffersrcFilterContext, pts int64) error {
	b := m.img.Bounds()
	f := astiav.AllocFrame()
	defer f.Free()
	f.SetWidth(b.Dx())
	f.SetHeight(b.Dy())
	f.SetPixelFormat(astiav.PixelFormatRgba)
	f.SetSampleAspectRatio(astiav.NewRational(1, 1))
	if err := f.AllocBuffer(0); err != nil {
		return fmt.Errorf("watermark: alloc image frame: %w", err)
	}
	if err := f.Data().FromImage(m.img); err != nil {
		return fmt.Errorf("watermark: image frame: %w", err)
	}
	f.SetPts(pts)
	if err := mark.AddFrame(f, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("watermark: add image: %w", err)
	}
	if err := mark.AddFrame(nil, astiav.NewBuffersrcFlags()); err != nil {
		return fmt.Errorf("watermark: end image: %w", err)
	}
	return nil
}

// freeGraph frees the graph; the next picture builds a new one.
func (m *watermarker) freeGraph() {
	if m.out != nil {
		m.out.Unref()
	}
	if m.graph != nil {
		m.graph.Free()
	}
	m.graph, m.src, m.sink, m.key = nil, nil, nil, graphKey{}
}

// close frees everything and removes the private directory. It may be
// called more than once.
func (m *watermarker) close() {
	m.freeGraph()
	if m.out != nil {
		m.out.Free()
		m.out = nil
	}
	if m.dir != "" {
		_ = os.RemoveAll(m.dir)
		m.dir = ""
	}
	m.hasWritten = false
	m.font = nil
}

// probeText, probeSizePct and the picture size are what ProbeFont draws.
const (
	probeText    = "Ag"
	probeSizePct = 20
)

// FontRefusedError is a ProbeFont error that is a verdict on the font:
// the watermark's checks refused it (CheckFont among them), or drawtext
// could not load it. Every other ProbeFont error (the temp directory,
// writing the font, a picture's alloc or fill) is the probe's own failure
// and says nothing about the font. Its text holds no path.
type FontRefusedError struct{ Reason error }

func (e *FontRefusedError) Error() string { return "transcode: probe font: " + e.Reason.Error() }

func (e *FontRefusedError) Unwrap() error { return e.Reason }

// ProbeFont draws text with the font on a 256×144 picture and reports why
// that fails: the watermark's checks (CheckFont among them), then FreeType
// through drawtext, exactly as a text watermark would load it. Those
// failures are a *FontRefusedError; any other error is an I/O or libav
// failure of the probe itself, which can hold the child's temp
// directory's path. Its private directory is removed before it returns.
func ProbeFont(font []byte) error {
	if len(font) == 0 {
		return &FontRefusedError{errors.New("empty")}
	}
	w := &picture.Watermark{Text: probeText, SizePct: probeSizePct, Font: font}
	if err := w.Validate(); err != nil {
		return &FontRefusedError{err}
	}
	m := newWatermarker(nil)
	defer m.close()
	// w passed Validate, so set fails only on the temp directory or the
	// font's write.
	if err := m.set(w); err != nil {
		return fmt.Errorf("transcode: probe font: %w", err)
	}
	fr := astiav.AllocFrame()
	defer fr.Free()
	fr.SetWidth(probeWidth)
	fr.SetHeight(probeHeight)
	fr.SetPixelFormat(astiav.PixelFormatYuv420P)
	fr.SetSampleAspectRatio(astiav.NewRational(1, 1))
	if err := fr.AllocBuffer(0); err != nil {
		return fmt.Errorf("transcode: probe font: alloc picture: %w", err)
	}
	if err := fr.ImageFillBlack(); err != nil {
		return fmt.Errorf("transcode: probe font: fill picture: %w", err)
	}
	if _, err := m.apply(fr); err != nil {
		if errors.As(err, new(*drawtextError)) {
			// The text is the probe's own, so the font is the likely cause.
			// libav's error is left out: when FreeType refuses the font it
			// is fontconfig's ENOENT, which reads as a missing file.
			return &FontRefusedError{errors.New("drawtext cannot load it")}
		}
		return fmt.Errorf("transcode: probe font: %w", err)
	}
	return nil
}
