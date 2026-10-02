package artwork

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"chokominto/internal/fetch"
	"chokominto/internal/names"
	"chokominto/internal/store"
)

// Finding pictures in the background, never while a page loads. Albums
// try Cover Art Archive (when the album has an MBID), then iTunes (Japan
// first), then Deezer. Artists try Deezer. A result is used on its own
// only when its title and an artist both match. Otherwise it's kept as a
// candidate to choose on the item's page. See docs/architecture.md,
// "Artwork pipeline".

// Hosts are the sites lookups and downloads may reach.
var Hosts = []string{
	"coverartarchive.org", "*.archive.org",
	"itunes.apple.com", "*.mzstatic.com",
	"api.deezer.com", "*.dzcdn.net",
}

// Sources are where pictures are looked up. Tests point them at a local
// server.
type Sources struct {
	CoverArtArchive string // https://coverartarchive.org
	ITunes          string // https://itunes.apple.com
	Deezer          string // https://api.deezer.com
}

var Live = Sources{"https://coverartarchive.org", "https://itunes.apple.com", "https://api.deezer.com"}

// PaceLive slows requests to the services that ask for it. iTunes answers
// "too many requests" above about 20 searches a minute.
func PaceLive(c *fetch.Client) {
	c.Pace("itunes.apple.com", 3*time.Second)
}

// Finder looks pictures up and stores what it finds.
type Finder struct {
	DB      *store.DB
	Store   Store
	Fetch   *fetch.Client
	Sources Sources
	Now     func() time.Time
}

// Found is one search result.
type Found struct {
	Origin string
	URL    string // the full-size picture
	Thumb  string // a small one, for choosing
	Title  string
	Artist string
}

// Retry waits: network errors after 1 h, 6 h, 1 d, then weekly. Nothing
// found, monthly, since services add releases over time.
func retryAfter(state string, tries int) time.Duration {
	if state == "notfound" || state == "candidates" {
		return 30 * 24 * time.Hour
	}
	switch tries {
	case 1:
		return time.Hour
	case 2:
		return 6 * time.Hour
	case 3:
		return 24 * time.Hour
	}
	return 7 * 24 * time.Hour
}

// Queue asks for a picture to be looked for, now. The key carries the try
// count, so a retry can be queued while the current job finishes.
func QueueTx(ctx context.Context, tx *sql.Tx, kind string, id int64) error {
	return store.EnqueueTx(ctx, tx, "artwork", fmt.Sprintf("%s:%d:0", kind, id), "", 0)
}

// Job handles an "artwork" job with key "<artist|release>:<id>:<try>".
func (f *Finder) Job(ctx context.Context, j store.Job) error {
	parts := strings.Split(j.Key, ":")
	if len(parts) != 3 || (parts[0] != "artist" && parts[0] != "release") {
		return fmt.Errorf("artwork job with key %q", j.Key)
	}
	kind := parts[0]
	id, err1 := strconv.ParseInt(parts[1], 10, 64)
	tries, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("artwork job with key %q", j.Key)
	}
	item, err := f.DB.ArtworkItem(ctx, kind, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil // merged or gone
	}
	if err != nil {
		return err
	}
	// Found pictures are never looked for again, and the owner's choice is
	// never replaced.
	if item.HasArtwork || !item.FindOnline {
		return nil
	}
	for k := range NameKeysOf(item.Names) {
		item.NameKeys[k] = true
	}
	state, lookErr := f.look(ctx, kind, item)
	tries++
	return f.DB.Write(ctx, func(tx *sql.Tx) error {
		msg := ""
		if lookErr != nil {
			msg = lookErr.Error()
		}
		if err := store.SetLookupTx(ctx, tx, kind, id, state, tries, msg); err != nil {
			return err
		}
		if state == "found" {
			return nil
		}
		at := f.now().Add(retryAfter(state, tries)).Unix()
		return store.EnqueueTx(ctx, tx, "artwork", fmt.Sprintf("%s:%d:%d", kind, id, tries), "", at)
	})
}

func (f *Finder) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// look tries each source in turn. It returns found, candidates, notfound
// or error (when a source couldn't be reached and nothing was found).
func (f *Finder) look(ctx context.Context, kind string, item store.ArtworkItem) (string, error) {
	var searches []func() ([]Found, error)
	if kind == "release" {
		if item.MBID != "" {
			searches = append(searches, func() ([]Found, error) { return f.coverArtArchive(item) })
		}
		searches = append(searches,
			func() ([]Found, error) { return f.iTunes(ctx, item, "JP") },
			func() ([]Found, error) { return f.iTunes(ctx, item, "US") },
			func() ([]Found, error) { return f.deezerAlbum(ctx, item) })
	} else {
		searches = append(searches, func() ([]Found, error) { return f.deezerArtist(ctx, item) })
	}
	var candidates []Found
	var lastErr error
	for _, search := range searches {
		results, err := search()
		if err != nil {
			lastErr = err
			continue
		}
		for _, r := range results {
			if !matches(kind, item, r) {
				candidates = append(candidates, r)
				continue
			}
			err := f.use(ctx, kind, item, r)
			if err == nil {
				return "found", nil
			}
			lastErr = err // a broken picture: try the next result
		}
	}
	if len(candidates) > 0 {
		if err := f.DB.AddCandidates(ctx, kind, item.ID, toCandidates(candidates[:min(len(candidates), 8)])); err != nil {
			return "error", err
		}
	}
	switch {
	case len(candidates) > 0:
		return "candidates", lastErr
	case lastErr != nil:
		return "error", lastErr
	}
	return "notfound", nil
}

func toCandidates(fs []Found) []store.Candidate {
	out := make([]store.Candidate, len(fs))
	for i, f := range fs {
		out[i] = store.Candidate{Origin: f.Origin, URL: f.URL, Thumb: f.Thumb, Title: f.Title, Artist: f.Artist}
	}
	return out
}

// matches reports whether a result is clearly this item: one of its
// names and, for albums, one of its artists, comparing match keys. Shop
// suffixes like " - Single" or "(Original Soundtrack)" don't count, and an
// artist list on the shop's side matches when one of its names does.
func matches(kind string, item store.ArtworkItem, r Found) bool {
	if r.Origin == "coverartarchive" {
		return true // looked up by the album's own MBID
	}
	if !anyKey(titleKeys(r.Title), item.NameKeys) {
		return false
	}
	if kind == "artist" {
		return true
	}
	for _, part := range artistParts.Split(r.Artist, -1) {
		if item.ArtistKeys[names.MatchKey(part)] {
			return true
		}
	}
	return item.ArtistKeys[names.MatchKey(r.Artist)]
}

func anyKey(keys []string, want map[string]bool) bool {
	for _, k := range keys {
		if k != "" && want[k] {
			return true
		}
	}
	return false
}

// A shop's suffix on an album name: " - Single", " - EP", or one trailing
// bracket like "(Original Game Soundtrack)", "(アーティスト盤)".
var (
	shopSuffix  = regexp.MustCompile(`(?i)\s+-\s+(?:single|ep)$`)
	shopBracket = regexp.MustCompile(`^(.+?)\s*(?:\([^()]*\)|\[[^\[\]]*\]|（[^（）]*）|【[^【】]*】)$`)
	artistParts = regexp.MustCompile(`\s*(?:,|&|×|、|/| feat\. | x )\s*`)
)

// Soundtrack wording that differs from shop to shop.
var soundtrackWords = regexp.MustCompile(`(?i)[\s:-]*(?:(?:official|original)\s+)?(?:(?:game|motion picture)\s+)?(?:soundtrack|sound track|ost)$`)

// titleKeys are the match keys a title is compared by: as written, and
// without a shop suffix, a trailing bracket or soundtrack wording. Each is
// also kept with "wo" read as "o", since を is romanized either way.
func titleKeys(t string) []string {
	var keys []string
	add := func(s string) {
		k := names.MatchKey(s)
		keys = append(keys, k, strings.ReplaceAll(k, "wo", "o"))
	}
	add(t)
	t = shopSuffix.ReplaceAllString(strings.TrimSpace(t), "")
	add(t)
	if m := shopBracket.FindStringSubmatch(t); m != nil {
		t = m[1]
		add(t)
	}
	add(soundtrackWords.ReplaceAllString(t, ""))
	return keys
}

// NameKeysOf turns an item's names into the keys titles are matched by.
func NameKeysOf(nameList []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range nameList {
		for _, k := range titleKeys(n) {
			out[k] = true
		}
	}
	return out
}

// Download fetches a picture and stores it. Used for lookups and for a
// candidate the owner chose.
func (f *Finder) Download(ctx context.Context, origin, rawURL string) (store.Artwork, error) {
	data, err := f.Fetch.Get(ctx, rawURL, "image/*")
	if err != nil {
		return store.Artwork{}, err
	}
	st, err := f.Store.Save(data)
	if err != nil {
		return store.Artwork{}, err
	}
	return store.Artwork{SHA256: st.SHA256, Format: st.Format, Width: st.Width, Height: st.Height, Origin: origin}, nil
}

// use downloads a matching result and shows it, as an automatic edit.
func (f *Finder) use(ctx context.Context, kind string, item store.ArtworkItem, r Found) error {
	a, err := f.Download(ctx, r.Origin, r.URL)
	if err != nil {
		return err
	}
	return f.DB.Write(ctx, func(tx *sql.Tx) error {
		artID, err := store.AddArtworkTx(ctx, tx, a, r.URL)
		if err != nil {
			return err
		}
		_, err = store.SetArtworkTx(ctx, tx, item.UserID, kind, item.ID, artID, false, true)
		return err
	})
}

func (f *Finder) getJSON(ctx context.Context, u string, v any) error {
	body, err := f.Fetch.Get(ctx, u, "application/json")
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// coverArtArchive returns the album's front cover, by MBID. Nothing is
// downloaded until it's used.
func (f *Finder) coverArtArchive(item store.ArtworkItem) ([]Found, error) {
	return []Found{{Origin: "coverartarchive",
		URL:   f.Sources.CoverArtArchive + "/release/" + url.PathEscape(item.MBID) + "/front-1200",
		Thumb: f.Sources.CoverArtArchive + "/release/" + url.PathEscape(item.MBID) + "/front-250",
		Title: item.Name}}, nil
}

func (f *Finder) iTunes(ctx context.Context, item store.ArtworkItem, country string) ([]Found, error) {
	q := url.Values{"term": {strings.TrimSpace(item.Artist + " " + item.Name)}, "entity": {"album"}, "country": {country}, "limit": {"10"}}
	var res struct {
		Results []struct {
			CollectionName string `json:"collectionName"`
			ArtistName     string `json:"artistName"`
			Artwork        string `json:"artworkUrl100"`
		} `json:"results"`
	}
	if err := f.getJSON(ctx, f.Sources.ITunes+"/search?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	var out []Found
	for _, r := range res.Results {
		if r.Artwork == "" {
			continue
		}
		// The same picture comes in any size by changing the name.
		full := strings.Replace(r.Artwork, "100x100bb", "1200x1200bb", 1)
		out = append(out, Found{Origin: "itunes", URL: full, Thumb: r.Artwork, Title: r.CollectionName, Artist: r.ArtistName})
	}
	return out, nil
}

func (f *Finder) deezerAlbum(ctx context.Context, item store.ArtworkItem) ([]Found, error) {
	q := url.Values{"q": {strings.TrimSpace(item.Artist + " " + item.Name)}, "limit": {"10"}}
	var res struct {
		Data []struct {
			Title  string `json:"title"`
			CoverX string `json:"cover_xl"`
			CoverM string `json:"cover_medium"`
			Artist struct {
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"data"`
	}
	if err := f.getJSON(ctx, f.Sources.Deezer+"/search/album?"+q.Encode(), &res); err != nil {
		return nil, err
	}
	var out []Found
	for _, r := range res.Data {
		if r.CoverX != "" {
			out = append(out, Found{Origin: "deezer", URL: r.CoverX, Thumb: r.CoverM, Title: r.Title, Artist: r.Artist.Name})
		}
	}
	return out, nil
}

func (f *Finder) deezerArtist(ctx context.Context, item store.ArtworkItem) ([]Found, error) {
	var res struct {
		Data []struct {
			Name     string `json:"name"`
			PictureX string `json:"picture_xl"`
			PictureM string `json:"picture_medium"`
		} `json:"data"`
	}
	if err := f.getJSON(ctx, f.Sources.Deezer+"/search/artist?"+url.Values{"q": {item.Name}, "limit": {"10"}}.Encode(), &res); err != nil {
		return nil, err
	}
	var out []Found
	for _, r := range res.Data {
		// Artists without a photo get a blank placeholder, with no picture
		// id in its address.
		if r.PictureX == "" || strings.Contains(r.PictureX, "/artist//") {
			continue
		}
		out = append(out, Found{Origin: "deezer", URL: r.PictureX, Thumb: r.PictureM, Title: r.Name, Artist: r.Name})
	}
	return out, nil
}
