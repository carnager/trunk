package media

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// exifJPEG builds a JPEG whose EXIF has a date, a camera make and GPS.
func exifJPEG(t *testing.T) []byte {
	le := binary.LittleEndian
	entry := func(b []byte, tag, typ uint16, n, val uint32) []byte {
		b = le.AppendUint16(b, tag)
		b = le.AppendUint16(b, typ)
		b = le.AppendUint32(b, n)
		return le.AppendUint32(b, val)
	}
	// Layout: header 8 | IFD0 (3 entries) 2+36+4=42 → 50 | make "Pixel\0" 6 → 56 |
	// ExifIFD (1 entry) 18 → 74 | date 20 → 94 | GPS IFD (4 entries) 54 → 148 | 2 rationals x3 = 48.
	t8 := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	t8 = le.AppendUint16(t8, 3)
	t8 = entry(t8, 0x10f, 2, 6, 50)
	t8 = entry(t8, 0x8769, 4, 1, 56)
	t8 = entry(t8, 0x8825, 4, 1, 94)
	t8 = le.AppendUint32(t8, 0)
	t8 = append(t8, "Pixel\x00"...)
	t8 = le.AppendUint16(t8, 1)
	t8 = entry(t8, 0x9003, 2, 20, 74)
	t8 = le.AppendUint32(t8, 0)
	t8 = append(t8, "2024:06:01 14:30:00\x00"...)
	t8 = le.AppendUint16(t8, 4)
	t8 = entry(t8, 1, 2, 2, 'N')
	t8 = entry(t8, 2, 5, 3, 148)
	t8 = entry(t8, 3, 2, 2, 'E')
	t8 = entry(t8, 4, 5, 3, 172)
	t8 = le.AppendUint32(t8, 0)
	for _, v := range []uint32{48, 1, 12, 1, 30, 1, 16, 1, 22, 1, 10, 1} {
		t8 = le.AppendUint32(t8, v)
	}
	var img bytes.Buffer
	jpeg.Encode(&img, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil)
	app1 := append([]byte("Exif\x00\x00"), t8...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(app1)+2))
	out := append([]byte{0xFF, 0xD8}, seg...)
	out = append(out, app1...)
	return append(out, img.Bytes()[2:]...)
}

func applyPatches(t *testing.T, path string, patches []Patch) []byte {
	f, _ := os.Open(path)
	defer f.Close()
	b, err := io.ReadAll(NewPatched(f, patches))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStripJPEG(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jpg")
	os.WriteFile(in, exifJPEG(t), 0o644)
	before, _ := Extract(in, "in.jpg")
	if before.Lat == nil {
		t.Fatal("test image has no GPS")
	}
	patches, ok, err := LocationPatches(in, "image/jpeg")
	if err != nil || !ok || len(patches) == 0 {
		t.Fatalf("patches %v ok %v err %v", patches, ok, err)
	}
	orig, _ := os.ReadFile(in)
	cleaned := applyPatches(t, in, patches)
	if len(cleaned) != len(orig) {
		t.Fatal("length changed")
	}
	out := filepath.Join(dir, "out.jpg")
	os.WriteFile(out, cleaned, 0o644)
	after, err := Extract(out, "out.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if after.Lat != nil || after.Lon != nil {
		t.Errorf("location still there: %v %v", *after.Lat, *after.Lon)
	}
	if !after.TakenAt.Equal(before.TakenAt) || after.Make == "" || after.Make != before.Make {
		t.Errorf("lost more than the location: %v %q", after.TakenAt, after.Make)
	}
	if _, err := jpeg.Decode(bytes.NewReader(cleaned)); err != nil {
		t.Errorf("no longer a valid JPEG: %v", err)
	}
	// Reading in pieces, as for Range requests, gives the same bytes.
	f, _ := os.Open(in)
	p := NewPatched(f, patches)
	p.Seek(100, io.SeekStart)
	part := make([]byte, 50)
	io.ReadFull(p, part)
	f.Close()
	if !bytes.Equal(part, cleaned[100:150]) {
		t.Error("ranged read differs")
	}
}

func TestStripMP4(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=10",
		"-pix_fmt", "yuv420p", "-metadata", "location=+48.2085+016.3721/", "-metadata", "creation_time=2024-01-02T03:04:05Z", in).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	before, _ := Extract(in, "in.mp4")
	if before.Lat == nil {
		t.Fatal("test video has no location")
	}
	patches, ok, err := LocationPatches(in, "video/mp4")
	if err != nil || !ok || len(patches) == 0 {
		t.Fatalf("patches %v ok %v err %v", patches, ok, err)
	}
	out := filepath.Join(dir, "out.mp4")
	os.WriteFile(out, applyPatches(t, in, patches), 0o644)
	after, _ := Extract(out, "out.mp4")
	if after.Lat != nil {
		t.Errorf("location still there: %v", *after.Lat)
	}
	if after.DurationMS != before.DurationMS || !after.TakenAt.Equal(before.TakenAt) {
		t.Errorf("video changed: %d vs %d, %v", after.DurationMS, before.DurationMS, after.TakenAt)
	}
	if _, err := videoFrame(out, after.DurationMS, 160); err != nil {
		t.Errorf("video no longer plays: %v", err)
	}
}

func TestStripOtherFormats(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.heic")
	os.WriteFile(p, []byte("whatever"), 0o644)
	if _, ok, _ := LocationPatches(p, "image/heic"); ok {
		t.Error("claims to clean HEIC")
	}
}
