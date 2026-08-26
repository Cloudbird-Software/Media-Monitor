package encrypt

import (
	"math/rand"
	"strings"
	"testing"
)

func TestEmbedExtractRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	cases := []string{
		"hello",
		"你好，世界",
		"mixed 文本 123",
		"a",
		"",
	}
	for _, in := range cases {
		enc := Embed(in, DefaultRandomMin, DefaultRandomMax, rng)
		if got := Extract(enc); got != in {
			t.Fatalf("round-trip %q: got %q", in, got)
		}
	}
}

// TestEmbedRunLengths: the total zero-width length equals len(runes)+1 runs
// (one after each rune plus a trailing run — the last two are contiguous),
// each of length in [min,max].
func TestEmbedRunLengths(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const min, max = 3, 5
	const in = "abc"
	enc := Embed(in, min, max, rng)
	n := len([]rune(in))
	zw := 0
	for _, r := range enc {
		if r == ZWChar {
			zw++
		}
	}
	lo, hi := (n+1)*min, (n+1)*max
	if zw < lo || zw > hi {
		t.Fatalf("total zw = %d, want within [%d,%d]: %q", zw, lo, hi, enc)
	}
	if !strings.HasPrefix(enc, "a") || Extract(enc) != in {
		t.Fatalf("structure wrong: %q", enc)
	}
}

// TestEmbedDegenerateRange mirrors randomInt: max <= min collapses to min.
// The trailing run is contiguous with the last rune's run.
func TestEmbedDegenerateRange(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	enc := Embed("ab", 4, 4, rng)
	want := "a" + strings.Repeat(string(ZWChar), 4) + "b" + strings.Repeat(string(ZWChar), 8)
	if enc != want {
		t.Fatalf("enc = %q, want %q", enc, want)
	}
}

func TestExtractStripsOnlyZW(t *testing.T) {
	zw := string(ZWChar)
	in := "a" + zw + "b" + zw + zw + "c" + zw
	if got := Extract(in); got != "abc" {
		t.Fatalf("extract = %q, want %q", got, "abc")
	}
	if Extract("plain") != "plain" {
		t.Fatal("plain text must be unchanged")
	}
}

// TestStylizeVariants: every digit is replaced by a member of its group;
// non-digits pass through.
func TestStylizeVariants(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	out := Stylize("13800138000", false, false, rng)
	if out == "13800138000" {
		t.Fatal("digits should have been replaced")
	}
	// Scan the output against the groups of the input digits (some variants
	// are multi-rune keycap emoji, so compare by prefix).
	remaining := out
	for _, d := range "13800138000" {
		matched := false
		for _, v := range groups[d] {
			if strings.HasPrefix(remaining, v) {
				remaining = remaining[len(v):]
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("output %q does not match digit %q's group (rest %q)", out, d, remaining)
		}
	}
	if remaining != "" {
		t.Fatalf("trailing unmatched output: %q", remaining)
	}

	// Non-digits pass through and break digit runs.
	out2 := Stylize("abc123", false, false, rng)
	if !strings.HasPrefix(out2, "abc") {
		t.Fatalf("non-digits must pass through: %q", out2)
	}
}

// TestStylizeUnifiedStyle: with 固定风格 every digit maps through one style.
func TestStylizeUnifiedStyle(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	out := Stylize("1234567890", false, true, rng)
	// Find which single style produced the whole output.
	matchedStyles := 0
	for _, name := range styleNames {
		st := unifiedStyles[name]
		ok := true
		rest := out
		for _, d := range "1234567890" {
			v := st[d]
			if !strings.HasPrefix(rest, v) {
				ok = false
				break
			}
			rest = rest[len(v):]
		}
		if ok && rest == "" {
			matchedStyles++
		}
	}
	if matchedStyles == 0 {
		t.Fatalf("output %q matches no unified style", out)
	}
}

// TestStylizeSeparator: with useSeparator, a separator appears between every
// pair of consecutive digits, and a non-digit breaks the run (no separator).
// Note the separator table itself contains ' ', so separators are counted by
// reconstruction rather than by character inspection.
func TestStylizeSeparator(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	// "1234" → glyph sep glyph sep glyph sep glyph: exactly 3 separators.
	out := Stylize("1234", true, false, rng)
	if n := countSeparators(t, out, "1234"); n != 3 {
		t.Fatalf("separators = %d, want 3: %q", n, out)
	}
	// "1a2" → the 'a' breaks the digit run: no separators at all.
	out2 := Stylize("1a2", true, false, rng)
	if n := countSeparators(t, out2, "1a2"); n != 0 {
		t.Fatalf("separators = %d, want 0 (run broken): %q", n, out2)
	}
	// Without useSeparator: none even between digits.
	out3 := Stylize("1234", false, false, rng)
	if n := countSeparators(t, out3, "1234"); n != 0 {
		t.Fatalf("separators = %d, want 0: %q", n, out3)
	}
}

// countSeparators reconstructs the conversion of in (digits + passthroughs)
// and returns how many separator-table strings were emitted.
func countSeparators(t *testing.T, out, in string) int {
	t.Helper()
	seps := 0
	rest := out
	for _, ch := range in {
		variants, isDigit := groups[ch]
		if !isDigit {
			if !strings.HasPrefix(rest, string(ch)) {
				t.Fatalf("passthrough %q missing in %q", ch, out)
			}
			rest = rest[len(string(ch)):]
			continue
		}
		// A separator (if any) precedes every digit except the first of a run.
		for _, sep := range separators {
			if strings.HasPrefix(rest, sep) {
				seps++
				rest = rest[len(sep):]
				break
			}
		}
		matched := false
		for _, v := range variants {
			if strings.HasPrefix(rest, v) {
				rest = rest[len(v):]
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("no group variant for %q in rest %q of %q", ch, rest, out)
		}
	}
	if rest != "" {
		t.Fatalf("unconsumed output %q in %q", rest, out)
	}
	return seps
}

// TestStylizeBatch: multi-line input converts per line, preserving line
// structure and empty lines.
func TestStylizeBatch(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	in := "13800138000\n\n13912345678"
	out := StylizeBatch(in, false, false, rng)
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3: %q", len(lines), out)
	}
	if lines[1] != "" {
		t.Fatalf("empty line must be preserved: %q", out)
	}
	if lines[0] == "13800138000" || lines[2] == "13912345678" {
		t.Fatalf("lines should be stylized: %q", out)
	}
}

// TestStylizeNoDigits: digit-free input is returned unchanged.
func TestStylizeNoDigits(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	if out := Stylize("没有数字", true, true, rng); out != "没有数字" {
		t.Fatalf("out = %q", out)
	}
}
