// Package resolve links received text to artists, songs and releases.
package resolve

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"

	"chokominto/internal/names"
	"chokominto/internal/store"
)

// Credit is one artist read out of the received text.
type Credit struct {
	Name    string
	Role    string   // "main" or "featured"
	Voices  []string // set for characters: who they're voiced by
	Members []Credit // set for a group named with its characters
}

type Parsed struct {
	Credits []Credit
	Title   string
	// AltTitle is the other side of "Japanese - romaji or English", added
	// as another name of the song.
	AltTitle string
	// Version is a tail like "Instrumental" or "movie ver.", read off the
	// title. The listen goes to that version of the song.
	Version string
	// Solo marks versions that rank on their own row by default:
	// instrumental, off vocal and karaoke.
	Solo bool
	// CoverBy is the singer a cover title named ("フォニイ / 星街すいせい
	// (Cover)"), when it isn't plainly the artist sent. The resolver checks
	// it's one of the artist's names, and reads the title as sent if not.
	CoverBy string
}

// "(feat. X)" in a title, in ASCII or full-width brackets. A title the
// client cut short ends in "(feat. A & B &..." with no closing bracket.
var titleFeat = regexp.MustCompile(`(?i)[\s　]*[(\[（［【][\s　]*(?:feat\.|ft\.|feat\s|ft\s|featuring\s)[\s　]*(.+?)[\s　]*(?:[)\]）］】]|(?:\.\.\.|…)$)`)

// " feat. X" at the end of a title, or "-feat.X-". Needs the dot or the
// whole word, so titles like "Feathers" are left alone.
var titleFeatTail = regexp.MustCompile(`(?i)(?:[\s　]+|[\s　]*[-－][\s　]*)(?:feat\.|ft\.|featuring\s)[\s　]*(.+?)[\s　]*[-－]?$`)

// Character credits. Matched against NFKC text, so full-width brackets and
// colons are already ASCII here.
var cvPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^([^()]+?)\s*\(\s*(?:cv|vo|c\.v\.)\s*[.:：]?\s*([^()]+?)\s*\)$`),
	regexp.MustCompile(`(?i)^([^()]+?)\s*\(\s*starring\s+([^()]+?)\s*\)$`),
	regexp.MustCompile(`(?i)^([^()]+?)\s+cv\s*[.:]\s*([^()]+)$`),
}

// Separators between several characters or voices. Only used when every
// part matches a CV pattern, so plain group names stay whole.
var listSeps = []string{", ", ",", "、", " / ", "/", " & ", " × ", "×"}

// Between characters, lists also end in "and": "A (CV: a), B (CV: b), and
// C (CV: c)". Longest first, so ", and " isn't read as ", ".
var characterSeps = append([]string{", and ", ", & ", " and "}, append(listSeps, "&")...)

// Separators inside a "(feat. …)" list in a title. Any of them, mixed.
var featSeps = []string{", ", "、", " & ", "&", " and ", " × "}

// Parse reads credits and the clean title out of received text, using the
// owner's split and CV rules.
func Parse(artist, title string, rules []store.Rule) Parsed {
	var splits []string
	var rd reading
	titles, versions, covers := false, false, false
	for _, r := range rules {
		switch {
		case r.Kind == "split" && r.Field == "artist" && r.Pattern != "":
			splits = append(splits, r.Pattern)
		case r.Kind == "cv":
			rd.cv = true
		case r.Kind == "group":
			rd.group = true
		case r.Kind == "titles":
			titles = true
		case r.Kind == "version":
			versions = true
		case r.Kind == "covers":
			covers = true
		}
	}

	p := Parsed{Title: strings.TrimSpace(title)}
	var featured []string
	if m := titleFeat.FindStringSubmatchIndex(p.Title); m != nil {
		featured = splitFold(strings.TrimRight(p.Title[m[2]:m[3]], " &、,"), featSeps)
		p.Title = strings.TrimSpace(p.Title[:m[0]] + p.Title[m[1]:])
	}
	if m := titleFeatTail.FindStringSubmatchIndex(p.Title); m != nil && m[0] > 0 {
		featured = append(featured, splitFold(p.Title[m[2]:m[3]], featSeps)...)
		p.Title = strings.TrimSpace(p.Title[:m[0]])
	}

	if covers {
		if t, by, ok := readCover(p.Title); ok {
			p.Title = t
			if by != "" && names.MatchKey(by) != names.MatchKey(artist) {
				p.CoverBy = by
			}
		}
	}
	p.Title, p.AltTitle, p.Version = readTitle(p.Title, titles, versions)
	p.Solo = p.Version != "" && soloVersion.MatchString(p.Version)

	// feat.-like rules separate main from featured artists. The others
	// (", " and " / " by default) separate artists of equal standing.
	var featSplits, listSplits []string
	for _, sp := range splits {
		if featWord.MatchString(sp) {
			featSplits = append(featSplits, sp)
		} else {
			listSplits = append(listSplits, sp)
		}
	}
	seen := map[string]bool{}
	add := func(c Credit) {
		k := names.MatchKey(c.Name)
		if c.Name == "" || seen[k] {
			return
		}
		seen[k] = true
		p.Credits = append(p.Credits, c)
	}
	for i, chunk := range splitTopFold(artist, featSplits) {
		role := "main"
		if i > 0 {
			role = "featured"
		}
		for _, c := range readList(splitList(chunk, listSplits), rd) {
			c.Role = role
			add(c)
		}
	}
	for _, f := range featured {
		for _, c := range parseChunk(f, rd) {
			c.Role = "featured"
			add(c)
		}
	}
	if versions && p.Version == "" {
		p.Title, p.Version = characterVersion(p.Title, p.Credits)
	}
	return p
}

// A tail in brackets: "Okay, Everyone! (Sayori)".
var bracketTail = regexp.MustCompile(`^(.+?)[\s　]*[(（]([^()（）]+)[)）]$`)

// One word and "Ver." at the end: "不可思議のカルテ 桜島麻衣 Ver.".
var wordVer = regexp.MustCompile(`(?i)^(.+)[\s　]+(\S+[\s　]+ver\.?)$`)

// characterVersion reads one artist's take on a song: a bracket naming a
// credited artist, or a name and "Ver." at the end.
func characterVersion(title string, credits []Credit) (string, string) {
	if m := bracketTail.FindStringSubmatch(title); m != nil {
		key := names.MatchKey(m[2])
		var all []Credit
		for _, c := range credits {
			all = append(append(all, c), c.Members...)
		}
		for _, c := range all {
			if key != "" && names.MatchKey(c.Name) == key {
				return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
			}
		}
	}
	if m := wordVer.FindStringSubmatch(title); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	}
	return title, ""
}

// Covers posted with the singer's name: "Shōjo Rei (Cover) - Hoshimachi
// Suisei", "フォニイ / 星街すいせい(Cover)", or just "Song (Cover)".
var (
	coverThenName = regexp.MustCompile(`(?i)^(.+?)[\s　]*[(（]cover[)）][\s　]*[-/／][\s　]*(.+)$`)
	nameThenCover = regexp.MustCompile(`(?i)^(.+?)[\s　]*[/／][\s　]*(.+?)[\s　]*[(（]cover[)）]$`)
	coverOnly     = regexp.MustCompile(`(?i)^(.+?)[\s　]*[(（]cover[)）]$`)
)

// readCover returns the title without the cover marking, and the singer
// it names, if any.
func readCover(t string) (title, by string, ok bool) {
	if m := coverThenName.FindStringSubmatch(t); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), true
	}
	if m := nameThenCover.FindStringSubmatch(t); m != nil {
		return strings.TrimSpace(m[1]), strings.TrimSpace(m[2]), true
	}
	if m := coverOnly.FindStringSubmatch(t); m != nil {
		return strings.TrimSpace(m[1]), "", true
	}
	return t, "", false
}

var featWord = regexp.MustCompile(`(?i)feat|\bft\b|featuring`)

// nameSuffix is a part after a comma that belongs to the name before it:
// "Wilbert Roget, II", "Harry Connick, Jr.".
var nameSuffix = regexp.MustCompile(`(?i)^(?:I{2,3}|IV|V|VI{1,3}|Jr\.?|Sr\.?)$`)

// splitList splits a chunk on the owner's list separators, outside
// brackets. A name suffix stays with its name, and "and" before the last
// name of a comma list is dropped.
func splitList(chunk string, seps []string) []string {
	parts := splitTopFold(chunk, seps)
	var out []string
	for i, part := range parts {
		if i > 0 && nameSuffix.MatchString(part) {
			out[len(out)-1] += ", " + part
			continue
		}
		if i > 0 {
			part = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(part, "and "), "& "))
		}
		out = append(out, part)
	}
	return out
}

// readList turns the parts of one list into credits. A list of characters
// with one plain name in it is that group and its characters: the group is
// credited, and the characters become its members.
func readList(parts []string, rd reading) []Credit {
	var all []Credit
	var groups, chars []Credit
	for _, part := range parts {
		for _, c := range parseChunk(part, rd) {
			all = append(all, c)
			switch {
			case len(c.Members) > 0:
				return all // a group already named with its characters
			case len(c.Voices) > 0:
				chars = append(chars, c)
			default:
				groups = append(groups, c)
			}
		}
	}
	if rd.group && len(groups) == 1 && len(chars) > 0 {
		g := groups[0]
		g.Members = chars
		return []Credit{g}
	}
	return all
}

// reading is which of the owner's readings apply to artist text.
type reading struct {
	cv    bool // "X (CV: Y)" is a character voiced by Y
	group bool // a group named with its characters credits the group
}

// parseChunk turns one artist chunk into one or more credits.
func parseChunk(chunk string, rd reading) []Credit {
	// A stray list separator at either end ("nicamoq ,") isn't part of the
	// name. Maloja's history has these, from clients joining artists.
	chunk = strings.Trim(chunk, " \t,、，　")
	// Nothing but separators ("Eminem,  & , JID" leaves a lone "&") is no
	// artist at all.
	if chunk == "" || names.MatchKey(chunk) == "" {
		return nil
	}
	if rd.cv {
		n := norm.NFKC.String(chunk)
		if g, ok := groupWithCharacters(n); ok && rd.group {
			return []Credit{g}
		}
		// "A (CV: a), B (CV: b) & C (CV: c)" only splits when every part is
		// a character. Separators inside brackets belong to the voices.
		parts := splitTop(n, characterSeps)
		if len(parts) == 0 {
			return nil
		}
		listed := len(parts) > 1
		if !listed {
			n = parts[0] // "A (CV: a) ," with a stray separator
			parts = splitAfterCV(n)
		}
		if len(parts) > 1 {
			var out []Credit
			for _, part := range parts {
				c, ok := parseCV(part)
				if !ok {
					out = nil
					break
				}
				out = append(out, c)
			}
			if out != nil && listed && rd.group {
				// Only a list with real separators can start with a group
				// name. With just spaces, the space is in a name.
				return leadingGroup(out)
			}
			if out != nil {
				return out
			}
		}
		if c, ok := parseCV(n); ok {
			return []Credit{c}
		}
	}
	return []Credit{{Name: chunk}}
}

// "桜高軽音部 [平沢唯・秋山澪・田井中律・琴吹紬(CV:豊崎愛生、日笠陽子、佐藤聡美、寿美菜子)]":
// a group, then its characters in brackets with their voices in order.
var bracketGroup = regexp.MustCompile(`^(.+?)\s*\[(.+)\]$`)

func groupWithCharacters(s string) (Credit, bool) {
	m := bracketGroup.FindStringSubmatch(s)
	if m == nil {
		return Credit{}, false
	}
	inner := m[2]
	var voices []string
	if cv := regexp.MustCompile(`(?i)\(\s*(?:cv|vo|c\.v\.)\s*[.:]?\s*([^()]+)\)\s*$`).FindStringSubmatchIndex(inner); cv != nil {
		voices = splitAny(inner[cv[2]:cv[3]], listSeps)
		inner = inner[:cv[0]]
	}
	if len(voices) == 0 {
		return Credit{}, false
	}
	names := splitAny(inner, []string{"・", "、", ", ", "/"})
	g := Credit{Name: strings.TrimSpace(m[1])}
	for i, n := range names {
		c := Credit{Name: n}
		if len(voices) == len(names) {
			c.Voices = []string{voices[i]}
		} else if len(names) == 1 {
			c.Voices = voices
		}
		g.Members = append(g.Members, c)
	}
	return g, g.Name != ""
}

// splitAfterCV splits characters written one after another with only a
// space between them: "小糸 侑(CV:高田憂希) 七海燈子(CV:寿 美菜子)".
func splitAfterCV(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth = max(depth-1, 0)
			rest := strings.TrimLeft(s[i+1:], " ")
			if depth == 0 && len(rest) < len(s[i+1:]) && rest != "" {
				if _, ok := parseCV(firstCV(rest)); ok {
					out = append(out, strings.TrimSpace(s[start:i+1]))
					start = i + 1
				}
			}
		}
	}
	return append(out, strings.TrimSpace(s[start:]))
}

// firstCV is the text up to the end of the first bracket.
func firstCV(s string) string {
	if i := strings.IndexByte(s, ')'); i >= 0 {
		return s[:i+1]
	}
	return s
}

// leadingGroup reads "B小町 ルビー(CV:…)、有馬かな(CV:…)" as the group B小町
// and its characters, when only the first character's name has a space.
func leadingGroup(chars []Credit) []Credit {
	if len(chars) < 2 {
		return chars
	}
	i := strings.LastIndex(chars[0].Name, " ")
	if i <= 0 {
		return chars
	}
	for _, c := range chars[1:] {
		if strings.Contains(c.Name, " ") {
			return chars
		}
	}
	g := Credit{Name: strings.TrimSpace(chars[0].Name[:i])}
	chars[0].Name = strings.TrimSpace(chars[0].Name[i+1:])
	g.Members = chars
	return []Credit{g}
}

func parseCV(s string) (Credit, bool) {
	for _, re := range cvPatterns {
		if m := re.FindStringSubmatch(s); m != nil {
			name := strings.TrimSpace(m[1])
			voices := splitAny(m[2], listSeps)
			// A name with a list separator in it is several artists, not
			// one character: "結束バンド, 後藤ひとり(CV:…)" stays as written.
			if name == "" || len(voices) == 0 || strings.ContainsAny(name, ",、&×/") {
				continue
			}
			return Credit{Name: name, Voices: voices}, true
		}
	}
	return Credit{}, false
}

// splitFold splits s on any of the delimiters, ignoring case.
func splitFold(s string, delims []string) []string {
	lower := strings.ToLower(s)
	var out []string
	start := 0
	for i := 0; i < len(s); {
		matched := 0
		for _, d := range delims {
			if d != "" && strings.HasPrefix(lower[i:], strings.ToLower(d)) {
				matched = len(d)
				break
			}
		}
		if matched > 0 {
			out = append(out, s[start:i])
			i += matched
			start = i
			continue
		}
		i++
	}
	out = append(out, s[start:])
	var kept []string
	for _, c := range out {
		if c = strings.TrimSpace(c); c != "" {
			kept = append(kept, c)
		}
	}
	return kept
}

// splitTop splits s on any of the separators outside brackets.
func splitTop(s string, seps []string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth = max(depth-1, 0)
		}
		if depth == 0 {
			if n := sepAt(s[i:], seps); n > 0 {
				out = append(out, s[start:i])
				i += n
				start = i
				continue
			}
		}
		i++
	}
	out = append(out, s[start:])
	var kept []string
	for _, p := range out {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return kept
}

func sepAt(s string, seps []string) int {
	for _, sep := range seps {
		if strings.HasPrefix(s, sep) {
			return len(sep)
		}
	}
	return 0
}

// splitAny splits on the first separator that occurs in s.
func splitAny(s string, seps []string) []string {
	for _, sep := range seps {
		if strings.Contains(s, sep) {
			var out []string
			for _, p := range strings.Split(s, sep) {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	if s = strings.TrimSpace(s); s != "" {
		return []string{s}
	}
	return nil
}

// splitTopFold splits s on any of the delimiters, ignoring case, outside
// brackets, so "X (CV: a, b)" keeps its voices together.
func splitTopFold(s string, delims []string) []string {
	lower := strings.ToLower(s)
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth = max(depth-1, 0)
		}
		if depth == 0 {
			if n := sepAt(lower[i:], lowerAll(delims)); n > 0 {
				out = append(out, s[start:i])
				i += n
				start = i
				continue
			}
		}
		// Full-width brackets are multi-byte, so step by rune.
		if strings.HasPrefix(s[i:], "（") || strings.HasPrefix(s[i:], "［") {
			depth++
		} else if strings.HasPrefix(s[i:], "）") || strings.HasPrefix(s[i:], "］") {
			depth = max(depth-1, 0)
		}
		i++
	}
	out = append(out, s[start:])
	var kept []string
	for _, c := range out {
		if c = strings.TrimSpace(c); c != "" {
			kept = append(kept, c)
		}
	}
	return kept
}

func lowerAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

// Titles

var japanese = regexp.MustCompile(`[\p{Hiragana}\p{Katakana}\p{Han}]`)

// A version tail in brackets at the end of a title: "(Instrumental)",
// "（TV size）", "[Remix]", "(From \"THE FIRST TAKE\")".
var versionBracket = regexp.MustCompile(`(?i)^(.+?)[\s　]*[(（\[［]([^()（）\[\]［］]*(?:instrumental|\binst\b\.?|off[ -]?vocal|karaoke|カラオケ|tv[ -]?size|tvサイズ|\bver\b\.?|version|バージョン|remix|\bmix\b|\bedit\b|\blive\b|acoustic|remaster|edition|\bfrom\s)[^()（）\[\]［］]*)[)）\]］]$`)

// The same after a dash: "Nandemonaiya - movie ver.", "Stellar Stellar -
// From THE FIRST TAKE". A tail with brackets of its own is another name
// with a version, not a version.
var versionDash = regexp.MustCompile(`(?i)^(.+)\s[-‐–—]\s([^()（）\[\]［］-]*(?:instrumental|\binst\b\.?|off[ -]?vocal|karaoke|tv[ -]?size|\bver\b\.?|version|remix|\bmix\b|\bedit\b|\blive\b|acoustic|remaster|edition|\bfrom\s)[^()（）\[\]［］-]*)$`)

// A version between hyphens or tildes at the end: "不可思議のカルテ
// -Instrumental-", "サインはB -有馬かな Solo Ver.-".
var versionHyphen = regexp.MustCompile(`(?i)^(.+?)[\s　]*[-－~～]([^-－~～]*(?:instrumental|\binst\b\.?|off[ -]?vocal|karaoke|カラオケ|tv[ -]?size|tvサイズ|\bver\b\.?|version|remix|\bedit\b|\blive\b|acoustic|remaster)[^-－~～]*)[-－~～]$`)

var soloVersion = regexp.MustCompile(`(?i)instrumental|\binst\b|off[ -]?vocal|karaoke|カラオケ`)

// readTitle splits a title into the song's name, another name for it, and
// a version, as far as the owner's titles and version rules are on.
func readTitle(t string, titles, versions bool) (title, alt, version string) {
	title = t
	if !titles && !versions {
		return title, "", ""
	}
	if m := versionDash.FindStringSubmatch(title); versions && m != nil {
		title, version = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
	}
	// "君のせい - Kiminosei": Japanese on the left, none on the right.
	for _, i := range dashes(title) {
		if !titles {
			break
		}
		left, right := strings.TrimSpace(title[:i]), strings.TrimSpace(title[i+3:])
		if japanese.MatchString(left) && !japanese.MatchString(right) && strings.IndexFunc(right, isLetter) >= 0 {
			title, alt = left, right
			break
		}
	}
	if versions {
		// A bracketed version comes off the title even when a dash tail gave
		// one already: they name the same version.
		if m := versionHyphen.FindStringSubmatch(title); m != nil {
			title = strings.TrimSpace(m[1])
			if version == "" {
				version = strings.TrimSpace(m[2])
			}
		}
		if am := versionHyphen.FindStringSubmatch(alt); am != nil {
			alt = strings.TrimSpace(am[1])
			if version == "" {
				version = strings.TrimSpace(am[2])
			}
		}
		if m := versionBracket.FindStringSubmatch(title); m != nil {
			title = strings.TrimSpace(m[1])
			if version == "" {
				version = strings.TrimSpace(m[2])
			}
		}
		// The other name can carry the tail too, or alone: "君のせい -
		// Kiminosei (Instrumental)" is the instrumental.
		if am := versionBracket.FindStringSubmatch(alt); am != nil {
			alt = strings.TrimSpace(am[1])
			if version == "" {
				version = strings.TrimSpace(am[2])
			}
		}
		// The same title twice: "Feel the winds(TV size) - Feel the Winds".
		for _, i := range dashes(title) {
			left, right := strings.TrimSpace(title[:i]), strings.TrimSpace(title[i+3:])
			lv := ""
			if m := versionBracket.FindStringSubmatch(left); m != nil {
				left, lv = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
			}
			if names.MatchKey(left) != "" && names.MatchKey(left) == names.MatchKey(right) {
				title = left
				if version == "" {
					version = lv
				}
				break
			}
		}
	}
	return title, alt, version
}

// dashes returns the positions of " - " outside brackets.
func dashes(s string) []int {
	var out []int
	depth := 0
	for i := 0; i+3 <= len(s); i++ {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth = max(depth-1, 0)
		}
		if depth == 0 && s[i:i+3] == " - " {
			out = append(out, i)
		}
	}
	return out
}

func isLetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
