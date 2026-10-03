package resolve

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"chokominto/internal/store"
)

var defaultRules = []store.Rule{
	{Kind: "split", Field: "artist", Pattern: " feat. "},
	{Kind: "split", Field: "artist", Pattern: " ft. "},
	{Kind: "split", Field: "artist", Pattern: " featuring "},
	{Kind: "split", Field: "artist", Pattern: ", "},
	{Kind: "split", Field: "artist", Pattern: " / "},
	{Kind: "cv", Field: "artist"},
	{Kind: "group", Field: "artist"},
	{Kind: "titles", Field: "title"},
	{Kind: "version", Field: "title"},
	{Kind: "covers", Field: "title"},
}

func showCredit(c Credit) string {
	s := c.Name
	if len(c.Voices) > 0 {
		s += "<" + strings.Join(c.Voices, "+") + ">"
	}
	if len(c.Members) > 0 {
		var ms []string
		for _, m := range c.Members {
			ms = append(ms, showCredit(m))
		}
		s += "{" + strings.Join(ms, ", ") + "}"
	}
	return s
}

func show(p Parsed) string {
	var parts []string
	for _, c := range p.Credits {
		parts = append(parts, c.Role+":"+showCredit(c))
	}
	return fmt.Sprintf("%s | %s", strings.Join(parts, ", "), p.Title)
}

func TestParse(t *testing.T) {
	cases := []struct{ artist, title, want string }{
		// Plain credits stay whole, including Japanese separators inside names.
		{"YOASOBI", "アイドル", "main:YOASOBI | アイドル"},
		// A stray separator from the client, seen in real Maloja history.
		{"nicamoq ,", "Song", "main:nicamoq | Song"},
		{"ココ ,", "Song", "main:ココ | Song"},
		{"結束バンド", "ギターと孤独と蒼い惑星", "main:結束バンド | ギターと孤独と蒼い惑星"},
		{"LiSA & Uru", "再会", "main:LiSA & Uru | 再会"},
		{"TK from 凛として時雨", "unravel", "main:TK from 凛として時雨 | unravel"},
		{"Aimer × Kiyoshi Kawanishi", "x", "main:Aimer × Kiyoshi Kawanishi | x"},

		// feat. splitting, any case.
		{"LiSA feat. Uru", "Leo", "main:LiSA, featured:Uru | Leo"},
		{"Kenshi Yonezu FEAT. Daoko", "打上花火", "main:Kenshi Yonezu, featured:Daoko | 打上花火"},
		{"DECO*27 feat. 初音ミク", "ヴァンパイア", "main:DECO*27, featured:初音ミク | ヴァンパイア"},
		{"A feat. A", "x", "main:A | x"},

		// feat. in the title.
		{"Eve", "ドラマツルギー (feat. Guiano)", "main:Eve, featured:Guiano | ドラマツルギー"},
		{"Eve", "Song [ft. A & B]", "main:Eve, featured:A, featured:B | Song"},
		{"Eve", "Song（feat. 花譜）", "main:Eve, featured:花譜 | Song"},
		// Lists written without spaces, or mixed.
		{"x", "Song (feat. 甲&乙&丙)", "main:x, featured:甲, featured:乙, featured:丙 | Song"},
		{"x", "Song (feat. A, B & C)", "main:x, featured:A, featured:B, featured:C | Song"},
		// feat. at the end, without brackets.
		{"x", "Song Title feat. A", "main:x, featured:A | Song Title"},
		{"x", "歌 feat.甲", "main:x, featured:甲 | 歌"},
		{"x", "歌 -feat.x-", "main:x | 歌"},
		// "(English ver.)" is a version.
		{"x", "Song (English ver.) feat. A", "main:x, featured:A | Song"},
		// No space after "feat.", any case.
		{"x", "Song (feat.A)", "main:x, featured:A | Song"},
		{"x", "歌(Feat.甲)", "main:x, featured:甲 | 歌"},
		{"x", "歌　feat.甲", "main:x, featured:甲 | 歌"},
		// Cut short by the client.
		{"x", "歌 (feat. 甲 & 乙 & 丙 &...", "main:x, featured:甲, featured:乙, featured:丙 | 歌"},
		{"x", "歌 (feat. 甲 & 乙…", "main:x, featured:甲, featured:乙 | 歌"},
		// Not a feat.
		{"x", "Light as Feathers", "main:x | Light as Feathers"},
		{"x", "feat. Only", "main:x | feat. Only"},

		// Character credits.
		{"後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "main:後藤ひとり<青山吉能> | ひとりぼっち東京"},
		{"後藤ひとり（CV：青山吉能）", "x", "main:後藤ひとり<青山吉能> | x"},
		{"Hitori Gotoh (CV: Yoshino Aoyama)", "x", "main:Hitori Gotoh<Yoshino Aoyama> | x"},
		{"Hitori Gotoh (CV. Yoshino Aoyama)", "x", "main:Hitori Gotoh<Yoshino Aoyama> | x"},
		{"Hitori Gotoh CV.Yoshino Aoyama", "x", "main:Hitori Gotoh<Yoshino Aoyama> | x"},
		{"Hitori Gotoh (starring Yoshino Aoyama)", "x", "main:Hitori Gotoh<Yoshino Aoyama> | x"},
		{"Hitori Gotoh (VO: Yoshino Aoyama)", "x", "main:Hitori Gotoh<Yoshino Aoyama> | x"},
		// Lists of characters.
		{"Hitori Gotoh (CV: Yoshino Aoyama), Nijika Ijichi (CV: Sayumi Suzushiro)", "x",
			"main:Hitori Gotoh<Yoshino Aoyama>, main:Nijika Ijichi<Sayumi Suzushiro> | x"},
		{"後藤ひとり(CV:青山吉能)、伊地知虹夏(CV:鈴代紗弓)", "x",
			"main:後藤ひとり<青山吉能>, main:伊地知虹夏<鈴代紗弓> | x"},
		{"A (CV: a) & B (CV: b)", "x", "main:A<a>, main:B<b> | x"},
		// Mixed separators, and a final "and".
		{"A (CV:a), B (CV:b) & C (CV:c)", "x", "main:A<a>, main:B<b>, main:C<c> | x"},
		{"A(CV:a), B(CV:b), and C(CV:c)", "x", "main:A<a>, main:B<b>, main:C<c> | x"},
		{"甲(CV:a)/乙(CV:b)", "x", "main:甲<a>, main:乙<b> | x"},
		{"甲(CV.a)&乙(CV.b)", "x", "main:甲<a>, main:乙<b> | x"},
		{"A (CV:a), B (CV:b), & C (CV:c)", "x", "main:A<a>, main:B<b>, main:C<c> | x"},
		{"A (CV: a) ,", "x", "main:A<a> | x"},
		// A plain name in a list of characters is their group.
		{"A (CV: a), Band, and B (CV: b)", "x", "main:Band{A<a>, B<b>} | x"},
		// Separators inside the brackets are between voices.
		{"A (CV: a, b) & B (CV: c)", "x", "main:A<a+b>, main:B<c> | x"},
		// A duet voice.
		{"Twins (CV: a, b)", "x", "main:Twins<a+b> | x"},
		{"結束バンド, 後藤ひとり(CV:青山吉能)", "x", "main:結束バンド{後藤ひとり<青山吉能>} | x"},
		// A character featured on someone's song.
		{"YOASOBI feat. 後藤ひとり(CV:青山吉能)", "x", "main:YOASOBI, featured:後藤ひとり<青山吉能> | x"},

		// Lists, from the owner's history.
		{"Asami Seto, Nao Toyama, Atsumi Tanezaki", "x", "main:Asami Seto, main:Nao Toyama, main:Atsumi Tanezaki | x"},
		{"CHiCO, HoneyWorks", "x", "main:CHiCO, main:HoneyWorks | x"},
		{"Wilbert Roget, II", "x", "main:Wilbert Roget, II | x"},
		{"Harry Connick, Jr.", "x", "main:Harry Connick, Jr. | x"},
		{"Hatsune Miku, Hoshino Ichika, and Yoisaki Kanade", "x", "main:Hatsune Miku, main:Hoshino Ichika, main:Yoisaki Kanade | x"},
		{"Azumi Takahashi / Lotus Juice / ATLUS Sound Team", "x", "main:Azumi Takahashi, main:Lotus Juice, main:ATLUS Sound Team | x"},
		{"Leo/need", "x", "main:Leo/need | x"},
		// A lone separator between commas, from the real history.
		{"Eminem,  & , JID", "x", "main:Eminem, main:JID | x"},
		{"MYTH & ROID", "x", "main:MYTH & ROID | x"},
		{"Tsumiki feat. KAFU, Kaai Yuki", "x", "main:Tsumiki, featured:KAFU, featured:Kaai Yuki | x"},
		// Groups named with their characters.
		{"平沢唯(CV:豊崎愛生), 秋山澪(CV:日笠陽子), 桜高軽音部, 田井中律(CV:佐藤聡美), and 琴吹紬(CV:寿美菜子)", "x",
			"main:桜高軽音部{平沢唯<豊崎愛生>, 秋山澪<日笠陽子>, 田井中律<佐藤聡美>, 琴吹紬<寿美菜子>} | x"},
		{"桜高軽音部 [平沢唯・秋山澪・田井中律・琴吹紬(CV:豊崎愛生、日笠陽子、佐藤聡美、寿美菜子)]", "x",
			"main:桜高軽音部{平沢唯<豊崎愛生>, 秋山澪<日笠陽子>, 田井中律<佐藤聡美>, 琴吹紬<寿美菜子>} | x"},
		{"B小町 ルビー（CV：伊駒ゆりえ）、有馬かな（CV：潘めぐみ）、MEMちょ（CV：大久保瑠美）", "x",
			"main:B小町{ルビー<伊駒ゆりえ>, 有馬かな<潘めぐみ>, MEMちょ<大久保瑠美>} | x"},
		// Two characters with only a space between them.
		{"小糸 侑（CV：高田憂希） 七海燈子（CV：寿 美菜子）", "x", "main:小糸 侑<高田憂希>, main:七海燈子<寿 美菜子> | x"},
		// A character list in a comma list stays several characters.
		{"Mai Sakurajima(CV:Asami Seto), Tomoe Koga(CV:Nao Toyama), and Shoko Makinohara(CV:Inori Minase)", "x",
			"main:Mai Sakurajima<Asami Seto>, main:Tomoe Koga<Nao Toyama>, main:Shoko Makinohara<Inori Minase> | x"},

		// Covers named after the singer.
		{"Hoshimachi Suisei", "Shōjo Rei (Cover) - Hoshimachi Suisei", "main:Hoshimachi Suisei | Shōjo Rei"},
		{"Ado", "うっせぇわ (Cover)", "main:Ado | うっせぇわ"},
		// Another script: the resolver checks the name.
		{"Suisei Hoshimachi", "フォニイ / 星街すいせい(Cover)", "main:Suisei Hoshimachi | フォニイ"},
		// One artist's take on a song.
		{"Mai Sakurajima(CV:Asami Seto)", "不可思議のカルテ 桜島麻衣 Ver.", "main:Mai Sakurajima<Asami Seto> | 不可思議のカルテ"},
		{"Dan Salvato", "Okay, Everyone! (Sayori)", "main:Dan Salvato | Okay, Everyone! (Sayori)"},
		{"Dan Salvato, Sayori", "Okay, Everyone! (Sayori)", "main:Dan Salvato, main:Sayori | Okay, Everyone!"},

		// Nothing sent.
		{"", "x", " | x"},
	}
	for _, c := range cases {
		if got := show(Parse(c.artist, c.title, defaultRules)); got != c.want {
			t.Errorf("Parse(%q, %q)\n got  %s\n want %s", c.artist, c.title, got, c.want)
		}
	}
}

func TestParseRulesOff(t *testing.T) {
	// With the CV rule turned off, character credits stay as written.
	rules := []store.Rule{{Kind: "split", Field: "artist", Pattern: " feat. "}}
	if got := show(Parse("後藤ひとり(CV:青山吉能)", "x", rules)); got != "main:後藤ひとり(CV:青山吉能) | x" {
		t.Error(got)
	}
	// Each reading can be switched off on its own.
	only := func(kinds ...string) []store.Rule {
		var rs []store.Rule
		for _, r := range defaultRules {
			if !slices.Contains(kinds, r.Kind) && !(r.Kind == "split" && slices.Contains(kinds, r.Pattern)) {
				rs = append(rs, r)
			}
		}
		return rs
	}
	for _, c := range []struct {
		off           []string
		artist, title string
		want          string
	}{
		{[]string{", "}, "CHiCO, HoneyWorks", "x", "main:CHiCO, HoneyWorks | x"},
		{[]string{" / "}, "A / B", "x", "main:A / B | x"},
		{[]string{"titles"}, "x", "君のせい - Kiminosei", "main:x | 君のせい - Kiminosei"},
		{[]string{"version"}, "x", "鏡面の波 (Instrumental)", "main:x | 鏡面の波 (Instrumental)"},
		{[]string{"group"}, "結束バンド, 後藤ひとり(CV:青山吉能)", "x", "main:結束バンド, main:後藤ひとり<青山吉能> | x"},
		{[]string{"group"}, "B小町 ルビー（CV：伊駒ゆりえ）、有馬かな（CV：潘めぐみ）", "x", "main:B小町 ルビー<伊駒ゆりえ>, main:有馬かな<潘めぐみ> | x"},
	} {
		if got := show(Parse(c.artist, c.title, only(c.off...))); got != c.want {
			t.Errorf("%v off: Parse(%q, %q)\n got  %s\n want %s", c.off, c.artist, c.title, got, c.want)
		}
	}
	// With no split rules, feat. stays in the name.
	if got := show(Parse("LiSA feat. Uru", "x", nil)); got != "main:LiSA feat. Uru | x" {
		t.Error(got)
	}
}

func TestReadTitle(t *testing.T) {
	for _, c := range []struct{ in, title, alt, version string }{
		{"君のせい - Kiminosei", "君のせい", "Kiminosei", ""},
		{"花になって - Be a flower", "花になって", "Be a flower", ""},
		{"you -卒業- - you -Graduation-", "you -卒業-", "you -Graduation-", ""},
		{"鏡面の波 (Instrumental)", "鏡面の波", "", "Instrumental"},
		{"ICE LIMIT (Off Vocal)", "ICE LIMIT", "", "Off Vocal"},
		{"Nandemonaiya - movie ver.", "Nandemonaiya", "", "movie ver."},
		{"Stellar Stellar - From THE FIRST TAKE", "Stellar Stellar", "", "From THE FIRST TAKE"},
		{"残響散歌 - From THE FIRST TAKE", "残響散歌", "", "From THE FIRST TAKE"},
		{"Zankyosanka (From \"THE FIRST TAKE\")", "Zankyosanka", "", "From \"THE FIRST TAKE\""},
		{"アイドル（TVサイズ）", "アイドル", "", "TVサイズ"},
		{"Nekozilla (LFZ Remix)", "Nekozilla", "", "LFZ Remix"},
		{"鏡面の波 (Instrumental) - Kyoumen no Nami (Instrumental)", "鏡面の波", "Kyoumen no Nami", "Instrumental"},
		// From the owner's history.
		{"secret base ～君がくれたもの～ (10 years after Ver.) - Secret Base~Kimigakuretamono - 10 Years After Version",
			"secret base ～君がくれたもの～", "Secret Base~Kimigakuretamono", "10 Years After Version"},
		{"Feel the winds(TV size) - Feel the Winds (TV Size)", "Feel the winds", "", "TV Size"},
		{"Ticket To Ride (Remastered 2009)", "Ticket To Ride", "", "Remastered 2009"},
		{"Spirit In The Sky (Deluxe Edition)", "Spirit In The Sky", "", "Deluxe Edition"},
		{"君のせい (Remastered 2022) - Kiminosei", "君のせい", "Kiminosei", "Remastered 2022"},
		// The version only on the other-language side.
		{"君のせい - Kiminosei (Instrumental)", "君のせい", "Kiminosei", "Instrumental"},
		{"不可思議のカルテ - Fukashigi no KARTE -Instrumental-", "不可思議のカルテ", "Fukashigi no KARTE", "Instrumental"},
		{"不可思議のカルテ -Instrumental- - Fukashigi no KARTE -Instrumental-", "不可思議のカルテ", "Fukashigi no KARTE", "Instrumental"},
		{"サインはB -有馬かな Solo Ver.-", "サインはB", "", "有馬かな Solo Ver."},
		// TV size with no brackets, and in angle brackets.
		{"五等分のカタチ TV Size", "五等分のカタチ", "", "TV Size"},
		{"The Cruel Angel's Thesis <TV. Size Version>", "The Cruel Angel's Thesis", "", "TV. Size Version"},
		{"恋愛ミリフィルム -TV size.- - Renai millimeter film TV size", "恋愛ミリフィルム", "Renai millimeter film", "TV size."},
		{"TV Size", "TV Size", "", ""},
		// Not versions or other names.
		{"Love Trip (2019)", "Love Trip (2019)", "", ""},
		{"A Storm, A Spire, and A Sanctum (Dvalin's Nest)", "A Storm, A Spire, and A Sanctum (Dvalin's Nest)", "", ""},
		{"Seishun Complex", "Seishun Complex", "", ""},
		{"Merry-Go-Round", "Merry-Go-Round", "", ""},
		{"you -卒業-", "you -卒業-", "", ""},
		{"HIBANA -Reloaded-", "HIBANA -Reloaded-", "", ""},
		{"Liver (Deliver)", "Liver (Deliver)", "", ""},
	} {
		title, alt, version := readTitle(c.in, true, true)
		if title != c.title || alt != c.alt || version != c.version {
			t.Errorf("readTitle(%q) = %q, %q, %q\n want %q, %q, %q", c.in, title, alt, version, c.title, c.alt, c.version)
		}
	}
	if p := Parse("x", "鏡面の波 (Instrumental)", defaultRules); !p.Solo {
		t.Error("instrumental not marked to rank on its own")
	}
	if p := Parse("x", "Nandemonaiya - movie ver.", defaultRules); p.Solo {
		t.Error("movie version marked to rank on its own")
	}
}
