package hls

import (
	"fmt"
	"strings"
)

// parseAttrs parses an HLS attribute list such as `A=1,B="x,y"`. Quoted
// values are unquoted and may contain commas.
func parseAttrs(s string) (map[string]string, error) {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("attribute without value in %q", s)
		}
		key := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		var val string
		if i < len(s) && s[i] == '"' {
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				return nil, fmt.Errorf("unterminated quoted string in %q", s)
			}
			val = s[i+1 : i+1+end]
			i += end + 2
			if i < len(s) && s[i] != ',' && s[i] != ' ' {
				return nil, fmt.Errorf("junk after quoted string in %q", s)
			}
		} else {
			end := strings.IndexByte(s[i:], ',')
			if end < 0 {
				end = len(s) - i
			}
			val = strings.TrimSpace(s[i : i+end])
			i += end
		}
		out[key] = val
	}
	return out, nil
}

// splitLines splits playlist text into trimmed, non-empty lines, dropping a
// leading BOM.
func splitLines(b []byte) []string {
	s := strings.TrimPrefix(string(b), "\xef\xbb\xbf")
	raw := strings.Split(s, "\n")
	lines := raw[:0]
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
