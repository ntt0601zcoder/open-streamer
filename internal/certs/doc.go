// Package certs holds the server's one certificate pair (R-S2) and reads it
// again while it is served (R-S3).
//
// The pair in force sits behind an atomic pointer that every TLS listener's
// config reads through GetCertificate, so taking a new pair is one pointer
// swap: new handshakes get it, connections already made are not touched, and
// no listener is restarted. A pair that does not load keeps the one in force.
//
// Watch polls the two files with os.Stat, which follows symlinks such as
// certbot's live/ directory, and reads them again when one changed, and on
// every tick while the latest read failed. Errors name the files and the
// reason, never their content.
//
// Client builds the config of the TLS clients (R-S7).
package certs
