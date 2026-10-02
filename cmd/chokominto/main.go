// Command chokominto is a self-hosted music scrobble server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"slices"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // the Pi may not have system time zone data

	"chokominto/internal/auth"
	"chokominto/internal/config"
	"chokominto/internal/store"
)

// version is set at build time by the Makefile.
var version = "dev"

const usage = `Usage: chokominto <command> [options]

Commands:
  serve                          Run the server
  user add <name>                Add another account and show its password
  user passwd <name>             Give an account a new password
  token add <user> <label>       Create a scrobbler token (shown once)
  token list <user>              List scrobbler tokens
  token revoke <user> <id>       Stop a token from working
  import maloja --db <file>      Import listens from a Maloja database
  backup                         Save a backup now
  mcp [-read-only]               Let an AI agent help tidy your music (see README)
  version                        Show the version

Every command takes -config <file> (default ` + config.DefaultPath + `).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := splitCommand(os.Args[1:]); cmd {
	case "serve":
		err = runServe(ctx, args)
	case "user":
		err = runUser(ctx, args)
	case "token":
		err = runToken(ctx, args)
	case "import":
		err = runImport(ctx, args)
	case "backup":
		err = runBackup(ctx, args)
	case "mcp":
		err = runMCP(ctx, args)
	case "version", "--version":
		fmt.Println("chokominto", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "":
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q.\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// splitCommand finds the command word, allowing -config in front of it
// ("chokominto -config x user passwd ruby"). The options before it are
// handed to the command with the rest.
func splitCommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-config" || a == "--config" {
			i++ // its value
			continue
		}
		if strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") {
			continue
		}
		return a, append(slices.Clone(args[:i]), args[i+1:]...)
	}
	return "", args
}

// commonFlags parses -config and returns the loaded config plus the
// remaining positional arguments. Options may come before or after the
// positional arguments ("user passwd ruby -config x" works), and "--" ends
// the options.
func commonFlags(name string, args []string, extra func(*flag.FlagSet)) (config.Config, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	path := fs.String("config", "", "config file")
	if extra != nil {
		extra(fs)
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return config.Config{}, nil, err
		}
		// flag stops at the first non-option, or just after "--".
		rest := fs.Args()
		consumed := args[:len(args)-len(rest)]
		if len(rest) == 0 || len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			positional = append(positional, rest...)
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	c, err := config.Load(*path)
	return c, positional, err
}

// noArgs refuses words a command doesn't take, so a mistyped option
// (a path without -config, say) isn't quietly ignored.
func noArgs(name string, rest []string) error {
	if len(rest) > 0 {
		return fmt.Errorf("%s doesn't take %q. Options start with -, for example -config <file>", name, rest[0])
	}
	return nil
}

func openDB(ctx context.Context, c config.Config) (*store.DB, error) {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(ctx, c.DBPath(), c.BackupDir())
}

func runUser(ctx context.Context, args []string) error {
	c, rest, err := commonFlags("user", args, nil)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("user needs a subcommand: add or passwd")
	}
	sub, rest := rest[0], rest[1:]
	if sub != "add" && sub != "passwd" {
		return fmt.Errorf("unknown user command %q. Use add or passwd", sub)
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: chokominto user %s <name>", sub)
	}
	name := rest[0]
	db, err := openDB(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()

	switch sub {
	case "add":
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/?#% ") {
			return errors.New("names can't be empty or contain spaces, /, ?, # or %")
		}
		if _, err := db.UserByName(ctx, name); err == nil {
			return fmt.Errorf("%s already exists", name)
		}
		password, hash, err := newPassword()
		if err != nil {
			return err
		}
		if _, err := db.CreateUser(ctx, name, hash); err != nil {
			return err
		}
		fmt.Printf("Created %s. Your password is:\n\n  %s\n\nLog in with it, and change it in Settings if you like.\n", name, password)
	case "passwd":
		u, err := db.UserByName(ctx, name)
		if err != nil {
			return fmt.Errorf("no account named %s", name)
		}
		password, hash, err := newPassword()
		if err != nil {
			return err
		}
		if err := db.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		fmt.Printf("New password for %s:\n\n  %s\n\nEveryone logged in as %s has been logged out.\n", name, password, name)
	default:
		return fmt.Errorf("unknown user command %q", sub)
	}
	return nil
}

// firstAccountName is the account made on first start.
const firstAccountName = "ruby"

// firstAccount makes the account on first start, when there's none yet.
// CHOKOMINTO_PASSWORD sets its password (for Docker or a service manager).
// Otherwise one is made up and returned, so the caller can show it once.
func firstAccount(ctx context.Context, db *store.DB) (u store.User, password string, created bool, err error) {
	u, err = db.FirstUser(ctx)
	if !errors.Is(err, store.ErrNotFound) {
		return u, "", false, err
	}
	password = os.Getenv("CHOKOMINTO_PASSWORD")
	hash := ""
	if password != "" {
		hash, err = auth.HashPassword(password)
		password = ""
	} else {
		password, hash, err = newPassword()
	}
	if err != nil {
		return u, "", false, fmt.Errorf("CHOKOMINTO_PASSWORD: %w", err)
	}
	if _, err = db.CreateUser(ctx, firstAccountName, hash); err != nil {
		return u, "", false, err
	}
	u, err = db.FirstUser(ctx)
	return u, password, true, err
}

// newPassword makes up a password, so setting one up never needs typing
// at a prompt. The owner can change it in Settings.
func newPassword() (password, hash string, err error) {
	password = auth.NewPassword()
	hash, err = auth.HashPassword(password)
	return password, hash, err
}

func runToken(ctx context.Context, args []string) error {
	c, rest, err := commonFlags("token", args, nil)
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("token needs a subcommand: add, list or revoke")
	}
	sub, rest := rest[0], rest[1:]
	if sub != "add" && sub != "list" && sub != "revoke" {
		return fmt.Errorf("unknown token command %q. Use add, list or revoke", sub)
	}
	if len(rest) < 1 {
		return fmt.Errorf("usage: chokominto token %s <user> ...", sub)
	}
	db, err := openDB(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	u, err := db.UserByName(ctx, rest[0])
	if err != nil {
		return fmt.Errorf("no account named %s", rest[0])
	}

	switch sub {
	case "add":
		if len(rest) != 2 || strings.TrimSpace(rest[1]) == "" {
			return errors.New(`usage: chokominto token add <user> <label>, for example "Pano Scrobbler"`)
		}
		secret := auth.NewSecret()
		if _, err := db.CreateToken(ctx, u.ID, strings.TrimSpace(rest[1]), auth.HashSecret(secret)); err != nil {
			return err
		}
		fmt.Printf("Token for %s:\n\n  %s\n\nPaste it into your scrobbler now. It won't be shown again.\n", rest[1], secret)
	case "list":
		ts, err := db.Tokens(ctx, u.ID)
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			fmt.Println("No tokens yet.")
		}
		for _, t := range ts {
			status := "never used"
			if t.LastUsedAt.Valid {
				status = "last used " + time.Unix(t.LastUsedAt.Int64, 0).Format("2 Jan 2006, 15:04")
			}
			if t.RevokedAt.Valid {
				status = "revoked"
			}
			fmt.Printf("%4d  %-30s  %s\n", t.ID, t.Label, status)
		}
	case "revoke":
		if len(rest) != 2 {
			return errors.New("usage: chokominto token revoke <user> <id>")
		}
		id, err := strconv.ParseInt(rest[1], 10, 64)
		if err != nil {
			return fmt.Errorf("%q isn't a token number. See chokominto token list", rest[1])
		}
		if err := db.RevokeToken(ctx, u.ID, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no working token %d for %s", id, u.Name)
			}
			return err
		}
		fmt.Println("Token revoked. That scrobbler can't send listens anymore.")
	default:
		return fmt.Errorf("unknown token command %q", sub)
	}
	return nil
}

func runBackup(ctx context.Context, args []string) error {
	c, rest, err := commonFlags("backup", args, nil)
	if err != nil {
		return err
	}
	if err := noArgs("backup", rest); err != nil {
		return err
	}
	db, err := openDB(ctx, c)
	if err != nil {
		return err
	}
	defer db.Close()
	path, err := db.Backup(ctx, c.BackupDir(), time.Now().Format("2006-01-02"))
	if err != nil {
		return err
	}
	fmt.Println("Saved", path)
	return nil
}
