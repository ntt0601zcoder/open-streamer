// Package metricstest parses the Prometheus text format strictly, for tests.
package metricstest

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Sample is one parsed series.
type Sample struct {
	Name       string
	Labels     map[string]string
	Value      float64
	Type, Help string
}

// Parse reads an exposition and refuses anything a scraper could misread: a
// sample whose family lacks HELP and TYPE before it, a family that is not
// contiguous, illegal names, or a duplicate series.
func Parse(r io.Reader) ([]Sample, error) {
	var out []Sample
	help := map[string]string{}
	typ := map[string]string{}
	closed := map[string]bool{}
	seen := map[string]bool{}
	cur := ""
	enter := func(n string) error {
		if n == cur {
			return nil
		}
		if cur != "" {
			closed[cur] = true
		}
		if closed[n] {
			return fmt.Errorf("family %s is split", n)
		}
		cur = n
		return nil
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for ln := 1; sc.Scan(); ln++ {
		line := sc.Text()
		fail := func(f string, a ...any) ([]Sample, error) {
			return nil, fmt.Errorf("line %d: %s", ln, fmt.Sprintf(f, a...))
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			n, text, _ := strings.Cut(line[7:], " ")
			if !validName(n, true) {
				return fail("bad name %q", n)
			}
			if _, dup := help[n]; dup {
				return fail("second HELP for %s", n)
			}
			if err := enter(n); err != nil {
				return fail("%v", err)
			}
			help[n] = unescape(text, false)
		case strings.HasPrefix(line, "# TYPE "):
			n, t, _ := strings.Cut(line[7:], " ")
			if _, ok := help[n]; !ok {
				return fail("TYPE for %s before HELP", n)
			}
			if _, dup := typ[n]; dup || n != cur {
				return fail("misplaced TYPE for %s", n)
			}
			if t != "counter" && t != "gauge" {
				return fail("unsupported type %q", t)
			}
			typ[n] = t
		case strings.HasPrefix(line, "#"), line == "":
			return fail("unexpected line %q", line)
		default:
			s, err := parseSample(line)
			if err != nil {
				return fail("%v", err)
			}
			t, ok := typ[s.Name]
			if !ok {
				return fail("sample of %s without HELP and TYPE", s.Name)
			}
			if err := enter(s.Name); err != nil {
				return fail("%v", err)
			}
			key := seriesKey(s)
			if seen[key] {
				return fail("duplicate series %s", key)
			}
			seen[key] = true
			s.Type, s.Help = t, help[s.Name]
			out = append(out, s)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("metricstest: %w", err)
	}
	return out, nil
}

func seriesKey(s Sample) string {
	ks := make([]string, 0, len(s.Labels))
	for k, v := range s.Labels {
		ks = append(ks, k+"="+strconv.Quote(v))
	}
	sort.Strings(ks)
	return s.Name + "{" + strings.Join(ks, ",") + "}"
}

func parseSample(line string) (Sample, error) {
	s := Sample{Labels: map[string]string{}}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return s, fmt.Errorf("no value in %q", line)
	}
	s.Name = line[:i]
	if !validName(s.Name, true) {
		return s, fmt.Errorf("bad name %q", s.Name)
	}
	rest := line[i:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			eq := strings.Index(rest, `="`)
			if eq < 0 {
				return s, fmt.Errorf("bad labels in %q", line)
			}
			name := rest[:eq]
			if !validName(name, false) {
				return s, fmt.Errorf("bad label name %q", name)
			}
			if _, dup := s.Labels[name]; dup {
				return s, fmt.Errorf("label %q twice", name)
			}
			rest = rest[eq+2:]
			var v strings.Builder
			closedQ := false
			for j := 0; j < len(rest); j++ {
				c := rest[j]
				if c == '\\' && j+1 < len(rest) {
					j++
					switch rest[j] {
					case 'n':
						v.WriteByte('\n')
					case '\\', '"':
						v.WriteByte(rest[j])
					default:
						return s, fmt.Errorf("bad escape in %q", line)
					}
					continue
				}
				if c == '"' {
					rest = rest[j+1:]
					closedQ = true
					break
				}
				v.WriteByte(c)
			}
			if !closedQ {
				return s, fmt.Errorf("unterminated label value in %q", line)
			}
			s.Labels[name] = v.String()
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
			} else if !strings.HasPrefix(rest, "}") {
				return s, fmt.Errorf("bad labels in %q", line)
			}
		}
	}
	if !strings.HasPrefix(rest, " ") {
		return s, fmt.Errorf("no value in %q", line)
	}
	v, err := strconv.ParseFloat(rest[1:], 64)
	if err != nil {
		return s, fmt.Errorf("bad value in %q", line)
	}
	s.Value = v
	return s, nil
}

func unescape(s string, quote bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch {
			case s[i] == 'n':
				b.WriteByte('\n')
			case s[i] == '\\' || (quote && s[i] == '"'):
				b.WriteByte(s[i])
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func validName(s string, colon bool) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (colon && r == ':')
		if i > 0 && r >= '0' && r <= '9' {
			ok = true
		}
		if !ok {
			return false
		}
	}
	return true
}

// Find returns the value of the one series of family name whose labels include
// every given pair (labels as name, value pairs). It is false when no series or
// more than one matches.
func Find(s []Sample, name string, labels ...string) (float64, bool) {
	if len(labels)%2 != 0 {
		return 0, false
	}
	var val float64
	n := 0
	for _, x := range s {
		if x.Name != name {
			continue
		}
		match := true
		for i := 0; i < len(labels); i += 2 {
			if v, ok := x.Labels[labels[i]]; !ok || v != labels[i+1] {
				match = false
				break
			}
		}
		if match {
			val = x.Value
			n++
		}
	}
	return val, n == 1
}
