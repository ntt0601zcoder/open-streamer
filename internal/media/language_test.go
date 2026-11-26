package media_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ntt0601zcoder/open-streamer/internal/media"
)

// accepted are the tags ValidLanguage accepts.
var accepted = []string{"vi", "en", "en-US", "EN-us", "zh-Hant-TW", "yue", "sr-Latn-RS-1994"}

func TestValidLanguage(t *testing.T) {
	longest := "en" + strings.Repeat("-abcdefgh", 3) + "-abcde"
	require.Len(t, longest, 35)
	over := longest + "f"
	require.Len(t, over, 36)

	for _, s := range append(append([]string{}, accepted...), longest) {
		assert.True(t, media.ValidLanguage(s), "%q", s)
	}
	for _, s := range []string{
		"", "e", "english", "x-klingon", "en_US", "en-", "-en", "en--US",
		"vi\"", "vi\n", "vi ", "en-abcdefghi", over,
	} {
		assert.False(t, media.ValidLanguage(s), "%q", s)
	}
}

func TestValidLanguageSyntax(t *testing.T) {
	long := "en" + strings.Repeat("-abcdefgh", 4) + "-a"
	require.Len(t, long, 40)

	for _, s := range append(append([]string{}, accepted...), "x-klingon", "i-klingon", "X-Klingon", "english", long) {
		assert.True(t, media.ValidLanguageSyntax(s), "%q", s)
	}
	for _, s := range []string{
		"", "e", "x", "x-", "en_US", "en--US", "vi\"", "vi\n", "en-abcdefghi",
	} {
		assert.False(t, media.ValidLanguageSyntax(s), "%q", s)
	}
}
