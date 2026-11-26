package media

import "strings"

// maxLanguageLen is the longest language tag ValidLanguage accepts.
const maxLanguageLen = 35

// ValidLanguage reports whether s is a language tag the server accepts (R-L2): a primary subtag of 2–3 ASCII letters,
// then zero or more "-"-separated subtags of 1–8 ASCII letters or digits, 35 bytes at most.
func ValidLanguage(s string) bool {
	if len(s) > maxLanguageLen {
		return false
	}
	primary, rest, more := strings.Cut(s, "-")
	if len(primary) < 2 || len(primary) > 3 || !allLetters(primary) {
		return false
	}
	return !more || validSubtags(rest)
}

// ValidLanguageSyntax reports whether s is a BCP 47 tag by RFC 5646's syntax: subtags of 1–8 ASCII letters or digits separated
// by "-", the first of 2–8 letters, or the private-use "x" or grandfathered "i" singleton followed by at least one subtag; no length cap.
func ValidLanguageSyntax(s string) bool {
	first, rest, more := strings.Cut(s, "-")
	switch first {
	case "x", "X", "i", "I":
		return more && validSubtags(rest)
	}
	if len(first) < 2 || len(first) > 8 || !allLetters(first) {
		return false
	}
	return !more || validSubtags(rest)
}

// validSubtags reports whether s is one or more "-"-separated subtags of
// 1–8 ASCII letters or digits.
func validSubtags(s string) bool {
	for {
		sub, rest, more := strings.Cut(s, "-")
		if len(sub) < 1 || len(sub) > 8 || !allAlnum(sub) {
			return false
		}
		if !more {
			return true
		}
		s = rest
	}
}

func allLetters(s string) bool {
	for i := range len(s) {
		if !isLetter(s[i]) {
			return false
		}
	}
	return true
}

func allAlnum(s string) bool {
	for i := range len(s) {
		if c := s[i]; !isLetter(c) && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
