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
const instructions = `Chokominto is a self-hosted scrobble server. These tools let you answer questions about its owner's listening, and help them tidy their music: find songs, artists and albums that got split, and link or merge them.

How it's organized:
- Sent text: exactly what a music player sent (artist, title, album). Never changed. Each one is linked to a recording, or not linked yet. All listens with the same text move together.
- Song: the piece of music, like 不可思議のカルテ. It can have several names (Japanese, romaji, English). Each name is marked as English, romaji or original script, and is either shown or hidden. Every name, shown or not, links new scrobbles and finds duplicates. Only real names should be shown: the official title, its romaji, an English title. Spellings players sent (YouTube titles, file names) stay hidden.
- Recording: one performance or version of a song, like the TV size, an instrumental, or one character's take ("桜島麻衣 Ver."). Each has its own artists and a version name ("" for the main one).
- Artist: a person, group or character. A character "also counts for" their voice actor, so listens count for both. Groups have members.
- Album: a release, with its tracks.

Good habits:
- For listening questions (most played, how often, what they listened to when), use top, listens_in and history. Periods are calendar weeks, months and years in the owner's time zone, or days of your choosing. Say which period you counted.
- Answer suggested merges from review with answer_suggestions once you've checked both sides.
- Look before you change: search, then show_song or show_artist, and search_sent_text for the raw spellings.
- Different versions of one song are recordings of one song, not separate songs. Merge songs that are the same piece, and give each recording a version name.
- Merge recordings only when they are the same performance and version.
- When a scrobbler got the artist wrong (a YouTube channel, an uploader, or the voice actor instead of the character), fix the recording's credits with set_credits rather than making a new song.
- Junk that isn't an artist or album ("4 million views", "4:00 AM") is cleaned up by moving what's on it, then deleting it. Things that aren't music go to the graveyard. Ask the owner before deleting or moving anything to the graveyard.
- Before a batch of changes, call start_task with a short name the owner will recognize, and finish_task when it's done. The owner sees the task as one entry on the Changes page and can undo all of it at once. If an attempt goes wrong, undo_task rolls back the whole task.
- Every change is one edit the owner can undo, and the result says which. Tell the owner what you changed and the task's name.
- Ask the owner before big or unclear changes. Listens they linked by hand were their own decision.`

type param struct {
	name, typ, desc string
	required        bool
	enum            []string
}

type tool struct {
	name, description string
	readOnly          bool
	destructive       bool // deletes something or hides listens, even if undoable
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

// nameInfo is one of an item's names, with how it's shown.
type nameInfo struct {
	NameID  int64  `json:"name_id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`               // english, romaji or original
	KindSet bool   `json:"kind_set,omitempty"` // set by the owner or an agent, not guessed
	Main    bool   `json:"main,omitempty"`     // the name it's shown by
	OnPage  bool   `json:"on_page"`
	InLists bool   `json:"in_lists,omitempty"`
}

var nameKinds = map[string]string{"en": "english", "romaji": "romaji", "original": "original"}

func nameInfos(as []store.Alias) []nameInfo {
	out := make([]nameInfo, len(as))
	for i, a := range as {
		out[i] = nameInfo{a.ID, a.Name, nameKinds[a.Lang], a.LangSet, i == 0, a.Shown || i == 0, a.InLists}
	}
	return out
}

type sentText struct {
	SourceID     int64      `json:"source_id"`
	Artist       string     `json:"artist"`
	Title        string     `json:"title"`
	Album        string     `json:"album,omitempty"`
	Listens      int        `json:"listens"`
	LinkedTo     *recording `json:"linked_to"`
	OnAlbum      *item      `json:"on_album,omitempty"`
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
		description: "Find the text music players sent, by part of its artist, title or album. Shows how many listens each text has, which recording it's linked to and which album its listens are on. Use this to see every spelling of a song.",
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
			var albumIDs []int64
			for _, src := range srcs {
				if src.ReleaseID.Valid {
					albumIDs = append(albumIDs, src.ReleaseID.Int64)
				}
			}
			albums, err := s.DB.ReleaseRefs(ctx, albumIDs)
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
				if al, ok := albums[src.ReleaseID.Int64]; ok && src.ReleaseID.Valid {
					t.OnAlbum = &item{al.ID, al.Name, al.OtherNames}
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
				Names      []nameInfo `json:"names"`
				Listens    int        `json:"listens"`
				FirstHeard string     `json:"first_heard,omitempty"`
				Recordings []rec      `json:"recordings"`
				Albums     []item     `json:"albums"`
			}{item: item{e.ID, e.Name, e.OtherNames}, Listens: stats.Listens, FirstHeard: when(stats.First), Albums: items(albums)}
			out.Names = nameInfos(names)
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
				Names       []nameInfo  `json:"names"`
				Kind        string      `json:"kind,omitempty"`
				Labels      []string    `json:"labels,omitempty"`
				CountsFor   []item      `json:"counts_for,omitempty"`
				CountedFrom []item      `json:"counted_from,omitempty"`
				Members     []item      `json:"members,omitempty"`
				MemberOf    []item      `json:"member_of,omitempty"`
				Top         []recording `json:"top_recordings"`
			}{item: item{e.ID, e.Name, e.OtherNames}, Kind: kind, Labels: labels,
				CountsFor: items(rel.CountsFor), CountedFrom: items(rel.CountedFrom), Members: items(rel.Members), MemberOf: items(rel.MemberOf)}
			out.Names = nameInfos(names)
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
				Names   []nameInfo  `json:"names"`
				Artists []item      `json:"artists"`
				Tracks  []recording `json:"tracks"`
			}{item: item{e.ID, e.Name, e.OtherNames}, Artists: items(artists)}
			out.Names = nameInfos(names)
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
		description: "What's waiting for the owner on the Review page: suggested merges (two items that look like the same thing) and sent text that isn't linked to anything yet. Most listens first. Answer suggested merges with answer_suggestions.",
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
			// Each side is an item, or a recording for suggested recordings.
			type suggestion struct {
				ID      int64  `json:"id"`
				Kind    string `json:"kind"`
				A       any    `json:"a"`
				B       any    `json:"b"`
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
			var recs []int64
			for _, sg := range sugs {
				if sg.Kind == "recording" {
					recs = append(recs, sg.A, sg.B)
				}
			}
			infos, err := s.DB.RecordingInfos(ctx, recs)
			if err != nil {
				return nil, err
			}
			side := func(kind string, id int64) (any, error) {
				if kind == "recording" {
					return recordingOf(infos[id]), nil
				}
				e, err := s.DB.Entity(ctx, s.UserID, kind, id)
				return item{e.ID, e.Name, e.OtherNames}, err
			}
			agentKind := map[string]string{"song": "song", "artist": "artist", "release": "album", "recording": "recording"}
			for _, sg := range sugs {
				a, err := side(sg.Kind, sg.A)
				if err != nil {
					return nil, err
				}
				b, err := side(sg.Kind, sg.B)
				if err != nil {
					return nil, err
				}
				out.Suggestions = append(out.Suggestions, suggestion{sg.ID, agentKind[sg.Kind], a, b, sg.Reason, sg.Weak, sg.Listens})
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
		name:        "answer_suggestions",
		description: "Answer suggested merges from review, by their ids, all the same way. same: they're the same thing, so they're merged, keeping the side with more listens. version: two recordings are versions of one song (recordings only). different: they aren't the same, and the pair is never suggested again. Look at both sides first. One edit, undoable.",
		params: []param{
			{"ids", "array of integer", "The suggestions' ids, from review.", true, nil},
			{"answer", "string", "The answer for all of them.", true, []string{"same", "version", "different"}},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				IDs    []int64
				Answer string
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if len(a.IDs) == 0 {
				return nil, fmt.Errorf("give at least one id")
			}
			editID, n, err := s.DB.AnswerSuggestions(ctx, s.UserID, a.IDs, a.Answer)
			switch {
			case errors.Is(err, store.ErrNotFound):
				return nil, fmt.Errorf("those suggestions were already answered, or their items were merged since")
			case errors.Is(err, store.ErrAnswer):
				return nil, fmt.Errorf("version only fits suggestions of two recordings")
			case err != nil:
				return nil, err
			}
			note := ""
			if skipped := len(a.IDs) - n; skipped > 0 {
				note = fmt.Sprintf("%d were skipped: already answered, or joined by an earlier one.", skipped)
			}
			return s.changed(ctx, editID, note)
		},
	},
	{
		name:        "recent_changes",
		description: "The latest changes, newest first, by anyone: the owner on the website, background linking, or you. Each has an edit id that undo takes, and changes made by agents have the task they're part of, which undo_task takes.",
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
				By        string `json:"by,omitempty"`
				TaskID    int64  `json:"task_id,omitempty"`
				Task      string `json:"task,omitempty"`
			}
			var taskIDs []int64
			for _, e := range edits {
				taskIDs = append(taskIDs, e.TaskID)
			}
			tasks, err := s.DB.Tasks(ctx, s.UserID, taskIDs)
			if err != nil {
				return nil, err
			}
			out := []edit{}
			for _, e := range edits {
				out = append(out, edit{e.ID, e.Summary, time.Unix(e.CreatedAt, 0).UTC().Format(time.DateTime), e.Automatic, e.UndoneAt.Valid,
					e.Agent, e.TaskID, tasks[e.TaskID].Name})
			}
			return out, nil
		},
	},

	// Changes. Each is one edit, undoable.

	{
		name:        "merge",
		description: "Merge one item into another: everything linked to from_id moves to into_id. from_id's names come along hidden: they still link new scrobbles sent with them, but aren't shown (see set_name). For songs, every recording moves over, so name each one's version with set_version, like a character's version (\"桜島麻衣 Ver.\") or a TV size, rather than leaving them all as the main one. For recordings, use it only for the same performance and version. One edit, undoable.",
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
		description: "Link sent text to a recording, as the owner would on the Fix page. Every listen with that text moves, and later listens sent with the same text go there too. The listens stay on the album they're on. Text linked this way counts as linked by hand, so automatic re-reading never moves it back. One edit, undoable.",
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
		name:        "set_sent_text_album",
		description: "Put the listens of sent text on another album, or on no album, without changing the song. For listens that ended up on the wrong album. Every listen with that text moves, and the text counts as linked by hand. One edit, undoable.",
		params: []param{
			{"source_ids", "array of integer", "The sent texts, from search_sent_text. They must be linked to a recording already.", true, nil},
			{"album_id", "integer", "The album they go on, or 0 for no album.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SourceIDs []int64 `json:"source_ids"`
				AlbumID   *int64  `json:"album_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if len(a.SourceIDs) == 0 || a.AlbumID == nil {
				return nil, errors.New("give at least one source_id, and album_id (0 for no album)")
			}
			res, err := s.DB.SetSourcesRelease(ctx, s.UserID, a.SourceIDs, *a.AlbumID)
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
		description: "Give a song, artist or album another name, like the romaji or English title. Doesn't change which name is shown first. One edit, undoable.",
		params: []param{
			{"kind", "string", "What gets the name.", true, []string{"song", "artist", "album"}},
			{"id", "integer", "The id of that song, artist or album.", true, nil},
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
		name: "set_name",
		description: `Change how one of a song's, artist's or album's names is shown. Every name, shown or not, still links new scrobbles and finds likely duplicates, so hiding a name is safe. Names that came over in a merge start hidden, since they're mostly spellings players sent, like YouTube titles or file names.
- kind: whether it's English, romaji or in the original script. Names are shown in that order, so this decides which one is shown first unless use_as_name is set.
- on_page: listed under the title on the item's own page. Show real names (the official title, its romaji, an English title), not spellings players sent.
- in_lists: true makes it the one name shown under the item's name in tables and rankings. False on the name that's there now leaves none.
- use_as_name: true shows the item by this name.
Leave out what shouldn't change. One edit, undoable.`,
		params: []param{
			{"kind", "string", "What the name belongs to.", true, []string{"song", "artist", "album"}},
			{"id", "integer", "The id of that song, artist or album.", true, nil},
			{"name_id", "integer", "The name, from show_song, show_artist or show_album.", true, nil},
			{"name_kind", "string", "What kind of name it is.", false, []string{"english", "romaji", "original"}},
			{"on_page", "boolean", "List it on the item's page.", false, nil},
			{"in_lists", "boolean", "Show it under the item's name in lists.", false, nil},
			{"use_as_name", "boolean", "Show the item by this name. Only true does anything.", false, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind      string
				ID        int64
				NameID    int64  `json:"name_id"`
				NameKind  string `json:"name_kind"`
				OnPage    *bool  `json:"on_page"`
				InLists   *bool  `json:"in_lists"`
				UseAsName bool   `json:"use_as_name"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "song", "artist", "album")
			if err != nil {
				return nil, err
			}
			names, err := s.DB.Aliases(ctx, kind, a.ID)
			if err != nil {
				return nil, fmt.Errorf("no %s with id %d", a.Kind, a.ID)
			}
			var cur *store.Alias
			for i := range names {
				if names[i].ID == a.NameID {
					cur = &names[i]
				}
			}
			if cur == nil {
				return nil, fmt.Errorf("name %d isn't one of the names there, see show_%s", a.NameID, a.Kind)
			}
			choice := store.NameChoice{AliasID: cur.ID, Shown: cur.Shown}
			if a.NameKind != "" {
				for lang, k := range nameKinds {
					if k == a.NameKind {
						choice.Lang = lang
					}
				}
				if choice.Lang == "" {
					return nil, errors.New("name_kind must be english, romaji or original")
				}
			}
			if a.OnPage != nil {
				choice.Shown = *a.OnPage
			}
			ch := store.NamesChange{Names: []store.NameChoice{choice}}
			if a.InLists != nil {
				var to int64
				if *a.InLists {
					to = cur.ID
				} else if !cur.InLists {
					to = -1 // it isn't there, nothing to take away
				}
				if to >= 0 {
					ch.InLists = &to
				}
			}
			if a.UseAsName {
				ch.First = cur.ID
			}
			id, err := s.DB.SetNames(ctx, s.UserID, kind, a.ID, ch)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "remove_name",
		description: "Take a name away from a song, artist or album. It then no longer links new scrobbles sent with that name, or finds duplicates by it. Listens already linked stay. To just stop showing a name, use set_name with on_page false instead. The only name can't be removed. One edit, undoable.",
		params: []param{
			{"kind", "string", "What the name belongs to.", true, []string{"song", "artist", "album"}},
			{"id", "integer", "The id of that song, artist or album.", true, nil},
			{"name_id", "integer", "The name, from show_song, show_artist or show_album.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind   string
				ID     int64
				NameID int64 `json:"name_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "song", "artist", "album")
			if err != nil {
				return nil, err
			}
			id, err := s.DB.RemoveName(ctx, s.UserID, kind, a.ID, a.NameID)
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
			{"id", "integer", "The id of that song, artist or album.", true, nil},
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
		description: `Make an artist's listens also count for another artist, or stop it. For a character and their voice actor (note "voice"), a persona and the person, or a project and the artist behind it. One edit, undoable.`,
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
		name: "set_credits",
		description: `Replace who a recording is credited to, for a recording credited to a YouTube channel, an uploader or a voice actor instead of the character who sings it. Give the full new list: artists left out stop being credited on this recording. Listens already linked stay with the song. A recording needs at least one main artist.
Someone with no artist yet (a character nobody scrobbled) goes in new_artists and is added in the same edit. Link a character to their voice actor with set_counts_for afterwards. One edit, undoable.`,
		params: []param{
			{"recording_id", "integer", "The recording, from show_song.", true, nil},
			{"artist_ids", "array of integer", "Main artists, in order.", false, nil},
			{"featured_ids", "array of integer", "Featured artists, in order.", false, nil},
			{"new_artists", "array of string", "Names of main artists to add as new artists. Check with search first that they don't exist.", false, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				RecordingID int64    `json:"recording_id"`
				ArtistIDs   []int64  `json:"artist_ids"`
				FeaturedIDs []int64  `json:"featured_ids"`
				NewArtists  []string `json:"new_artists"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			song, err := s.DB.RecordingSong(ctx, s.UserID, a.RecordingID)
			if err != nil {
				return nil, fmt.Errorf("no recording with id %d", a.RecordingID)
			}
			var credits []store.CreditChoice
			for _, id := range a.ArtistIDs {
				credits = append(credits, store.CreditChoice{ArtistID: id, Role: "main"})
			}
			for _, n := range a.NewArtists {
				credits = append(credits, store.CreditChoice{NewArtist: n, Role: "main"})
			}
			for _, id := range a.FeaturedIDs {
				credits = append(credits, store.CreditChoice{ArtistID: id, Role: "featured"})
			}
			id, err := s.DB.SetRecordingCredits(ctx, s.UserID, song, a.RecordingID, credits)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "set_album_artists",
		description: "Replace who an album is credited to. Give the full new list, or an empty one for none (the album then goes with its songs' artists). New artists are added in the same edit. One edit, undoable.",
		params: []param{
			{"album_id", "integer", "The album.", true, nil},
			{"artist_ids", "array of integer", "Album artists, in order.", false, nil},
			{"new_artists", "array of string", "Names of artists to add as new artists.", false, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				AlbumID    int64    `json:"album_id"`
				ArtistIDs  []int64  `json:"artist_ids"`
				NewArtists []string `json:"new_artists"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			var artists []store.CreditChoice
			for _, id := range a.ArtistIDs {
				artists = append(artists, store.CreditChoice{ArtistID: id})
			}
			for _, n := range a.NewArtists {
				artists = append(artists, store.CreditChoice{NewArtist: n})
			}
			id, err := s.DB.SetAlbumArtists(ctx, s.UserID, a.AlbumID, artists)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "create_artist",
		description: "Add an artist nobody has scrobbled yet, like a character's voice actor, to link with set_counts_for or set_member. Search first so you don't make a second one. One edit, undoable.",
		params:      []param{{"name", "string", "Their name, in any script.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Name string }
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			artist, id, err := s.DB.CreateArtist(ctx, s.UserID, a.Name)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, fmt.Sprintf("The new artist is %d.", artist))
		},
	},
	{
		name:        "take_off_album",
		description: `Take recordings off an album they ended up on by mistake, like listens sent with "4:00 AM" or a playlist name as the album. Their listens stay, with no album, and reading their text again never puts them back. Once nothing is on the album it can be deleted with delete. One edit, undoable.`,
		params: []param{
			{"album_id", "integer", "The album.", true, nil},
			{"recording_ids", "array of integer", "The recordings to take off, from show_album.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				AlbumID      int64   `json:"album_id"`
				RecordingIDs []int64 `json:"recording_ids"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if len(a.RecordingIDs) == 0 {
				return nil, errors.New("give at least one recording_id")
			}
			id, err := s.DB.TakeOffAlbum(ctx, s.UserID, a.AlbumID, a.RecordingIDs)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "delete",
		description: `Delete an artist or album nothing uses anymore, like "4 million views" sent as an artist. It only works once no song, album, listen or other artist points at it: fix credits with set_credits or set_album_artists, move songs off with take_off_album, or merge it instead when it's a real duplicate. Songs can't be deleted, use move_to_graveyard. Ask the owner first. One edit, undoable.`,
		destructive: true,
		params: []param{
			{"kind", "string", "What to delete.", true, []string{"artist", "album"}},
			{"id", "integer", "The id of that artist or album.", true, nil},
		},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				Kind string
				ID   int64
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			kind, err := kindOf(a.Kind, "artist", "album")
			if err != nil {
				return nil, err
			}
			id, err := s.DB.DeleteItem(ctx, s.UserID, kind, a.ID)
			if errors.Is(err, store.ErrInUse) {
				u, uerr := s.DB.ItemUsage(ctx, kind, a.ID)
				if uerr != nil {
					return nil, uerr
				}
				return nil, fmt.Errorf("still in use: %d songs, %d albums, %d listens, %d links to other artists, %d remembered links, merged into by others: %v",
					u.Songs, u.Albums, u.Listens, u.Links, u.Rules, u.MergedInto)
			}
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "move_to_graveyard",
		description: "Move a song that isn't music (a video, a podcast, a stream) to the graveyard. Its listens are kept but stop counting anywhere, and later listens of it go there too. The owner reviews the graveyard and brings songs back or deletes their listens. For a song that's just a duplicate, merge instead. Ask the owner first. One edit, undoable.",
		destructive: true,
		params:      []param{{"song_id", "integer", "The song.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SongID int64 `json:"song_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.BurySong(ctx, s.UserID, a.SongID)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "bring_back",
		description: "Bring a song back from the graveyard, with its listens. One edit, undoable.",
		params:      []param{{"song_id", "integer", "The song.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				SongID int64 `json:"song_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.UnburySong(ctx, s.UserID, a.SongID)
			if err != nil {
				return nil, err
			}
			return s.changed(ctx, id, "")
		},
	},
	{
		name:        "start_task",
		description: `Start a task before a batch of changes, named so the owner recognizes it on the Changes page, like "Tidy Fukashigi no Carte". Your changes from now on are grouped under it, until you call finish_task or start another, and the owner can undo the whole task at once. Without one, your changes are grouped by themselves, with no name.`,
		params:      []param{{"name", "string", "What you're about to do, in a few words.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct{ Name string }
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			id, err := s.DB.StartTask(ctx, s.UserID, a.Name)
			if err != nil {
				return nil, err
			}
			return map[string]any{"task_id": id, "note": "Your changes are now grouped under this task. undo_task takes its id."}, nil
		},
	},
	{
		name:        "finish_task",
		description: "Finish the task you started, once its changes are done. Tell the owner the task's name, so they can find it on the Changes page.",
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			if err := decode(raw, &struct{}{}); err != nil {
				return nil, err
			}
			id, err := s.DB.FinishTask(ctx, s.UserID)
			if err != nil {
				return nil, err
			}
			if id == 0 {
				return change{Note: "No task was open."}, nil
			}
			return map[string]any{"task_id": id}, nil
		},
	},
	{
		name:        "undo_task",
		description: "Undo every change of a task at once, newest first, to roll back an attempt that went wrong. It's all or nothing: if a later change outside the task touched the same things, nothing is undone and the result lists those changes. Ask the owner before undoing a task you didn't make.",
		params:      []param{{"task_id", "integer", "The task, from start_task or recent_changes.", true, nil}},
		run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var a struct {
				TaskID int64 `json:"task_id"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			n, err := resolve.UndoTask(ctx, s.DB, s.UserID, a.TaskID)
			var conflict *store.ConflictError
			if errors.As(err, &conflict) {
				sums, serr := s.DB.EditSummaries(ctx, s.UserID, conflict.Later)
				if serr != nil {
					return nil, serr
				}
				return nil, fmt.Errorf("nothing was undone, because later changes touched the same things: %s (edit ids %v)", strings.Join(sums, "; "), conflict.Later)
			}
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("no task with id %d", a.TaskID)
			}
			if err != nil {
				return nil, err
			}
			return change{Note: fmt.Sprintf("Undid all %d changes of the task.", n)}, nil
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
