//go:build !libav

package main

import (
	"fmt"
	"os"
)

// main refuses every invocation, -version and -probe nvenc or font
// included: one line on stderr and exit 2, which the server reads as no
// verdict.
func main() {
	fmt.Fprintln(os.Stderr, "ostranscoder: built without -tags libav")
	os.Exit(2)
}
