// Package names turns artist, title and album text into keys for matching,
// so the same name written with different width, case, spacing, kana type
// or long-vowel spelling lands on the same key. See docs/architecture.md,
// "Matching".
package names

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var fold = cases.Fold()

// MatchKey is the key used for auto-linking: NFKC, case folded, katakana
// turned into hiragana, Latin accents removed, and whitespace and
// punctuation dropped (except the long-vowel mark ー, which changes the
// word). A name made only of punctuation or symbols keeps them, so it
// still has a key.
func MatchKey(s string) string {
	s = norm.NFKC.String(s)
	s = fold.String(s)
	s = stripLatinMarks(s)
	var b strings.Builder
	for _, r := range s {
		r = kataToHira(r)
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Mn, r) || r == 'ー' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return strings.Join(strings.Fields(s), " ")
	}
	return b.String()
}

// stripLatinMarks removes accents from Latin letters (Ō → O) but keeps
// every other combining mark, like the dakuten in が.
func stripLatinMarks(s string) string {
	d := norm.NFD.String(s)
	var b strings.Builder
	var prev rune
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) && prev != 0 && prev < 0x0250 {
			continue
		}
		b.WriteRune(r)
		if !unicode.Is(unicode.Mn, r) {
			prev = r
		}
	}
	return norm.NFC.String(b.String())
}

func kataToHira(r rune) rune {
	if r >= 'ァ' && r <= 'ヶ' {
		return r - 0x60
	}
	return r
}

// IsJapanese reports whether s contains kana or kanji.
func IsJapanese(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) {
			return true
		}
	}
	return false
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// RomajiKey is the match key of s romanized, with long vowels folded, for
// names written in kana (Latin parts are kept). It's empty when s has
// kanji, which can't be romanized without guessing, or has no kana at all.
func RomajiKey(s string) string {
	if !IsJapanese(s) || hasHan(s) {
		return ""
	}
	return FoldLongVowels(MatchKey(Romaji(s)))
}

// FoldLongVowels collapses long-vowel spellings so "Toukyou", "Tōkyō" (after
// MatchKey) and "Tookyoo" all become "tokyo". Apply it to Latin match keys
// before comparing them with a RomajiKey.
func FoldLongVowels(key string) string {
	r := strings.NewReplacer("ou", "o", "oo", "o", "uu", "u", "aa", "a", "ii", "i", "ee", "e")
	for {
		next := r.Replace(key)
		if next == key {
			return key
		}
		key = next
	}
}

// soundtrackName says an album is a soundtrack or compilation.
var soundtrackName = regexp.MustCompile(`(?i)\bost\b|soundtrack|sound track|original score|\bscore\b|サウンドトラック|サントラ|compilation|コンピレーション`)

// CompilationName reports whether albums of different artists with this
// name are likely the same album: the name says it's a soundtrack, or it's
// long enough to be specific. "Best" by two bands stays two albums (owner,
// 2026-10-01).
func CompilationName(album string) bool {
	return soundtrackName.MatchString(album) || len([]rune(MatchKey(album))) >= 12
}
