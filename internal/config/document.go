package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ntt0601zcoder/open-streamer/internal/auth"
	"github.com/ntt0601zcoder/open-streamer/internal/spec"
)

// Version is the only config version this build reads, and MaxBytes the
// largest config text.
const (
	Version  = 1
	MaxBytes = 4 << 20
)

// The bounds of what a parse reports, so that neither its work nor its
// error text (which is logged, kept as the last reload error and sent to
// the client) grows with a hostile document.
const (
	// MaxErrors is how many errors Parse and ParseLenient report, each
	// list followed by one "and N more" when there were more.
	MaxErrors = 100
	// maxEntryErrors is how many reasons one stream entry's error joins.
	maxEntryErrors = 10
	// maxMessage is the longest message taken from the YAML library.
	maxMessage = 300
)

// Document is the whole config file.
type Document struct {
	Version int           `yaml:"version"`
	Server  Server        `yaml:"server"`
	Users   []User        `yaml:"users"`
	Streams []spec.Stream `yaml:"streams"`
}

// User is one entry of the users section.
type User struct {
	Name     string `yaml:"name"`
	Role     string `yaml:"role"`     // "viewer" or "editor"
	Password string `yaml:"password"` // a hash made by openstreamer hash-password
}

// Options tune Parse. RequiredIterations, tests only, is the iteration
// count every user's hash must have; 0 means auth.Iterations (600000).
// cmd/openstreamer never sets it.
type Options struct{ RequiredIterations int }

// Parsed is a parsed document. UserList holds the parsed users, for
// auth.Users.Equal.
type Parsed struct {
	Doc      Document
	Users    *auth.Users
	UserList []auth.User
}

// SyntaxError is a text that is not one YAML document of the expected
// shape. Its Error hides the values the YAML library quotes between
// backquotes ("cannot unmarshal !!str `hunter2` into int"), since one may
// be a secret typed on the wrong line: in each error entry, everything
// from the first backquote to the last, newlines included, reads `…`. It
// has no Unwrap, so the library's own message cannot be reached and
// printed by accident.
type SyntaxError struct{ Err error }

func (e *SyntaxError) Error() string {
	var te *yaml.TypeError
	if errors.As(e.Err, &te) {
		return "config: yaml: unmarshal errors: " + strings.Join(capped(cutEach(te.Errors), MaxErrors), "; ")
	}
	return "config: " + public(e.Err.Error())
}

// capped returns the first limit of msgs, followed by "and N more" when
// there are more.
func capped(msgs []string, limit int) []string {
	if len(msgs) <= limit {
		return msgs
	}
	return append(slices.Clone(msgs[:limit]), fmt.Sprintf("and %d more", len(msgs)-limit))
}

// cappedErrors is capped for errors, with limit MaxErrors.
func cappedErrors(errs []error) []error {
	if len(errs) <= MaxErrors {
		return errs
	}
	return append(slices.Clone(errs[:MaxErrors]), fmt.Errorf("and %d more", len(errs)-MaxErrors))
}

// fieldRE finds the key the YAML library names in an unknown-key message.
var fieldRE = regexp.MustCompile(`field (.*) not found in type `)

// public makes a message of the YAML library fit for a client: the values
// it quotes are hidden (cutValue), an unknown key is cut to 64 bytes, the
// names of this package's private types are replaced, and the whole is cut
// to maxMessage bytes.
func public(msg string) string {
	msg = cutValue(msg)
	msg = fieldRE.ReplaceAllStringFunc(msg, func(m string) string {
		key := fieldRE.FindStringSubmatch(m)[1]
		if len(key) <= 64 {
			return m
		}
		return "field " + strings.ToValidUTF8(key[:64], "") + "… not found in type "
	})
	msg = strings.ReplaceAll(msg, "in type config.rawDocument", "at the top level of the config")
	msg = strings.ReplaceAll(msg, "into config.rawDocument", "into the config document")
	if len(msg) > maxMessage {
		msg = strings.ToValidUTF8(msg[:maxMessage], "") + "…"
	}
	return msg
}

// Errors is every validation error of a document.
type Errors []error

func (e Errors) Error() string {
	s := make([]string, len(e))
	for i, err := range e {
		s[i] = err.Error()
	}
	return strings.Join(s, "; ")
}

// The node-tree rules.
const (
	msgAnchor = "anchors, aliases and merge keys are not supported"
	msgNumber = "write the number as a plain decimal integer, or quote it if it is text"
	msgBinary = "!!binary is not supported"
)

// decimalRE is the only form a number may take in the config.
var decimalRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// userNameRE is auth's user name rule, checked here so each bad name is
// reported at its own place; auth.NewUsers checks it again.
var userNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// lineRE finds the line numbers in the YAML library's messages.
var lineRE = regexp.MustCompile(`line ([0-9]+)`)

// rawDocument is Document with each stream entry left as a node, so that
// the top level decodes strictly and each entry can be decoded, and
// skipped, on its own.
type rawDocument struct {
	Version int         `yaml:"version"`
	Server  Server      `yaml:"server"`
	Users   []User      `yaml:"users"`
	Streams []yaml.Node `yaml:"streams"`
}

// Parse is strict (R-D4): one document, known keys only, at most MaxBytes;
// then every check, collecting every error. It returns *SyntaxError or
// Errors.
func Parse(text []byte, opt Options) (Parsed, error) {
	p, skipped, err := parse(text, opt)
	var all Errors
	if err != nil {
		var es Errors
		if !errors.As(err, &es) {
			return Parsed{}, err
		}
		all = append(all, es...)
	}
	all = append(all, skipped...)
	if len(all) > 0 {
		return Parsed{}, Errors(cappedErrors(all))
	}
	return p, nil
}

// ParseLenient is Parse for the start: a stream entry that does not
// decode, fails Validate, breaks a node-tree rule or repeats a name is left
// out of Doc.Streams and reported in skipped ("streams[3] (cam): …"); every
// other error is err, as from Parse, and then nothing else is returned.
func ParseLenient(text []byte, opt Options) (p Parsed, skipped []error, err error) {
	p, skipped, err = parse(text, opt)
	if err != nil {
		var es Errors
		if errors.As(err, &es) {
			err = Errors(cappedErrors(es))
		}
		return Parsed{}, nil, err
	}
	return p, cappedErrors(skipped), nil
}

// parse does the work of Parse and ParseLenient. err is a *SyntaxError or
// the Errors outside streams; skipped are the stream entries' errors. When
// err is not nil, p is empty.
func parse(text []byte, opt Options) (p Parsed, skipped []error, err error) {
	if len(text) > MaxBytes {
		return Parsed{}, nil, &SyntaxError{Err: fmt.Errorf("the text is larger than %d bytes", MaxBytes)}
	}
	root, err := decodeNode(text)
	if err != nil {
		return Parsed{}, nil, &SyntaxError{Err: err}
	}
	seq := streamsNode(root)
	top := walk(root, seq, true)
	var entries []*yaml.Node
	if seq != nil {
		entries = seq.Content
	}
	// The entries past the limit are counted, which fails the parse, and
	// not looked at.
	total := len(entries)
	if total > spec.MaxStreams {
		entries = entries[:spec.MaxStreams]
	}
	bad := make([][]string, len(entries))
	for i, e := range entries {
		for _, v := range walk(e, nil, true) {
			bad[i] = append(bad[i], v.String())
		}
		if len(bad[i]) > 0 {
			skipped = append(skipped, fmt.Errorf("%s: %s", entryRef(i, e), strings.Join(capped(bad[i], maxEntryErrors), "; ")))
		}
	}
	if len(top) > 0 {
		errs := make(Errors, len(top))
		for i, v := range top {
			errs[i] = errors.New(v.String())
		}
		return Parsed{}, skipped, errs
	}

	var raw rawDocument
	dec := yaml.NewDecoder(bytes.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Parsed{}, nil, &SyntaxError{Err: err}
	}

	var errs Errors
	if raw.Version != Version {
		errs = append(errs, fmt.Errorf("version: %d, want %d", raw.Version, Version))
	}
	errs = append(errs, raw.Server.check()...)
	list, users, uerrs := checkUsers(raw.Users, opt)
	errs = append(errs, uerrs...)
	if total > spec.MaxStreams {
		errs = append(errs, fmt.Errorf("streams: %d streams, want at most %d", total, spec.MaxStreams))
	}

	streams := make([]spec.Stream, 0, len(entries))
	first := make(map[string]int, len(entries))
	for i, e := range entries {
		if len(bad[i]) > 0 {
			continue
		}
		ref := entryRef(i, e)
		s, msg := decodeEntry(e)
		if msg != "" {
			skipped = append(skipped, fmt.Errorf("%s: %s", ref, msg))
			continue
		}
		if err := s.Validate(); err != nil {
			if inner := errors.Unwrap(err); inner != nil {
				err = inner
			}
			skipped = append(skipped, fmt.Errorf("%s: line %d: %w", ref, e.Line, err))
			continue
		}
		if j, dup := first[s.Name]; dup {
			skipped = append(skipped, fmt.Errorf("%s: line %d: name: %q is repeated (streams[%d])", ref, e.Line, s.Name, j))
			continue
		}
		first[s.Name] = i
		streams = append(streams, s)
	}
	if len(errs) > 0 {
		return Parsed{}, skipped, errs
	}
	return Parsed{
		Doc:      Document{Version: raw.Version, Server: raw.Server, Users: raw.Users, Streams: streams},
		Users:    users,
		UserList: list,
	}, skipped, nil
}

// decodeNode decodes the one document of text into a node tree.
func decodeNode(text []byte) (*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(text))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no document")
		}
		return nil, err
	}
	var next yaml.Node
	if err := dec.Decode(&next); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("more than one document")
	}
	return &root, nil
}

// topMapping returns the document's top-level mapping, or nil.
func topMapping(root *yaml.Node) *yaml.Node {
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	return root.Content[0]
}

// streamsNode returns the value of the top-level streams key when it is a
// sequence, or nil.
func streamsNode(root *yaml.Node) *yaml.Node {
	m := topMapping(root)
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if k, v := m.Content[i], m.Content[i+1]; k.Kind == yaml.ScalarNode && k.Value == "streams" && v.Kind == yaml.SequenceNode {
			return v
		}
	}
	return nil
}

// violation is one break of a node-tree rule.
type violation struct {
	line int
	msg  string
}

func (v violation) String() string { return fmt.Sprintf("line %d: %s", v.line, v.msg) }

// walk returns the node-tree violations under n, in document order, one
// per line and rule. The children of skip are not visited (skip itself
// is). numbers adds the plain-decimal rule and the refusal of !!binary to
// the anchor rule.
func walk(n, skip *yaml.Node, numbers bool) []violation {
	var out []violation
	seen := map[violation]bool{}
	add := func(line int, msg string) {
		v := violation{line, msg}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Anchor != "" || n.Kind == yaml.AliasNode {
			add(n.Line, msgAnchor)
		}
		if numbers && n.ShortTag() == binaryTag {
			add(n.Line, msgBinary)
		}
		switch n.Kind {
		case yaml.AliasNode:
			return // its target is checked where it is written
		case yaml.ScalarNode:
			if t := n.ShortTag(); numbers && (t == "!!int" || t == "!!float") && !decimalRE.MatchString(n.Value) {
				add(n.Line, msgNumber)
			}
		case yaml.MappingNode:
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].ShortTag() == "!!merge" {
					add(n.Content[i].Line, msgAnchor)
				}
			}
		case yaml.DocumentNode, yaml.SequenceNode:
		}
		if n == skip {
			return
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	visit(n)
	return out
}

// entryName returns the value of a stream entry's name key, when the entry
// is a mapping with a scalar name. A name or a name key tagged !!binary is
// no name: its text is base64 of what the decoder reads, and a mask must
// not be resolved against another stream by it.
func entryName(e *yaml.Node) (string, bool) {
	if e.Kind != yaml.MappingNode {
		return "", false
	}
	for i := 0; i+1 < len(e.Content); i += 2 {
		k, v := e.Content[i], e.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == "name" && v.Kind == yaml.ScalarNode && k.ShortTag() != binaryTag && v.ShortTag() != binaryTag {
			return v.Value, true
		}
	}
	return "", false
}

// entryRef is how errors name stream entry i: "streams[3] (cam)", the name
// quoted when it is not a valid name, or "streams[3]" when it has none.
func entryRef(i int, e *yaml.Node) string {
	ref := "streams[" + strconv.Itoa(i) + "]"
	name, ok := entryName(e)
	switch {
	case !ok:
		return ref
	case spec.ValidName(name):
		return ref + " (" + name + ")"
	default:
		if len(name) > 64 {
			name = name[:64] + "…"
		}
		return ref + " (" + strconv.Quote(name) + ")"
	}
}

// decodeEntry decodes one stream entry strictly: a node cannot be decoded
// strictly by itself, so it is encoded back to text first. On failure it
// returns the message, values hidden, with the snippet's line numbers moved
// to the file's: exact for the entry's first line and for entries in the
// library's own formatting, which the re-encoding keeps line for line.
func decodeEntry(e *yaml.Node) (spec.Stream, string) {
	c := *e
	c.HeadComment, c.FootComment = "", ""
	b, err := yaml.Marshal(&c)
	if err != nil {
		return spec.Stream{}, "the entry cannot be encoded"
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var s spec.Stream
	if err := dec.Decode(&s); err != nil {
		var msgs []string
		var te *yaml.TypeError
		if errors.As(err, &te) {
			msgs = capped(cutEach(te.Errors), maxEntryErrors)
		} else {
			msgs = []string{public(strings.TrimPrefix(err.Error(), "yaml: "))}
		}
		off := e.Line - 1
		for i, m := range msgs {
			msgs[i] = lineRE.ReplaceAllStringFunc(m, func(s string) string {
				n, _ := strconv.Atoi(s[len("line "):])
				return "line " + strconv.Itoa(n+off)
			})
		}
		return spec.Stream{}, strings.Join(msgs, "; ")
	}
	return s, ""
}

// cutValue replaces everything from the first backquote of s to its last
// by `…`, or from a lone backquote to the end.
func cutValue(s string) string {
	i := strings.IndexByte(s, '`')
	if i < 0 {
		return s
	}
	j := strings.LastIndexByte(s, '`')
	if j == i {
		return s[:i] + "`…`"
	}
	return s[:i] + "`…`" + s[j+1:]
}

// cutEach is public for each of at most MaxErrors+1 entries; the rest are
// kept as they are, for capped to count.
func cutEach(entries []string) []string {
	out := slices.Clone(entries)
	for i := range min(len(out), MaxErrors+1) {
		out[i] = public(out[i])
	}
	return out
}

// checkUsers checks the users section and builds the user set. Its errors
// never contain a password field.
func checkUsers(list []User, opt Options) ([]auth.User, *auth.Users, []error) {
	required := opt.RequiredIterations
	if required == 0 {
		required = auth.Iterations
	}
	var errs []error
	if len(list) > auth.MaxUsers {
		// Not looked at one by one: each would cost a hash parse and an
		// error.
		return nil, nil, []error{fmt.Errorf("users: %d users, want at most %d", len(list), auth.MaxUsers)}
	}
	out := make([]auth.User, 0, len(list))
	seen := make(map[string]bool, len(list))
	editor := false
	for i, u := range list {
		who := ""
		if userNameRE.MatchString(u.Name) {
			who = fmt.Sprintf("user %q: ", u.Name)
			if seen[u.Name] {
				errs = append(errs, fmt.Errorf("users[%d].name: user %q appears twice", i, u.Name))
			}
			seen[u.Name] = true
		} else {
			errs = append(errs, fmt.Errorf("users[%d].name: must match %s", i, userNameRE))
		}
		role := auth.Role(u.Role)
		switch role {
		case auth.Editor:
			editor = true
		case auth.Viewer:
		default:
			errs = append(errs, fmt.Errorf("users[%d].role: %swant viewer or editor", i, who))
		}
		h, err := auth.ParseHash(u.Password, 1)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("users[%d].password: %s%w", i, who, err))
		case h.Iterations != required:
			errs = append(errs, fmt.Errorf("users[%d].password: %sthe hash has %d iterations, want %d", i, who, h.Iterations, required))
		}
		out = append(out, auth.User{Name: u.Name, Role: role, Hash: h})
	}
	if !editor {
		errs = append(errs, errors.New("users: no editor"))
	}
	if len(errs) > 0 {
		return nil, nil, errs
	}
	users, err := auth.NewUsers(out)
	if err != nil {
		return nil, nil, []error{fmt.Errorf("users: %w", err)}
	}
	return out, users, nil
}
