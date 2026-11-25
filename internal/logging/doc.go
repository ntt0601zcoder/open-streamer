// Package logging builds the server's slog logger. Its handler rewrites every
// record before it is written, so a URL carrying a password or stream key, and
// any attribute named like a secret, never reaches a log.
package logging
