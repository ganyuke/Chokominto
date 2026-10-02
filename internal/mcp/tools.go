package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

// instructions tell the agent how Chokominto thinks about music, so it can
// use the tools well. Sent when it connects.
const instructions = `Chokominto is a self-hosted scrobble server. These tools let you help its owner tidy their music: find songs, artists and albums that got split, and link or merge them.

How it's organized:
- Sent text: exactly what a music player sent (artist, title, album). Never changed. Each one is linked to a recording, or not linked yet. All listens with the same text move together.
- Song: the piece of music, like 不可思議のカルテ. It can have several names (Japanese, romaji, English).
- Recording: one performance or version of a song, like the TV size, an instrumental, or one character's take ("桜島麻衣 Ver."). Each has its own artists and a version name ("" for the main one).
- Artist: a person, group or character. A character "also counts for" their voice actor, so listens count for both. Groups have members.
- Album: a release, with its tracks.

Good habits:
- Look before you change: search, then show_song or show_artist, and search_sent_text for the raw spellings.
- Different versions of one song are recordings of one song, not separate songs. Merge songs that are the same piece, and give each recording a version name.
- Merge recordings only when they are the same performance and version.
- Every change is one edit the owner can undo, and the result says which. Tell the owner what you changed and keep a list of edit ids.
- Ask the owner before big or unclear changes. Listens they linked by hand were their own decision.`

type param struct {
	name, typ, desc string
	required        bool
	enum            []string
}

type tool struct {
	name, description string
	readOnly          bool
	params            []param
	run               func(ctx context.Context, s *Server, args json.RawMessage) (any, error)
}

func (t tool) schema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.params {
		var prop map[string]any
		if typ, ok := strings.CutPrefix(p.typ, "array of "); ok {
			prop = map[string]any{"type": "array", "items": map[string]any{"type": typ}}
		} else {
			prop = map[string]any{"type": p.typ}
		}
		prop["description"] = p.desc
		if p.enum != nil {
			prop["enum"] = p.enum
		}
		props[p.name] = prop
		if p.required {
			required = append(required, p.name)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// decode reads a tool's arguments, refusing ones it doesn't take, so a
// misspelled argument is an error rather than silently ignored.
func decode(raw json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}

// Kinds as the agent names them, and as the store does.
var kinds = map[string]string{"song": "song", "artist": "artist", "album": "release", "recording": "recording"}

func kindOf(k string, allowed ...string) (string, error) {
	for _, a := range allowed {
		if k == a {
			return kinds[k], nil
		}
	}
	return "", fmt.Errorf("kind must be one of %s", strings.Join(allowed, ", "))
}

// What the tools return.

type item struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	OtherNames string `json:"other_names,omitempty"`
}

func items(rs []store.Ref) []item {
	out := []item{}
	for _, r := range rs {
		out = append(out, item{r.ID, r.Name, r.OtherNames})
	}
	return out
}

type recording struct {
	RecordingID int64  `json:"recording_id"`
	Song        item   `json:"song"`
	Version     string `json:"version"`
	Artists     []item `json:"artists"`
	Listens     *int   `json:"listens,omitempty"`
}

func recordingOf(r store.RecordingInfo) recording {
	return recording{RecordingID: r.RecordingID, Song: item{r.Song.ID, r.Song.Name, r.Song.OtherNames}, Version: r.Version, Artists: items(r.Artists)}
}

type sentText struct {
	SourceID     int64      `json:"source_id"`
	Artist       string     `json:"artist"`
	Title        string     `json:"title"`
	Album        string     `json:"album,omitempty"`
	Listens      int        `json:"listens"`
	LinkedTo     *recording `json:"linked_to"`
	LinkedByHand bool       `json:"linked_by_hand,omitempty"`
}

type change struct {
	EditID  int64  `json:"edit_id"`
	Summary string `json:"summary"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) changed(ctx context.Context, editID int64, note string) (change, error) {
	if editID == 0 {
		return change{Note: "Nothing to change, it was like that already."}, nil
	}
	sums, err := s.DB.EditSummaries(ctx, s.UserID, []int64{editID})
	c := change{EditID: editID, Note: note}
	if len(sums) > 0 {
		c.Summary = sums[0]
	}
	return c, err
}

// live returns an artist, song or album, or says where it was merged to.
func (s *Server) live(ctx context.Context, kind string, id int64) (store.Entity, error) {
	e, err := s.DB.Entity(ctx, s.UserID, kind, id)
	if errors.Is(err, store.ErrNotFound) {
		return e, fmt.Errorf("no %s with id %d", kind, id)
	}
	if err == nil && e.MergedInto != 0 {
		return e, fmt.Errorf("%s %d was merged into %d, use that one", kind, id, e.MergedInto)
	}
	return e, err
}

func when(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

var tools = []tool{
	{
		name:        "search",
		description: "Find songs, artists or albums by any of their names. Ignores case, width and spacing, and finds romaji for kana.",
		readOnly:    true,
		params: []param{
			{"kind", "string", "What to look for.", true, []string{"song", "artist", "album"}},
			{"query", "string", "Part of a name.", true, nil},
			{"limit", "integer", "At most this many, 25 if left out.", false, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind, Query string
				Limit       int
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "song", "artist", "album")
			if err != nil {
				return nil, err
			}
			rs, err := s.DB.SearchItems(ctx, s.UserID, kind, a.Query, limit(a.Limit))
			return items(rs), err
		},
	},
	{
		name:        "search_sent_text",
		description: "Find the text music players sent, by part of its artist, title or album. Shows how many listens each text has and which recording it's linked to. Use this to see every spelling of a song.",
		readOnly:    true,
		params: []param{
			{"query", "string", "Part of the artist, title or album as sent.", true, nil},
			{"limit", "integer", "At most this many, 25 if left out. Most listens first.", false, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Query string
				Limit int
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			srcs, err := s.DB.SearchSources(ctx, s.UserID, a.Query, limit(a.Limit))
			if err != nil {
				return nil, err
			}
			var ids []int64
			for _, src := range srcs {
				if src.RecordingID.Valid {
					ids = append(ids, src.RecordingID.Int64)
				}
			}
			infos, err := s.DB.RecordingInfos(ctx, ids)
			if err != nil {
				return nil, err
			}
			out := []sentText{}
			for _, src := range srcs {
				t := sentText{SourceID: src.ID, Artist: src.Artist, Title: src.Title, Album: src.Album, Listens: src.Listens, LinkedByHand: src.LinkedByOwner}
				if info, ok := infos[src.RecordingID.Int64]; ok && src.RecordingID.Valid {
					r := recordingOf(info)
					t.LinkedTo = &r
				}
				out = append(out, t)
			}
			return out, nil
		},
	},
	{
		name:        "show_song",
		description: "A song with all its names, its recordings (versions, artists, listens), and the albums it's on.",
		readOnly:    true,
		params:      []param{{"song_id", "integer", "The song.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SongID int64 `json:"song_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			e, err := s.live(ctx, "song", a.SongID)
			if err != nil {
				return nil, err
			}
			names, err := s.DB.Aliases(ctx, "song", a.SongID)
			if err != nil {
				return nil, err
			}
			recs, err := s.DB.SongRecordingDetails(ctx, s.UserID, a.SongID)
			if err != nil {
				return nil, err
			}
			albums, err := s.DB.SongReleases(ctx, a.SongID)
			if err != nil {
				return nil, err
			}
			stats, err := s.DB.EntityStats(ctx, s.UserID, "song", a.SongID)
			if err != nil {
				return nil, err
			}
			type rec struct {
				RecordingID int64  `json:"recording_id"`
				Version     string `json:"version"`
				Original    bool   `json:"original,omitempty"`
				OwnRow      bool   `json:"own_row,omitempty"`
				Artists     []item `json:"artists"`
				Listens     int    `json:"listens"`
			}
			out := struct {
				item
				Names      []string `json:"names"`
				Listens    int      `json:"listens"`
				FirstHeard string   `json:"first_heard,omitempty"`
				Recordings []rec    `json:"recordings"`
				Albums     []item   `json:"albums"`
			}{item: item{e.ID, e.Name, e.OtherNames}, Listens: stats.Listens, FirstHeard: when(stats.First), Albums: items(albums)}
			for _, n := range names {
				out.Names = append(out.Names, n.Name)
			}
			for _, r := range recs {
				out.Recordings = append(out.Recordings, rec{r.ID, r.Version, r.Original, r.OwnRow, items(r.Artists), r.Listens})
			}
			return out, nil
		},
	},
	{
		name:        "show_artist",
		description: "An artist with all their names, kind, who they also count for (a character's voice actor), members and groups, and their most listened recordings.",
		readOnly:    true,
		params: []param{
			{"artist_id", "integer", "The artist.", true, nil},
			{"limit", "integer", "At most this many recordings, 25 if left out.", false, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				ArtistID int64 `json:"artist_id"`
				Limit    int
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			e, err := s.live(ctx, "artist", a.ArtistID)
			if err != nil {
				return nil, err
			}
			names, err := s.DB.Aliases(ctx, "artist", a.ArtistID)
			if err != nil {
				return nil, err
			}
			kind, err := s.DB.ArtistKind(ctx, a.ArtistID)
			if err != nil {
				return nil, err
			}
			rel, err := s.DB.ArtistRelations(ctx, a.ArtistID)
			if err != nil {
				return nil, err
			}
			labels, err := s.DB.EntityLabels(ctx, "artist", a.ArtistID)
			if err != nil {
				return nil, err
			}
			top, err := s.DB.ArtistTopRecordings(ctx, s.UserID, a.ArtistID, limit(a.Limit))
			if err != nil {
				return nil, err
			}
			out := struct {
				item
				Names       []string    `json:"names"`
				Kind        string      `json:"kind,omitempty"`
				Labels      []string    `json:"labels,omitempty"`
				CountsFor   []item      `json:"counts_for,omitempty"`
				CountedFrom []item      `json:"counted_from,omitempty"`
				Members     []item      `json:"members,omitempty"`
				MemberOf    []item      `json:"member_of,omitempty"`
				Top         []recording `json:"top_recordings"`
			}{item: item{e.ID, e.Name, e.OtherNames}, Kind: kind, Labels: labels,
				CountsFor: items(rel.CountsFor), CountedFrom: items(rel.CountedFrom), Members: items(rel.Members), MemberOf: items(rel.MemberOf)}
			for _, n := range names {
				out.Names = append(out.Names, n.Name)
			}
			for _, r := range top {
				rec := recordingOf(r.RecordingInfo)
				rec.Listens = &r.Listens
				out.Top = append(out.Top, rec)
			}
			return out, nil
		},
	},
	{
		name:        "show_album",
		description: "An album with all its names, its artists and its tracks.",
		readOnly:    true,
		params:      []param{{"album_id", "integer", "The album.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				AlbumID int64 `json:"album_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			e, err := s.live(ctx, "release", a.AlbumID)
			if err != nil {
				return nil, err
			}
			names, err := s.DB.Aliases(ctx, "release", a.AlbumID)
			if err != nil {
				return nil, err
			}
			artists, err := s.DB.ReleaseArtists(ctx, a.AlbumID)
			if err != nil {
				return nil, err
			}
			tracks, err := s.DB.AlbumTracks(ctx, s.UserID, a.AlbumID)
			if err != nil {
				return nil, err
			}
			out := struct {
				item
				Names   []string    `json:"names"`
				Artists []item      `json:"artists"`
				Tracks  []recording `json:"tracks"`
			}{item: item{e.ID, e.Name, e.OtherNames}, Artists: items(artists)}
			for _, n := range names {
				out.Names = append(out.Names, n.Name)
			}
			for _, t := range tracks {
				rec := recordingOf(t.RecordingInfo)
				rec.Listens = &t.Listens
				out.Tracks = append(out.Tracks, rec)
			}
			return out, nil
		},
	},
	{
		name:        "review",
		description: "What's waiting for the owner on the Review page: suggested merges (two items that look like the same thing) and sent text that isn't linked to anything yet. Most listens first.",
		readOnly:    true,
		params:      []param{{"limit", "integer", "At most this many of each, 25 if left out.", false, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Limit int }
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			sugs, total, err := s.DB.OpenSuggestions(ctx, s.UserID, limit(a.Limit))
			if err != nil {
				return nil, err
			}
			type suggestion struct {
				Kind    string `json:"kind"`
				A       item   `json:"a"`
				B       item   `json:"b"`
				Reason  string `json:"reason"`
				Guess   bool   `json:"only_a_guess,omitempty"`
				Listens int    `json:"listens"`
			}
			out := struct {
				Suggestions     []suggestion `json:"suggested_merges"`
				MoreSuggestions int          `json:"more_suggested_merges,omitempty"`
				Unlinked        []sentText   `json:"unlinked_text"`
				MoreUnlinked    int          `json:"more_unlinked_text,omitempty"`
			}{Suggestions: []suggestion{}, Unlinked: []sentText{}}
			agentKind := map[string]string{"song": "song", "artist": "artist", "release": "album", "recording": "recording"}
			for _, sg := range sugs {
				ea, err := s.DB.Entity(ctx, s.UserID, sg.Kind, sg.A)
				if err != nil {
					return nil, err
				}
				eb, err := s.DB.Entity(ctx, s.UserID, sg.Kind, sg.B)
				if err != nil {
					return nil, err
				}
				out.Suggestions = append(out.Suggestions, suggestion{agentKind[sg.Kind], item{ea.ID, ea.Name, ea.OtherNames}, item{eb.ID, eb.Name, eb.OtherNames}, sg.Reason, sg.Weak, sg.Listens})
			}
			out.MoreSuggestions = total - len(sugs)
			unlinked, err := s.DB.UnlinkedSources(ctx, s.UserID)
			if err != nil {
				return nil, err
			}
			for i, u := range unlinked {
				if i == limit(a.Limit) {
					out.MoreUnlinked = len(unlinked) - i
					break
				}
				out.Unlinked = append(out.Unlinked, sentText{SourceID: u.ID, Artist: u.Artist, Title: u.Title, Album: u.Album, Listens: u.Listens})
			}
			return out, nil
		},
	},
	{
		name:        "recent_changes",
		description: "The latest changes, newest first, by anyone: the owner on the website, background linking, or you. Each has an edit id that undo takes.",
		readOnly:    true,
		params:      []param{{"limit", "integer", "At most this many, 25 if left out.", false, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Limit int }
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			edits, err := s.DB.Edits(ctx, s.UserID, 0, limit(a.Limit))
			if err != nil {
				return nil, err
			}
			type edit struct {
				EditID    int64  `json:"edit_id"`
				Summary   string `json:"summary"`
				When      string `json:"when"`
				Automatic bool   `json:"automatic,omitempty"`
				Undone    bool   `json:"undone,omitempty"`
			}
			out := []edit{}
			for _, e := range edits {
				out = append(out, edit{e.ID, e.Summary, time.Unix(e.CreatedAt, 0).UTC().Format(time.DateTime), e.Automatic, e.UndoneAt.Valid})
			}
			return out, nil
		},
	},

	// Changes. Each is one edit, undoable.

	{
		name:        "merge",
		description: "Merge one item into another: everything linked to from_id moves to into_id, and from_id's names become other names of into_id. For songs, every recording moves over. For recordings, use it only for the same performance and version. One edit, undoable.",
		params: []param{
			{"kind", "string", "What's being merged.", true, []string{"song", "artist", "album", "recording"}},
			{"from_id", "integer", "The one that goes away.", true, nil},
			{"into_id", "integer", "The one that stays.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind   string
				FromID int64 `json:"from_id"`
				IntoID int64 `json:"into_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "song", "artist", "album", "recording")
			if err != nil {
				return nil, err
			}
			id, err := s.DB.Merge(ctx, s.UserID, kind, a.FromID, a.IntoID)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "link_sent_text",
		description: "Link sent text to a recording, as the owner would on the Fix page. Every listen with that text moves. Text linked this way counts as linked by hand, so automatic re-reading never moves it back. One edit, undoable.",
		params: []param{
			{"source_ids", "array of integer", "The sent texts, from search_sent_text or review.", true, nil},
			{"recording_id", "integer", "Where they go.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SourceIDs   []int64 `json:"source_ids"`
				RecordingID int64   `json:"recording_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if len(a.SourceIDs) == 0 {
				return nil, errors.New("give at least one source_id")
			}
			res, err := s.DB.LinkSources(ctx, s.UserID, a.SourceIDs, a.RecordingID)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, res.EditID, fmt.Sprintf("%d listens moved.", res.Listens))
		},
	},
	{
		name:        "set_version",
		description: `Name a recording's version, like "TV size", "Instrumental" or "桜島麻衣 Ver.". An empty version is the main one. One edit, undoable.`,
		params: []param{
			{"song_id", "integer", "The song the recording belongs to.", true, nil},
			{"recording_id", "integer", "The recording.", true, nil},
			{"version", "string", "The version name, or empty for the main one.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SongID      int64 `json:"song_id"`
				RecordingID int64 `json:"recording_id"`
				Version     string
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.SetVersion(ctx, s.UserID, a.SongID, a.RecordingID, a.Version)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "set_own_row",
		description: "Whether a recording ranks on its own row in Top songs instead of counting with its song. Instrumentals and karaoke start on their own row. One edit, undoable.",
		params: []param{
			{"song_id", "integer", "The song the recording belongs to.", true, nil},
			{"recording_id", "integer", "The recording.", true, nil},
			{"own_row", "boolean", "True for its own row, false to count with the song.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SongID      int64 `json:"song_id"`
				RecordingID int64 `json:"recording_id"`
				OwnRow      bool  `json:"own_row"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.SetOwnRow(ctx, s.UserID, a.SongID, a.RecordingID, a.OwnRow)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "split_song",
		description: "Move some of a song's recordings to a new song with the same name, for two different pieces that only share a title (two games' \"Main Theme\"). One edit, undoable.",
		params: []param{
			{"song_id", "integer", "The song.", true, nil},
			{"recording_ids", "array of integer", "The recordings that become the new song.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SongID       int64   `json:"song_id"`
				RecordingIDs []int64 `json:"recording_ids"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			newSong, id, err := s.DB.SplitSong(ctx, s.UserID, a.SongID, a.RecordingIDs)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, fmt.Sprintf("The new song is %d.", newSong))
		},
	},
	{
		name:        "add_name",
		description: "Give a song, artist or album another name, like its romaji or English title. Doesn't change which name is shown first. One edit, undoable.",
		params: []param{
			{"kind", "string", "What gets the name.", true, []string{"song", "artist", "album"}},
			{"id", "integer", "Its id.", true, nil},
			{"name", "string", "The other name.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind string
				ID   int64
				Name string
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "song", "artist", "album")
			if err != nil {
				return nil, err
			}
			id, err := s.DB.AddName(ctx, s.UserID, kind, a.ID, a.Name)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "rename",
		description: "Change the name shown for a song, artist or album. The old name stays as another name. One edit, undoable.",
		params: []param{
			{"kind", "string", "What to rename.", true, []string{"song", "artist", "album"}},
			{"id", "integer", "Its id.", true, nil},
			{"name", "string", "The new name.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind string
				ID   int64
				Name string
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "song", "artist", "album")
			if err != nil {
				return nil, err
			}
			id, err := s.DB.Rename(ctx, s.UserID, kind, a.ID, a.Name)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "set_counts_for",
		description: `Make an artist's listens also count for another artist, or stop it. For a character and their voice actor (note "voice"), a persona and the person, or a project and its artist. One edit, undoable.`,
		params: []param{
			{"artist_id", "integer", "The character, persona or project.", true, nil},
			{"counts_for_id", "integer", "Who the listens also count for.", true, nil},
			{"note", "string", `What the link is, like "voice". Only used when adding.`, false, nil},
			{"on", "boolean", "True to add the link, false to remove it.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				ArtistID    int64 `json:"artist_id"`
				CountsForID int64 `json:"counts_for_id"`
				Note        string
				On          bool
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.SetArtistLink(ctx, s.UserID, "artist_counts_for", a.ArtistID, a.CountsForID, a.Note, a.On)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "set_member",
		description: "Add an artist to a group as a member, or take them out. Listens of the group count for each member. One edit, undoable.",
		params: []param{
			{"group_id", "integer", "The group.", true, nil},
			{"member_id", "integer", "The member.", true, nil},
			{"on", "boolean", "True to add, false to take out.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				GroupID  int64 `json:"group_id"`
				MemberID int64 `json:"member_id"`
				On       bool
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.SetArtistLink(ctx, s.UserID, "group_members", a.GroupID, a.MemberID, "", a.On)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "undo",
		description: "Undo one change by its edit id, from a tool result or recent_changes. Later changes that touched the same things have to be undone first.",
		params:      []param{{"edit_id", "integer", "The change to undo.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				EditID int64 `json:"edit_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := resolve.Undo(ctx, s.DB, s.UserID, a.EditID)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
}

func limit(n int) int {
	if n <= 0 {
		return 25
	}
	return min(n, 200)
}
