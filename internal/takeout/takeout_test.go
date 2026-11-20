package takeout

import (
	"bytes"
	"io"
	"testing"
)

func sc(title string) *Sidecar {
	s := &Sidecar{Title: title}
	s.PhotoTakenTime = &struct {
		Timestamp string `json:"timestamp"`
	}{"1690000000"}
	return s
}

func TestMatchSidecar(t *testing.T) {
	long := "Screenshot_20230717-143721_Some Very Long App Name.png"
	sidecars := map[string]*Sidecar{
		"IMG_0001.jpg":                                  sc("old style"),
		"IMG_0002.jpg.supplemental-metadata":            sc("new style"),
		"IMG_0003.jpg(1)":                               sc("old dup"),
		"IMG_0003.jpg":                                  sc("old dup original"),
		"IMG_0004.jpg.supplemental-metadata(1)":         sc("new dup"),
		"Screenshot_20230717-143721_Some Very Long App": sc("truncated"),
		"IMG_0005.HEIC.supplemental-metadata":           sc("live still"),
		"weird name.jpg.supplemental-me":                sc("short truncation"),
		"a.jpg.supplemental-metadata":                   sc("a.jpg"),
		"something-else.json-ish":                       sc("by title.jpg"),
		"Screenshot_2023":                               sc("too short"),
	}
	cases := map[string]string{
		"IMG_0001.jpg":                     "old style",
		"IMG_0002.jpg":                     "new style",
		"IMG_0003(1).jpg":                  "old dup",
		"IMG_0003.jpg":                     "old dup original",
		"IMG_0004(1).jpg":                  "new dup",
		long:                               "truncated",
		"IMG_0005.MP4":                     "live still",
		"weird name.jpg":                   "short truncation",
		"Screenshot_2023-07-17_143721.png": "", // a short prefix cut inside the name would be a guess
		"ab.jpg":                           "", // "a.jpg" must not match a longer name
		"by title.jpg":                     "by title.jpg",
		"IMG_0004.jpg":                     "", // only the (1) copy has a sidecar
		"nothing-matches.jpeg":             "",
	}
	for name, want := range cases {
		got := MatchSidecar(name, sidecars)
		title := ""
		if got != nil {
			title = got.Title
		}
		if title != want {
			t.Errorf("%s: matched %q, want %q", name, title, want)
		}
	}
}

func TestEditedOriginal(t *testing.T) {
	for name, want := range map[string]string{
		"IMG_1234-edited.jpg":     "IMG_1234.jpg",
		"IMG_1234-bearbeitet.jpg": "IMG_1234.jpg",
		"PXL_1-EDITED.JPG":        "PXL_1.JPG",
		"edited.jpg":              "",
		"IMG_1234.jpg":            "",
	} {
		got, ok := editedOriginal(name)
		if !ok {
			got = ""
		}
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func entry(path, content string) Entry {
	return Entry{Path: path, Size: int64(len(content)), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte(content))), nil
	}}
}

func TestNewPlan(t *testing.T) {
	side := `{"title":"x","photoTakenTime":{"timestamp":"1690000000"},"geoData":{"latitude":52.5,"longitude":13.4},"favorited":true}`
	p := NewPlan([]Entry{
		entry("Takeout/Google Photos/Photos from 2023/IMG_1.jpg", "1"),
		entry("Takeout/Google Photos/Photos from 2023/IMG_1.jpg.supplemental-metadata.json", side),
		entry("Takeout/Google Photos/Photos from 2023/IMG_1-bearbeitet.jpg", "1e"),
		entry("Takeout/Google Photos/Photos from 2023/IMG_2.HEIC", "2"),
		entry("Takeout/Google Photos/Photos from 2023/IMG_2-edited.jpg", "2e"),
		entry("Takeout/Google Photos/Photos from 2023/IMG_3-edited.jpg", "3e"), // original missing
		entry("Takeout/Google Photos/Urlaub/IMG_1.jpg", "1"),
		entry("Takeout/Google Photos/Urlaub/Metadaten.json", `{"title":"Urlaub am Meer","date":{"timestamp":"1"}}`),
		entry("Takeout/Google Photos/Papierkorb/IMG_9.jpg", "9"),
		entry("Takeout/Google Photos/Photos from 2023/notes.txt", "?"),
		entry("Takeout/archive_browser.html", "<html>"),
	})
	var originals, edits []string
	for _, it := range p.Items {
		if it.EditOf != nil {
			edits = append(edits, it.Name+"->"+it.EditOf.Name)
		} else {
			originals = append(originals, it.Name)
		}
	}
	if len(originals) != 4 || len(edits) != 2 {
		t.Fatalf("originals %v, edits %v", originals, edits)
	}
	for _, it := range p.Items {
		if it.Path == "Takeout/Google Photos/Photos from 2023/IMG_1.jpg" {
			lat, _, _, ok := it.Sidecar.Location()
			if it.Sidecar == nil || !it.Sidecar.Favorited || !ok || lat != 52.5 || it.Sidecar.Taken().Unix() != 1690000000 {
				t.Errorf("IMG_1 sidecar: %+v", it.Sidecar)
			}
		}
		if it.Name == "IMG_1-bearbeitet.jpg" && it.Sidecar == nil {
			t.Error("edited copy should share the original's sidecar")
		}
	}
	if len(p.Albums) != 1 || p.Albums[0].Name != "Urlaub am Meer" || len(p.Albums[0].Items) != 1 {
		t.Errorf("albums: %+v", p.Albums)
	}
	// The album's copy has no sidecar of its own but is the same file as the year folder's.
	if p.Albums[0].Items[0].Sidecar == nil || !p.Albums[0].Items[0].Sidecar.Favorited {
		t.Error("album copy did not get its twin's sidecar")
	}
	if len(p.Skipped) != 3 {
		t.Errorf("skipped: %v", p.Skipped)
	}
}
