package auth

import (
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := CheckPassword(h, "correct horse"); !ok || err != nil {
		t.Fatalf("right password rejected: %v", err)
	}
	if ok, _ := CheckPassword(h, "wrong horse"); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Fatal("same salt twice")
	}
	if _, err := CheckPassword("$2a$bcrypt", "x"); err == nil {
		t.Fatal("foreign hash accepted")
	}
}

func TestLimiter(t *testing.T) {
	l := NewLoginLimiter()
	base := time.Unix(0, 0)
	l.now = func() time.Time { return base }
	for range 5 {
		if !l.Allowed("1.2.3.4") {
			t.Fatal("blocked too early")
		}
		l.Fail("1.2.3.4")
	}
	if l.Allowed("1.2.3.4") {
		t.Fatal("not blocked after 5 failures")
	}
	if !l.Allowed("5.6.7.8") {
		t.Fatal("other address blocked")
	}
	l.now = func() time.Time { return base.Add(15 * time.Minute) }
	if !l.Allowed("1.2.3.4") {
		t.Fatal("still blocked after the window")
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	cases := []struct {
		remote, xff, want string
	}{
		{"203.0.113.9:5000", "1.1.1.1", "203.0.113.9"},      // untrusted peer, header ignored
		{"127.0.0.1:5000", "1.1.1.1", "1.1.1.1"},            // trusted proxy
		{"127.0.0.1:5000", "6.6.6.6, 1.1.1.1", "1.1.1.1"},   // client-supplied hop ignored
		{"127.0.0.1:5000", "", "127.0.0.1"},                 // no header
		{"127.0.0.1:5000", "1.1.1.1, 127.0.0.1", "1.1.1.1"}, // chained trusted proxies
		{"[::ffff:127.0.0.1]:5000", "2.2.2.2", "2.2.2.2"},   // v4-mapped peer
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r, trusted); got != c.want {
			t.Errorf("%s + %q: got %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestNewPassword(t *testing.T) {
	p := NewPassword()
	if len(p) != 24 || strings.Count(p, "-") != 4 {
		t.Fatalf("password %q", p)
	}
	if strings.ContainsAny(p, "01ilo") || p == NewPassword() {
		t.Fatalf("password %q", p)
	}
}
