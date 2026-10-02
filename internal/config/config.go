// Package config loads the small TOML config file. Every key can be
// overridden with a CHOKOMINTO_<KEY> environment variable. Per-user
// settings live in the database, not here.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const DefaultPath = "/etc/chokominto/config.toml"

type Config struct {
	Listen         string   `toml:"listen"`
	DataDir        string   `toml:"data_dir"`
	PublicURL      string   `toml:"public_url"`
	PublicPages    bool     `toml:"public_pages"`
	TrustedProxies []string `toml:"trusted_proxies"`

	Proxies []netip.Prefix `toml:"-"`
	HTTPS   bool           `toml:"-"` // public_url is https
}

func Defaults() Config {
	return Config{
		Listen:         "127.0.0.1:3939",
		DataDir:        "/var/lib/chokominto",
		PublicPages:    true,
		TrustedProxies: []string{"127.0.0.1/32", "::1/128"},
	}
}

// Load reads path over the defaults. A missing file at the default path is
// fine. A missing file given explicitly is an error.
func Load(path string) (Config, error) {
	c := Defaults()
	explicit := path != ""
	if !explicit {
		path = DefaultPath
	}
	md, err := toml.DecodeFile(path, &c)
	switch {
	case err == nil:
		if und := md.Undecoded(); len(und) > 0 {
			return c, fmt.Errorf("%s: unknown setting %q", path, und[0].String())
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
	default:
		return c, err
	}
	if err := c.applyEnv(os.LookupEnv); err != nil {
		return c, err
	}
	return c, c.finish()
}

func (c *Config) applyEnv(lookup func(string) (string, bool)) error {
	if v, ok := lookup("CHOKOMINTO_LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := lookup("CHOKOMINTO_DATA_DIR"); ok {
		c.DataDir = v
	}
	if v, ok := lookup("CHOKOMINTO_PUBLIC_URL"); ok {
		c.PublicURL = v
	}
	if v, ok := lookup("CHOKOMINTO_PUBLIC_PAGES"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("CHOKOMINTO_PUBLIC_PAGES: %w", err)
		}
		c.PublicPages = b
	}
	if v, ok := lookup("CHOKOMINTO_TRUSTED_PROXIES"); ok {
		c.TrustedProxies = nil
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				c.TrustedProxies = append(c.TrustedProxies, p)
			}
		}
	}
	return nil
}

func (c *Config) finish() error {
	if c.DataDir == "" {
		return errors.New("data_dir is empty")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return err
	}
	c.DataDir = abs
	c.Proxies = nil
	for _, p := range c.TrustedProxies {
		pre, err := netip.ParsePrefix(p)
		if err != nil {
			addr, err2 := netip.ParseAddr(p)
			if err2 != nil {
				return fmt.Errorf("trusted_proxies: %q: %w", p, err)
			}
			pre = netip.PrefixFrom(addr, addr.BitLen())
		}
		c.Proxies = append(c.Proxies, pre.Masked())
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("public_url %q must be an http or https URL", c.PublicURL)
		}
		c.HTTPS = u.Scheme == "https"
	}
	return nil
}

func (c Config) DBPath() string     { return filepath.Join(c.DataDir, "chokominto.db") }
func (c Config) BackupDir() string  { return filepath.Join(c.DataDir, "backups") }
func (c Config) ArtworkDir() string { return filepath.Join(c.DataDir, "artwork") }
