package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"chokominto/internal/artwork"
	"chokominto/internal/config"
	"chokominto/internal/fetch"
	"chokominto/internal/jobs"
	"chokominto/internal/listenbrainz"
	"chokominto/internal/resolve"
	"chokominto/internal/store"
	"chokominto/internal/web"
)

// Daily backups kept.
const keepBackups = 14

func runServe(ctx context.Context, args []string) error {
	var debug bool
	c, rest, err := commonFlags("serve", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&debug, "debug", false, "log every request")
	})
	if err != nil {
		return err
	}
	if err := noArgs("serve", rest); err != nil {
		return err
	}
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	log := slog.New(handler)

	update, err := store.NeedsUpdate(ctx, c.DBPath())
	if err != nil {
		return err
	}
	start := time.Now()
	if update {
		log.Info("Saving a backup and updating your data for this version. This can take a few minutes. Scrobblers try again later on their own.")
	}
	db, err := openDB(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	if update {
		log.Info("Update done", "took", time.Since(start).Round(time.Second))
	}
	if u, password, created, err := firstAccount(ctx, db); err != nil {
		return err
	} else if created && password != "" {
		log.Info("Created your account. Log in with this name and password, and change the password in Settings if you like.", "name", u.Name, "password", password)
	} else if created {
		log.Info("Created your account with the password you set.", "name", u.Name)
	}

	web.Version = version
	site, err := web.New(c, db, listenbrainz.NewNowPlaying(), log)
	if err != nil {
		return err
	}
	hs := &http.Server{
		Addr:              c.Listen,
		Handler:           site,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(handler, slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	log.Info("Chokominto is running", "address", "http://"+ln.Addr().String(), "data", c.DataDir)

	go maintain(ctx, db, c, log)

	// Background work: linking new listens to songs, artists and albums.
	resolver := &resolve.Resolver{DB: db}
	runner := &jobs.Runner{DB: db, Workers: 1, Batch: 32, Log: log, Handlers: map[string]jobs.Handler{"resolve": resolver.Job, "reparse": resolver.ReparseJob, "guess_keys": db.GuessKeysJob, "suggest": db.SuggestJob}}
	// Pictures are looked up by a runner of their own: each lookup waits
	// on other sites, and linking new listens shouldn't wait on it.
	ua := "Chokominto/" + version
	if c.PublicURL != "" {
		ua += " ( " + c.PublicURL + " )"
	}
	finder := &artwork.Finder{DB: db, Store: artwork.Store{Dir: c.ArtworkDir()}, Fetch: fetch.New(ua, artwork.Hosts...), Sources: artwork.Live}
	artwork.PaceLive(finder.Fetch)
	pictures := &jobs.Runner{DB: db, Workers: 1, Log: log, Handlers: map[string]jobs.Handler{"artwork": finder.Job}}
	jobsCtx, stopJobs := context.WithCancel(ctx)
	jobsDone := make(chan struct{})
	go func() { runner.Run(jobsCtx); close(jobsDone) }()
	picturesDone := make(chan struct{})
	go func() { pictures.Run(jobsCtx); close(picturesDone) }()
	// Runs before the database closes (defers run last-in, first-out).
	defer func() { stopJobs(); <-jobsDone; <-picturesDone }()

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// maintain takes a backup once a day (and right away if today's is
// missing), prunes old backups and clears expired sessions and agent sign-ins.
func maintain(ctx context.Context, db *store.DB, c config.Config, log *slog.Logger) {
	run := func() {
		today := time.Now().Format("2006-01-02")
		if _, err := os.Stat(filepath.Join(c.BackupDir(), fmt.Sprintf("chokominto-%s.db", today))); errors.Is(err, os.ErrNotExist) {
			if path, err := db.Backup(ctx, c.BackupDir(), today); err != nil {
				log.Error("backup failed", "err", err)
			} else {
				log.Info("backup saved", "file", path)
			}
			if err := store.PruneBackups(c.BackupDir(), keepBackups); err != nil {
				log.Error("removing old backups failed", "err", err)
			}
		}
		if err := db.DeleteExpiredSessions(ctx); err != nil && ctx.Err() == nil {
			log.Error("clearing expired sessions failed", "err", err)
		}
		if err := db.DeleteStaleOAuth(ctx); err != nil && ctx.Err() == nil {
			log.Error("clearing unused agent sign-ins failed", "err", err)
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
