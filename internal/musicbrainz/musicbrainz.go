// Package musicbrainz reads names from MusicBrainz, on demand only. See
// docs/architecture.md, "MusicBrainz names".
package musicbrainz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"chokominto/internal/fetch"
)

const Host = "musicbrainz.org"

type Client struct {
	Fetch *fetch.Client
	Base  string // https://musicbrainz.org/ws/2, or a test server
}

func New(userAgent string) *Client {
	return &Client{Fetch: fetch.New(userAgent, Host), Base: "https://" + Host + "/ws/2"}
}

// Kinds of entity, by the name Chokominto uses: artists, songs (works in
// MusicBrainz) and albums (releases).
var entityPaths = map[string]string{"artist": "artist", "song": "work", "release": "release"}

// Match is one search result.
type Match struct {
	MBID           string
	Name           string
	Disambiguation string
	Details        string // type, country, date or artists, to tell matches apart
}

// Name is a name with its language as MusicBrainz marks it: en, romaji,
// original, or "" when it doesn't say.
type Name struct {
	Name string
	Lang string
}

// Relation is an artist MusicBrainz links to this one.
type Relation struct {
	MBID string
	Name string
}

// Entity is what a lookup found.
type Entity struct {
	MBID     string
	Name     string
	Names    []Name
	Members  []Relation // artists: members of this group
	PersonOf []Relation // artists: the person behind this persona
	Released string     // albums: release date
}

var mbidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidMBID reports whether s looks like an MBID.
func ValidMBID(s string) bool { return mbidPattern.MatchString(s) }

func (c *Client) get(ctx context.Context, path string, q url.Values, v any) error {
	q.Set("fmt", "json")
	body, err := c.Fetch.Get(ctx, c.Base+"/"+path+"?"+q.Encode(), "application/json")
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// quote makes text a Lucene phrase.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

type alias struct {
	Name   string  `json:"name"`
	Locale *string `json:"locale"`
}

type artistCredit struct {
	Name string `json:"name"`
}

// Search finds up to 10 matches for a name.
func (c *Client) Search(ctx context.Context, kind, name string) ([]Match, error) {
	path, ok := entityPaths[kind]
	if !ok {
		return nil, fmt.Errorf("no %s in MusicBrainz", kind)
	}
	field := map[string]string{"artist": "artist", "song": "work", "release": "release"}[kind]
	var res struct {
		Artists []struct {
			ID, Name, Disambiguation, Type, Country string
		} `json:"artists"`
		Works []struct {
			ID             string   `json:"id"`
			Title          string   `json:"title"`
			Disambiguation string   `json:"disambiguation"`
			Type           string   `json:"type"`
			Languages      []string `json:"languages"`
		} `json:"works"`
		Releases []struct {
			ID             string         `json:"id"`
			Title          string         `json:"title"`
			Disambiguation string         `json:"disambiguation"`
			Date           string         `json:"date"`
			Country        string         `json:"country"`
			Credit         []artistCredit `json:"artist-credit"`
		} `json:"releases"`
	}
	if err := c.get(ctx, path, url.Values{"query": {field + ":" + quote(name)}, "limit": {"10"}}, &res); err != nil {
		return nil, err
	}
	var out []Match
	for _, a := range res.Artists {
		out = append(out, Match{a.ID, a.Name, a.Disambiguation, join(a.Type, a.Country)})
	}
	for _, w := range res.Works {
		out = append(out, Match{w.ID, w.Title, w.Disambiguation, join(w.Type, strings.Join(w.Languages, ", "))})
	}
	for _, r := range res.Releases {
		var by []string
		for _, a := range r.Credit {
			by = append(by, a.Name)
		}
		out = append(out, Match{r.ID, r.Title, r.Disambiguation, join(strings.Join(by, ", "), r.Date, r.Country)})
	}
	return out, nil
}

func join(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ", ")
}

// Lang turns a MusicBrainz locale into a kind of name: ja is original
// script, ja-Latn romaji, en English. Other locales aren't used.
func Lang(locale string) string {
	l := strings.ToLower(strings.ReplaceAll(locale, "_", "-"))
	switch {
	case l == "ja-latn":
		return "romaji"
	case l == "ja" || strings.HasPrefix(l, "ja-"):
		return "original"
	case l == "en" || strings.HasPrefix(l, "en-"):
		return "en"
	}
	return "skip"
}

func names(main string, aliases []alias) []Name {
	out := []Name{{Name: main}}
	seen := map[string]bool{main: true}
	for _, a := range aliases {
		if a.Locale == nil || seen[a.Name] {
			continue
		}
		if l := Lang(*a.Locale); l != "skip" {
			seen[a.Name] = true
			out = append(out, Name{a.Name, l})
		}
	}
	return out
}

// Lookup reads an entity's names (and for artists, its members and the
// person behind a persona) by MBID.
func (c *Client) Lookup(ctx context.Context, kind, mbid string) (Entity, error) {
	e := Entity{MBID: mbid}
	if !ValidMBID(mbid) {
		return e, fmt.Errorf("%q is not an MBID", mbid)
	}
	switch kind {
	case "artist":
		var a struct {
			Name      string  `json:"name"`
			Aliases   []alias `json:"aliases"`
			Relations []struct {
				Type      string `json:"type"`
				Direction string `json:"direction"`
				Artist    struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"relations"`
		}
		if err := c.get(ctx, "artist/"+mbid, url.Values{"inc": {"aliases+artist-rels"}}, &a); err != nil {
			return e, err
		}
		e.Name, e.Names = a.Name, names(a.Name, a.Aliases)
		for _, r := range a.Relations {
			rel := Relation{r.Artist.ID, r.Artist.Name}
			switch {
			// On a group, its members are linked backward.
			case r.Type == "member of band" && r.Direction == "backward":
				e.Members = append(e.Members, rel)
			// A persona "is person": the real person, linked backward.
			case r.Type == "is person" && r.Direction == "backward":
				e.PersonOf = append(e.PersonOf, rel)
			}
		}
	case "song":
		var w struct {
			Title   string  `json:"title"`
			Aliases []alias `json:"aliases"`
		}
		if err := c.get(ctx, "work/"+mbid, url.Values{"inc": {"aliases"}}, &w); err != nil {
			return e, err
		}
		e.Name, e.Names = w.Title, names(w.Title, w.Aliases)
	case "release":
		var r struct {
			Title string `json:"title"`
			Date  string `json:"date"`
			Group struct {
				ID string `json:"id"`
			} `json:"release-group"`
		}
		if err := c.get(ctx, "release/"+mbid, url.Values{"inc": {"release-groups"}}, &r); err != nil {
			return e, err
		}
		e.Name, e.Released = r.Title, r.Date
		e.Names = []Name{{Name: r.Title}}
		// Releases take their other names from their release group.
		if ValidMBID(r.Group.ID) {
			var g struct {
				Title   string  `json:"title"`
				Aliases []alias `json:"aliases"`
			}
			if err := c.get(ctx, "release-group/"+r.Group.ID, url.Values{"inc": {"aliases"}}, &g); err != nil {
				return e, err
			}
			for _, n := range names(g.Title, g.Aliases) {
				dup := false
				for _, have := range e.Names {
					dup = dup || have.Name == n.Name
				}
				if !dup {
					e.Names = append(e.Names, n)
				}
			}
		}
	default:
		return e, fmt.Errorf("no %s in MusicBrainz", kind)
	}
	return e, nil
}
