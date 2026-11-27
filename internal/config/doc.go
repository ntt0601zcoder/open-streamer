// Package config is the server's one configuration file: a YAML document
// with a version and three sections, server (listen addresses and TLS
// listen addresses, apps, the TLS certificate pair, the transcoder binary,
// the assets directory, logging), users (names, roles
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
// transcoder binary, the assets directory and the TLS certificate and key
// files exist; the server asks it once, at start, so a file that
// disappears later fails no edit.
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
// # Secrets
//
// The file holds every secret in clear; no API response does. Mask is the
// text as a response shows it: in every scalar, keys and values, each
// URL-shaped token is shown as redact.URL shows it, a scalar holding a
// token that does not parse is shown as the bare mask ("xxxxx"), every
// non-empty srt_passphrase is the mask, and the URLs in comments are
// redacted as a log line is. The users' password hashes are not hidden.
// Unmask is the way back for a write: in each stream entry, an
// inputs[i].url, an outputs.push[j].url or an outputs.srt_passphrase that
// holds the mask takes the value the stored entry of the same name holds
// for it (spec.KeepSecrets), and a mask that stands for no one stored
// secret is refused, naming the field and never a value. A mask anywhere
// else is not resolved: a comment, or a URL under another key, is stored
// as it was sent.
//
// Both work on the bytes. A scalar is replaced where it is written and
// everything else is copied, so a text with nothing to hide is returned
// as it is, and Unmask(Mask(t), t) is t. Each decodes its result and
// compares it with the node tree it wanted; a scalar that cannot be
// replaced in place (one with an escape, an anchor or a tag, a folded or
// multi-line one) sends the whole document through the node tree instead,
// with the loss of layout described under Editing. Mask also searches the
// bytes of what it returns, because the parser drops some comments: a
// URL-shaped token there that still has something to hide re-encodes the
// document, which drops those comments, and Mask returns an error rather
// than a text that holds one. In the bytes a token is judged without the
// quotes, commas and brackets that close it, a double-quoted scalar by its
// value, and what follows a '#' inside a token by itself, since a comment
// may start there. A text whose bytes still fail after one re-encoding (a
// URL as a mapping key, read together with the colon after it) is encoded
// once more with its URLs in double quotes. Mask does not depend on the
// text passing Parse: anchors, aliases, merge keys and entries the lenient
// start skipped are masked like everything else.
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
