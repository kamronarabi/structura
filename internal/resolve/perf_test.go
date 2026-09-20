package resolve_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kamronarabi/structura/internal/resolve"
)

// Redaction runs on every string a scan produces, from files that may be up
// to the 1 MiB walker limit. Go's regexp engine is RE2, so these patterns
// cannot backtrack exponentially — but a pattern rewritten carelessly could
// still be quadratic, and a scan that hangs on one file is indistinguishable
// from a crash to the person running it.
func TestRedactPerformanceOnLargeInput(t *testing.T) {
	if raceEnabled {
		t.Skip("wall-clock thresholds are meaningless under the race detector")
	}
	if testing.Short() {
		t.Skip("allocates several megabytes")
	}

	cases := map[string]string{
		"repeated userinfo":  strings.Repeat("a:b@", 250_000),
		"long token prefix":  "sk-" + strings.Repeat("a", 1_000_000),
		"unterminated pem":   "-----BEGIN RSA PRIVATE KEY-----" + strings.Repeat("x", 1_000_000),
		"many jwt prefixes":  strings.Repeat("eyJx.eyJy.", 100_000),
		"plain text at 1MiB": strings.Repeat("the quick brown fox ", 52_000),
	}
	for name, input := range cases {
		start := time.Now()
		_ = resolve.RedactString(input)
		elapsed := time.Since(start)
		t.Logf("%-20s %7d bytes  %v", name, len(input), elapsed)
		if elapsed > 3*time.Second {
			t.Errorf("%s took %v on %d bytes", name, elapsed, len(input))
		}
	}
}
