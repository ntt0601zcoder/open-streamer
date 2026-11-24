//go:build !libav

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ostranscoder: built without -tags libav")
	os.Exit(2)
}
