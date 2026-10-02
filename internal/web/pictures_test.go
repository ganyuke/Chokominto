package web

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"chokominto/internal/artwork"
	"chokominto/internal/fetch"
	"chokominto/internal/store"
)

func jpegBytes(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 99, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// upload posts a picture file the way a browser on this site would.
func (e *env) upload(path string, data []byte) (int, http.Header) {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("picture", "cover.jpg")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func TestPictures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, true)
	e.scrobbleNow([3]string{"YOASOBI", "アイドル", "THE BOOK 3"})
	var album int64
	e.db.Reader().QueryRow(`SELECT id FROM releases`).Scan(&album)
	path := fmt.Sprintf("/album/%d", album)
	e.login()

	_, body, _ := e.get(path)
	if !strings.Contains(body, `<span class="cover no-art"></span>`) {
		t.Fatal("no empty cover")
	}
	if _, body, _ := e.get(path + "/edit"); !strings.Contains(body, `id="picture"`) {
		t.Fatal("no Picture part in the Edit view")
	}

	// Not a picture, then a real one.
	code, h := e.upload(path+"/picture", []byte("<html>"))
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "err=picture-bad") {
		t.Fatalf("bad upload: %d %s", code, h.Get("Location"))
	}
	code, h = e.upload(path+"/picture", jpegBytes(t, 500, 500))
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "done=") {
		t.Fatalf("upload: %d %s", code, h.Get("Location"))
	}
	_, body, _ = e.get(h.Get("Location"))
	cover := regexp.MustCompile(`<img class="cover" src="(/art/[0-9a-f]{64}-440\.jpg)"`).FindStringSubmatch(body)
	if cover == nil || !strings.Contains(body, "You chose this one") {
		t.Fatalf("cover not shown:\n%s", body)
	}
	if code, _, h := e.get(cover[1]); code != 200 || h.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("cover file: %d", code)
	}
	_, body, _ = e.get("/history")
	if !regexp.MustCompile(`<td class="thumb"><img src="/art/[0-9a-f]{64}-64\.jpg"`).MatchString(body) {
		t.Fatal("no thumbnail in History")
	}
	_, body, _ = e.get("/top/albums?period=all")
	if !strings.Contains(body, `-64.jpg"`) {
		t.Fatal("no thumbnail in Top albums")
	}

	// Undo the upload: no picture again, and the file counts as unused only
	// once nothing in the log could bring it back.
	undo := regexp.MustCompile(`action="/changes/(\d+)/undo"`).FindStringSubmatch(func() string { _, b, _ := e.get(h.Get("Location")); return b }())
	if undo == nil {
		t.Fatal("no undo")
	}
	e.post("/changes/"+undo[1]+"/undo", url.Values{"back": {path + "/edit"}})
	if _, body, _ := e.get(path); !strings.Contains(body, `cover no-art`) {
		t.Fatal("undo kept the picture")
	}

	// Candidates: the thumbnail comes through this site, and choosing one
	// downloads and shows it.
	img := jpegBytes(t, 300, 300)
	pics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(img) }))
	defer pics.Close()
	host, _ := url.Parse(pics.URL)
	srv := e.srv.Config.Handler.(*Server)
	srv.SetFinder(&artwork.Finder{DB: e.db, Store: srv.art, Fetch: fetch.NewLocal("test", host.Hostname())})
	e.db.AddCandidates(ctx, "release", album, []store.Candidate{{Origin: "itunes", URL: pics.URL + "/big.jpg", Thumb: pics.URL + "/small.jpg", Title: "THE BOOK 3", Artist: "YOASOBI"}})
	_, body, _ = e.get(path + "/edit")
	cand := regexp.MustCompile(`<img src="/art/candidate/(\d+)"`).FindStringSubmatch(body)
	if cand == nil {
		t.Fatal("candidate not shown")
	}
	if code, b, _ := e.get("/art/candidate/" + cand[1]); code != 200 || len(b) == 0 {
		t.Fatalf("candidate thumbnail: %d", code)
	}
	code, _, h = e.post(path+"/edit", url.Values{"do": {"picture-choose"}, "candidate": {cand[1]}})
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "done=") {
		t.Fatalf("choose: %d %s", code, h.Get("Location"))
	}
	if a, chosen, _ := e.db.ItemArtwork(ctx, "release", album); a == nil || !chosen || a.Origin != "itunes" {
		t.Fatalf("chosen picture %+v %v", a, chosen)
	}
	if cs, _ := e.db.Candidates(ctx, "release", album); len(cs) != 0 {
		t.Fatal("candidates kept after choosing")
	}

	// Settings: switching lookups off.
	e.post("/settings/pictures", url.Values{"on": {"0"}})
	if _, body, _ := e.get("/settings"); !strings.Contains(body, "Look for pictures online") {
		t.Fatal("not switched off")
	}
}
