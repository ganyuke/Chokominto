package web

import (
	"chokominto/internal/artwork"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"chokominto/internal/auth"
	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

const historyPageSize = 100

func location(u store.User) *time.Location {
	if loc, err := time.LoadLocation(u.TimeZone); err == nil {
		return loc
	}
	return time.UTC
}

// History

type dayGroup struct {
	Label string
	Rows  []listenRow
}

type historyPage struct {
	Page
	Playing *playingBox
	Days    []dayGroup
	Newer   string
	Older   string
	Live    bool // the newest page, which updates itself
}

func parseCursor(v string) (store.Cursor, bool) {
	ts, id, ok := strings.Cut(v, ".")
	if !ok {
		return store.Cursor{}, false
	}
	t, err1 := strconv.ParseInt(ts, 10, 64)
	i, err2 := strconv.ParseInt(id, 10, 64)
	return store.Cursor{TS: t, ID: i}, err1 == nil && err2 == nil
}

func cursorParam(l store.Listen) string { return fmt.Sprintf("%d.%d", l.ListenedAt, l.ID) }

func (s *Server) history(w http.ResponseWriter, r *http.Request, viewer *store.User, owner store.User) {
	ctx := r.Context()
	p := historyPage{Page: Page{Title: "History", Nav: "history", User: viewer}}
	s.doneNotice(r, viewer, &p.Page)
	playing, err := s.nowPlaying(ctx, owner)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Playing = playing
	if err := s.historyDays(ctx, r.URL.Query(), viewer, owner, &p); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "history", p)
}

// historyDays fills in one page of listens, with links to the pages
// around it. The newest page keeps itself current in the browser.
func (s *Server) historyDays(ctx context.Context, q url.Values, viewer *store.User, owner store.User, p *historyPage) error {
	rng := store.ListenRange{Limit: historyPageSize}
	if c, ok := parseCursor(q.Get("before")); ok {
		rng.Before = &c
	} else if c, ok := parseCursor(q.Get("after")); ok {
		rng.After = &c
		rng.Oldest = true
	}
	p.Live = rng.Before == nil && rng.After == nil
	ls, err := s.db.Listens(ctx, owner.ID, rng)
	if err != nil {
		return err
	}
	loc := location(owner)
	rows, err := s.listenRows(ctx, ls, loc, viewer != nil)
	if err != nil {
		return err
	}
	for i, l := range ls {
		label := time.Unix(l.ListenedAt, 0).In(loc).Format("Monday, 2 January 2006")
		if len(p.Days) == 0 || p.Days[len(p.Days)-1].Label != label {
			p.Days = append(p.Days, dayGroup{Label: label})
		}
		d := &p.Days[len(p.Days)-1]
		d.Rows = append(d.Rows, rows[i])
	}
	if len(ls) > 0 {
		first, last := ls[0], ls[len(ls)-1]
		newer, err := s.db.Listens(ctx, owner.ID, store.ListenRange{After: &store.Cursor{TS: first.ListenedAt, ID: first.ID}, Limit: 1})
		if err != nil {
			return err
		}
		if len(newer) > 0 {
			p.Newer = "/history?after=" + cursorParam(first)
		}
		older, err := s.db.Listens(ctx, owner.ID, store.ListenRange{Before: &store.Cursor{TS: last.ListenedAt, ID: last.ID}, Limit: 1})
		if err != nil {
			return err
		}
		if len(older) > 0 {
			p.Older = "/history?before=" + cursorParam(last)
		}
	}
	return nil
}

// Login

type loginPage struct {
	Page
	Name string
	Next string
}

// safeNext only allows paths on this site, so a login link can't send
// someone elsewhere afterwards.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

var (
	dummyHashOnce sync.Once
	dummyHash     string
)

// checkNoUser spends the same time as a real password check, so response
// times don't reveal which names exist.
func checkNoUser(password string) {
	dummyHashOnce.Do(func() { dummyHash, _ = auth.HashPassword("chokominto timing placeholder") })
	auth.CheckPassword(dummyHash, password)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.sessionUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", loginPage{Page: Page{Title: "Log in", Nav: "login"}, Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	page := loginPage{Page: Page{Title: "Log in", Nav: "login"}, Name: name, Next: next}

	ip := auth.ClientIP(r, s.cfg.Proxies)
	if !s.limiter.Allowed(ip) {
		page.Error = "Too many tries. Wait 15 minutes and try again."
		s.render(w, http.StatusTooManyRequests, "login", page)
		return
	}
	u, err := s.db.UserByName(r.Context(), name)
	ok := false
	switch {
	case errors.Is(err, store.ErrNotFound):
		checkNoUser(password)
	case err != nil:
		s.serverError(w, r, err)
		return
	default:
		ok, err = auth.CheckPassword(u.PasswordHash, password)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if !ok {
		s.limiter.Fail(ip)
		s.log.Info("login failed", "ip", ip)
		page.Error = "That name or password is wrong."
		s.render(w, http.StatusUnauthorized, "login", page)
		return
	}
	s.limiter.Reset(ip)
	if err := s.setSession(w, r, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("logged in", "user", u.Name, "ip", ip)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Settings

type tokenRow struct {
	ID       int64
	Label    string
	Created  string
	LastUsed string
	Revoked  bool
	Change   bool // an agent token that can change things
}

type newToken struct {
	Label  string
	Secret string
}

type settingsPage struct {
	Page
	Tokens    []tokenRow
	NewToken  *newToken
	Agents    []tokenRow
	NewAgent  *newAgent
	BaseURL   string
	TimeZone  string
	WeekStart int
	Zones     []string
	Labels    []store.LabelUse
	Readings  []readingGroup
	OwnRules  []resolve.OwnRule
	Unused    int    // pictures nothing shows anymore
	UnusedMB  string // the space they take
}

type readingGroup struct {
	Section  string
	Readings []resolve.ReadingState
}

// Messages shown after a redirect, keyed so no text comes from the URL.
var notices = map[string]string{
	"revoked":          "Token revoked. That scrobbler can't send listens anymore.",
	"agent-revoked":    "Token revoked. That agent can't reach your music anymore.",
	"tz":               "Time zone saved.",
	"display-name":     "Name saved.",
	"password":         "Password changed. You've been logged out everywhere else.",
	"undone":           "Undone.",
	"already":          "That change was already undone.",
	"task-undone":      "Undone, all of it.",
	"scrobbled":        "Scrobbled.",
	"label":            "Label saved.",
	"unlabeled":        "Label deleted.",
	"pictures-on":      "Chokominto now looks for pictures online.",
	"pictures-off":     "Chokominto no longer looks for pictures online. Pictures already found stay.",
	"pictures-cleaned": "Unused pictures removed.",
	"names-on":         "Other names are shown under each name.",
	"names-off":        "Other names are hidden.",
	"readings-same":    "Nothing changed. Check or uncheck a reading, then save.",
}

var zones = []string{
	"Asia/Tokyo", "Asia/Seoul", "Asia/Shanghai", "Asia/Taipei", "Asia/Hong_Kong", "Asia/Singapore",
	"Asia/Manila", "Asia/Jakarta", "Asia/Bangkok", "Asia/Kolkata", "Asia/Dubai",
	"Australia/Sydney", "Australia/Melbourne", "Australia/Perth", "Pacific/Auckland",
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin", "Europe/Madrid",
	"Europe/Rome", "Europe/Amsterdam", "Europe/Stockholm", "Europe/Warsaw", "Europe/Helsinki",
	"Europe/Istanbul", "Europe/Moscow",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
	"America/Anchorage", "Pacific/Honolulu", "America/Toronto", "America/Vancouver",
	"America/Mexico_City", "America/Sao_Paulo", "America/Argentina/Buenos_Aires", "UTC",
}

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimRight(s.cfg.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) settingsData(r *http.Request, u *store.User) (settingsPage, error) {
	p := settingsPage{
		Page:      Page{Title: "Settings", Nav: "settings", User: u, Notice: notices[r.URL.Query().Get("notice")], Undo: undoParam(r)},
		BaseURL:   s.baseURL(r),
		TimeZone:  u.TimeZone,
		WeekStart: u.WeekStart,
		Zones:     zones,
	}
	if r.URL.Query().Get("done") != "" {
		s.doneNotice(r, u, &p.Page)
	}
	ts, err := s.db.Tokens(r.Context(), u.ID)
	if err != nil {
		return p, err
	}
	if p.Labels, err = s.db.Labels(r.Context(), u.ID); err != nil {
		return p, err
	}
	states, own, err := resolve.ReadingSettings(r.Context(), s.db, u.ID)
	if err != nil {
		return p, err
	}
	for _, st := range states {
		if n := len(p.Readings); n == 0 || p.Readings[n-1].Section != st.Section {
			p.Readings = append(p.Readings, readingGroup{Section: st.Section})
		}
		g := &p.Readings[len(p.Readings)-1]
		g.Readings = append(g.Readings, st)
	}
	p.OwnRules = own
	unused, err := s.db.UnusedArtwork(r.Context())
	if err != nil {
		return p, err
	}
	var bytes int64
	for _, a := range unused {
		bytes += s.artSize(a)
	}
	p.Unused, p.UnusedMB = len(unused), fmt.Sprintf("%.1f", float64(bytes)/(1<<20))
	loc := location(*u)
	for _, t := range ts {
		row := tokenRow{ID: t.ID, Label: t.Label, Created: time.Unix(t.CreatedAt, 0).In(loc).Format("2 Jan 2006"), Revoked: t.RevokedAt.Valid, Change: t.Access == "change"}
		if t.LastUsedAt.Valid {
			row.LastUsed = time.Unix(t.LastUsedAt.Int64, 0).In(loc).Format("2 Jan 2006, 15:04")
		}
		if t.Access == "scrobble" {
			p.Tokens = append(p.Tokens, row)
		} else {
			p.Agents = append(p.Agents, row)
		}
	}
	return p, nil
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request, u *store.User) {
	p, err := s.settingsData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "settings", p)
}

func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, u *store.User, msg string) {
	p, err := s.settingsData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Notice = ""
	p.Error = msg
	s.render(w, http.StatusBadRequest, "settings", p)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	label := strings.TrimSpace(r.PostFormValue("label"))
	if label == "" || len([]rune(label)) > 100 {
		s.settingsError(w, r, u, "Name the token after the scrobbler that will use it.")
		return
	}
	secret := auth.NewSecret()
	if _, err := s.db.CreateToken(r.Context(), u.ID, label, auth.HashSecret(secret)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("token created", "user", u.Name, "label", label)
	p, err := s.settingsData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.NewToken = &newToken{Label: label, Secret: secret}
	s.render(w, http.StatusOK, "settings", p)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.revoke(w, r, u, "/settings?notice=revoked#scrobblers")
}

// revoke stops a scrobbler's or an agent's token from working.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request, u *store.User, back string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	err = s.db.RevokeToken(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("token revoked", "user", u.Name, "token", id)
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (s *Server) setTimeZone(w http.ResponseWriter, r *http.Request, u *store.User) {
	tz := strings.TrimSpace(r.PostFormValue("time_zone"))
	if _, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" {
		s.settingsError(w, r, u, "That time zone isn't recognized. Use a name like Asia/Tokyo.")
		return
	}
	weekStart := 1
	if r.PostFormValue("week_start") == "0" {
		weekStart = 0
	}
	if err := s.db.SetPreferences(r.Context(), u.ID, tz, weekStart); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?notice=tz#time-zone", http.StatusSeeOther)
}

// displayNameMax is the longest display name, in characters.
const displayNameMax = 60

func (s *Server) setDisplayName(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := strings.Join(strings.Fields(r.PostFormValue("display_name")), " ")
	if utf8.RuneCountInString(name) > displayNameMax {
		s.settingsError(w, r, u, "That name is too long. Keep it to 60 characters.")
		return
	}
	if name == u.Name {
		name = ""
	}
	if err := s.db.SetDisplayName(r.Context(), u.ID, name); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings?notice=display-name#display-name", http.StatusSeeOther)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u *store.User) {
	ip := auth.ClientIP(r, s.cfg.Proxies)
	if !s.limiter.Allowed(ip) {
		s.settingsError(w, r, u, "Too many tries. Wait 15 minutes and try again.")
		return
	}
	ok, err := auth.CheckPassword(u.PasswordHash, r.PostFormValue("current"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok {
		s.limiter.Fail(ip)
		s.settingsError(w, r, u, "Your current password is wrong.")
		return
	}
	newPW := r.PostFormValue("new")
	if newPW != r.PostFormValue("again") {
		s.settingsError(w, r, u, "The new passwords don't match.")
		return
	}
	if newPW == "" {
		s.settingsError(w, r, u, "Type a new password.")
		return
	}
	hash, err := auth.HashPassword(newPW)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// This signs out every session, then keeps this one signed in.
	if err := s.db.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.setSession(w, r, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("password changed", "user", u.Name)
	http.Redirect(w, r, "/settings?notice=password#password", http.StatusSeeOther)
}

// Changes

const changesPageSize = 50

type editRow struct {
	ID      int64
	When    string
	Summary string
	Undone  bool
	Agent   string // the agent token's name, when an AI agent made it
}

// taskRow is an agent's task on Changes: one row for all its edits, with
// Undo all.
type taskRow struct {
	ID     int64
	When   string
	Name   string // "" when the agent didn't name it
	Agent  string
	Undone bool
	Edits  []editRow
}

// changeRow is one row of Changes: an edit, or a task.
type changeRow struct {
	editRow
	Task *taskRow
}

type changesPage struct {
	Page
	Rows     []changeRow
	Conflict []string
	Here     string // this page of Changes, for Undo to come back to
	Newer    string
	Older    string
}

func (s *Server) changesData(r *http.Request, u *store.User) (changesPage, error) {
	p := changesPage{Page: Page{Title: "Changes", Nav: "changes", User: u}, Here: "/changes"}
	s.doneNotice(r, u, &p.Page)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if before > 0 {
		p.Here = fmt.Sprintf("/changes?before=%d", before)
		p.Newer = "/changes"
	}
	// One more than fits, to know whether there's an older page.
	es, err := s.db.ChangeRows(r.Context(), u.ID, before, changesPageSize+1)
	if err != nil {
		return p, err
	}
	if len(es) > changesPageSize {
		es = es[:changesPageSize]
		p.Older = fmt.Sprintf("/changes?before=%d", es[len(es)-1].ID)
	}
	loc := location(*u)
	row := func(e store.Edit) editRow {
		return editRow{e.ID, time.Unix(e.CreatedAt, 0).In(loc).Format("2 Jan 2006, 15:04"), e.Summary, e.UndoneAt.Valid, e.Agent}
	}
	var taskIDs []int64
	for _, e := range es {
		taskIDs = append(taskIDs, e.TaskID)
	}
	tasks, err := s.db.Tasks(r.Context(), u.ID, taskIDs)
	if err != nil {
		return p, err
	}
	// A task is one row, where its newest edit is, with all its edits.
	for _, e := range es {
		t, ok := tasks[e.TaskID]
		if !ok {
			p.Rows = append(p.Rows, changeRow{editRow: row(e)})
			continue
		}
		edits, err := s.db.TaskEdits(r.Context(), u.ID, t.ID)
		if err != nil {
			return p, err
		}
		tr := &taskRow{ID: t.ID, When: row(e).When, Name: t.Name, Agent: t.Agent, Undone: t.UndoneAt.Valid}
		for _, te := range edits {
			tr.Edits = append(tr.Edits, row(te))
		}
		p.Rows = append(p.Rows, changeRow{Task: tr})
	}
	return p, nil
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request, u *store.User) {
	p, err := s.changesData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusOK, "changes", p)
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	_, err = resolve.Undo(r.Context(), s.db, u.ID, id)
	var conflict *store.ConflictError
	switch {
	case err == nil:
		s.log.Info("undone", "user", u.Name, "edit", id)
		back := "/changes"
		if b := r.PostFormValue("back"); b != "" {
			back = safeNext(b)
		}
		http.Redirect(w, r, withNotice(back, "undone"), http.StatusSeeOther)
	case errors.Is(err, store.ErrAlreadyUndone):
		http.Redirect(w, r, "/changes?notice=already", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, store.ErrStale):
		p, err := s.changesData(r, u)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		p.Notice = ""
		p.Error = "This can't be undone anymore. What it changed has been changed again since."
		s.render(w, http.StatusConflict, "changes", p)
	case errors.As(err, &conflict):
		s.undoConflict(w, r, u, conflict)
	default:
		s.serverError(w, r, err)
	}
}

// undoConflict shows Changes with the later edits that block an undo.
func (s *Server) undoConflict(w http.ResponseWriter, r *http.Request, u *store.User, conflict *store.ConflictError) {
	p, err := s.changesData(r, u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p.Conflict, err = s.db.EditSummaries(r.Context(), u.ID, conflict.Later)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, http.StatusConflict, "changes", p)
}

// undoTask undoes all of an agent's task, or nothing.
func (s *Server) undoTask(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	n, err := resolve.UndoTask(r.Context(), s.db, u.ID, id)
	var conflict *store.ConflictError
	switch {
	case err == nil:
		s.log.Info("task undone", "user", u.Name, "task", id, "edits", n)
		back := "/changes"
		if b := r.PostFormValue("back"); b != "" {
			back = safeNext(b)
		}
		http.Redirect(w, r, withNotice(back, "task-undone"), http.StatusSeeOther)
	case errors.Is(err, store.ErrAlreadyUndone):
		http.Redirect(w, r, "/changes?notice=already", http.StatusSeeOther)
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, store.ErrStale):
		p, err := s.changesData(r, u)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		p.Notice = ""
		p.Error = "This can't be undone anymore. What it changed has been changed again since."
		s.render(w, http.StatusConflict, "changes", p)
	case errors.As(err, &conflict):
		s.undoConflict(w, r, u, conflict)
	default:
		s.serverError(w, r, err)
	}
}

// withNotice adds a notice to a local path, keeping its query.
func withNotice(path, notice string) string {
	u, err := url.Parse(path)
	if err != nil {
		return "/"
	}
	q := u.Query()
	q.Del("done")
	q.Set("notice", notice)
	u.RawQuery = q.Encode()
	return u.String()
}

// doneNotice shows the edit named by ?done= as the notice, with Undo, so
// a page can say exactly what just changed ("Linked 2 listens to Idol").
func (s *Server) doneNotice(r *http.Request, u *store.User, p *Page) {
	if u == nil {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("done"), 10, 64)
	if id <= 0 {
		if n := notices[r.URL.Query().Get("notice")]; n != "" {
			p.Notice = n
		}
		return
	}
	if sums, err := s.db.EditSummaries(r.Context(), u.ID, []int64{id}); err == nil && len(sums) == 1 {
		p.Notice, p.Undo = sums[0]+".", id
		p.Back = r.URL.Path
	}
}

// undoParam reads the edit a notice offers to undo.
func undoParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.URL.Query().Get("undo"), 10, 64)
	return max(id, 0)
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	editID, err := s.db.DeleteLabel(r.Context(), u.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.log.Info("label deleted", "user", u.Name, "label", id)
		// No jump to the labels, so the notice with its Undo is in view.
		http.Redirect(w, r, fmt.Sprintf("/settings?notice=unlabeled&undo=%d", editID), http.StatusSeeOther)
	}
}

func (s *Server) updateLabel(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len([]rune(name)) > 60 {
		s.settingsError(w, r, u, "Give the label a name.")
		return
	}
	err = s.db.UpdateLabel(r.Context(), u.ID, id, name, r.PostFormValue("hide_default") == "1")
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, store.ErrLabelName):
		s.settingsError(w, r, u, "There's already a label with that name.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/settings?notice=label#labels", http.StatusSeeOther)
	}
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request, u *store.User) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len([]rune(name)) > 60 {
		s.settingsError(w, r, u, "Give the label a name.")
		return
	}
	editID, err := s.db.CreateLabel(r.Context(), u.ID, name, r.PostFormValue("hide_default") == "1")
	switch {
	case errors.Is(err, store.ErrLabelName):
		s.settingsError(w, r, u, "There's already a label with that name.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, fmt.Sprintf("/settings?done=%d", editID), http.StatusSeeOther)
	}
}

func (s *Server) moveLabel(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	by := 1
	if r.PostFormValue("by") == "-1" {
		by = -1
	}
	_, err = s.db.MoveLabel(r.Context(), u.ID, id, by)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		// Straight back to the list: moving is easy to undo by moving back.
		http.Redirect(w, r, "/settings#labels", http.StatusSeeOther)
	}
}

type readingsPage struct {
	Page
	Changes []resolve.ReadingChange
	On      []string // every reading to be on after saving
}

// readingsWanted is every reading's state as the Settings form sent it:
// on when checked.
func readingsWanted(r *http.Request) (map[string]bool, []string) {
	checked := r.Form["on"]
	want := map[string]bool{}
	for _, rd := range resolve.Readings {
		want[rd.Key] = slices.Contains(checked, rd.Key)
	}
	return want, checked
}

// previewReadings shows what the readings checked in Settings would
// change, before anything is saved.
func (s *Server) previewReadings(w http.ResponseWriter, r *http.Request, u *store.User) {
	r.ParseForm()
	want, checked := readingsWanted(r)
	changes, err := resolve.PreviewReadings(r.Context(), s.db, u.ID, want)
	switch {
	case errors.Is(err, resolve.ErrNoReading):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	case len(changes) == 0:
		http.Redirect(w, r, "/settings?notice=readings-same#reading", http.StatusSeeOther)
	default:
		s.render(w, http.StatusOK, "readings", readingsPage{
			Page:    Page{Title: "Change how scrobbles are read", Nav: "settings", User: u},
			Changes: changes, On: checked})
	}
}

func (s *Server) saveReadings(w http.ResponseWriter, r *http.Request, u *store.User) {
	r.ParseForm()
	want, _ := readingsWanted(r)
	editID, err := resolve.SetReadings(r.Context(), s.db, u.ID, want)
	switch {
	case errors.Is(err, resolve.ErrNoReading):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	case editID == 0:
		http.Redirect(w, r, "/settings?notice=readings-same#reading", http.StatusSeeOther)
	default:
		s.log.Info("readings changed", "user", u.Name)
		http.Redirect(w, r, fmt.Sprintf("/settings?done=%d", editID), http.StatusSeeOther)
	}
}

func (s *Server) changeRule(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	editID, err := resolve.SetOwnRule(r.Context(), s.db, u.ID, id, r.PostFormValue("action"))
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, resolve.ErrNoReading), errors.Is(err, store.ErrValue):
		s.notFound(w, r)
	case errors.Is(err, store.ErrStale):
		s.settingsError(w, r, u, "Something changed at the same time. Try again.")
	case err != nil:
		s.serverError(w, r, err)
	case editID == 0:
		http.Redirect(w, r, "/settings#reading", http.StatusSeeOther)
	default:
		s.log.Info("rule changed", "user", u.Name, "rule", id, "action", r.PostFormValue("action"))
		http.Redirect(w, r, fmt.Sprintf("/settings?done=%d", editID), http.StatusSeeOther)
	}
}

// artSize is the space a picture's files take.
func (s *Server) artSize(a store.Artwork) int64 {
	var n int64
	for _, size := range append([]int{0}, artwork.Sizes...) {
		if fi, err := os.Stat(s.art.File(a.SHA256, a.Format, size)); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func (s *Server) setFindArtwork(w http.ResponseWriter, r *http.Request, u *store.User) {
	on := r.PostFormValue("on") == "1"
	if err := s.db.SetFindArtwork(r.Context(), u.ID, on); err != nil {
		s.serverError(w, r, err)
		return
	}
	notice := "pictures-on"
	if !on {
		notice = "pictures-off"
	}
	http.Redirect(w, r, "/settings?notice="+notice+"#pictures", http.StatusSeeOther)
}

func (s *Server) setShowOtherNames(w http.ResponseWriter, r *http.Request, u *store.User) {
	on := r.PostFormValue("on") == "1"
	if err := s.db.SetShowOtherNames(r.Context(), u.ID, on); err != nil {
		s.serverError(w, r, err)
		return
	}
	if first, err := s.db.FirstUser(r.Context()); err == nil && first.ID == u.ID {
		s.otherNames.Store(on)
	}
	notice := "names-on"
	if !on {
		notice = "names-off"
	}
	http.Redirect(w, r, "/settings?notice="+notice+"#names", http.StatusSeeOther)
}

// cleanArtwork removes pictures nothing shows, and that no change in the
// log could bring back.
func (s *Server) cleanArtwork(w http.ResponseWriter, r *http.Request, u *store.User) {
	unused, err := s.db.UnusedArtwork(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	for _, a := range unused {
		if err := s.db.DeleteArtwork(r.Context(), a.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
		if err := s.art.Remove(a.SHA256, a.Format); err != nil {
			s.log.Warn("removing a picture", "sha256", a.SHA256, "err", err)
		}
	}
	s.log.Info("unused pictures removed", "user", u.Name, "count", len(unused))
	http.Redirect(w, r, "/settings?notice=pictures-cleaned#pictures", http.StatusSeeOther)
}
