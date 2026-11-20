package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testMP4 makes a one-second MP4 with a location of its own.
func testMP4(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	p := filepath.Join(t.TempDir(), "v.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=10",
		"-pix_fmt", "yuv420p", "-metadata", "location=+48.2085+016.3721/", p).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	b, _ := os.ReadFile(p)
	return b
}

// motionJPEG puts an XMP segment saying how long the video is into a JPEG
// and appends the video, as a Pixel does. format is "motion" or "micro";
// lie adds to the length written in the XMP.
func motionJPEG(jpg, video []byte, format string, lie int) []byte {
	var xmp string
	n := len(video) + lie
	if format == "micro" {
		xmp = fmt.Sprintf(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF><rdf:Description GCamera:MicroVideo="1" GCamera:MicroVideoVersion="1" GCamera:MicroVideoOffset="%d"/></rdf:RDF></x:xmpmeta>`, n)
	} else {
		xmp = fmt.Sprintf(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF><rdf:Description GCamera:MotionPhoto="1">`+
			`<Container:Directory><rdf:Seq><rdf:li rdf:parseType="Resource"><Container:Item Item:Mime="image/jpeg" Item:Semantic="Primary"/></rdf:li>`+
			`<rdf:li rdf:parseType="Resource"><Container:Item Item:Mime="video/mp4" Item:Semantic="MotionPhoto" Item:Length="%d" Item:Padding="0"/></rdf:li>`+
			`</rdf:Seq></Container:Directory></rdf:Description></rdf:RDF></x:xmpmeta>`, n)
	}
	body := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), xmp...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(body)+2))
	out := append([]byte{0xFF, 0xD8}, seg...)
	out = append(out, body...)
	out = append(out, jpg[2:]...)
	return append(out, video...)
}

func TestMotionVideo(t *testing.T) {
	video := testMP4(t)
	jpg := exifJPEG(t)
	dir := t.TempDir()
	for _, format := range []string{"motion", "micro"} {
		p := filepath.Join(dir, format+".jpg")
		os.WriteFile(p, motionJPEG(jpg, video, format, 0), 0o644)
		off, n, ok, err := MotionVideo(p)
		if err != nil || !ok || n != int64(len(video)) {
			t.Fatalf("%s: off %d n %d ok %v err %v", format, off, n, ok, err)
		}
		b, _ := os.ReadFile(p)
		if !bytes.Equal(b[off:off+n], video) {
			t.Errorf("%s: wrong bytes", format)
		}
		in, _ := Extract(p, "PXL_20240601_120000.MP.jpg")
		if in.MotionOffset != off || in.MotionLength != n || in.Kind != "image" {
			t.Errorf("%s: Extract %+v", format, in)
		}

		// Location removal reaches the video too.
		patches, ok, err := LocationPatches(p, "image/jpeg")
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
		cleaned := applyPatches(t, p, patches)
		v := filepath.Join(dir, format+"-video.mp4")
		os.WriteFile(v, cleaned[off:off+n], 0o644)
		if vin, _ := Extract(v, "v.mp4"); vin.Lat != nil {
			t.Errorf("%s: video keeps its location", format)
		}
	}

	// A wrong length (Google Photos keeps the original's in an edit) is not
	// trusted: the video is found by itself.
	stale := filepath.Join(dir, "stale.jpg")
	staleData := motionJPEG(jpg, video, "motion", 100)
	os.WriteFile(stale, staleData, 0o644)
	if off, n, ok, _ := MotionVideo(stale); !ok || n != int64(len(video)) || !bytes.Equal(staleData[off:], video) {
		t.Errorf("stale length: off %d n %d ok %v", off, n, ok)
	}
	// Marked, but the video was dropped (as some exporters do): a still.
	dropped := filepath.Join(dir, "dropped.jpg")
	os.WriteFile(dropped, motionJPEG(jpg, nil, "motion", len(video)), 0o644)
	if _, _, ok, _ := MotionVideo(dropped); ok {
		t.Error("photo without its video taken for a motion photo")
	}
	still := filepath.Join(dir, "still.jpg")
	os.WriteFile(still, jpg, 0o644)
	if _, _, ok, _ := MotionVideo(still); ok {
		t.Error("still taken for a motion photo")
	}
}
