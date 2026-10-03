package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"chokominto/internal/artwork"
	"chokominto/internal/store"
)

// Pictures on pages: thumbnails in table rows, the cover in the infobox,
// and choosing or uploading one on the item's page.

// artURL is where a stored picture is served, at 64 or 440 px (0 = full).
func artURL(a store.Artwork, size int) string {
	name := a.SHA256
	if size > 0 {
		name += fmt.Sprintf("-%d", size)
	}
	return "/art/" + name + "." + artwork.Ext(a.Format)
}

func thumbOf(arts map[int64]store.Artwork, id int64) string {
	if a, ok := arts[id]; ok {
		return artURL(a, 64)
	}
	return ""
}

// pictureData is the Picture part of an item's Edit section.
type pictureData struct {
	Current    string // 440 px
	Chosen     bool   // the owner chose it
	Candidates []candidateRow
	Looking    bool   // a lookup is waiting or running
	Result     string // how the last lookup ended: found, candidates, notfound, error or ""
	Online     bool   // looking for pictures online is on
	Path       string // the item's page
	Live       string // its name for /live
	Song       bool   // a song: its own picture, or else an album's
	FromAlbum  *name  // songs: the album whose cover shows now
}

type candidateRow struct {
	ID     int64
	Title  string
	Artist string
}

func (s *Server) loadPicture(ctx context.Context, u *store.User, kind string, id int64) (*pictureData, error) {
	if kind == "song" {
		return s.loadSongPicture(ctx, u, id)
	}
	p := &pictureData{Online: u.FindArtwork, Path: fmt.Sprintf("%s/%d", pagePaths[kind], id), Live: fmt.Sprintf("picture:%s:%d", kind, id)}
	a, chosen, err := s.db.ItemArtwork(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if a != nil {
		p.Current, p.Chosen = artURL(*a, 440), chosen
	}
	cs, err := s.db.Candidates(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		p.Candidates = append(p.Candidates, candidateRow{c.ID, c.Title, c.Artist})
	}
	if p.Result, err = s.db.Lookup(ctx, kind, id); err != nil {
		return nil, err
	}
	if u.FindArtwork && !chosen {
		p.Looking, err = s.db.ArtworkLooking(ctx, kind, id)
	}
	return p, err
}

// loadSongPicture is the Picture part of a song's Edit view: the song's own
// picture, or the album cover that shows in its place.
func (s *Server) loadSongPicture(ctx context.Context, u *store.User, id int64) (*pictureData, error) {
	p := &pictureData{Song: true, Path: fmt.Sprintf("/song/%d", id)}
	a, _, err := s.db.ItemArtwork(ctx, "song", id)
	if err != nil {
		return nil, err
	}
	if a != nil {
		p.Current, p.Chosen = artURL(*a, 440), true
		return p, nil
	}
	recs, err := s.db.SongRecordings(ctx, u.ID, id)
	if err != nil || len(recs) == 0 {
		return p, err
	}
	covers, err := s.db.RecordingCoverSources(ctx, []int64{recs[0].RecordingID})
	if err != nil {
		return nil, err
	}
	c, ok := covers[recs[0].RecordingID]
	if !ok {
		return p, nil
	}
	p.Current = artURL(c.Artwork, 440)
	albums, err := s.db.ReleaseRefs(ctx, []int64{c.ReleaseID})
	if err != nil {
		return nil, err
	}
	if al, ok := albums[c.ReleaseID]; ok {
		n := albumName(al)
		p.FromAlbum = &n
	}
	return p, nil
}

// candidateThumb serves a candidate's small picture. It's fetched once,
// through the same checks as any picture, and kept, so viewers' browsers
// never contact other sites.
func (s *Server) candidateThumb(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	src, sha, format, err := s.db.CandidateThumb(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	thumbs := artwork.Store{Dir: s.art.Dir + "/candidates"}
	if sha == "" || !thumbs.Has(sha, format) {
		data, err := s.finder.Fetch.Get(r.Context(), src, "image/*")
		if err == nil {
			var st artwork.Stored
			if st, err = thumbs.Save(data); err == nil {
				sha, format = st.SHA256, st.Format
				err = s.db.SetCandidateThumb(r.Context(), id, sha, format)
			}
		}
		if err != nil {
			s.log.Warn("candidate picture", "candidate", id, "err", err)
			s.notFound(w, r)
			return
		}
	}
	f, err := os.Open(thumbs.File(sha, format, 440))
	if err != nil {
		s.notFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", map[string]string{"jpeg": "image/jpeg", "png": "image/png"}[format])
	w.Header().Set("Cache-Control", "private, max-age=86400")
	io.Copy(w, f)
}

// choosePicture downloads a candidate and shows it, as the owner's choice.
func (s *Server) choosePicture(ctx context.Context, u *store.User, kind string, id, candidateID int64) (int64, error) {
	c, err := s.db.Candidate(ctx, kind, id, candidateID)
	if err != nil {
		return 0, err
	}
	a, err := s.finder.Download(ctx, c.Origin, c.URL)
	if err != nil {
		return 0, errPicture
	}
	return s.showPicture(ctx, u, kind, id, a, c.URL)
}

var errPicture = errors.New("that picture couldn't be used")

func (s *Server) showPicture(ctx context.Context, u *store.User, kind string, id int64, a store.Artwork, from string) (int64, error) {
	var editID int64
	err := s.db.Write(ctx, func(tx *sql.Tx) error {
		artID, err := store.AddArtworkTx(ctx, tx, a, from)
		if err != nil {
			return err
		}
		editID, err = store.SetArtworkTx(ctx, tx, u.ID, kind, id, artID, true, false)
		return err
	})
	if err == nil && kind != "song" {
		err = s.db.ClearCandidates(ctx, kind, id)
	}
	return editID, err
}

// lookAgain forgets the candidates and looks for a picture again now.
func (s *Server) lookAgain(ctx context.Context, kind string, id int64) error {
	return s.db.LookAgain(ctx, kind, id)
}

// uploadPicture takes a picture from the owner's computer.
func (s *Server) uploadPicture(kind string) func(http.ResponseWriter, *http.Request, *store.User) {
	return func(w http.ResponseWriter, r *http.Request, u *store.User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.notFound(w, r)
			return
		}
		path := fmt.Sprintf("%s/%d/edit", pagePaths[kind], id)
		back := func(code string) { http.Redirect(w, r, path+"?err="+code+"#picture", http.StatusSeeOther) }
		file, _, err := r.FormFile("picture")
		if err != nil {
			back("picture-none")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, artwork.MaxSize+1))
		if err != nil {
			back("picture-bad")
			return
		}
		st, err := s.art.Save(data)
		switch {
		case errors.Is(err, artwork.ErrTooSmall):
			back("picture-small")
			return
		case errors.Is(err, artwork.ErrTooBig):
			back("picture-big")
			return
		case err != nil:
			back("picture-bad")
			return
		}
		editID, err := s.showPicture(r.Context(), u, kind, id,
			store.Artwork{SHA256: st.SHA256, Format: st.Format, Width: st.Width, Height: st.Height, Origin: "upload"}, "")
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.log.Info("picture uploaded", "user", u.Name, "kind", kind, "id", id)
		if editID == 0 {
			http.Redirect(w, r, path+"#picture", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s?done=%d", path, editID), http.StatusSeeOther)
	}
}
