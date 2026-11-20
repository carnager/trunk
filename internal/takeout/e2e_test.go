package takeout

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/carnager/trunk/internal/server"
)

func jpegBytes(t *testing.T, seed uint8, exifDate string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = uint8(i) ^ seed
	}
	img.Set(0, 0, color.RGBA{seed, seed, seed, 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if exifDate == "" {
		return buf.Bytes()
	}
	// TIFF with IFD0 → ExifIFD → DateTimeOriginal, little endian, no offset tag.
	le := binary.LittleEndian
	date := append([]byte(exifDate), 0) // 20 bytes
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	tiff = le.AppendUint16(tiff, 1) // IFD0: one entry
	tiff = le.AppendUint16(tiff, 0x8769)
	tiff = le.AppendUint16(tiff, 4)
	tiff = le.AppendUint32(tiff, 1)
	tiff = le.AppendUint32(tiff, 26) // ExifIFD at 8+2+12+4
	tiff = le.AppendUint32(tiff, 0)
	tiff = le.AppendUint16(tiff, 1) // ExifIFD: one entry
	tiff = le.AppendUint16(tiff, 0x9003)
	tiff = le.AppendUint16(tiff, 2)
	tiff = le.AppendUint32(tiff, uint32(len(date)))
	tiff = le.AppendUint32(tiff, 26+2+12+4)
	tiff = le.AppendUint32(tiff, 0)
	tiff = append(tiff, date...)
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(app1)+2))
	out := append([]byte{0xFF, 0xD8}, seg...)
	out = append(out, app1...)
	return append(out, buf.Bytes()[2:]...)
}

// writeTgz writes the files in sorted order, which puts "IMG_A-bearbeitet.jpg"
// before "IMG_A.jpg": an edit that arrives before its original.
func writeTgz(t *testing.T, path string, files map[string][]byte) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg})
		tw.Write(files[n])
	}
	tw.Close()
	zw.Close()
	f.Close()
}

func writeZip(t *testing.T, path string, files map[string][]byte) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for name, data := range files {
		w, _ := z.Create(name)
		w.Write(data)
	}
	z.Close()
	f.Close()
}

func sidecarJSON(title string, ts int64, lat, lon float64, fav bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"title":          title,
		"photoTakenTime": map[string]string{"timestamp": fmt.Sprint(ts)},
		"geoData":        map[string]float64{"latitude": lat, "longitude": lon, "altitude": 0},
		"favorited":      fav,
	})
	return b
}

func TestImportEndToEnd(t *testing.T) {
	t.Run("zip", func(t *testing.T) { testImport(t, "zip") })
	t.Run("tgz", func(t *testing.T) { testImport(t, "tgz") })
}

func testImport(t *testing.T, format string) {
	dir := t.TempDir()
	a := jpegBytes(t, 1, "2023:07:17 16:00:00") // local time, offset not in the file
	aEdit := jpegBytes(t, 2, "")
	pxl := jpegBytes(t, 3, "")
	scan := jpegBytes(t, 4, "")
	gp := "Takeout/Google Photos/"
	write := writeZip
	if format == "tgz" {
		write = writeTgz
	}
	in1, in2 := filepath.Join(dir, "takeout-001."+format), filepath.Join(dir, "takeout-002."+format)
	write(t, in1, map[string][]byte{
		gp + "Photos from 2023/IMG_A.jpg":                            a,
		gp + "Photos from 2023/IMG_A.jpg.supplemental-metadata.json": sidecarJSON("IMG_A.jpg", 1689602400, 52.52, 13.405, true), // 14:00Z
		gp + "Photos from 2023/IMG_A-bearbeitet.jpg":                 aEdit,
		gp + "Photos from 2023/PXL_20230801_101500000.jpg":           pxl,
		gp + "Photos from 2023/PXL_20230801_101500000.jpg.json":      sidecarJSON("PXL_20230801_101500000.jpg", 1690877700, 0, 0, false), // 10:15 local = 08:15Z
		gp + "Photos from 2023/scan.jpg":                             scan,
		gp + "Photos from 2023/scan.jpg.json":                        sidecarJSON("scan.jpg", 1000000000, 0, 0, false),
		gp + "Urlaub/PXL_20230801_101500000.jpg":                     pxl,
		gp + "Urlaub/IMG_A.jpg":                                      a,
		"Takeout/archive_browser.html":                               []byte("<html>"),
	})
	// The album's title travels in another zip than its photos.
	write(t, in2, map[string][]byte{
		gp + "Urlaub/metadata.json": []byte(`{"title": "Urlaub an der Ostsee"}`),
	})

	srv, err := server.New(server.Config{DataDir: filepath.Join(dir, "data")}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h, _ := server.HashPassword("pw")
	srv.DB().Exec(`INSERT INTO users (name, pw_hash, created_at) VALUES ('alice', ?, 0)`, h)

	run := func() Result {
		entries, closer, err := Sources([]string{in1, in2}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer closer.Close()
		im, err := srv.Importer(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		return Run(context.Background(), im, NewPlan(entries), 3, io.Discard)
	}
	res := run()
	if res.Added != 3 || res.Existing != 2 || res.Edits != 1 || res.Failed != 0 || res.AlbumsCreated != 1 {
		t.Fatalf("first run: %+v", res)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "data", "uploads")); len(left) != 0 {
		t.Errorf("spooled files left behind: %v", left)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	var login struct{ Token string }
	post, _ := http.Post(ts.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"user":"alice","password":"pw"}`))
	json.NewDecoder(post.Body).Decode(&login)
	post.Body.Close()
	sync := func() (assets map[string]server.Asset, albums []server.Album) {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/changes", nil)
		req.Header.Set("Authorization", "Bearer "+login.Token)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var f struct {
			Assets []server.Asset
			Albums []server.Album
		}
		json.NewDecoder(r.Body).Decode(&f)
		assets = map[string]server.Asset{}
		for _, a := range f.Assets {
			assets[a.Name] = a
		}
		return assets, f.Albums
	}
	assets, albums := sync()

	imgA := assets["IMG_A.jpg"]
	if imgA.TZOffset == nil || *imgA.TZOffset != 120 || imgA.LocalTime != "2023-07-17T16:00:00" ||
		imgA.TakenAt.Unix() != 1689602400 {
		t.Errorf("IMG_A time: %v offset %v local %s", imgA.TakenAt, imgA.TZOffset, imgA.LocalTime)
	}
	if !imgA.Favorite || imgA.Lat == nil || *imgA.Lat != 52.52 {
		t.Errorf("IMG_A favourite %v, location %v", imgA.Favorite, imgA.Lat)
	}
	if imgA.EditedAt == nil || imgA.SHA256 == imgA.OriginalSHA256 {
		t.Errorf("IMG_A edit not applied: %+v", imgA)
	}
	p := assets["PXL_20230801_101500000.jpg"]
	if p.TZOffset == nil || *p.TZOffset != 120 || p.LocalTime != "2023-08-01T10:15:00" || p.Lat != nil {
		t.Errorf("PXL: offset %v local %s taken %v lat %v", p.TZOffset, p.LocalTime, p.TakenAt, p.Lat)
	}
	sc := assets["scan.jpg"]
	if sc.TakenAt == nil || sc.TakenAt.Unix() != 1000000000 || sc.TZOffset != nil {
		t.Errorf("scan: %v %v", sc.TakenAt, sc.TZOffset)
	}
	if len(assets) != 3 {
		t.Errorf("%d assets, want 3: %v", len(assets), assets)
	}
	if len(albums) != 1 || albums[0].Name != "Urlaub an der Ostsee" ||
		fmt.Sprint(albums[0].AssetIDs) != fmt.Sprint([]int64{imgA.ID, p.ID}) {
		t.Errorf("album: %+v", albums)
	}

	// Again: nothing new, nothing doubled.
	res = run()
	if res.Added != 0 || res.Existing != 5 || res.AlbumsCreated != 0 || res.AlbumsUpdated != 1 || res.Failed != 0 {
		t.Errorf("second run: %+v", res)
	}
	assets, albums = sync()
	if len(assets) != 3 || len(albums) != 1 || len(albums[0].AssetIDs) != 2 {
		t.Errorf("after second run: %d assets, albums %+v", len(assets), albums)
	}
}
