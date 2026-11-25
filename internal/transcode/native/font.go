//go:build libav

package native

import _ "embed"

// dejaVuSans is DejaVu Sans 2.37 (fonts-dejavu-core), the font of text
// watermarks. Its licence is font/LICENSE.
//
//go:embed font/DejaVuSans.ttf
var dejaVuSans []byte
