package artwork

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func picture(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 200, alpha})
		}
	}
	return img
}

func encoded(t *testing.T, img image.Image, format string) []byte {
	t.Helper()
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, img)
	case "gif":
		err = gif.Encode(&buf, img, nil)
	default:
		err = jpeg.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSave(t *testing.T) {
	webp, err := os.ReadFile("testdata/blue-purple-pink.lossy.webp")
	if err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: t.TempDir()}
	for _, c := range []struct {
		name   string
		data   []byte
		format string
		err    error
	}{
		{"jpeg", encoded(t, picture(600, 400, 255), "jpeg"), "jpeg", nil},
		{"opaque png", encoded(t, picture(300, 300, 255), "png"), "jpeg", nil},
		{"png with transparency", encoded(t, picture(300, 300, 128), "png"), "png", nil},
		{"gif", encoded(t, picture(100, 100, 255), "gif"), "jpeg", nil},
		{"webp", webp, "jpeg", nil},
		{"too small", encoded(t, picture(40, 400, 255), "png"), "", ErrTooSmall},
		{"not a picture", []byte("<html>404 Not Found</html>"), "", ErrNotImage},
		{"cut off", encoded(t, picture(300, 300, 255), "jpeg")[:200], "", ErrNotImage},
	} {
		st, err := s.Save(c.data)
		if err != c.err {
			t.Errorf("%s: %v, want %v", c.name, err, c.err)
			continue
		}
		if err != nil {
			continue
		}
		if st.Format != c.format {
			t.Errorf("%s stored as %s, want %s", c.name, st.Format, c.format)
		}
		for _, size := range append([]int{0}, Sizes...) {
			f, err := os.Open(s.File(st.SHA256, st.Format, size))
			if err != nil {
				t.Fatalf("%s size %d: %v", c.name, size, err)
			}
			cfg, _, err := image.DecodeConfig(f)
			f.Close()
			if err != nil {
				t.Fatalf("%s size %d doesn't decode: %v", c.name, size, err)
			}
			if size > 0 && min(cfg.Width, cfg.Height) > size {
				t.Errorf("%s size %d is %dx%d", c.name, size, cfg.Width, cfg.Height)
			}
		}
	}
	// No temp files left behind, and saving again keeps one copy.
	before := countFiles(t, s.Dir)
	if _, err := s.Save(encoded(t, picture(600, 400, 255), "jpeg")); err != nil {
		t.Fatal(err)
	}
	if after := countFiles(t, s.Dir); after != before {
		t.Errorf("%d files after saving again, want %d", after, before)
	}
}

func TestHugePictureNotDecoded(t *testing.T) {
	// A PNG header claiming 9000x9000: refused from the header alone.
	var buf bytes.Buffer
	png.Encode(&buf, picture(9000, 1, 255))
	data := buf.Bytes()
	if _, err := (Store{Dir: t.TempDir()}).Save(data); err != ErrTooSmall && err != ErrTooBig {
		t.Fatalf("got %v", err)
	}
	big := bytes.Repeat([]byte{0}, MaxSize+1)
	if _, err := (Store{Dir: t.TempDir()}).Save(big); err != ErrTooBig {
		t.Fatalf("over the size limit: %v", err)
	}
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if strings.HasPrefix(d.Name(), ".tmp-") {
				t.Errorf("temp file left: %s", p)
			}
			n++
		}
		return nil
	})
	return n
}

func TestFileName(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for name, ok := range map[string]bool{
		sha + ".jpg": true, sha + "-64.jpg": true, sha + "-440.png": true,
		sha + "-65.jpg": false, "../" + sha + ".jpg": false, sha + ".gif": false,
	} {
		if FileName.MatchString(name) != ok {
			t.Errorf("%s: %v", name, !ok)
		}
	}
}
