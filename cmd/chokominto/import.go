package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"chokominto/internal/maloja"
	"chokominto/internal/store"
)

func runImport(ctx context.Context, args []string) error {
	var dbPath, keysPath, userName string
	c, rest, err := commonFlags("import maloja", args, func(fs *flag.FlagSet) {
		fs.StringVar(&dbPath, "db", "", "Maloja's malojadb.sqlite")
		fs.StringVar(&keysPath, "apikeys", "", "Maloja's apikeys.yml, to keep your scrobblers' keys working")
		fs.StringVar(&userName, "user", "", "account to import into (default: the first account)")
	})
	if err != nil {
		return err
	}
	if len(rest) != 1 || rest[0] != "maloja" {
		return errors.New("usage: chokominto import maloja --db <malojadb.sqlite> [--apikeys <apikeys.yml>] [--user <name>]")
	}
	if dbPath == "" && keysPath == "" {
		return errors.New("give --db, --apikeys, or both")
	}
	db, err := openDB(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()

	var u store.User
	if userName != "" {
		u, err = db.UserByName(ctx, userName)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no account named %s", userName)
		}
	} else {
		var password string
		var created bool
		u, password, created, err = firstAccount(ctx, db)
		if created && password != "" {
			fmt.Printf("Created your account. Log in as %s with the password %s and change it in Settings if you like.\n", u.Name, password)
		} else if created {
			fmt.Printf("Created your account %s with the password you set.\n", u.Name)
		}
	}
	if err != nil {
		return err
	}

	p := message.NewPrinter(language.English)
	if dbPath != "" {
		p.Printf("Importing into %s. This is safe to run again, listens already here are skipped.\n", u.Name)
		res, err := maloja.Import(ctx, db, u.ID, dbPath, func(done, total int) {
			p.Printf("\r%d of %d listens", done, total)
		})
		fmt.Println()
		if err != nil {
			return err
		}
		p.Printf("Added %s. %s already here.\n", count(p, res.Stored, "listen"), wasWere(p, res.Duplicates))
		if res.Reconstructed > 0 && res.Stored > 0 {
			p.Printf("For %d of them Maloja hadn't kept what your scrobbler sent, so Maloja's cleaned-up artist and title were used instead.\n", res.Reconstructed)
		}
	}
	if keysPath != "" {
		res, err := maloja.ImportAPIKeys(ctx, db, u.ID, keysPath)
		if err != nil {
			return err
		}
		p.Printf("Added %s from Maloja. %s already here. Your scrobblers can keep using their keys.\n", count(p, res.Added, "scrobbler token"), wasWere(p, res.Skipped))
	}
	return nil
}

func count(p *message.Printer, n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return p.Sprintf("%d %ss", n, noun)
}

func wasWere(p *message.Printer, n int) string {
	if n == 1 {
		return "1 was"
	}
	return p.Sprintf("%d were", n)
}
