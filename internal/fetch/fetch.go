// Package fetch makes outgoing requests safely: only to allowlisted hosts,
// never to private addresses, with a timeout, a body limit and at most one
// request per second per host. MusicBrainz names use it now and artwork
// will. See docs/architecture.md, "Artwork pipeline".
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	Timeout = 15 * time.Second
	MaxBody = 10 << 20
	perHost = time.Second
)

var (
	ErrHost    = errors.New("host not allowed")
	ErrAddress = errors.New("address not allowed")
	ErrTooBig  = errors.New("response too big")
)

// StatusError is a response that wasn't 200.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return fmt.Sprintf("status %d", e.Code) }

type Client struct {
	hosts map[string]bool
	ua    string
	http  *http.Client
	local bool // tests only: allow loopback and plain http

	mu   sync.Mutex
	next map[string]time.Time     // when each host may be asked again
	pace map[string]time.Duration // hosts that want more than a second between requests
}

// Pace sets how long to wait between requests to one host, for services
// that allow fewer than one a second (iTunes allows about 20 a minute).
func (c *Client) Pace(host string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pace[strings.ToLower(host)] = d
}

// New returns a client for the given hosts. A host written "*.example.org"
// allows example.org and every name under it. userAgent must say who's
// asking, as MusicBrainz requires.
func New(userAgent string, hosts ...string) *Client {
	return newClient(userAgent, false, hosts)
}

// NewLocal is New for tests against a local server.
func NewLocal(userAgent string, hosts ...string) *Client {
	return newClient(userAgent, true, hosts)
}

func newClient(ua string, local bool, hosts []string) *Client {
	c := &Client{hosts: map[string]bool{}, ua: ua, local: local, next: map[string]time.Time{}, pace: map[string]time.Duration{}}
	for _, h := range hosts {
		c.hosts[strings.ToLower(h)] = true
	}
	dialer := &net.Dialer{Timeout: Timeout, Control: c.checkAddress}
	c.http = &http.Client{
		Timeout: Timeout,
		Transport: &http.Transport{
			Proxy:               nil, // the address check has to see the real server
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: Timeout,
			MaxIdleConns:        4,
			IdleConnTimeout:     time.Minute,
		},
		// Redirects are followed only to allowlisted hosts.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return c.checkURL(req.URL)
		},
	}
	return c
}

// checkAddress runs at connect time, after DNS, so a name that resolves to
// a private address (including by DNS rebinding) is refused.
func (c *Client) checkAddress(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ErrAddress
	}
	if c.local && ip.IsLoopback() {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%s: %w", ip, ErrAddress)
	}
	return nil
}

func (c *Client) checkURL(u *url.URL) error {
	if u.Scheme != "https" && !(c.local && u.Scheme == "http") {
		return fmt.Errorf("%s: %w", u.Scheme, ErrHost)
	}
	if !c.allowed(strings.ToLower(u.Hostname())) {
		return fmt.Errorf("%s: %w", u.Hostname(), ErrHost)
	}
	return nil
}

func (c *Client) allowed(host string) bool {
	if c.hosts[host] {
		return true
	}
	for h := host; ; {
		i := strings.IndexByte(h, '.')
		if i < 0 {
			return false
		}
		if c.hosts["*."+h] {
			return true
		}
		h = h[i+1:]
	}
}

// wait holds a request until its host may be asked again.
func (c *Client) wait(ctx context.Context, host string) error {
	c.mu.Lock()
	now := time.Now()
	at := c.next[host]
	if at.Before(now) {
		at = now
	}
	gap := perHost
	if d, ok := c.pace[host]; ok {
		gap = d
	}
	c.next[host] = at.Add(gap)
	c.mu.Unlock()
	select {
	case <-time.After(time.Until(at)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Get fetches a URL and returns the body of a 200 response.
func (c *Client) Get(ctx context.Context, rawURL, accept string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := c.checkURL(u); err != nil {
		return nil, err
	}
	if err := c.wait(ctx, strings.ToLower(u.Hostname())); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBody {
		return nil, ErrTooBig
	}
	return body, nil
}
