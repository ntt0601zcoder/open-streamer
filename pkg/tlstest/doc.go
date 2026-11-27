// Package tlstest provides an in-process test certificate authority: leaf
// certificates for given hosts, written as PEM or as files, and ready TLS
// server and client configs. No private key is ever checked in; every test
// makes its own. It imports only the standard library and testing, nothing
// from internal/.
package tlstest
