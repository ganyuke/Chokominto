// Package artwork checks, cleans and stores pictures. Nothing unverified
// is kept: a picture is decoded in full, checked for a sensible size, and
// written out again as a fresh JPEG (or PNG when it has transparency),
// which also drops any metadata. Files are written so that a crash can
// never leave half of one. See docs/architecture.md, "Artwork pipeline".
package artwork

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif" // GIFs too: the first frame
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	MinSide = 64
	MaxSide = 8000
	MaxSize = 10 << 20 // bytes, for uploads and downloads alike
)

// Sizes are the smaller copies kept besides the full picture: 64 px for
// table rows and 440 px for the infobox, both twice the CSS size.
var Sizes = []int{64, 440}

var (
	ErrNotImage = errors.New("not a picture")
	ErrTooSmall = errors.New("picture too small")
	ErrTooBig   = errors.New("picture too big")
)

// Stored is a picture as kept.
type Stored struct {
	SHA256        string // hex, of the full-size file
	Format        string // jpeg or png
	Width, Height int
}

// Ext is the file extension for a format.
func Ext(format string) string {
	if format == "png" {
		return "png"
	}
	return "jpg"
}

// Store keeps pictures in a folder.
type Store struct {
	Dir string
}

// File is the path of one stored size (0 = full size).
func (s Store) File(sha, format string, size int) string {
	name := sha
	if size > 0 {
		name += fmt.Sprintf("-%d", size)
	}
	return filepath.Join(s.Dir, sha[:2], name+"."+Ext(format))
}

// Save checks a picture and stores it with its smaller copies. Saving the
// same picture twice keeps one copy.
func (s Store) Save(data []byte) (Stored, error) {
	if len(data) > MaxSize {
		return Stored{}, ErrTooBig
	}
	// Check the size before decoding, so a small file claiming to be a huge
	// picture never gets decoded.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Stored{}, ErrNotImage
	}
	if cfg.Width < MinSide || cfg.Height < MinSide {
		return Stored{}, ErrTooSmall
	}
	if cfg.Width > MaxSide || cfg.Height > MaxSide {
		return Stored{}, ErrTooBig
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Stored{}, ErrNotImage
	}
	format := "jpeg"
	if hasTransparency(img) {
		format = "png"
	}
	full, err := encode(img, format)
	if err != nil {
		return Stored{}, err
	}
	sum := sha256.Sum256(full)
	st := Stored{SHA256: hex.EncodeToString(sum[:]), Format: format, Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}
	if err := os.MkdirAll(filepath.Join(s.Dir, st.SHA256[:2]), 0o755); err != nil {
		return st, err
	}
	// Smaller copies first, the full picture last: the full file being
	// there means all of it is.
	for _, size := range Sizes {
		b, err := encode(resize(img, size), format)
		if err != nil {
			return st, err
		}
		if err := writeFile(s.File(st.SHA256, format, size), b); err != nil {
			return st, err
		}
	}
	return st, writeFile(s.File(st.SHA256, format, 0), full)
}

// Has reports whether a picture is stored in full.
func (s Store) Has(sha, format string) bool {
	_, err := os.Stat(s.File(sha, format, 0))
	return err == nil
}

// Remove deletes a picture and its copies.
func (s Store) Remove(sha, format string) error {
	for _, size := range append([]int{0}, Sizes...) {
		if err := os.Remove(s.File(sha, format, size)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// FileName matches what's served: <sha>[-size].<ext>.
var FileName = regexp.MustCompile(`^([0-9a-f]{64})(?:-(64|440))?\.(jpg|png)$`)

func hasTransparency(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

func encode(img image.Image, format string) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		// JPEG has no transparency, so draw onto an opaque canvas first.
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), image.White, image.Point{}, draw.Src)
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Over)
		err = jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: 90})
	}
	return buf.Bytes(), err
}

// resize scales a picture so its shorter side is size (never larger than it
// was), keeping its shape. Pages crop it square.
func resize(img image.Image, size int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	short := min(w, h)
	if short <= size {
		return img
	}
	nw, nh := w*size/short, h*size/short
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Over, nil)
	return dst
}

// writeFile writes data to a temp file beside path, syncs it, renames it
// into place and syncs the folder, so path is either whole or absent.
func writeFile(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already stored, and files are never changed
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
