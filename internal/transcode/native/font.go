//go:build libav

package native

import (
	_ "embed"
	"os"
)

// dejaVuSans is DejaVu Sans 2.37 (fonts-dejavu-core), the font of text
// watermarks. Its licence is font/LICENSE.
//
//go:embed font/DejaVuSans.ttf
var dejaVuSans []byte

// noSystemFonts is what FONTCONFIG_FILE names in the child: a directory,
// from which fontconfig loads only the files named [0-9]*.conf, and the
// root has none. fontconfig then knows no font at all.
const noSystemFonts = "/"

// init keeps drawtext from drawing in a system font (R-C7). drawtext falls
// back to fontconfig when FreeType refuses font.ttf, and the pinned FFmpeg
// has fontconfig: an operator's font FreeType cannot load would otherwise be
// drawn silently in whatever font fontconfig finds. With no font known to
// fontconfig the fallback fails, and so does the graph. fontconfig reads the
// variable each time drawtext asks it, so it is set once, here, before any
// libav thread runs; the child draws nothing else with fontconfig. The
// variable is inherited by every subprocess of a binary that links this
// package, test binaries included.
func init() {
	if err := os.Setenv("FONTCONFIG_FILE", noSystemFonts); err != nil {
		panic("native: FONTCONFIG_FILE: " + err.Error())
	}
}
