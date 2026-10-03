package resolve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"chokominto/internal/names"
	"chokominto/internal/store"
)

// Clean and link rules. See docs/architecture.md, "Rules".
//
// A rule's artist, title and album conditions pick the text it applies to.
// For link rules, match_mode says how they compare: exact, key (ignoring
// case, width and spacing), prefix, suffix or regex. Clean rule conditions
// always compare exactly, and match_mode says how the pattern is removed
// from the rule's field: exact (every occurrence), prefix, suffix, or regex
// (keeps the first group when there is one, else removes every match).

// Text is received text as the resolver reads it.
type Text struct {
	Artist, Title, Album string
}

func matches(mode string, cond *string, text string) bool {
	if cond == nil {
		return true
	}
	c := *cond
	switch mode {
	case "key":
		return names.MatchKey(c) == names.MatchKey(text)
	case "prefix":
		return strings.HasPrefix(text, c)
	case "suffix":
		return strings.HasSuffix(text, c)
	case "regex":
		re, err := regexp.Compile(c)
		return err == nil && re.MatchString(text)
	}
	return c == text
}

func (t *Text) field(f string) *string {
	switch f {
	case "artist":
		return &t.Artist
	case "title":
		return &t.Title
	case "album":
		return &t.Album
	}
	return nil
}

// Clean applies the enabled clean rules in order.
func Clean(t Text, rules []store.Rule) Text {
	for _, r := range rules {
		if r.Kind != "clean" || r.Pattern == "" {
			continue
		}
		if !matches("exact", r.ArtistMatch, t.Artist) || !matches("exact", r.TitleMatch, t.Title) || !matches("exact", r.AlbumMatch, t.Album) {
			continue
		}
		f := t.field(r.Field)
		if f == nil {
			continue
		}
		v := *f
		switch r.MatchMode {
		case "prefix":
			v = strings.TrimPrefix(v, r.Pattern)
		case "suffix":
			v = strings.TrimSuffix(v, r.Pattern)
		case "regex":
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				continue
			}
			if re.NumSubexp() > 0 {
				if m := re.FindStringSubmatch(v); m != nil {
					v = m[1]
				}
			} else {
				v = re.ReplaceAllString(v, "")
			}
		default:
			v = strings.ReplaceAll(v, r.Pattern, "")
		}
		*f = strings.TrimSpace(v)
	}
	return t
}

// LinkRule returns the first enabled link rule for this text.
func LinkRule(t Text, rules []store.Rule) (store.Rule, bool) {
	for _, r := range rules {
		if r.Kind != "link" || r.RecordingID == 0 {
			continue
		}
		if matches(r.MatchMode, r.ArtistMatch, t.Artist) && matches(r.MatchMode, r.TitleMatch, t.Title) && matches(r.MatchMode, r.AlbumMatch, t.Album) {
			return r, true
		}
	}
	return store.Rule{}, false
}

// ReparseJob handles a "reparse" job with key "source:<id>": resolving
// text again after a rule changed. Text linked by hand is left alone.
func (r *Resolver) ReparseJob(ctx context.Context, j store.Job) error {
	id, err := sourceKey(j.Key)
	if err != nil {
		return err
	}
	return r.DB.Write(ctx, func(tx *sql.Tx) error {
		src, err := store.SourceTx(ctx, tx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = reparseTx(ctx, tx, src)
		return err
	})
}

func sourceKey(key string) (int64, error) {
	idText, ok := strings.CutPrefix(key, "source:")
	if !ok {
		return 0, fmt.Errorf("job with key %q", key)
	}
	return strconv.ParseInt(idText, 10, 64)
}

func reparseTx(ctx context.Context, tx *sql.Tx, src store.Source) (Outcome, error) {
	if src.RecordingID.Valid {
		auto, err := store.AutoLinkedTx(ctx, tx, src.ID)
		if err != nil || !auto {
			return AlreadyLinked, err
		}
	}
	return linkTx(ctx, tx, src)
}

// Rule offers after linking by hand ("remember this?").

// Offer is a broader rule the owner can save after linking text by hand.
type Offer struct {
	Kind    string // "album", "key" or "clean"
	Rule    store.Rule
	Target  int64 // the recording the text was linked to
	Listens int   // other listens it would move, not counting the text just linked
	// For "clean": what's removed before and after the title.
	Prefix, Suffix string
}

// Offers returns the rules worth offering after sourceID was linked to
// recordingID by hand.
func Offers(ctx context.Context, db *store.DB, userID, sourceID, recordingID int64) ([]Offer, error) {
	src, err := db.SourceInfo(ctx, userID, sourceID)
	if err != nil {
		return nil, err
	}
	titles, ambiguous, err := db.RecordingTitles(ctx, userID, recordingID)
	if err != nil {
		return nil, err
	}
	artist, title := src.Artist, src.Title
	var out []Offer
	if !ambiguous && strings.TrimSpace(artist) != "" && strings.TrimSpace(title) != "" {
		out = append(out,
			Offer{Kind: "album", Target: recordingID, Rule: store.Rule{Kind: "link", MatchMode: "exact", ArtistMatch: &artist, TitleMatch: &title, RecordingID: recordingID}},
			Offer{Kind: "key", Target: recordingID, Rule: store.Rule{Kind: "link", MatchMode: "key", ArtistMatch: &artist, TitleMatch: &title, RecordingID: recordingID}})
	}
	// Titles like this: the song's name inside the received title.
	for _, name := range titles {
		i := strings.Index(title, name)
		if i < 0 || (i == 0 && len(name) == len(title)) {
			continue
		}
		prefix, suffix := title[:i], title[i+len(name):]
		pattern := "^" + regexp.QuoteMeta(prefix) + "(.*)" + regexp.QuoteMeta(suffix) + "$"
		out = append(out, Offer{Kind: "clean", Target: recordingID, Prefix: strings.TrimSpace(prefix), Suffix: strings.TrimSpace(suffix),
			Rule: store.Rule{Kind: "clean", Field: "title", MatchMode: "regex", Pattern: pattern, ArtistMatch: &artist}})
		break
	}
	if len(out) == 0 {
		return nil, nil
	}
	all, err := db.SourcesFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		var ids []int64
		for _, s := range affected(out[i].Rule, recordingID, all) {
			if s.ID != sourceID {
				ids = append(ids, s.ID)
			}
		}
		if out[i].Listens, err = db.ListensWithText(ctx, ids); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// affected lists the text a new rule would move to recordingID: text it
// applies to that isn't linked by hand and isn't there already. A clean
// rule is counted when the cleaned title is one of the song's names, which
// is what makes it land there.
func affected(r store.Rule, recordingID int64, all []store.SourceInfo) []store.SourceInfo {
	var out []store.SourceInfo
	for _, s := range all {
		if s.LinkedByOwner || (s.RecordingID.Valid && s.RecordingID.Int64 == recordingID) {
			continue
		}
		t := Text{s.Artist, s.Title, s.Album}
		switch r.Kind {
		case "link":
			if _, ok := LinkRule(t, []store.Rule{r}); !ok {
				continue
			}
		case "clean":
			if c := Clean(t, []store.Rule{r}); c == t {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// SaveOffer saves an offered rule as one edit and reparses the text it
// applies to in the background. It returns the edit. With futureOnly, the
// listens already received with that text are kept where they are, and only
// later ones are linked by the rule.
func SaveOffer(ctx context.Context, db *store.DB, userID int64, o Offer, summary string, futureOnly bool) (int64, error) {
	all, err := db.SourcesFor(ctx, userID)
	if err != nil {
		return 0, err
	}
	var editID int64
	err = db.Write(ctx, func(tx *sql.Tx) error {
		if o.Target != 0 {
			var merged sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT merged_into FROM recordings WHERE id = ? AND user_id = ?`, o.Target, userID).Scan(&merged)
			if errors.Is(err, sql.ErrNoRows) || merged.Valid {
				return store.ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		// Rules made from a link by hand go before the defaults.
		o.Rule.Priority = 10
		var keep []int64
		if futureOnly {
			for _, s := range affected(o.Rule, o.Target, all) {
				keep = append(keep, s.ID)
			}
		}
		_, id, err := store.AddRuleTx(ctx, tx, userID, o.Rule, summary, keep...)
		if err != nil {
			return err
		}
		editID = id
		for _, s := range affected(o.Rule, o.Target, all) {
			if err := store.EnqueueTx(ctx, tx, "reparse", fmt.Sprint("source:", s.ID), "", 0); err != nil {
				return err
			}
		}
		return nil
	})
	return editID, err
}

// Undo undoes an edit. When the edit changed a rule, all text not linked
// by hand is read again in the background, so undoing a rule also undoes
// what it did.
func Undo(ctx context.Context, db *store.DB, userID, editID int64) (int64, error) {
	var undoID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		ruleID, err := store.RuleEditTx(ctx, tx, editID)
		if err != nil {
			return err
		}
		if undoID, err = store.UndoEditTx(ctx, tx, userID, editID); err != nil {
			return err
		}
		if ruleID == 0 {
			return nil
		}
		return reparseAllTx(ctx, tx, userID)
	})
	return undoID, err
}

// UndoTask undoes every change of an agent's task at once, newest first,
// or none of them when a later change outside it is in the way. Undoing a
// rule reads the text again, as Undo does. It returns how many changes it
// undid.
func UndoTask(ctx context.Context, db *store.DB, userID, taskID int64) (int, error) {
	var n int
	err := db.Write(ctx, func(tx *sql.Tx) error {
		edits, err := store.TaskUndoEdits(ctx, tx, userID, taskID)
		if err != nil {
			return err
		}
		rules := false
		for _, id := range edits {
			ruleID, err := store.RuleEditTx(ctx, tx, id)
			if err != nil {
				return err
			}
			rules = rules || ruleID != 0
		}
		if n, err = store.UndoTaskTx(ctx, tx, userID, taskID, edits); err != nil {
			return err
		}
		if !rules {
			return nil
		}
		return reparseAllTx(ctx, tx, userID)
	})
	return n, err
}

// NewSong splits received text off into a new song of its own, as one
// edit. Linked text keeps its song's name and artists. Unlinked text is
// read with the owner's rules, like auto-linking would. With listenID set,
// the fix starts from that listen and sc says what it applies to.
func NewSong(ctx context.Context, db *store.DB, userID, sourceID, listenID int64, sc store.LinkScope) (int64, store.LinkResult, error) {
	var rec int64
	var res store.LinkResult
	err := db.Write(ctx, func(tx *sql.Tx) error {
		src, err := store.SourceTx(ctx, tx, sourceID)
		if err != nil || src.UserID != userID {
			return store.ErrNotFound
		}
		var title string
		var credits []store.Credit
		if src.RecordingID.Valid {
			if title, credits, err = store.RecordingSongTx(ctx, tx, src.RecordingID.Int64); err != nil {
				return err
			}
		} else {
			rules, err := store.RulesTx(ctx, tx, userID)
			if err != nil {
				return err
			}
			t := Clean(Text{src.Artist, src.Title, src.Album}, rules)
			p := Parse(t.Artist, t.Title, rules)
			if p.Title == "" || len(p.Credits) == 0 {
				return ErrNothingToGoOn
			}
			title = p.Title
			for i, c := range p.Credits {
				id, err := artistFor(ctx, tx, userID, c)
				if err != nil {
					return err
				}
				role := c.Role
				if i == 0 {
					role = "main"
				}
				credits = append(credits, store.Credit{ArtistID: id, Role: role})
			}
		}
		rec, res, err = store.NewSongTx(ctx, tx, userID, sourceID, listenID, sc, title, credits)
		return err
	})
	return rec, res, err
}

// ErrNothingToGoOn means received text has no artist or title to make a
// song from.
var ErrNothingToGoOn = errors.New("no artist or title to go on")

// ErrAmbiguous means typed text could be more than one song.
var ErrAmbiguous = errors.New("that could be more than one song")

// placeTyped finds or creates the recording and album that text reads as,
// the way a scrobble with that text would be linked.
func placeTyped(ctx context.Context, tx *sql.Tx, src store.Source, t Text) (rec, rel int64, err error) {
	rules, err := store.RulesTx(ctx, tx, src.UserID)
	if err != nil {
		return 0, 0, err
	}
	var ruleID int64
	rec, rel, out, err := place(ctx, tx, src, Clean(t, rules), rules, &ruleID)
	switch {
	case err != nil:
		return 0, 0, err
	case out == Ambiguous:
		return 0, 0, ErrAmbiguous
	case out != Linked:
		return 0, 0, ErrNothingToGoOn
	}
	if err := store.QueueSuggestTx(ctx, tx, src.UserID); err != nil {
		return 0, 0, err
	}
	return rec, rel, store.RebuildRecordingArtistsTx(ctx, tx, rec)
}

// LinkAs links listens by hand to what the typed artist, title and album
// read as, creating the song, version and album when they aren't there yet.
// The received text itself is kept. The fix starts from one listen, and sc
// says what it applies to.
func LinkAs(ctx context.Context, db *store.DB, userID, listenID int64, t Text, sc store.LinkScope) (store.LinkResult, error) {
	var res store.LinkResult
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM listens WHERE id = ? AND user_id = ?`, listenID, userID).Scan(&sourceID); err != nil {
			return store.ErrNotFound
		}
		src, err := store.SourceTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		rec, rel, err := placeTyped(ctx, tx, src, t)
		if err != nil {
			return err
		}
		res, err = store.LinkScopedTx(ctx, tx, userID, listenID, store.LinkTarget{RecordingID: rec, Album: store.AlbumChoice{ID: rel}}, sc)
		return err
	})
	return res, err
}

// Reread reads received texts again the way a new scrobble would be read,
// as one edit, and hands them back to automatic reading: later changes to
// readings and rules move them again. Text that can't be placed stays
// where it is. It returns 0 when nothing could be placed.
func Reread(ctx context.Context, db *store.DB, userID int64, sourceIDs []int64) (int64, error) {
	var editID int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var links []store.SourceLink
		for _, id := range sourceIDs {
			src, err := store.SourceTx(ctx, tx, id)
			if err != nil || src.UserID != userID {
				return store.ErrNotFound
			}
			rec, rel, err := placeTyped(ctx, tx, src, Text{src.Artist, src.Title, src.Album})
			if errors.Is(err, ErrAmbiguous) || errors.Is(err, ErrNothingToGoOn) {
				continue
			}
			if err != nil {
				return err
			}
			links = append(links, store.SourceLink{SourceID: id, RecordingID: rec, ReleaseID: rel})
		}
		if len(links) == 0 {
			return nil
		}
		what := "1 received text"
		if len(links) > 1 {
			what = fmt.Sprintf("%d received texts", len(links))
		}
		res, err := store.LinkSourcesToTx(ctx, tx, userID, links, false, "Read "+what+" again automatically")
		editID = res.EditID
		return err
	})
	return editID, err
}
