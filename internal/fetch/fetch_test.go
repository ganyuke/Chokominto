package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hi")) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	// Allowlisted by name, but it's a loopback address: refused at connect
	// time. (Plain http is only allowed for local tests, so ask a real
	// client over https.)
	c := New("test", u.Hostname())
	c.local = false
	if _, err := c.Get(context.Background(), "https://"+u.Host+"/", ""); !errors.Is(err, ErrAddress) {
		t.Fatalf("loopback: %v", err)
	}
	for _, addr := range []string{"10.0.0.1:443", "192.168.1.1:443", "169.254.169.254:80", "[::1]:443", "[fe80::1]:443", "0.0.0.0:80"} {
		if err := c.checkAddress("tcp", addr, nil); !errors.Is(err, ErrAddress) {
			t.Errorf("%s: %v", addr, err)
		}
	}
	if err := c.checkAddress("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
}

func TestAllowlistAndRedirects(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		case "/big":
			w.Write([]byte(strings.Repeat("x", MaxBody+10)))
		case "/missing":
			http.NotFound(w, r)
		default:
			if r.Header.Get("User-Agent") != "Chokominto/test" {
				t.Errorf("user agent %q", r.Header.Get("User-Agent"))
			}
			w.Write([]byte("ok"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := NewLocal("Chokominto/test", u.Hostname())
	ctx := context.Background()

	if b, err := c.Get(ctx, srv.URL+"/", ""); err != nil || string(b) != "ok" {
		t.Fatalf("get: %q %v", b, err)
	}
	if _, err := c.Get(ctx, "http://example.com/", ""); !errors.Is(err, ErrHost) {
		t.Errorf("other host: %v", err)
	}
	if _, err := c.Get(ctx, srv.URL+"/away", ""); !errors.Is(err, ErrHost) {
		t.Errorf("redirect elsewhere: %v", err)
	}
	if _, err := c.Get(ctx, srv.URL+"/big", ""); !errors.Is(err, ErrTooBig) {
		t.Errorf("big body: %v", err)
	}
	var se *StatusError
	if _, err := c.Get(ctx, srv.URL+"/missing", ""); !errors.As(err, &se) || se.Code != 404 {
		t.Errorf("404: %v", err)
	}
}

func TestOneRequestPerSecondPerHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := NewLocal("test", u.Hostname())
	start := time.Now()
	for range 3 {
		if _, err := c.Get(context.Background(), srv.URL, ""); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("3 requests took %v", d)
	}
}

func TestWildcardHosts(t *testing.T) {
	c := New("test", "*.archive.org", "coverartarchive.org")
	for host, ok := range map[string]bool{
		"archive.org": true, "ia800.us.archive.org": true, "coverartarchive.org": true,
		"evilarchive.org": false, "archive.org.evil.com": false, "www.coverartarchive.org": false,
	} {
		if c.allowed(host) != ok {
			t.Errorf("%s: %v", host, !ok)
		}
	}
}
