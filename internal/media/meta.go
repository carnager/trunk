// Package media inspects photos and videos: type sniffing, metadata and
// thumbnails. It is pure Go; HEIC and AVIF decode through WebAssembly builds
// of the C libraries, and videos use ffmpeg/ffprobe when they are on PATH.
package media

import (
	"bytes"
	"errors"
	"image"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/evanoberholster/imagemeta"

	_ "github.com/gen2brain/avif" // registers image/avif
	_ "github.com/gen2brain/heic" // registers image/heic
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// ErrUnsupported is returned for files that are neither photos nor videos.
var ErrUnsupported = errors.New("unsupported media type")

// Info is everything trunk records about a file. Width and Height are display
// dimensions, i.e. with Orientation already applied, so clients can lay out
// the grid without knowing about EXIF rotation.
type Info struct {
	Kind        string // "image" or "video"
	MIME        string
	TakenAt     time.Time // zero if unknown
	TZOffset    *int      // minutes east of UTC; nil if unknown
	Width       int
	Height      int
	Orientation int // EXIF 1..8, 0 if unknown
	DurationMS  int64
	Lat, Lon    *float64
	Alt         *float64
	Make        string
	Model       string
	Lens        string
	FNumber     float64
	Exposure    float64 // seconds
	ISO         int
	FocalLength float64 // mm
}

// Sniff returns the MIME type and kind of the file at path. name is the
// original file name; its extension is only a tie-breaker.
func Sniff(path, name string) (mime, kind string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	return sniffBytes(head, filepath.Ext(name))
}

func sniffBytes(head []byte, ext string) (string, string, error) {
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		switch brand := string(head[8:12]); brand {
		case "heic", "heix", "hevc", "hevx", "heim", "heis", "mif1", "msf1":
			return "image/heic", "image", nil
		case "avif", "avis":
			return "image/avif", "image", nil
		case "qt  ":
			return "video/quicktime", "video", nil
		default:
			if strings.HasPrefix(brand, "3gp") || strings.HasPrefix(brand, "3g2") {
				return "video/3gpp", "video", nil
			}
			return "video/mp4", "video", nil
		}
	}
	ct := http.DetectContentType(head)
	switch {
	case ct == "image/jpeg", ct == "image/png", ct == "image/gif", ct == "image/webp", ct == "image/bmp":
		return ct, "image", nil
	case ct == "video/webm", ct == "video/avi":
		return ct, "video", nil
	}
	// Matroska shares the EBML magic with WebM; trust the extension.
	if bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "video/x-matroska", "video", nil
	}
	if strings.EqualFold(ext, ".dng") {
		return "image/x-adobe-dng", "image", nil
	}
	return "", "", ErrUnsupported
}

// Extract reads metadata from the file at path; name is its original file
// name, used for the date when the file itself has none. Failing to read
// metadata is not an error: a photo without EXIF is still a photo.
func Extract(path, name string) (Info, error) {
	mime, kind, err := Sniff(path, name)
	if err != nil {
		return Info{}, err
	}
	in := Info{Kind: kind, MIME: mime}
	if kind == "video" {
		probeVideo(path, &in)
	} else {
		extractImage(path, &in)
	}
	if fn, ok := TimeFromFilename(filepath.Base(name)); ok {
		if in.TakenAt.IsZero() {
			in.TakenAt = fn
		} else if in.TZOffset == nil {
			in.TZOffset = offsetFrom(fn, in.TakenAt, in.DurationMS)
		}
	}
	return in, nil
}

// offsetFrom recovers the UTC offset when a file has a UTC timestamp (as
// videos do) and its name holds the local time of the same moment, as phone
// cameras write it: VID_20230610_140418.mp4 with creation_time 12:04:35Z is
// UTC+2. The name usually marks the start of a recording and creation_time
// the end, so the clip's duration is allowed as slack.
func offsetFrom(local, utc time.Time, durationMS int64) *int {
	const step = 15 * time.Minute
	diff := local.Sub(utc)
	q := diff.Round(step)
	slack := time.Duration(durationMS)*time.Millisecond + 2*time.Minute
	if q.Abs() > 14*time.Hour || (diff-q).Abs() > min(slack, 7*time.Minute) {
		return nil
	}
	m := int(q / time.Minute)
	return &m
}

func extractImage(path string, in *Info) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	if x, err := imagemeta.Decode(f); err == nil {
		in.Make = strings.TrimSpace(x.IFD0.Make)
		in.Model = strings.TrimSpace(x.IFD0.Model)
		in.Lens = strings.TrimSpace(x.ExifIFD.LensModel)
		in.Orientation = int(x.IFD0.Orientation)
		in.FNumber = round(float64(x.ExifIFD.FNumber), 2)
		in.Exposure = float64(x.ExifIFD.ExposureTime)
		in.FocalLength = round(float64(x.ExifIFD.FocalLength), 2)
		in.ISO = int(x.ExifIFD.ISOSpeedRatings)
		if d := x.SelectedDate(); !d.IsZero() && d.Year() > 1900 {
			in.TakenAt = d
			if loc := x.ExifIFD.OffsetTimeOriginal; loc != nil {
				_, off := d.In(loc).Zone()
				m := off / 60
				in.TZOffset = &m
			}
		}
		lat, lon := x.GPS.Latitude(), x.GPS.Longitude()
		if (lat != 0 || lon != 0) && math.Abs(lat) <= 90 && math.Abs(lon) <= 180 {
			in.Lat, in.Lon = &lat, &lon
			if a := float64(x.GPS.Altitude()); a != 0 {
				a = round(a, 1)
				in.Alt = &a
			}
		}
		if w, h := x.ExifIFD.PixelXDimension, x.ExifIFD.PixelYDimension; w > 0 && h > 0 {
			in.Width, in.Height = int(w), int(h)
		}
	}

	// The decoder's view of the size beats EXIF, which editors often leave stale.
	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if cfg, _, err := image.DecodeConfig(f); err == nil {
			in.Width, in.Height = cfg.Width, cfg.Height
		}
	}
	if in.Orientation >= 5 && in.Orientation <= 8 {
		in.Width, in.Height = in.Height, in.Width
	}
}

var (
	// IMG_20240131_142530, PXL_20240131_142530123, VID_20240131_142530, Screenshot_20240131-142530
	reCompact = regexp.MustCompile(`(?:^|\D)((?:19|20)\d{2})(\d{2})(\d{2})[_-]?(\d{2})(\d{2})(\d{2})`)
	// signal-2024-01-31-142530, Screenshot 2024-01-31 at 14.25.30, 2024-01-31 14-25-30
	reDashed = regexp.MustCompile(`((?:19|20)\d{2})-(\d{2})-(\d{2})[ _T-](?:at )?(\d{2})[.:-]?(\d{2})[.:-]?(\d{2})`)
)

// TimeFromFilename recognises the timestamps that cameras and messengers put
// in file names. The result is a wall-clock time in UTC with unknown offset.
func TimeFromFilename(name string) (time.Time, bool) {
	for _, re := range []*regexp.Regexp{reCompact, reDashed} {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		var p [6]int
		for i := range p {
			p[i], _ = strconv.Atoi(m[i+1])
		}
		if p[1] < 1 || p[1] > 12 || p[2] < 1 || p[2] > 31 || p[3] > 23 || p[4] > 59 || p[5] > 59 {
			continue
		}
		return time.Date(p[0], time.Month(p[1]), p[2], p[3], p[4], p[5], 0, time.UTC), true
	}
	return time.Time{}, false
}

func round(v float64, places int) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
