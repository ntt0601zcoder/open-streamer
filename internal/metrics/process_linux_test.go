package metrics

import "testing"

func TestParseStat(t *testing.T) {
	// Fields 1 to 25 of /proc/<pid>/stat, with a command name that holds
	// a space and a parenthesis: utime is field 14, stime 15, rss 24.
	const good = "1234 (a b) c) S 1 2 3 4 5 6 7 8 9 10 250 50 13 14 15 16 17 18 19 20 777 25"
	cpu, rss, ok := parseStat(good)
	if !ok || cpu != 3 || rss != 777 {
		t.Fatalf("parseStat(good) = %v, %v, %v; want 3, 777, true", cpu, rss, ok)
	}
	for name, s := range map[string]string{
		"empty":        "",
		"no paren":     "1234 cmd S 1 2 3",
		"short":        "1234 (cmd) S 1 2 3",
		"bad utime":    "1234 (cmd) S 1 2 3 4 5 6 7 8 9 10 x 50 13 14 15 16 17 18 19 20 777 25",
		"bad stime":    "1234 (cmd) S 1 2 3 4 5 6 7 8 9 10 250 x 13 14 15 16 17 18 19 20 777 25",
		"bad rss":      "1234 (cmd) S 1 2 3 4 5 6 7 8 9 10 250 50 13 14 15 16 17 18 19 20 x 25",
		"rss past end": "1234 (cmd) S 1 2 3 4 5 6 7 8 9 10 250 50 13 14 15 16 17 18 19 20",
	} {
		if _, _, ok := parseStat(s); ok {
			t.Errorf("%s: parseStat = ok, want not ok", name)
		}
	}
}
