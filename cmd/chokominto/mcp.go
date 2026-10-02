package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"

	"chokominto/internal/mcp"
	"chokominto/internal/store"
)

// runMCP lets an AI agent help tidy the music, over the Model Context
// Protocol on stdin and stdout. The agent starts this command itself,
// on the server or over ssh. It can run while the server does.
func runMCP(ctx context.Context, args []string) error {
	var userName string
	var readOnly bool
	c, _, err := commonFlags("mcp", args, func(fs *flag.FlagSet) {
		fs.StringVar(&userName, "user", "", "whose music (default: the first account)")
		fs.BoolVar(&readOnly, "read-only", false, "only let the agent look, not change anything")
	})
	if err != nil {
		return err
	}
	db, err := openDB(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	var u store.User
	if userName != "" {
		u, err = db.UserByName(ctx, userName)
	} else {
		u, err = db.FirstUser(ctx)
	}
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("no account yet. Start Chokominto once first")
	}
	if err != nil {
		return err
	}
	// Stdout carries the protocol, so messages go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	s := &mcp.Server{DB: db, UserID: u.ID, Version: version, ReadOnly: readOnly, Log: log}
	return s.Serve(ctx, os.Stdin, os.Stdout)
}
