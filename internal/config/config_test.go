package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	os.WriteFile(p, []byte(`
data_dir = "`+dir+`"
public_url = "https://music.example.org"
trusted_proxies = ["10.0.0.0/8", "192.168.1.5"]
`), 0o600)
	t.Setenv("CHOKOMINTO_PUBLIC_PAGES", "false")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:3939" || c.PublicPages || !c.HTTPS || len(c.Proxies) != 2 {
		t.Fatalf("%+v", c)
	}
	if c.Proxies[1].String() != "192.168.1.5/32" {
		t.Fatalf("bare address: %s", c.Proxies[1])
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte(`listn = "0.0.0.0:1"`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("typo in config accepted")
	}
}

func TestLoadMissingExplicitFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.toml")); err == nil {
		t.Fatal("missing explicit config accepted")
	}
}
