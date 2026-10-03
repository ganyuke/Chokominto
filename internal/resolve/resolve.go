package resolve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"chokominto/internal/artwork"
	"chokominto/internal/names"
	"chokominto/internal/store"
)

// Resolver links sources (distinct received text) to recordings and
// releases, creating artists, songs and releases the first time they're
// seen. It never guesses between several existing matches: those stay
// unlinked for the owner to pick.
type Resolver struct {
	DB *store.DB
}

// Job handles a "resolve" job with key "source:<id>".
func (r *Resolver) Job(ctx context.Context, j store.Job) error {
	idText, ok := strings.CutPrefix(j.Key, "source:")
	if !ok {
		return fmt.Errorf("resolve job with key %q", j.Key)
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return err
	}
	_, err = r.Source(ctx, id)
	return err
}

// Outcome says what happened to a source.
type Outcome int

const (
	AlreadyLinked Outcome = iota
	Linked
	Incomplete // no artist or title to go on
	Ambiguous  // several existing recordings fit
)

func (r *Resolver) Source(ctx context.Context, sourceID int64) (Outcome, error) {
	var out Outcome
	err := r.DB.Write(ctx, func(tx *sql.Tx) error {
		src, err := store.SourceTx(ctx, tx, sourceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // removed since it was queued
		}
		if err != nil {
			return err
		}
		out, err = resolveTx(ctx, tx, src)
		return err
	})
	return out, err
}

func resolveTx(ctx context.Context, tx *sql.Tx, src store.Source) (Outcome, error) {
	// A link, automatic or the owner's, is never replaced here. Reparsing
	// automatic links is a separate, explicit action.
	if src.RecordingID.Valid {
		return AlreadyLinked, nil
	}
	return linkTx(ctx, tx, src)
}

// linkTx works out where text goes and links it there, unless it's there
// already. Text that's linked stays where it is when it can't be placed,
// except text that turns out to have no artist or title at all ("Unknown"
// read as nothing sent), which is unlinked and goes to Review.
func linkTx(ctx context.Context, tx *sql.Tx, src store.Source) (Outcome, error) {
	rules, err := store.RulesTx(ctx, tx, src.UserID)
	if err != nil {
		return 0, err
	}
	raw := Text{src.Artist, src.Title, src.Album}
	t := Clean(raw, rules)
	var ruleID int64
	if t != raw {
		ruleID = cleanRuleFor(raw, rules)
	}
	rec, rel, out, err := place(ctx, tx, src, t, rules, &ruleID)
	if err == nil && out == Incomplete && src.RecordingID.Valid {
		return out, store.UnlinkSourceTx(ctx, tx, src)
	}
	if err != nil || out != Linked {
		return out, err
	}
	if src.RecordingID.Valid && src.RecordingID.Int64 == rec && src.ReleaseID.Int64 == rel {
		return AlreadyLinked, nil
	}
	if err := store.LinkSourceByRuleTx(ctx, tx, src, rec, rel, ruleID); err != nil {
		return 0, err
	}
	// New names may pair up with old ones.
	if err := store.QueueSuggestTx(ctx, tx, src.UserID); err != nil {
		return 0, err
	}
	return Linked, store.RebuildRecordingArtistsTx(ctx, tx, rec)
}

// namesArtist reports whether name is one of the names of a credited
// artist that exists already.
func namesArtist(ctx context.Context, tx *sql.Tx, userID int64, name string, credits []Credit) (bool, error) {
	id, err := store.FindArtistTx(ctx, tx, userID, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, c := range credits {
		cid, err := store.FindArtistTx(ctx, tx, userID, c.Name)
		if err == nil && cid == id {
			return true, nil
		}
	}
	return false, nil
}

func without(rules []store.Rule, kind string) []store.Rule {
	var out []store.Rule
	for _, r := range rules {
		if r.Kind != kind {
			out = append(out, r)
		}
	}
	return out
}

// cleanRuleFor returns the first clean rule that changes the text.
func cleanRuleFor(t Text, rules []store.Rule) int64 {
	for _, r := range rules {
		if r.Kind == "clean" {
			if Clean(t, []store.Rule{r}) != t {
				return r.ID
			}
		}
	}
	return 0
}

// place finds (or creates) the recording and release for cleaned text.
func place(ctx context.Context, tx *sql.Tx, src store.Source, t Text, rules []store.Rule, ruleID *int64) (rec, rel int64, out Outcome, err error) {
	album := albumText(t.Album)
	if r, ok := LinkRule(t, rules); ok {
		*ruleID = r.ID
		rec, rel = r.RecordingID, r.ReleaseID
		if rel == 0 && album != "" {
			rel, err = albumFor(ctx, tx, src, album, rules, rec, nil)
		}
		return rec, rel, Linked, err
	}

	t.Artist = artistText(t.Artist)
	p := Parse(t.Artist, t.Title, rules)
	if strings.TrimSpace(t.Artist) == "" || p.Title == "" || len(p.Credits) == 0 {
		return 0, 0, Incomplete, nil
	}
	// A cover title naming someone other than the artist is read as sent.
	if p.CoverBy != "" {
		ok, err := namesArtist(ctx, tx, src.UserID, p.CoverBy, p.Credits)
		if err != nil {
			return 0, 0, 0, err
		}
		if !ok {
			p = Parse(t.Artist, t.Title, without(rules, "covers"))
		}
	}
	var mains, credits []store.Credit
	for _, c := range p.Credits {
		id, err := artistFor(ctx, tx, src.UserID, c)
		if err != nil {
			return 0, 0, 0, err
		}
		credits = append(credits, store.Credit{ArtistID: id, Role: c.Role})
		if c.Role == "main" {
			mains = append(mains, store.Credit{ArtistID: id, Role: c.Role})
		}
	}
	if len(mains) == 0 {
		mains = credits[:1]
		credits[0].Role = "main"
	}
	mainIDs := make([]int64, len(mains))
	for i, m := range mains {
		mainIDs[i] = m.ArtistID
	}

	rec, err = pickVersion(ctx, tx, src.UserID, p, mainIDs, album, credits)
	if err != nil {
		return 0, 0, 0, err
	}
	if rec == -1 {
		return 0, 0, Ambiguous, nil
	}
	if album != "" {
		if rel, err = albumFor(ctx, tx, src, album, rules, rec, mainIDs); err != nil {
			return 0, 0, 0, err
		}
	}
	return rec, rel, Linked, nil
}

// albumFor finds or creates the release for received album text and puts
// the recording on it. The album artist is the one the client sent, else
// the track's main artists (from the recording when a rule placed it).
func albumFor(ctx context.Context, tx *sql.Tx, src store.Source, album string, rules []store.Rule, rec int64, mainIDs []int64) (int64, error) {
	// "Various Artists" as the album artist says nothing about who made the
	// album, so it's read like no album artist at all.
	sentArtist := artistText(src.AlbumArtist)
	if variousArtists.MatchString(strings.TrimSpace(sentArtist)) {
		sentArtist = ""
	}
	albumArtists, err := albumArtistsFor(ctx, tx, src.UserID, sentArtist, rules)
	if err != nil {
		return 0, err
	}
	if len(albumArtists) == 0 {
		albumArtists = mainIDs
	}
	if len(albumArtists) == 0 {
		if albumArtists, err = store.MainArtistsTx(ctx, tx, rec); err != nil {
			return 0, err
		}
	}
	rel, err := store.FindReleaseTx(ctx, tx, src.UserID, album, albumArtists)
	// A soundtrack or compilation: tracks by different artists, with no
	// album artist of their own, are one album credited to all of them.
	if errors.Is(err, store.ErrNotFound) && sentArtist == "" && hasRule(rules, "compilation") && compilationName(album) {
		if rel, err = store.FindReleaseByNameTx(ctx, tx, src.UserID, album); err == nil {
			err = store.AddReleaseCreditsTx(ctx, tx, rel, albumArtists)
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		if rel, err = store.CreateReleaseTx(ctx, tx, src.UserID, album, albumArtists); err == nil {
			err = artwork.QueueTx(ctx, tx, "release", rel)
		}
	}
	if err != nil {
		return 0, err
	}
	return rel, store.AddReleaseTrackTx(ctx, tx, rel, rec)
}

var variousArtists = regexp.MustCompile(`(?i)^(?:various(?: artists)?|v\.?\s?a\.?|ヴァリアス・アーティスト|オムニバス)$`)

// compilationName reports whether albums of different artists with this
// name are the same album. See names.CompilationName.
func compilationName(album string) bool { return names.CompilationName(album) }

func hasRule(rules []store.Rule, kind string) bool {
	for _, r := range rules {
		if r.Kind == kind {
			return true
		}
	}
	return false
}

// albumArtistsFor finds or creates the artists an album is credited to,
// read from the album artist the scrobbler sent with the same rules as
// track artists. Featured artists are left off the album. None means the
// album goes with the track's main artists.
func albumArtistsFor(ctx context.Context, tx *sql.Tx, userID int64, text string, rules []store.Rule) ([]int64, error) {
	var ids []int64
	for _, c := range Parse(text, "", rules).Credits {
		if c.Role != "main" {
			continue
		}
		id, err := artistFor(ctx, tx, userID, c)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// viewCount matches what YouTube and YouTube Music show under a video,
// which Web Scrobbler sometimes sends as the album: "33M plays",
// "1,234 views", "4 million views", "2.1万回再生".
var viewCount = regexp.MustCompile(`(?i)^\d[\d.,]*\s*(?:[KMB]|thousand|million|billion|万|億)?\s*(?:plays|views|回再生|回視聴)$`)

// placeholder matches what players send when they don't know the artist or
// album: "Unknown", "Unknown Artist", Android's "<unknown>". It's read like
// nothing was sent, so it never becomes an artist or an album. The update
// in migration 0009 matches the same texts.
var placeholder = regexp.MustCompile(`(?i)^(?:unknown(?: artist| album)?|<unknown>|\[unknown\]|不明なアーティスト|不明なアルバム)$`)

// albumText is the album to resolve with: the received text, or "" when it
// isn't really an album. The source keeps the text as received.
func albumText(s string) string {
	s = strings.TrimSpace(s)
	if viewCount.MatchString(s) || placeholder.MatchString(s) {
		return ""
	}
	return s
}

// artistText is the artist to resolve with, "" for a placeholder.
func artistText(s string) string {
	if placeholder.MatchString(strings.TrimSpace(s)) {
		return ""
	}
	return s
}

// pickVersion returns the recording to link to, creating a song or a new
// version of one when needed, or -1 when several songs fit and the owner
// should choose.
//
// The same artist and title is the same song, whatever the album: a single
// and the album it's on, OST editions with different names and
// compilations are far more common than one artist reusing a title (two
// games' "Main Theme"), which the owner splits by hand. Once there are
// several songs, the one on this album wins, and otherwise the owner
// picks. Within the song, the version decides: "Instrumental" goes to the
// instrumental recording, made the first time it's heard.
func pickVersion(ctx context.Context, tx *sql.Tx, userID int64, p Parsed, mains []int64, album string, credits []store.Credit) (int64, error) {
	candidates, err := store.FindRecordingsTx(ctx, tx, userID, p.Title, mains)
	if err != nil {
		return 0, err
	}
	// The other name only counts when the title itself finds nothing. When
	// both find a song, they're different songs that the text says are the
	// same: the title's song wins, and the other name added to it makes
	// Review suggest merging them. Nothing is merged automatically.
	if len(candidates) == 0 && p.AltTitle != "" {
		if candidates, err = store.FindRecordingsTx(ctx, tx, userID, p.AltTitle, mains); err != nil {
			return 0, err
		}
	}
	if len(candidates) == 0 {
		rec, err := store.CreateSongVersionTx(ctx, tx, userID, p.Title, p.Version, p.Solo, credits)
		if err != nil {
			return 0, err
		}
		return rec, addAltTitle(ctx, tx, rec, p.AltTitle)
	}
	versions, err := store.RecordingVersionsTx(ctx, tx, candidates)
	if err != nil {
		return 0, err
	}
	songs := songsOf(versions)
	if len(songs) > 1 && album != "" {
		onAlbum, err := store.OnReleaseTx(ctx, tx, candidates, album)
		if err != nil {
			return 0, err
		}
		v, err := store.RecordingVersionsTx(ctx, tx, onAlbum)
		if err != nil {
			return 0, err
		}
		songs = songsOf(v)
	}
	if len(songs) != 1 {
		return -1, nil
	}
	song := songs[0]
	recs, err := store.SongVersionsTx(ctx, tx, song, mains)
	if err != nil {
		return 0, err
	}
	// The recording of this version with the same artists. One featured
	// artist more or less is still the same recording ("siinamota" and
	// "siinamota feat. Kagamine Rin"). Otherwise it's another recording of
	// the song, like six characters singing one character's song.
	want := names.MatchKey(p.Version)
	sent := map[int64]bool{}
	for _, c := range credits {
		sent[c.ArtistID] = true
	}
	var rec int64
	best := -1
	for _, r := range recs {
		if names.MatchKey(r.Version) != want {
			continue
		}
		have, err := store.CreditedIDsTx(ctx, tx, r.ID)
		if err != nil {
			return 0, err
		}
		if d := setDistance(sent, have); d >= 0 && d <= 1 && (best < 0 || d < best) {
			rec, best = r.ID, d
		}
	}
	if rec == 0 {
		if rec, err = store.CreateVersionTx(ctx, tx, userID, song, p.Version, p.Solo, credits); err != nil {
			return 0, err
		}
	}
	return rec, addAltTitle(ctx, tx, rec, p.AltTitle)
}

// setDistance is how many artists one set has beyond the other, when one
// contains the other, or -1 when neither does.
func setDistance(a map[int64]bool, b []int64) int {
	inA := 0
	for _, id := range b {
		if a[id] {
			inA++
		}
	}
	switch {
	case inA == len(b): // b within a
		return len(a) - len(b)
	case inA == len(a): // a within b
		return len(b) - len(a)
	}
	return -1
}

func songsOf(vs []store.RecordingVersion) []int64 {
	var out []int64
	for _, v := range vs {
		if !slices.Contains(out, v.SongID) {
			out = append(out, v.SongID)
		}
	}
	return out
}

// addAltTitle gives the recording's song the other name it was sent with.
func addAltTitle(ctx context.Context, tx *sql.Tx, rec int64, alt string) error {
	if alt == "" {
		return nil
	}
	var song int64
	if err := tx.QueryRowContext(ctx, `SELECT song_id FROM recordings WHERE id = ?`, rec).Scan(&song); err != nil {
		return err
	}
	return store.AddAliasTx(ctx, tx, "song_aliases", song, alt)
}

// artistFor finds or creates the artist for a credit. A character credit
// also finds or creates the voice actors. A new character gets the
// Character label (if the owner still has one) and counts for its voices.
// A known character is left as the owner has it.
func artistFor(ctx context.Context, tx *sql.Tx, userID int64, c Credit) (int64, error) {
	id, err := store.FindArtistTx(ctx, tx, userID, c.Name)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return id, err
	}
	if id, err = store.CreateArtistTx(ctx, tx, userID, c.Name); err != nil {
		return 0, err
	}
	if err := artwork.QueueTx(ctx, tx, "artist", id); err != nil {
		return 0, err
	}
	// A new group named with its characters gets them as members.
	for _, m := range c.Members {
		mid, err := artistFor(ctx, tx, userID, m)
		if err != nil {
			return 0, err
		}
		if err := store.AddMemberTx(ctx, tx, id, mid); err != nil {
			return 0, err
		}
	}
	if len(c.Voices) == 0 {
		return id, nil
	}
	if label, err := store.LabelIDTx(ctx, tx, userID, "Character"); err == nil {
		if err := store.AddLabelTx(ctx, tx, label, "artist", id); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	for _, v := range c.Voices {
		voice, err := artistFor(ctx, tx, userID, Credit{Name: v})
		if err != nil {
			return 0, err
		}
		if err := store.AddCountsForTx(ctx, tx, id, voice, "voice"); err != nil {
			return 0, err
		}
	}
	return id, nil
}
