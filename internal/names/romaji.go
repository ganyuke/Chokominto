package names

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Hepburn romanization for hiragana. Katakana is converted to hiragana
// first. Kana has exactly one reading per character, so this never guesses.
var mono = map[rune]string{
	'あ': "a", 'い': "i", 'う': "u", 'え': "e", 'お': "o",
	'か': "ka", 'き': "ki", 'く': "ku", 'け': "ke", 'こ': "ko",
	'が': "ga", 'ぎ': "gi", 'ぐ': "gu", 'げ': "ge", 'ご': "go",
	'さ': "sa", 'し': "shi", 'す': "su", 'せ': "se", 'そ': "so",
	'ざ': "za", 'じ': "ji", 'ず': "zu", 'ぜ': "ze", 'ぞ': "zo",
	'た': "ta", 'ち': "chi", 'つ': "tsu", 'て': "te", 'と': "to",
	'だ': "da", 'ぢ': "ji", 'づ': "zu", 'で': "de", 'ど': "do",
	'な': "na", 'に': "ni", 'ぬ': "nu", 'ね': "ne", 'の': "no",
	'は': "ha", 'ひ': "hi", 'ふ': "fu", 'へ': "he", 'ほ': "ho",
	'ば': "ba", 'び': "bi", 'ぶ': "bu", 'べ': "be", 'ぼ': "bo",
	'ぱ': "pa", 'ぴ': "pi", 'ぷ': "pu", 'ぺ': "pe", 'ぽ': "po",
	'ま': "ma", 'み': "mi", 'む': "mu", 'め': "me", 'も': "mo",
	'や': "ya", 'ゆ': "yu", 'よ': "yo",
	'ら': "ra", 'り': "ri", 'る': "ru", 'れ': "re", 'ろ': "ro",
	'わ': "wa", 'ゐ': "i", 'ゑ': "e", 'を': "o", 'ん': "n",
	'ゔ': "vu",
	'ぁ': "a", 'ぃ': "i", 'ぅ': "u", 'ぇ': "e", 'ぉ': "o",
	'ゃ': "ya", 'ゅ': "yu", 'ょ': "yo", 'ゎ': "wa",
	'ゕ': "ka", 'ゖ': "ke",
}

// Two-kana combinations: a kana followed by a small ya/yu/yo or small vowel.
var pairs = map[string]string{
	"きゃ": "kya", "きゅ": "kyu", "きょ": "kyo", "ぎゃ": "gya", "ぎゅ": "gyu", "ぎょ": "gyo",
	"しゃ": "sha", "しゅ": "shu", "しょ": "sho", "じゃ": "ja", "じゅ": "ju", "じょ": "jo",
	"ちゃ": "cha", "ちゅ": "chu", "ちょ": "cho", "ぢゃ": "ja", "ぢゅ": "ju", "ぢょ": "jo",
	"にゃ": "nya", "にゅ": "nyu", "にょ": "nyo", "ひゃ": "hya", "ひゅ": "hyu", "ひょ": "hyo",
	"びゃ": "bya", "びゅ": "byu", "びょ": "byo", "ぴゃ": "pya", "ぴゅ": "pyu", "ぴょ": "pyo",
	"みゃ": "mya", "みゅ": "myu", "みょ": "myo", "りゃ": "rya", "りゅ": "ryu", "りょ": "ryo",
	"しぇ": "she", "じぇ": "je", "ちぇ": "che",
	"ふぁ": "fa", "ふぃ": "fi", "ふぇ": "fe", "ふぉ": "fo", "ふゅ": "fyu",
	"てぃ": "ti", "でぃ": "di", "てゅ": "tyu", "でゅ": "dyu", "とぅ": "tu", "どぅ": "du",
	"うぃ": "wi", "うぇ": "we", "うぉ": "wo",
	"ゔぁ": "va", "ゔぃ": "vi", "ゔぇ": "ve", "ゔぉ": "vo",
	"つぁ": "tsa", "つぃ": "tsi", "つぇ": "tse", "つぉ": "tso",
	"いぇ": "ye", "くぁ": "kwa", "ぐぁ": "gwa",
}

// Romaji romanizes the kana in s and leaves everything else as it is.
func Romaji(s string) string {
	rs := []rune(norm.NFKC.String(s))
	for i, r := range rs {
		rs[i] = kataToHira(r)
	}
	var b strings.Builder
	double := false // after a small tsu
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		var syl string
		if i+1 < len(rs) {
			if p, ok := pairs[string(rs[i:i+2])]; ok {
				syl = p
				i++
			}
		}
		if syl == "" {
			switch {
			case r == 'っ':
				double = true
				continue
			case r == 'ー' || r == '〜':
				// Long vowel mark: repeat the previous vowel.
				if v := lastVowel(b.String()); v != 0 {
					b.WriteByte(v)
				}
				continue
			default:
				m, ok := mono[r]
				if !ok {
					if double {
						double = false
					}
					b.WriteRune(r)
					continue
				}
				syl = m
			}
		}
		if double {
			double = false
			if strings.HasPrefix(syl, "ch") {
				b.WriteByte('t')
			} else if c := syl[0]; !strings.ContainsRune("aiueon", rune(c)) {
				b.WriteByte(c)
			}
		}
		b.WriteString(syl)
	}
	return b.String()
}

func lastVowel(s string) byte {
	if s == "" {
		return 0
	}
	c := s[len(s)-1]
	if strings.IndexByte("aiueo", c) >= 0 {
		return c
	}
	return 0
}
