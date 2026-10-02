package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCommonFlagsAnyOrder(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("data_dir = \""+dir+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"ruby", "-config", cfg}, []string{"ruby"}},
		{[]string{"-config", cfg, "ruby"}, []string{"ruby"}},
		{[]string{"ruby", "-config", cfg, "Pano Scrobbler"}, []string{"ruby", "Pano Scrobbler"}},
		{[]string{"-config", cfg, "--", "-odd-name"}, []string{"-odd-name"}},
		{[]string{"-config", cfg}, nil},
	}
	for _, c := range cases {
		conf, rest, err := commonFlags("test", c.args, nil)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if !slices.Equal(rest, c.want) {
			t.Errorf("%q: got %q, want %q", c.args, rest, c.want)
		}
		if conf.DataDir != dir {
			t.Errorf("%q: config not read, data_dir = %q", c.args, conf.DataDir)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		args []string
		cmd  string
		rest []string
	}{
		{[]string{"user", "passwd", "ruby"}, "user", []string{"passwd", "ruby"}},
		{[]string{"-config", "c.toml", "user", "passwd", "ruby"}, "user", []string{"-config", "c.toml", "passwd", "ruby"}},
		{[]string{"--config=c.toml", "backup"}, "backup", []string{"--config=c.toml"}},
		{[]string{"-config", "c.toml"}, "", []string{"-config", "c.toml"}},
		{[]string{"--help"}, "--help", []string{}},
	}
	for _, c := range cases {
		cmd, rest := splitCommand(c.args)
		if cmd != c.cmd || !slices.Equal(rest, c.rest) {
			t.Errorf("%q: got %q %q, want %q %q", c.args, cmd, rest, c.cmd, c.rest)
		}
	}
}
