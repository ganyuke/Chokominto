package resolve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"chokominto/internal/store"
)

// Readings are the built-in ways of reading scrobbles, each one or more
// ordinary rules the owner switches on and off in Settings. See
// docs/architecture.md, "Rules".

// Reading is one checkbox in Settings.
type Reading struct {
	Key     string
	Section string // Artists or Titles
	Label   string
	Example string // received text → what it becomes
	rules   []store.Rule
}

func split(pattern string) store.Rule {
	return store.Rule{Kind: "split", Field: "artist", MatchMode: "exact", Pattern: pattern}
}

// Readings in the order Settings lists them.
var Readings = []Reading{
	{"feat", "Artists", "Featured artists", "LiSA feat. Uru → LiSA, featuring Uru",
		[]store.Rule{split(" feat. "), split(" ft. "), split(" featuring ")}},
	{"comma", "Artists", "Lists with commas", "Asami Seto, Nao Toyama → two artists",
		[]store.Rule{split(", ")}},
	{"slash", "Artists", "Lists with a spaced slash", "Azumi Takahashi / Lotus Juice → two artists",
		[]store.Rule{split(" / ")}},
	{"amp", "Artists", "Lists with &", "LiSA & Uru → two artists (off by default: MYTH & ROID is one)",
		[]store.Rule{split(" & ")}},
	{"cross", "Artists", "Lists with ×", "Aimer × Kiyoshi Kawanishi → two artists",
		[]store.Rule{split(" × "), split("×")}},
	{"touten", "Artists", "Lists with 、", "甲、乙 → two artists",
		[]store.Rule{split("、")}},
	{"bareslash", "Artists", "Lists with a slash and no spaces", "A/B → two artists (off by default: Leo/need is one)",
		[]store.Rule{split("/")}},
	{"cv", "Artists", "Character credits", "後藤ひとり(CV:青山吉能) → the character 後藤ひとり, counting for 青山吉能",
		[]store.Rule{{Kind: "cv", Field: "artist", MatchMode: "regex"}}},
	{"group", "Artists", "Groups named with their characters", "桜高軽音部 [平沢唯・秋山澪(CV:…)] → the group, with the characters as members",
		[]store.Rule{{Kind: "group", Field: "artist", MatchMode: "exact"}}},
	{"titles", "Titles", "Titles in two languages", "君のせい - Kiminosei → the song 君のせい, also called Kiminosei",
		[]store.Rule{{Kind: "titles", Field: "title", MatchMode: "exact"}}},
	{"version", "Titles", "Versions", "鏡面の波 (Instrumental) → the instrumental version of 鏡面の波",
		[]store.Rule{{Kind: "version", Field: "title", MatchMode: "exact"}}},
	{"covers", "Titles", "Covers named after the singer", "Shōjo Rei (Cover) - Hoshimachi Suisei, sent by Hoshimachi Suisei → Shōjo Rei",
		[]store.Rule{{Kind: "covers", Field: "title", MatchMode: "exact"}}},
	{"compilation", "Albums", "Soundtracks and compilations", "Tracks of one soundtrack by different composers → one album, credited to all of them",
		[]store.Rule{{Kind: "compilation", Field: "album", MatchMode: "exact"}}},
}

func sameRule(a, b store.Rule) bool {
	return a.Kind == b.Kind && a.Pattern == b.Pattern && a.ArtistMatch == nil && a.TitleMatch == nil && a.AlbumMatch == nil
}

// isReading reports whether a rule is one of the built-in readings.
func isReading(r store.Rule) bool {
	for _, rd := range Readings {
		for _, want := range rd.rules {
			if sameRule(r, want) {
				return true
			}
		}
	}
	return false
}

// ReadingState is a reading and whether it's on.
type ReadingState struct {
	Reading
	On bool
}

// OwnRule is a rule the owner made, described in words.
type OwnRule struct {
	ID      int64
	Text    string
	On      bool
	Listens int // listens it applies to, not counting ones linked by hand
	// The recording a link rule links to, 0 for other rules.
	RecordingID int64
}

// RuleTexts describes each of the owner's rules in words, by rule.
func RuleTexts(ctx context.Context, db *store.DB, userID int64) (map[int64]string, error) {
	rules, err := db.AllRules(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[int64]string{}
	for _, r := range rules {
		if isReading(r) {
			continue
		}
		if out[r.ID], err = describeRule(ctx, db, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReadingSettings returns the built-in readings and the owner's own rules.
func ReadingSettings(ctx context.Context, db *store.DB, userID int64) ([]ReadingState, []OwnRule, error) {
	rules, err := db.AllRules(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	var states []ReadingState
	for _, rd := range Readings {
		on := true
		for _, want := range rd.rules {
			if !slices.ContainsFunc(rules, func(r store.Rule) bool { return r.Enabled && sameRule(r, want) }) {
				on = false
			}
		}
		states = append(states, ReadingState{rd, on})
	}
	var own []OwnRule
	var sources []store.SourceInfo
	for _, r := range rules {
		if isReading(r) {
			continue
		}
		if sources == nil {
			if sources, err = db.SourcesFor(ctx, userID); err != nil {
				return nil, nil, err
			}
		}
		text, err := describeRule(ctx, db, r)
		if err != nil {
			return nil, nil, err
		}
		o := OwnRule{ID: r.ID, Text: text, On: r.Enabled, RecordingID: r.RecordingID}
		var ids []int64
		for _, s := range affected(r, r.RecordingID, sources) {
			ids = append(ids, s.ID)
		}
		if o.Listens, err = db.ListensWithText(ctx, ids); err != nil {
			return nil, nil, err
		}
		own = append(own, o)
	}
	return states, own, nil
}

// describeRule says what a rule does, in the owner's words.
func describeRule(ctx context.Context, db *store.DB, r store.Rule) (string, error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	switch r.Kind {
	case "split":
		return fmt.Sprintf("Split artists on “%s”", r.Pattern), nil
	case "link":
		target, err := db.RecordingName(ctx, r.RecordingID)
		if err != nil {
			return "", err
		}
		what := fmt.Sprintf("“%s” by %s", str(r.TitleMatch), str(r.ArtistMatch))
		if r.MatchMode == "key" {
			return fmt.Sprintf("Always link %s to %s, also with other capitals, full-width letters or spacing", what, target), nil
		}
		if r.AlbumMatch == nil {
			return fmt.Sprintf("Always link %s to %s, whatever the album", what, target), nil
		}
		return fmt.Sprintf("Always link %s on %s to %s", what, str(r.AlbumMatch), target), nil
	case "clean":
		by := ""
		if r.ArtistMatch != nil {
			by = " by " + *r.ArtistMatch
		}
		if r.MatchMode == "regex" {
			if pre, suf, ok := cleanParts(r.Pattern); ok {
				switch {
				case pre == "":
					return fmt.Sprintf("Remove “%s” from the end of titles%s", suf, by), nil
				case suf == "":
					return fmt.Sprintf("Remove “%s” from the start of titles%s", pre, by), nil
				}
				return fmt.Sprintf("Remove “%s” and “%s” from titles%s", pre, suf, by), nil
			}
		}
		return fmt.Sprintf("Remove “%s” from %ss%s", r.Pattern, r.Field, by), nil
	}
	return "A rule", nil
}

// cleanParts reads back the prefix and suffix of a "titles like this" rule.
func cleanParts(pattern string) (string, string, bool) {
	body, ok := strings.CutPrefix(pattern, "^")
	if !ok {
		return "", "", false
	}
	body, ok = strings.CutSuffix(body, "$")
	pre, suf, found := strings.Cut(body, "(.*)")
	if !ok || !found {
		return "", "", false
	}
	return strings.TrimSpace(unquoteMeta(pre)), strings.TrimSpace(unquoteMeta(suf)), true
}

// unquoteMeta undoes regexp.QuoteMeta.
func unquoteMeta(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var ErrNoReading = errors.New("no such reading")

// SetReadings switches built-in readings on or off, keyed by reading, as
// one edit, and reads the text linked automatically again in the
// background. Readings not in on are left as they are. It returns 0 when
// nothing changed.
func SetReadings(ctx context.Context, db *store.DB, userID int64, on map[string]bool) (int64, error) {
	changes, err := readingChanges(ctx, db, userID, on)
	if err != nil || len(changes) == 0 {
		return 0, err
	}
	var want []store.RuleSwitch
	var summary []string
	for _, c := range changes {
		for _, r := range c.rules {
			want = append(want, store.RuleSwitch{Rule: r, On: c.On})
		}
		summary = append(summary, c.Verb()+" reading: "+c.Label)
	}
	var editID int64
	err = db.Write(ctx, func(tx *sql.Tx) error {
		id, err := store.SetRulesTx(ctx, tx, userID, want, strings.Join(summary, ". "))
		if err != nil || id == 0 {
			return err
		}
		editID = id
		return reparseAllTx(ctx, tx, userID)
	})
	return editID, err
}

// ReadingChange is a reading the owner asked to switch.
type ReadingChange struct {
	Reading
	On       bool
	Examples []ReadingExample // a few of the owner's listens it changes
}

// Verb says what the change does, for buttons, headings and summaries.
func (c ReadingChange) Verb() string {
	if c.On {
		return "Turned on"
	}
	return "Turned off"
}

// ReadingExample is received text and how it's read before and after.
type ReadingExample struct {
	Artist, Title string // as received
	Before, After string
	Listens       int
}

// examplesPer is how many examples a change shows. Enough to see what it
// does, few enough to read before saving.
const examplesPer = 4

// readingChanges lists the readings in on whose state differs from now.
func readingChanges(ctx context.Context, db *store.DB, userID int64, on map[string]bool) ([]ReadingChange, error) {
	for key := range on {
		if !slices.ContainsFunc(Readings, func(r Reading) bool { return r.Key == key }) {
			return nil, ErrNoReading
		}
	}
	states, _, err := ReadingSettings(ctx, db, userID)
	if err != nil {
		return nil, err
	}
	var out []ReadingChange
	for _, st := range states {
		if want, ok := on[st.Key]; ok && want != st.On {
			out = append(out, ReadingChange{Reading: st.Reading, On: want})
		}
	}
	return out, nil
}

// PreviewReadings lists what switching readings would change, with a few
// examples from the owner's own listens for each. Nothing is saved.
//
// Examples only compare how the text is read (credits, title, version),
// not where linking would put it, which needs the full resolver. The
// soundtrack reading works on albums, so it has none.
func PreviewReadings(ctx context.Context, db *store.DB, userID int64, on map[string]bool) ([]ReadingChange, error) {
	changes, err := readingChanges(ctx, db, userID, on)
	if err != nil || len(changes) == 0 {
		return changes, err
	}
	rules, err := db.AllRules(ctx, userID)
	if err != nil {
		return nil, err
	}
	now := enabled(rules)
	after := withReadings(rules, changes)
	sources, err := db.SourcesFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, s := range sources {
		if s.LinkedByOwner {
			continue
		}
		raw := Text{s.Artist, s.Title, s.Album}
		t := Clean(raw, now)
		if _, ok := LinkRule(t, now); ok {
			continue
		}
		t.Artist = artistText(t.Artist)
		before, final := previewParse(t, now), previewParse(t, after)
		if before == final {
			continue
		}
		full := true
		for i := range changes {
			c := &changes[i]
			if len(c.Examples) == examplesPer {
				continue
			}
			full = false
			// The change shows text that reads differently without it.
			undone := withReadings(rules, append(slices.Clone(changes[:i:i]), changes[i+1:]...))
			if previewParse(t, undone) == final {
				continue
			}
			n, err := db.ListensWithText(ctx, []int64{s.ID})
			if err != nil {
				return nil, err
			}
			c.Examples = append(c.Examples, ReadingExample{Artist: s.Artist, Title: s.Title, Before: before, After: final, Listens: n})
		}
		if full {
			break
		}
	}
	return changes, nil
}

func enabled(rules []store.Rule) []store.Rule {
	var out []store.Rule
	for _, r := range rules {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out
}

// withReadings is the enabled rules as they'd be after the changes.
func withReadings(rules []store.Rule, changes []ReadingChange) []store.Rule {
	out := slices.Clone(rules)
	for _, c := range changes {
		for _, want := range c.rules {
			found := false
			for i := range out {
				if sameRule(out[i], want) {
					out[i].Enabled, found = c.On, true
				}
			}
			if !found && c.On {
				want.Enabled = true
				out = append(out, want)
			}
		}
	}
	return enabled(out)
}

// previewParse describes how text is read with the given rules. A cover
// naming someone other than the artist is read as sent, as the resolver
// does when the name isn't one of the artist's.
func previewParse(t Text, rules []store.Rule) string {
	p := Parse(t.Artist, t.Title, rules)
	if p.CoverBy != "" {
		p = Parse(t.Artist, t.Title, without(rules, "covers"))
	}
	if strings.TrimSpace(t.Artist) == "" || p.Title == "" || len(p.Credits) == 0 {
		return "Nothing to link, goes to Review"
	}
	return describeParsed(p)
}

// describeParsed says in words what Parse read: "LiSA, featuring Uru —
// 再会", with characters, members, other names and versions. Artists are
// joined with " · ", so two artists never look like one name with a
// comma in it.
func describeParsed(p Parsed) string {
	var mains, feats []string
	for _, c := range p.Credits {
		if c.Role == "featured" {
			feats = append(feats, describeCredit(c))
		} else {
			mains = append(mains, describeCredit(c))
		}
	}
	b := strings.Join(mains, " · ")
	if len(feats) > 0 {
		if b != "" {
			b += ", "
		}
		b += "featuring " + strings.Join(feats, " · ")
	}
	b += " — " + p.Title
	if p.AltTitle != "" {
		b += ", also called " + p.AltTitle
	}
	if p.Version != "" {
		b += " (version: " + p.Version + ")"
	}
	return b
}

func describeCredit(c Credit) string {
	s := c.Name
	if len(c.Voices) > 0 {
		s += " (voiced by " + strings.Join(c.Voices, " · ") + ")"
	}
	if len(c.Members) > 0 {
		var ms []string
		for _, m := range c.Members {
			ms = append(ms, describeCredit(m))
		}
		s += " (with " + strings.Join(ms, " · ") + ")"
	}
	return s
}

// SetOwnRule switches one of the owner's rules on or off, or deletes it,
// as one edit.
func SetOwnRule(ctx context.Context, db *store.DB, userID, ruleID int64, action string) (int64, error) {
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		r, err := store.RuleTx(ctx, tx, ruleID)
		if err != nil {
			return err
		}
		if isReading(r) {
			return ErrNoReading
		}
		text, err := describeRule(ctx, db, r)
		if err != nil {
			return err
		}
		if editID, err = store.ChangeRuleTx(ctx, tx, userID, ruleID, action, text); err != nil || editID == 0 {
			return err
		}
		return reparseAllTx(ctx, tx, userID)
	})
	return editID, err
}

// reparseAllTx queues all text not linked by hand to be read again.
func reparseAllTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	ids, err := store.AutoSourcesTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := store.EnqueueTx(ctx, tx, "reparse", fmt.Sprint("source:", id), "", 0); err != nil {
			return err
		}
	}
	return nil
}
