package sched

import (
	"strings"
	"testing"
)

// The monitor's diff reads as Hermes' (Python difflib.unified_diff, n=3, lineterm=""): the
// expected texts are difflib's output for the same inputs.
func TestUnifiedDiff(t *testing.T) {
	for _, c := range []struct{ old, new, want string }{
		{"a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl", "a\nb\nC\nd\ne\nf\ng\nh\ni\nj\nk\nL\nm",
			"--- previous\n+++ current\n@@ -1,6 +1,6 @@\n a\n b\n-c\n+C\n d\n e\n f\n@@ -9,4 +9,5 @@\n i\n j\n k\n-l\n+L\n+m"},
		{"SIG x 1\nSIG y 2", "SIG y 2\nSIG z 3", "--- previous\n+++ current\n@@ -1,2 +1,2 @@\n-SIG x 1\n SIG y 2\n+SIG z 3"},
		{"", "only", "--- previous\n+++ current\n@@ -0,0 +1 @@\n+only"},
		{"one\ntwo", "", "--- previous\n+++ current\n@@ -1,2 +0,0 @@\n-one\n-two"},
		{"same\n", "same", ""},
	} {
		if got := unifiedDiff(c.old, c.new); got != c.want {
			t.Errorf("diff(%q, %q) =\n%s\nwant\n%s", c.old, c.new, got, c.want)
		}
	}
	// Too large to compare line by line: one hunk, everything replaced.
	big := strings.Repeat("x\n", 3000)
	if got := unifiedDiff(big+"a", big+"b"); !strings.Contains(got, "-a\n+b") {
		t.Errorf("suffix diff of a large input: %.80q", got)
	}
	l1, l2 := []string{}, []string{}
	for i := range 2500 {
		l1 = append(l1, "a"+strings.Repeat("1", i%7))
		l2 = append(l2, "b"+strings.Repeat("2", i%5))
	}
	if got := unifiedDiff(strings.Join(l1, "\n"), strings.Join(l2, "\n")); !strings.HasPrefix(got, "--- previous\n+++ current\n@@ -1,2500 +1,2500 @@\n-a") {
		t.Errorf("large diff starts %.80q", got)
	}
}
