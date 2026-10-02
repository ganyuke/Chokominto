package names

import "testing"

func TestGuessKey(t *testing.T) {
	for in, want := range map[string]string{
		"青春コンプレックス": "seishunkonpurekkusu",
		"群青":        "gunjo",
		"残酷な天使のテーゼ": "zankokunatenshinoteze",
		"アイドル":      "", // kana only: RomajiKey has it exactly
		"Idol":      "",
	} {
		if got := GuessKey(in); got != want {
			t.Errorf("GuessKey(%q) = %q, want %q", in, got, want)
		}
	}
}
