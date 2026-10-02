// Package listenbrainz implements the ListenBrainz API subset that
// scrobblers use, so Chokominto can be used as a custom ListenBrainz server.
package listenbrainz

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"chokominto/internal/ingest"
)

// Limits match ListenBrainz's own.
const (
	MaxBodyBytes   = 10 << 20
	MaxListenBytes = 10240
	MaxImport      = 1000
)

// Submission is a decoded submit-listens request.
type Submission struct {
	Type       string // "single", "import" or "playing_now"
	Listens    []ingest.Listen
	DurationMS int64 // playing_now only, 0 if unknown
}

// DecodeError is a structural problem with a request. Content problems
// (empty fields, odd timestamps) are not errors, see ingest.Store.
type DecodeError struct{ Msg string }

func (e *DecodeError) Error() string { return e.Msg }

func bad(format string, args ...any) error { return &DecodeError{fmt.Sprintf(format, args...)} }

func DecodeSubmit(body []byte) (Submission, error) {
	var req struct {
		ListenType *string            `json:"listen_type"`
		Payload    *[]json.RawMessage `json:"payload"`
	}
	if err := unmarshalObject(body, &req); err != nil {
		return Submission{}, bad("Invalid JSON document submitted.")
	}
	if req.ListenType == nil {
		return Submission{}, bad("JSON document must contain listen_type.")
	}
	if req.Payload == nil {
		return Submission{}, bad("JSON document must contain payload.")
	}
	s := Submission{Type: *req.ListenType}
	payload := *req.Payload
	switch s.Type {
	case "single", "playing_now":
		if len(payload) != 1 {
			return s, bad("JSON document must contain exactly one listen for listen_type %s.", s.Type)
		}
	case "import":
		if len(payload) == 0 {
			return s, bad("JSON document contains no listens.")
		}
		if len(payload) > MaxImport {
			return s, bad("Too many listens. You may not submit more than %d listens at once.", MaxImport)
		}
	default:
		return s, bad("listen_type must be one of single, import or playing_now.")
	}
	for i, raw := range payload {
		l, dur, err := decodeListen(raw, s.Type == "playing_now")
		if err != nil {
			return Submission{}, bad("Listen %d: %s", i+1, err.Error())
		}
		s.Listens = append(s.Listens, l)
		s.DurationMS = dur
	}
	return s, nil
}

func decodeListen(raw json.RawMessage, playingNow bool) (ingest.Listen, int64, error) {
	if len(raw) > MaxListenBytes {
		return ingest.Listen{}, 0, fmt.Errorf("listen is larger than %d bytes", MaxListenBytes)
	}
	var obj map[string]json.RawMessage
	if err := unmarshalObject(raw, &obj); err != nil {
		return ingest.Listen{}, 0, errors.New("listen must be a JSON object")
	}
	l := ingest.Listen{Payload: bytes.Clone(raw)}
	if !playingNow {
		if v, ok := obj["listened_at"]; ok && !isNull(v) {
			t, err := decodeTime(v)
			if err != nil {
				return l, 0, err
			}
			l.ListenedAt, l.HasTime = t, true
		}
	}
	tm, ok := obj["track_metadata"]
	if !ok || isNull(tm) {
		return l, 0, nil
	}
	var meta map[string]json.RawMessage
	if err := unmarshalObject(tm, &meta); err != nil {
		return l, 0, errors.New("track_metadata must be an object")
	}
	var err error
	if l.Artist, err = optString(meta, "artist_name"); err != nil {
		return l, 0, err
	}
	if l.Title, err = optString(meta, "track_name"); err != nil {
		return l, 0, err
	}
	if l.Album, err = optString(meta, "release_name"); err != nil {
		return l, 0, err
	}
	var dur int64
	if ai, ok := meta["additional_info"]; ok && !isNull(ai) {
		var info map[string]json.RawMessage
		if err := unmarshalObject(ai, &info); err != nil {
			return l, 0, errors.New("additional_info must be an object")
		}
		// Web Scrobbler sends the album's artist when it differs from the
		// track's. Like durations, a value of the wrong type is ignored
		// rather than failing the listen.
		if v, ok := info["release_artist_name"]; ok {
			json.Unmarshal(v, &l.AlbumArtist)
		}
		// Pano Scrobbler sends duration_ms, Web Scrobbler sends duration in
		// seconds. Only used to expire now playing, so bad values are ignored.
		if ms, ok := optNumber(info, "duration_ms"); ok {
			dur = int64(ms)
		} else if s, ok := optNumber(info, "duration"); ok {
			dur = int64(s * 1000)
		}
	}
	return l, dur, nil
}

// unmarshalObject decodes JSON that must be an object, keeping numbers exact.
func unmarshalObject(b []byte, v any) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return errors.New("not an object")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

func isNull(v json.RawMessage) bool { return string(bytes.TrimSpace(v)) == "null" }

// decodeTime accepts an integer, an integral float, or a string holding one.
func decodeTime(v json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(v))
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(v, &s); err != nil {
			return 0, errors.New("listened_at must be a number")
		}
		s = strings.TrimSpace(s)
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f > math.MaxInt64 || f < math.MinInt64 {
		return 0, errors.New("listened_at must be a number")
	}
	return int64(f), nil
}

func optString(m map[string]json.RawMessage, key string) (string, error) {
	v, ok := m[key]
	if !ok || isNull(v) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return s, nil
}

func optNumber(m map[string]json.RawMessage, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	var n json.Number
	s := strings.Trim(strings.TrimSpace(string(v)), `"`)
	n = json.Number(s)
	f, err := n.Float64()
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}
