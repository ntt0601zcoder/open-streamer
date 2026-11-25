// Package config is the server's one configuration file: a YAML document
// with a version and three sections, server (listen addresses, apps, the
// transcoder binary, the assets directory, logging), users (names, roles
// and PBKDF2 password hashes) and streams (stream documents, as in
// internal/spec).
//
// # Parsing
//
// Parse is strict: one document of at most MaxBytes, known keys only, no
// duplicate keys, then every check, collecting every error. It returns a
// *SyntaxError when the text is not one YAML document of the expected
// shape, and Errors, each naming its place ("server.srt.latency_ms: …",
// "users[1].role: …", "streams[3] (cam): line 41: …"), when it is but the
// values are wrong. Neither ever quotes a password field or a value the
// YAML library quoted: a secret typed on the wrong line stays out of logs.
//
// ParseLenient is Parse for the start of the server: a stream entry that
// does not decode, fails spec.Stream.Validate or repeats a name is left
// out and reported, so one bad stream cannot keep every other one offline;
// an error anywhere else stops the start.
//
// Two rules are checked on the YAML node tree before anything is decoded:
//
//   - Anchors, aliases and merge keys (&a, *a, <<:) are refused. They make
//     one entry's text depend on another place in the file, which neither
//     the per-entry lenient decoding nor the node edits can keep whole.
//   - Every scalar the library resolves as a number must be a plain
//     decimal integer. YAML converts silently (0720 is 464, 1.5 is 1,
//     1_000 is 1000), so 0720, 0x2d0, 1_080 and 720.0 are refused. The
//     rule is on the scalar, not on the field, so unquoted text that looks
//     like a number (a stream named 007, a watermark text 24.7) is refused
//     as well and must be quoted.
//
// Parse never touches the file system. Server.CheckFiles asks whether the
// transcoder binary and the assets directory exist; the server asks it
// once, at start, so a file that disappears later fails no edit.
//
// # Editing
//
// SetStream and DeleteStream edit one stream through the YAML node tree
// and re-encode the whole document. That round trip keeps the order of
// keys and entries and the head comments (the lines above a key or an
// entry), but loses the rest of the layout: blank lines are dropped,
// sequences and mappings are re-indented to 2 spaces, the spacing before a
// line comment is normalised, and the comments inside the replaced entry,
// including a line comment on its name, are lost. Only a text already in
// the library's own formatting comes back byte for byte outside the edited
// entry. A whole-config write never goes through a node: File.Write writes
// the exact bytes it is given.
//
// # The file
//
// File reads the file, refuses it on Unix when its mode has any group or
// other bit, writes it atomically (a temp file in the same directory,
// fsync, rename, fsync of the directory) and remembers the SHA-256 of the
// text it last loaded, wrote or adopted. A write finds the file changed on
// disk by that hash and is refused with ErrStale until a reload adopts the
// text, so neither the API nor a hand edit silently overwrites the other.
package config
