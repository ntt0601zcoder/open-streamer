//go:build libav

package main

/*
#cgo pkg-config: libavcodec
#include <libavcodec/avcodec.h>
*/
import "C"

import (
	"fmt"
	"os"
)

// wireVersion is the wire protocol version the child speaks.
// TODO(M4a T6): use wire.Version once internal/transcode/wire defines it.
const wireVersion = 1

// main serves only -version until the child speaks the wire protocol;
// anything else exits 2.
func main() {
	if len(os.Args) == 2 && os.Args[1] == "-version" {
		fmt.Printf("ostranscoder wire %d libavcodec %s\n", wireVersion, libavcodecVersion())
		return
	}
	fmt.Fprintln(os.Stderr, "usage: ostranscoder -version")
	os.Exit(2)
}

// libavcodecVersion is the version of the libavcodec loaded at run time, as
// major.minor.micro.
func libavcodecVersion() string {
	v := uint32(C.avcodec_version())
	return fmt.Sprintf("%d.%d.%d", v>>16, v>>8&0xff, v&0xff)
}
