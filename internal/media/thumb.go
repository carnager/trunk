package media

import (
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

// Size names a thumbnail rendition.
type Size string

const (
	Small   Size = "small"   // grid: short side 300 px
	Medium  Size = "medium"  // dense grid on big screens: short side 480 px
	Preview Size = "preview" // viewer: long side 2048 px
)

// Sizes lists every rendition, largest first (each is derived from the previous).
var Sizes = []Size{Preview, Medium, Small}

// ValidSize reports whether s names a rendition.
func ValidSize(s string) bool {
	return s == string(Small) || s == string(Medium) || s == string(Preview)
}

const previewMax = 2048

// targetSize returns the output size for a source of w×h.
func targetSize(s Size, w, h int) (int, int) {
	var scale float64
	switch s {
	case Preview:
		scale = float64(previewMax) / float64(max(w, h))
	case Medium:
		scale = 480 / float64(min(w, h))
	default:
		scale = 300 / float64(min(w, h))
	}
	if scale >= 1 {
		return w, h // never upscale
	}
	return max(1, int(float64(w)*scale+0.5)), max(1, int(float64(h)*scale+0.5))
}

// Source describes the original a set of thumbnails is rendered from.
type Source struct {
	Path        string
	Kind        string
	Orientation int // EXIF orientation of the original; ignored for video
	DurationMS  int64
}

// Render produces every size for src as JPEG and writes each to out(size).
// All renditions come out upright, so clients show them as they are. They are
// derived from one another, so the original is decoded and rotated only once.
func Render(src Source, out func(Size) string) error {
	var img image.Image
	var err error
	if src.Kind == "video" {
		img, err = videoFrame(src.Path, src.DurationMS, previewMax) // ffmpeg rotates
	} else {
		img, err = decodeFile(src.Path)
	}
	if err != nil {
		return err
	}
	for i, s := range Sizes {
		img = scale(img, s)
		if i == 0 && src.Kind != "video" {
			// Rotate after the first downscale: far fewer pixels to move.
			img = orient(img, src.Orientation)
		}
		if err := writeJPEG(out(s), img); err != nil {
			return err
		}
	}
	return nil
}

// scale fits img to rendition s; target sizes depend only on the short and
// long side, so it is the same before and after a 90° rotation.
func scale(img image.Image, s Size) image.Image {
	b := img.Bounds()
	w, h := targetSize(s, b.Dx(), b.Dy())
	if w == b.Dx() && h == b.Dy() {
		return img
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}

func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return img, nil
}

func writeJPEG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".thumb-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := jpeg.Encode(tmp, img, &jpeg.Options{Quality: 82}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// orient applies an EXIF orientation (1..8) to img.
func orient(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // rotate 90 cw
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 90 ccw
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
