package names

import "testing"

func TestMatchKey(t *testing.T) {
	same := [][]string{
		{"YOASOBI", "ＹＯＡＳＯＢＩ", "yoasobi", " Yoasobi "},
		{"アイドル", "あいどる", "ｱｲﾄﾞﾙ"},
		{"結束バンド", "結束 バンド", "結束・バンド"},
		{"World Is Mine", "world is mine", "World-Is-Mine!"},
		{"Ōkami", "Okami"},
		{"ギターと孤独と蒼い惑星", "ギターと孤独と蒼い惑星 "},
	}
	for _, group := range same {
		want := MatchKey(group[0])
		for _, s := range group[1:] {
			if got := MatchKey(s); got != want {
				t.Errorf("MatchKey(%q) = %q, want %q (same as %q)", s, got, want, group[0])
			}
		}
	}
	different := [][2]string{
		{"がっこう", "かっこう"}, // dakuten matters
		{"コーヒー", "コヒ"},   // the long-vowel mark changes the word
		{"Main Theme", "Main Themes"},
	}
	for _, p := range different {
		if MatchKey(p[0]) == MatchKey(p[1]) {
			t.Errorf("%q and %q should differ", p[0], p[1])
		}
	}
	if MatchKey("♡") == "" || MatchKey("!!!") == "" {
		t.Error("symbol-only names need a key")
	}
}

func TestRomaji(t *testing.T) {
	cases := map[string]string{
		"アイドル":           "aidoru",
		"ひとりぼっち":         "hitoribotchi",
		"しゃかしゃか":         "shakashaka",
		"マッチ":            "matchi",
		"がっこう":           "gakkou",
		"コーヒー":           "koohii",
		"ヴァイオリン":         "vaiorin",
		"ティーンエイジャー":      "tiineijaa", // no n' apostrophe, keys don't need it
		"きゃりーぱみゅぱみゅ":     "kyariipamyupamyu",
		"アイドル (TV size)": "aidoru (TV size)",
	}
	for in, want := range cases {
		if got := Romaji(in); got != want {
			t.Errorf("Romaji(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRomajiKeyMatchesLatinSpellings(t *testing.T) {
	cases := [][2]string{
		{"アイドル", "Aidoru"},
		{"とうきょう", "Tōkyō"},
		{"とうきょう", "Toukyou"},
		{"とうきょう", "Tokyo"},
		{"コネクト", "Konekuto"},
	}
	for _, c := range cases {
		if got, want := RomajiKey(c[0]), FoldLongVowels(MatchKey(c[1])); got != want {
			t.Errorf("RomajiKey(%q) = %q, Latin %q folds to %q", c[0], got, c[1], want)
		}
	}
	if RomajiKey("紅蓮華") != "" {
		t.Error("kanji must not be romanized")
	}
	if RomajiKey("LiSA") != "" {
		t.Error("Latin-only names have no romaji key")
	}
}

func TestIsJapanese(t *testing.T) {
	for s, want := range map[string]bool{"アイドル": true, "紅蓮華": true, "YOASOBI": false, "LiSA × Uru": false} {
		if IsJapanese(s) != want {
			t.Errorf("IsJapanese(%q) != %v", s, want)
		}
	}
}
