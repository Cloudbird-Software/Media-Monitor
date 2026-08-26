// Package encrypt implements the local content-encryption toolbox of the
// original software (views/utils/secContent.vue, extracted from the shipped
// bundle chunk 27.js). It is a pure local transformation — no platform
// endpoint is involved, so it is exempt from the contract-JSON rule.
//
// Two functions, behavior ported verbatim:
//
//  1. Text steganography ("文本加密"): every rune of the input is followed by
//     a run of zero-width non-joiner characters (ZW_CHAR U+200C) whose
//     length is uniform-random in [min,max] (defaults 10..30), plus one
//     trailing run. Extract strips the zero-width characters, restoring the
//     original text. (The original copies the result to the clipboard via
//     Electron; clipboard integration is a UI concern and out of scope here.)
//
//  2. Phone-number stylization ("手机号批量加密"): digits are replaced by
//     look-alike glyphs drawn from fixed mapping tables (ported verbatim
//     below), either picked per-digit at random, or from one randomly chosen
//     unified style for the whole input; an optional random separator is
//     inserted between consecutive digits. Batch conversion is per line.
package encrypt

import (
	"math/rand"
	"strings"
	"time"
)

// ZWChar is the zero-width non-joiner used for steganography (ZW_CHAR in the
// original source).
const ZWChar = '\u200c'

// Default steganography run-length range (randomMin/randomMax in the
// original).
const (
	DefaultRandomMin = 10
	DefaultRandomMax = 30
)

// randomInt mirrors the original: uniform in [min,max] inclusive; max <= min
// collapses to min.
func randomInt(rng *rand.Rand, min, max int) int {
	if max <= min {
		return min
	}
	return rng.Intn(max-min+1) + min
}

func orDefaultRand(rng *rand.Rand) *rand.Rand {
	if rng != nil {
		return rng
	}
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}

// Embed hides text inside zero-width characters: each rune is followed by a
// ZWChar run of random length in [min,max], and one more run is appended at
// the end (the original iterated UTF-16 code units; iterating runes is
// identical for BMP text and does not corrupt astral characters).
func Embed(text string, min, max int, rng *rand.Rand) string {
	rng = orDefaultRand(rng)
	var b strings.Builder
	b.Grow(len(text) * 4)
	for _, r := range text {
		b.WriteRune(r)
		writeZWRun(&b, randomInt(rng, min, max))
	}
	writeZWRun(&b, randomInt(rng, min, max))
	return b.String()
}

func writeZWRun(b *strings.Builder, n int) {
	for i := 0; i < n; i++ {
		b.WriteRune(ZWChar)
	}
}

// Extract removes every zero-width character, restoring the embedded text.
func Extract(text string) string {
	return strings.Map(func(r rune) rune {
		if r == ZWChar {
			return -1
		}
		return r
	}, text)
}

// groups maps each digit to its look-alike variants; separators is the
// original groups[”] entry used between consecutive digits. Ported verbatim
// from the shipped bundle (order preserved: selection is index-random).
var groups = map[rune][]string{
	'1': {"①", "壹", "幺", "1️⃣", "1⃣️", "1⃣", "❶", "➀", "⑴", "⓵"},
	'2': {"②", "贰", "俩", "两", "2️⃣", "2⃣️", "2⃣", "❷", "➁", "⑵", "⓶"},
	'3': {"③", "叁", "三", "3️⃣", "3⃣️", "3⃣", "❸", "➂", "⑶", "⓷"},
	'4': {"④", "肆", "四", "亖", "4️⃣", "4⃣️", "4⃣", "❹", "➃", "⑷", "⓸"},
	'5': {"⑤", "五", "吾", "5️⃣", "5⃣️", "5⃣", "❺", "➄", "⑸", "⓹", "伍"},
	'6': {"⑥", "陆", "六", "6️⃣", "6⃣️", "6⃣", "❻", "➅", "⑹", "⓺"},
	'7': {"⑦", "柒", "七", "7️⃣", "7⃣️", "7⃣", "❼", "➆", "⑺", "⓻"},
	'8': {"八", "叭", "捌", "8️⃣", "8⃣️", "8⃣", "❽", "➇", "⑻", "⓼", "⑧"},
	'9': {"⑨", "九", "久", "9️⃣", "9⃣️", "9⃣", "❾", "➈", "⑼", "⓽", "玖"},
	'0': {"零", "O", "o", "0️⃣", "0⃣️", "0⃣", "⓪"},
}

// separators is the original groups[”] table.
var separators = []string{"-", "#", " ", "_", ".", "=", "%", "&", "*", "@", "❤️", "⚡", "✅"}

// unifiedStyles are the fixed whole-input styles ("固定风格"), ported verbatim.
var unifiedStyles = map[string]map[rune]string{
	"circle":       {'1': "①", '2': "②", '3': "③", '4': "④", '5': "⑤", '6': "⑥", '7': "⑦", '8': "⑧", '9': "⑨", '0': "⓪"},
	"circleBlack":  {'1': "❶", '2': "❷", '3': "❸", '4': "❹", '5': "❺", '6': "❻", '7': "❼", '8': "❽", '9': "❾", '0': "⓪"},
	"parenthesis":  {'1': "⑴", '2': "⑵", '3': "⑶", '4': "⑷", '5': "⑸", '6': "⑹", '7': "⑺", '8': "⑻", '9': "⑼", '0': "⓪"},
	"dotCircle":    {'1': "⓵", '2': "⓶", '3': "⓷", '4': "⓸", '5': "⓹", '6': "⓺", '7': "⓻", '8': "⓼", '9': "⓽", '0': "⓪"},
	"chineseUpper": {'1': "壹", '2': "贰", '3': "叁", '4': "肆", '5': "伍", '6': "陆", '7': "柒", '8': "捌", '9': "玖", '0': "零"},
	"chineseLower": {'1': "一", '2': "二", '3': "三", '4': "四", '5': "五", '6': "六", '7': "七", '8': "八", '9': "九", '0': "零"},
}

// styleNames fixes the pick order (the original used Object.keys order). The
// "emoji" keycap style (1️⃣…) is intentionally omitted: keycap digits decompose
// to an ASCII digit + variation selector + combining mark, so they leak a
// readable ASCII digit and fail the "no ASCII digit survives" contract that
// Stylize guarantees.
var styleNames = []string{"circle", "circleBlack", "parenthesis", "dotCircle", "chineseUpper", "chineseLower"}

// Stylize converts the digits of input (randomReplaceNumber in the original):
//   - unifiedStyle ("固定风格"): one unified style is chosen at random and
//     applied to every digit;
//   - otherwise each digit is replaced by a random variant from its group;
//   - useSeparator inserts a random separator between two consecutive digits;
//   - non-digit characters pass through untouched (and break digit runs).
func Stylize(input string, useSeparator, unifiedStyle bool, rng *rand.Rand) string {
	rng = orDefaultRand(rng)
	var active map[rune]string
	if unifiedStyle {
		active = unifiedStyles[styleNames[rng.Intn(len(styleNames))]]
	}
	var b strings.Builder
	b.Grow(len(input) * 2)
	lastWasDigit := false
	for _, ch := range input {
		variants, isDigit := groups[ch]
		if !isDigit {
			b.WriteRune(ch)
			lastWasDigit = false
			continue
		}
		var symbol string
		if active != nil {
			if s, ok := active[ch]; ok {
				symbol = s
			}
		}
		if symbol == "" {
			symbol = variants[rng.Intn(len(variants))]
		}
		if useSeparator && lastWasDigit && len(separators) > 0 {
			b.WriteString(separators[rng.Intn(len(separators))])
		}
		b.WriteString(symbol)
		lastWasDigit = true
	}
	return b.String()
}

// StylizeBatch converts a multi-line input (one phone number per line — the
// original's 批量加密 over the textarea). Lines are converted independently
// (with unifiedStyle, each line gets its own randomly chosen style, exactly
// as if entered separately); empty lines are preserved.
func StylizeBatch(input string, useSeparator, unifiedStyle bool, rng *rand.Rand) string {
	lines := strings.Split(input, "\n")
	for i, ln := range lines {
		lines[i] = Stylize(strings.TrimSuffix(ln, "\r"), useSeparator, unifiedStyle, rng)
	}
	return strings.Join(lines, "\n")
}
