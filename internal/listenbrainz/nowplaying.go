package listenbrainz

import (
	"sync"
	"time"
)

// NowPlaying keeps each user's current track in memory. It's never stored:
// after a restart nothing is playing until the scrobbler says so again.
type NowPlaying struct {
	mu  sync.Mutex
	m   map[int64]Track
	now func() time.Time
}

type Track struct {
	Artist  string
	Title   string
	Album   string
	Expires time.Time
}

const (
	defaultPlaying = 10 * time.Minute
	maxPlaying     = 2 * time.Hour
)

func NewNowPlaying() *NowPlaying {
	return &NowPlaying{m: map[int64]Track{}, now: time.Now}
}

// Set records what's playing. It expires after the track's length when
// known, otherwise after 10 minutes.
func (n *NowPlaying) Set(userID int64, t Track, durationMS int64) {
	d := defaultPlaying
	if durationMS > 0 {
		d = min(time.Duration(durationMS)*time.Millisecond, maxPlaying)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	t.Expires = n.now().Add(d)
	n.m[userID] = t
}

func (n *NowPlaying) Get(userID int64) (Track, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	t, ok := n.m[userID]
	if !ok {
		return Track{}, false
	}
	if !n.now().Before(t.Expires) {
		delete(n.m, userID)
		return Track{}, false
	}
	return t, true
}

// A scrobble doesn't clear now playing: scrobblers submit about halfway
// through a track, while it's still playing. The next playing_now replaces
// it, or it expires.
