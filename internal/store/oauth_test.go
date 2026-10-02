package store

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestStaleOAuthClients(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	user := testUser(t, db)
	for _, id := range []string{"waiting", "connected"} {
		if err := db.CreateOAuthClient(ctx, id, id, []string{"https://claude.ai/cb"}); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := db.OAuthClient(ctx, "connected")
	if _, err := db.ConnectAgent(ctx, OAuthCode{ClientID: c.ID, UserID: user, Access: "look"}, "Claude", []byte("a"), []byte("r"), unix()+3600); err != nil {
		t.Fatal(err)
	}
	// Two days pass.
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE oauth_clients SET created_at = created_at - 2*86400`)
		return err
	})
	if err := db.DeleteStaleOAuth(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.OAuthClient(ctx, "waiting"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("app that never connected kept: %v", err)
	}
	if _, err := db.OAuthClient(ctx, "connected"); err != nil {
		t.Fatalf("connected app removed: %v", err)
	}

	// Anyone can register, so apps waiting to connect are capped.
	var err error
	for i := 0; err == nil; i++ {
		err = db.CreateOAuthClient(ctx, fmt.Sprint(i), "x", []string{"https://x.example/cb"})
		if i > maxWaitingClients {
			t.Fatal("no cap")
		}
	}
	if !errors.Is(err, ErrTooManyClients) {
		t.Fatal(err)
	}
}

func TestAgentTokenExpires(t *testing.T) {
	db := openTest(t)
	ctx := t.Context()
	user := testUser(t, db)
	db.CreateOAuthClient(ctx, "app", "Claude", []string{"https://claude.ai/cb"})
	c, _ := db.OAuthClient(ctx, "app")
	db.ConnectAgent(ctx, OAuthCode{ClientID: c.ID, UserID: user, Access: "change"}, "Claude", []byte("old"), []byte("r"), unix()-1)
	if _, _, _, err := db.AgentTokenUser(ctx, []byte("old")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired token works: %v", err)
	}
	if _, err := db.RefreshAgent(ctx, c.ID, []byte("r"), []byte("new"), []byte("r2"), unix()+3600); err != nil {
		t.Fatal(err)
	}
	if _, _, canChange, err := db.AgentTokenUser(ctx, []byte("new")); err != nil || !canChange {
		t.Fatalf("refreshed token: %v %v", canChange, err)
	}
}
