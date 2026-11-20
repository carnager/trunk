package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type env struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv, err := New(Config{DataDir: dir, TrashDays: 30}, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv.Run(ctx)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); cancel(); srv.Close() })
	for _, u := range []string{"alice", "bob"} {
		h, _ := HashPassword(u + "-secret")
		if _, err := srv.db.Exec(`INSERT INTO users (name, pw_hash, created_at) VALUES (?, ?, 0)`, u, h); err != nil {
			t.Fatal(err)
		}
	}
	return &env{t, srv, ts}
}

func (e *env) do(method, path, token string, body io.Reader, hdr map[string]string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { res.Body.Close() })
	return res
}

func (e *env) json(method, path, token string, in any, want int, out any) {
	e.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	res := e.do(method, path, token, body, map[string]string{"Content-Type": "application/json"})
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.StatusCode, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, data)
		}
	}
}

func (e *env) login(user, device string) string {
	e.t.Helper()
	var out struct{ Token string }
	e.json("POST", "/api/v1/auth/login", "", map[string]string{
		"user": user, "password": user + "-secret", "device": device}, 200, &out)
	return out.Token
}

type uploadResult struct {
	Asset   Asset
	Created bool
}

func (e *env) upload(token, name string, data []byte, want int) Asset {
	e.t.Helper()
	res := e.do("POST", "/api/v1/assets?filename="+name+"&mtime=1600000000000", token, bytes.NewReader(data), nil)
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != want {
		e.t.Fatalf("upload %s: status %d, want %d: %s", name, res.StatusCode, want, b)
	}
	var out uploadResult
	json.Unmarshal(b, &out)
	return out.Asset
}

// testJPEG makes a w×h JPEG; with exif set it carries orientation 6 (rotate
// 90° cw), a capture time with offset, GPS and camera data.
func testJPEG(t *testing.T, w, h int, seed uint8, exif bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x) ^ seed, uint8(y), seed, 255})
		}
	}
	// Mark the top-left corner red so rotation can be checked.
	for y := 0; y < h/10; y++ {
		for x := 0; x < w/10; x++ {
			img.Set(x, y, color.RGBA{255, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if !exif {
		return buf.Bytes()
	}
	app1 := append([]byte("Exif\x00\x00"), tiffExif()...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(app1)+2))
	out := append([]byte{0xFF, 0xD8}, seg...)
	out = append(out, app1...)
	return append(out, buf.Bytes()[2:]...)
}

type tiffEntry struct {
	tag, typ uint16
	count    uint32
	data     []byte
}

func ascii(tag uint16, s string) tiffEntry {
	return tiffEntry{tag, 2, uint32(len(s) + 1), append([]byte(s), 0)}
}

func short(tag, v uint16) tiffEntry {
	return tiffEntry{tag, 3, 1, binary.LittleEndian.AppendUint16(nil, v)}
}

func long(tag uint16, v uint32) tiffEntry {
	return tiffEntry{tag, 4, 1, binary.LittleEndian.AppendUint32(nil, v)}
}

func rationals(tag uint16, v ...[2]uint32) tiffEntry {
	var b []byte
	for _, r := range v {
		b = binary.LittleEndian.AppendUint32(b, r[0])
		b = binary.LittleEndian.AppendUint32(b, r[1])
	}
	return tiffEntry{tag, 5, uint32(len(v)), b}
}

func ifdSize(es []tiffEntry) int {
	n := 2 + 12*len(es) + 4
	for _, e := range es {
		if len(e.data) > 4 {
			n += len(e.data) + len(e.data)%2
		}
	}
	return n
}

func writeIFD(off int, es []tiffEntry) []byte {
	le := binary.LittleEndian
	b := le.AppendUint16(nil, uint16(len(es)))
	extra := off + 2 + 12*len(es) + 4
	var tail []byte
	for _, e := range es {
		b = le.AppendUint16(b, e.tag)
		b = le.AppendUint16(b, e.typ)
		b = le.AppendUint32(b, e.count)
		if len(e.data) <= 4 {
			v := make([]byte, 4)
			copy(v, e.data)
			b = append(b, v...)
		} else {
			b = le.AppendUint32(b, uint32(extra+len(tail)))
			tail = append(tail, e.data...)
			if len(e.data)%2 == 1 {
				tail = append(tail, 0)
			}
		}
	}
	b = le.AppendUint32(b, 0)
	return append(b, tail...)
}

func tiffExif() []byte {
	exifIFD := []tiffEntry{
		rationals(0x829d, [2]uint32{18, 10}), // FNumber 1.8
		short(0x8827, 125),                   // ISO
		ascii(0x9003, "2024:06:01 14:30:00"), // DateTimeOriginal
		ascii(0x9011, "+02:00"),              // OffsetTimeOriginal
		rationals(0x920a, [2]uint32{47, 10}), // FocalLength 4.7
	}
	gpsIFD := []tiffEntry{
		ascii(0x1, "N"),
		rationals(0x2, [2]uint32{52, 1}, [2]uint32{31, 1}, [2]uint32{12, 1}),
		ascii(0x3, "E"),
		rationals(0x4, [2]uint32{13, 1}, [2]uint32{24, 1}, [2]uint32{36, 1}),
	}
	ifd0 := []tiffEntry{
		ascii(0x10f, "Google"),
		ascii(0x110, "Pixel 8"),
		short(0x112, 6),
		long(0x8769, 0), // ExifIFD pointer, patched below
		long(0x8825, 0), // GPS IFD pointer
	}
	exifOff := 8 + ifdSize(ifd0)
	gpsOff := exifOff + ifdSize(exifIFD)
	ifd0[3] = long(0x8769, uint32(exifOff))
	ifd0[4] = long(0x8825, uint32(gpsOff))
	b := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	b = append(b, writeIFD(8, ifd0)...)
	b = append(b, writeIFD(exifOff, exifIFD)...)
	return append(b, writeIFD(gpsOff, gpsIFD)...)
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	e.json("POST", "/api/v1/auth/login", "", map[string]string{"user": "alice", "password": "nope"}, 401, nil)
	e.json("POST", "/api/v1/auth/login", "", map[string]string{"user": "nobody", "password": "nope"}, 401, nil)
	e.json("GET", "/api/v1/me", "", nil, 401, nil)
	e.json("GET", "/api/v1/me", "trk_bogus", nil, 401, nil)

	phone := e.login("alice", "Pixel 8")
	tablet := e.login("alice", "Tablet")
	var me struct{ Name string }
	e.json("GET", "/api/v1/me", phone, nil, 200, &me)
	if me.Name != "alice" {
		t.Fatalf("me = %q", me.Name)
	}
	var devs struct {
		Devices []struct {
			ID      int64
			Name    string
			Current bool
		}
	}
	e.json("GET", "/api/v1/devices", phone, nil, 200, &devs)
	if len(devs.Devices) != 2 {
		t.Fatalf("devices = %+v", devs)
	}
	var tabletID int64
	for _, d := range devs.Devices {
		if d.Name == "Tablet" {
			tabletID = d.ID
		}
	}
	// Bob cannot revoke Alice's devices.
	bob := e.login("bob", "bob phone")
	e.json("DELETE", fmt.Sprintf("/api/v1/devices/%d", tabletID), bob, nil, 404, nil)
	e.json("DELETE", fmt.Sprintf("/api/v1/devices/%d", tabletID), phone, nil, 204, nil)
	e.json("GET", "/api/v1/me", tablet, nil, 401, nil)
	e.json("GET", "/api/v1/me", phone, nil, 200, nil)
	e.json("POST", "/api/v1/auth/logout", phone, nil, 204, nil)
	e.json("GET", "/api/v1/me", phone, nil, 401, nil)
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	for range limitMax {
		e.json("POST", "/api/v1/auth/login", "", map[string]string{"user": "alice", "password": "x"}, 401, nil)
	}
	// Even the right password is refused while blocked.
	e.json("POST", "/api/v1/auth/login", "", map[string]string{"user": "alice", "password": "alice-secret"}, 429, nil)
}

func TestUploadMetadataAndThumbs(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	data := testJPEG(t, 1200, 800, 1, true)
	a := e.upload(tok, "IMG_0001.jpg", data, 201)

	if a.Kind != "image" || a.MIME != "image/jpeg" || a.Size != int64(len(data)) {
		t.Errorf("basic fields: %+v", a)
	}
	if a.Width != 800 || a.Height != 1200 || a.Orientation != 6 {
		t.Errorf("dimensions %dx%d orientation %d, want 800x1200 (rotated) and 6", a.Width, a.Height, a.Orientation)
	}
	if a.TakenAt == nil || !a.TakenAt.Equal(time.Date(2024, 6, 1, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("taken_at = %v, want 2024-06-01 12:30 UTC", a.TakenAt)
	}
	if a.TZOffset == nil || *a.TZOffset != 120 || a.LocalTime != "2024-06-01T14:30:00" {
		t.Errorf("tz_offset %v local %q", a.TZOffset, a.LocalTime)
	}
	if a.Lat == nil || math.Abs(*a.Lat-52.52) > 0.001 || a.Lon == nil || math.Abs(*a.Lon-13.41) > 0.001 {
		t.Errorf("gps %v %v", a.Lat, a.Lon)
	}
	if a.Make != "Google" || a.Model != "Pixel 8" || a.ISO != 125 || a.FNumber != 1.8 || a.FocalLength != 4.7 {
		t.Errorf("camera: make %q model %q iso %d f %v focal %v", a.Make, a.Model, a.ISO, a.FNumber, a.FocalLength)
	}
	if _, err := os.Stat(filepath.Join(e.srv.originals, "alice", "2024", "06", "IMG_0001.jpg")); err != nil {
		t.Errorf("original not in library tree: %v", err)
	}

	// Same bytes again: no second copy.
	again := e.upload(tok, "copy.jpg", data, 200)
	if again.ID != a.ID {
		t.Errorf("duplicate got id %d, want %d", again.ID, a.ID)
	}
	res := e.do("POST", "/api/v1/assets?filename=x.jpg", tok, strings.NewReader("never read"),
		map[string]string{"X-Sha256": a.SHA256})
	if res.StatusCode != 200 {
		t.Errorf("upload with known X-Sha256: %d", res.StatusCode)
	}
	var chk struct{ Existing map[string]int64 }
	e.json("POST", "/api/v1/assets/check", tok, map[string]any{"sha256": []string{a.SHA256, strings.Repeat("0", 64)}}, 200, &chk)
	if len(chk.Existing) != 1 || chk.Existing[a.SHA256] != a.ID {
		t.Errorf("check = %+v", chk)
	}

	// Same file name, different content: kept side by side.
	b := e.upload(tok, "IMG_0001.jpg", testJPEG(t, 640, 480, 2, true), 201)
	if b.ID == a.ID {
		t.Fatal("different content deduplicated")
	}
	if _, err := os.Stat(filepath.Join(e.srv.originals, "alice", "2024", "06", "IMG_0001_1.jpg")); err != nil {
		t.Errorf("name collision not resolved: %v", err)
	}

	// No EXIF: the date comes from the file name.
	c := e.upload(tok, "PXL_20230105_101112345.jpg", testJPEG(t, 300, 200, 3, false), 201)
	if c.LocalTime != "2023-01-05T10:11:12" || c.TZOffset != nil || c.Orientation != 0 {
		t.Errorf("filename date: local %q tz %v orientation %d", c.LocalTime, c.TZOffset, c.Orientation)
	}
	// Neither: the client's mtime.
	d := e.upload(tok, "scan.jpg", testJPEG(t, 300, 200, 4, false), 201)
	if !d.TakenAt.Equal(time.UnixMilli(1600000000000)) {
		t.Errorf("mtime fallback: %v", d.TakenAt)
	}

	e.upload(tok, "notes.txt", []byte("hello, this is not a photo"), 415)

	// Thumbnails come out upright and at the documented sizes.
	for size, want := range map[string][2]int{"small": {300, 450}, "medium": {480, 720}, "preview": {800, 1200}} {
		res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d/thumb/%s", a.ID, size), tok, nil, nil)
		if res.StatusCode != 200 {
			t.Fatalf("thumb %s: %d", size, res.StatusCode)
		}
		img, err := jpeg.Decode(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got := img.Bounds().Size(); got.X != want[0] || got.Y != want[1] {
			t.Errorf("thumb %s is %v, want %v", size, got, want)
		}
		if size == "preview" {
			// Orientation 6 moves the original's top-left corner to the top right.
			r, g, _, _ := img.At(img.Bounds().Dx()-5, 5).RGBA()
			if r>>8 < 200 || g>>8 > 60 {
				t.Errorf("preview not rotated: top-right pixel r=%d g=%d", r>>8, g>>8)
			}
		}
		etag := res.Header.Get("ETag")
		res = e.do("GET", fmt.Sprintf("/api/v1/assets/%d/thumb/%s", a.ID, size), tok, nil, map[string]string{"If-None-Match": etag})
		if res.StatusCode != 304 {
			t.Errorf("thumb %s revalidation: %d", size, res.StatusCode)
		}
	}

	// Originals support ranges (video seeking).
	res = e.do("GET", fmt.Sprintf("/api/v1/assets/%d/original", a.ID), tok, nil, map[string]string{"Range": "bytes=0-9"})
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != 206 || !bytes.Equal(got, data[:10]) {
		t.Errorf("range: %d %x", res.StatusCode, got)
	}

	// Bob sees none of it.
	bob := e.login("bob", "phone")
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", a.ID), bob, nil, 404, nil)
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d/original", a.ID), bob, nil, 404, nil)
	var feed changes
	e.json("GET", "/api/v1/changes", bob, nil, 200, &feed)
	if len(feed.Assets) != 0 {
		t.Errorf("bob sees %d assets", len(feed.Assets))
	}
	// But he can own the same file independently.
	if e.upload(bob, "same.jpg", data, 201).ID == a.ID {
		t.Error("bob's upload shares alice's asset")
	}
}

type changes struct {
	Assets []Asset
	Cursor int64
	More   bool
}

func TestChangesFeed(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	var ids []int64
	for i := range 5 {
		ids = append(ids, e.upload(tok, fmt.Sprintf("p%d.jpg", i), testJPEG(t, 64, 48, uint8(i+10), false), 201).ID)
	}

	// Paging.
	var p1, p2 changes
	e.json("GET", "/api/v1/changes?since=0&limit=3", tok, nil, 200, &p1)
	if len(p1.Assets) != 3 || !p1.More {
		t.Fatalf("page 1: %d more=%v", len(p1.Assets), p1.More)
	}
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d&limit=3", p1.Cursor), tok, nil, 200, &p2)
	if len(p2.Assets) != 2 || p2.More {
		t.Fatalf("page 2: %d more=%v", len(p2.Assets), p2.More)
	}
	cur := p2.Cursor

	// Nothing new.
	var none changes
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), tok, nil, 200, &none)
	if len(none.Assets) != 0 || none.Cursor != cur {
		t.Fatalf("idle poll: %+v", none)
	}

	// Favourite one, trash two, restore one, delete one for good.
	var fav Asset
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", ids[0]), tok, map[string]bool{"favorite": true}, 200, &fav)
	if !fav.Favorite {
		t.Error("favorite not set")
	}
	e.json("POST", "/api/v1/assets/trash", tok, map[string][]int64{"ids": {ids[1], ids[2]}}, 204, nil)
	e.json("POST", "/api/v1/assets/restore", tok, map[string][]int64{"ids": {ids[2]}}, 204, nil)

	// Deleting needs the trash first.
	var del struct{ Deleted int }
	e.json("POST", "/api/v1/assets/delete", tok, map[string][]int64{"ids": {ids[3]}}, 200, &del)
	if del.Deleted != 0 {
		t.Fatal("deleted an asset that was not in the trash")
	}
	var trashed Asset
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", ids[1]), tok, nil, 200, &trashed)
	if trashed.TrashedAt == nil || trashed.TrashExpiresAt == nil {
		t.Fatalf("trashed asset: %+v", trashed)
	}
	path := filepath.Join(e.srv.originals, "alice")
	e.json("POST", "/api/v1/assets/delete", tok, map[string][]int64{"ids": {ids[1]}}, 200, &del)
	if del.Deleted != 1 {
		t.Fatal("delete from trash failed")
	}
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", ids[1]), tok, nil, 404, nil)

	var ch changes
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), tok, nil, 200, &ch)
	byID := map[int64]Asset{}
	for _, a := range ch.Assets {
		byID[a.ID] = a
	}
	if len(ch.Assets) != 3 {
		t.Fatalf("changes since cursor: %+v", ch.Assets)
	}
	if !byID[ids[0]].Favorite {
		t.Error("favourite change missing")
	}
	if !byID[ids[1]].Deleted || byID[ids[1]].SHA256 != "" {
		t.Errorf("tombstone: %+v", byID[ids[1]])
	}
	if byID[ids[2]].TrashedAt != nil {
		t.Error("restore not reflected")
	}

	// A fresh client does not get tombstones.
	var fresh changes
	e.json("GET", "/api/v1/changes", tok, nil, 200, &fresh)
	if len(fresh.Assets) != 4 {
		t.Errorf("full sync: %d assets, want 4", len(fresh.Assets))
	}
	files := 0
	filepath.Walk(path, func(_ string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() {
			files++
		}
		return nil
	})
	if files != 4 {
		t.Errorf("%d files on disk, want 4", files)
	}

	// Expired trash is emptied by the janitor.
	e.json("POST", "/api/v1/assets/trash", tok, map[string][]int64{"ids": {ids[4]}}, 204, nil)
	e.srv.db.Exec(`UPDATE assets SET trashed_at = 1 WHERE id = ?`, ids[4])
	e.srv.sweep(context.Background())
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", ids[4]), tok, nil, 404, nil)
}

func tusMeta(kv ...string) string {
	var parts []string
	for i := 0; i < len(kv); i += 2 {
		parts = append(parts, kv[i]+" "+base64.StdEncoding.EncodeToString([]byte(kv[i+1])))
	}
	return strings.Join(parts, ",")
}

func TestTusUpload(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	data := testJPEG(t, 900, 600, 7, true)
	tus := map[string]string{"Tus-Resumable": "1.0.0"}

	res := e.do("OPTIONS", "/api/v1/uploads", "", nil, nil)
	if res.Header.Get("Tus-Version") != "1.0.0" {
		t.Fatalf("OPTIONS: %v", res.Header)
	}
	res = e.do("POST", "/api/v1/uploads", tok, nil, map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Length": strconv.Itoa(len(data)),
		"Upload-Metadata": tusMeta("filename", "IMG_tus.jpg", "mtime", "1700000000000"),
	})
	if res.StatusCode != 201 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("create: %d %s", res.StatusCode, b)
	}
	loc := res.Header.Get("Location")

	// Bob cannot see or write Alice's upload.
	bob := e.login("bob", "phone")
	if res := e.do("HEAD", loc, bob, nil, tus); res.StatusCode != 404 {
		t.Errorf("bob HEAD: %d", res.StatusCode)
	}

	patch := func(off int, chunk []byte) *http.Response {
		return e.do("PATCH", loc, tok, bytes.NewReader(chunk), map[string]string{
			"Tus-Resumable": "1.0.0", "Content-Type": "application/offset+octet-stream",
			"Upload-Offset": strconv.Itoa(off),
		})
	}
	half := len(data) / 2
	if res := patch(0, data[:half]); res.StatusCode != 204 || res.Header.Get("Upload-Offset") != strconv.Itoa(half) {
		t.Fatalf("first chunk: %d %v", res.StatusCode, res.Header)
	}
	if res := patch(0, data[:10]); res.StatusCode != 409 {
		t.Errorf("stale offset: %d", res.StatusCode)
	}
	res = e.do("HEAD", loc, tok, nil, tus)
	if res.Header.Get("Upload-Offset") != strconv.Itoa(half) {
		t.Errorf("HEAD offset %q", res.Header.Get("Upload-Offset"))
	}
	res = patch(half, data[half:])
	if res.StatusCode != 204 || res.Header.Get("Trunk-Asset-Created") != "true" {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("last chunk: %d %v %s", res.StatusCode, res.Header, b)
	}
	id := res.Header.Get("Trunk-Asset-Id")
	var a Asset
	e.json("GET", "/api/v1/assets/"+id, tok, nil, 200, &a)
	if a.Name != "IMG_tus.jpg" || a.Size != int64(len(data)) || a.Orientation != 6 {
		t.Errorf("asset from tus: %+v", a)
	}
	// A client that lost the last response asks again.
	if res := e.do("HEAD", loc, tok, nil, tus); res.Header.Get("Trunk-Asset-Id") != id {
		t.Errorf("HEAD after completion: %v", res.Header)
	}

	// A wrong checksum discards the upload.
	res = e.do("POST", "/api/v1/uploads", tok, nil, map[string]string{
		"Tus-Resumable": "1.0.0", "Upload-Length": "5",
		"Upload-Metadata": tusMeta("filename", "x.jpg", "sha256", strings.Repeat("a", 64)),
	})
	loc = res.Header.Get("Location")
	if res := patch(0, []byte("hello")); res.StatusCode != 400 {
		t.Errorf("bad checksum: %d", res.StatusCode)
	}
	if res := e.do("HEAD", loc, tok, nil, tus); res.StatusCode != 404 {
		t.Errorf("upload kept after bad checksum: %d", res.StatusCode)
	}
}

func TestVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	e := newEnv(t)
	tok := e.login("alice", "phone")
	clip := filepath.Join(t.TempDir(), "VID_20240102_030405.mp4")
	out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=10",
		"-pix_fmt", "yuv420p", "-metadata", "creation_time=2024-01-02T03:04:05Z",
		"-metadata", "location=+48.8577+002.2950/", clip).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	data, _ := os.ReadFile(clip)
	a := e.upload(tok, filepath.Base(clip), data, 201)
	if a.Kind != "video" || a.MIME != "video/mp4" || a.Width != 320 || a.Height != 240 {
		t.Errorf("video: %+v", a)
	}
	if a.DurationMS < 2900 || a.DurationMS > 3100 {
		t.Errorf("duration %d", a.DurationMS)
	}
	if !a.TakenAt.Equal(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("taken_at %v", a.TakenAt)
	}
	if a.Lat == nil || math.Abs(*a.Lat-48.8577) > 0.001 {
		t.Errorf("location %v", a.Lat)
	}
	res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d/thumb/small", a.ID), tok, nil, nil)
	if res.StatusCode != 200 {
		t.Fatalf("poster: %d", res.StatusCode)
	}
	img, err := jpeg.Decode(res.Body)
	if err != nil || img.Bounds().Dy() != 240 {
		t.Errorf("poster: %v %v", err, img.Bounds())
	}
}

func TestSourceHash(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	source := strings.Repeat("ab", 32) // the phone's original, never uploaded itself
	small := testJPEG(t, 300, 200, 21, false)
	res := e.do("POST", "/api/v1/assets?filename=big.jpg&source_sha256="+source, tok, bytes.NewReader(small), nil)
	if res.StatusCode != 201 {
		t.Fatalf("upload: %d", res.StatusCode)
	}
	var up uploadResult
	json.NewDecoder(res.Body).Decode(&up)

	var chk struct{ Existing map[string]int64 }
	chk.Existing = nil
	e.json("POST", "/api/v1/assets/check", tok, map[string]any{"sha256": []string{source}}, 200, &chk)
	if chk.Existing[source] != up.Asset.ID {
		t.Errorf("source hash not known: %+v", chk)
	}
	// Shrunk differently next time: still recognised by the source, body not needed.
	res = e.do("POST", "/api/v1/assets?filename=big.jpg&source_sha256="+source, tok,
		bytes.NewReader(testJPEG(t, 200, 100, 22, false)), nil)
	var again uploadResult
	json.NewDecoder(res.Body).Decode(&again)
	if res.StatusCode != 200 || again.Asset.ID != up.Asset.ID {
		t.Errorf("re-upload with same source: %d id %d", res.StatusCode, again.Asset.ID)
	}
	// Other users are not affected.
	bob := e.login("bob", "phone")
	chk.Existing = nil
	e.json("POST", "/api/v1/assets/check", bob, map[string]any{"sha256": []string{source}}, 200, &chk)
	if len(chk.Existing) != 0 {
		t.Error("bob sees alice's source hash")
	}
	// Deleting for good forgets every hash.
	e.json("POST", "/api/v1/assets/trash", tok, map[string][]int64{"ids": {up.Asset.ID}}, 204, nil)
	e.json("POST", "/api/v1/assets/delete", tok, map[string][]int64{"ids": {up.Asset.ID}}, 200, nil)
	chk.Existing = nil
	e.json("POST", "/api/v1/assets/check", tok, map[string]any{"sha256": []string{source, up.Asset.SHA256}}, 200, &chk)
	if len(chk.Existing) != 0 {
		t.Errorf("hashes survive deletion: %+v", chk)
	}
}

func TestEdits(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	orig := testJPEG(t, 1200, 800, 31, true) // orientation 6, shown as 800x1200
	a := e.upload(tok, "IMG_0042.jpg", orig, 201)
	thumb := func(id int64) image.Point {
		res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d/thumb/preview", id), tok, nil, nil)
		img, err := jpeg.Decode(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return img.Bounds().Size()
	}
	thumb(a.ID)

	put := func(data []byte, want int) Asset {
		t.Helper()
		res := e.do("PUT", fmt.Sprintf("/api/v1/assets/%d/edit", a.ID), tok, bytes.NewReader(data), nil)
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("PUT edit: %d, want %d: %s", res.StatusCode, want, b)
		}
		var out Asset
		json.Unmarshal(b, &out)
		return out
	}
	get := func(path string) []byte {
		t.Helper()
		res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d/%s", a.ID, path), tok, nil, nil)
		b, _ := io.ReadAll(res.Body)
		return b
	}

	// A crop: upright already, no EXIF, different size.
	edit := testJPEG(t, 500, 700, 32, false)
	ed := put(edit, 200)
	if ed.EditedAt == nil || ed.OriginalSHA256 != a.SHA256 || ed.OriginalSize != a.Size {
		t.Fatalf("edited asset: %+v", ed)
	}
	if ed.Width != 500 || ed.Height != 700 || ed.Orientation != 0 || ed.Size != int64(len(edit)) {
		t.Errorf("edit dims %dx%d orientation %d size %d", ed.Width, ed.Height, ed.Orientation, ed.Size)
	}
	if !ed.TakenAt.Equal(*a.TakenAt) || ed.Make != "Google" || ed.Lat == nil {
		t.Errorf("edit lost when/where/camera: %+v", ed)
	}
	if ed.Seq <= a.Seq {
		t.Error("edit did not bump seq")
	}
	if !bytes.Equal(get("file"), edit) || !bytes.Equal(get("original"), orig) {
		t.Error("file/original do not serve edit/first upload")
	}
	if got := thumb(a.ID); got != (image.Point{500, 700}) {
		t.Errorf("thumbnail after edit: %v", got)
	}
	for _, f := range []string{"IMG_0042.jpg", "IMG_0042_edit.jpg"} {
		if _, err := os.Stat(filepath.Join(e.srv.originals, "alice", "2024", "06", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}

	// All three hashes are known; the phone will not upload the original again.
	var chk struct{ Existing map[string]int64 }
	chk.Existing = nil
	e.json("POST", "/api/v1/assets/check", tok, map[string]any{"sha256": []string{a.SHA256, ed.SHA256}}, 200, &chk)
	if len(chk.Existing) != 2 {
		t.Errorf("check after edit: %+v", chk)
	}
	if e.upload(tok, "IMG_0042.jpg", orig, 200).ID != a.ID {
		t.Error("original uploaded again as a new asset")
	}

	// Editing again replaces the edit, not the original.
	edit2 := testJPEG(t, 400, 400, 33, false)
	ed2 := put(edit2, 200)
	if ed2.OriginalSHA256 != a.SHA256 || ed2.Width != 400 {
		t.Errorf("second edit: %+v", ed2)
	}
	if _, err := os.Stat(filepath.Join(e.srv.originals, "alice", "2024", "06", "IMG_0042_edit.jpg")); !os.IsNotExist(err) {
		// the second edit took the _edit name only after the first was removed
		files, _ := os.ReadDir(filepath.Join(e.srv.originals, "alice", "2024", "06"))
		if len(files) != 2 {
			t.Errorf("files after second edit: %v", files)
		}
	}
	chk.Existing = nil
	e.json("POST", "/api/v1/assets/check", tok, map[string]any{"sha256": []string{ed.SHA256}}, 200, &chk)
	if len(chk.Existing) != 0 {
		t.Error("replaced edit's hash still known")
	}

	// Another asset's file cannot become this one's edit.
	other := testJPEG(t, 100, 100, 34, false)
	e.upload(tok, "other.jpg", other, 201)
	put(other, 409)
	put([]byte("not an image at all"), 415)

	// Revert.
	e.json("DELETE", fmt.Sprintf("/api/v1/assets/%d/edit", a.ID), tok, nil, 200, nil)
	var back Asset
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", a.ID), tok, nil, 200, &back)
	if back.EditedAt != nil || back.SHA256 != a.SHA256 || back.Width != 800 || back.Orientation != 6 {
		t.Errorf("after revert: %+v", back)
	}
	if !bytes.Equal(get("file"), orig) {
		t.Error("file after revert is not the original")
	}
	if got := thumb(a.ID); got != (image.Point{800, 1200}) {
		t.Errorf("thumbnail after revert: %v", got)
	}
	files, _ := os.ReadDir(filepath.Join(e.srv.originals, "alice", "2024", "06"))
	if len(files) != 1 {
		t.Errorf("edit files left after revert: %v", files)
	}

	// Edit, then delete for good: both files go.
	put(edit, 200)
	e.json("POST", "/api/v1/assets/trash", tok, map[string][]int64{"ids": {a.ID}}, 204, nil)
	e.json("POST", "/api/v1/assets/delete", tok, map[string][]int64{"ids": {a.ID}}, 200, nil)
	files, _ = os.ReadDir(filepath.Join(e.srv.originals, "alice", "2024", "06"))
	if len(files) != 0 {
		t.Errorf("files left after delete: %v", files)
	}
}

type feed struct {
	Assets []Asset
	Albums []Album
	Cursor int64
	More   bool
}

func TestAlbums(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	var ids []int64
	for i := range 4 {
		ids = append(ids, e.upload(tok, fmt.Sprintf("a%d.jpg", i), testJPEG(t, 32, 32, uint8(40+i), false), 201).ID)
	}
	var start feed
	e.json("GET", "/api/v1/changes", tok, nil, 200, &start)

	var al Album
	e.json("POST", "/api/v1/albums", tok, map[string]any{"name": " Urlaub ", "asset_ids": []int64{ids[0], ids[1]}}, 201, &al)
	if al.Name != "Urlaub" || fmt.Sprint(al.AssetIDs) != fmt.Sprint([]int64{ids[0], ids[1]}) {
		t.Fatalf("created: %+v", al)
	}
	path := fmt.Sprintf("/api/v1/albums/%d", al.ID)
	e.json("POST", path+"/add", tok, map[string]any{"ids": []int64{ids[2], ids[3]}}, 200, &al)
	if fmt.Sprint(al.AssetIDs) != fmt.Sprint([]int64{ids[2], ids[3], ids[0], ids[1]}) {
		t.Errorf("add to front: %v", al.AssetIDs)
	}
	e.json("POST", path+"/remove", tok, map[string]any{"ids": []int64{ids[3]}}, 200, &al)
	e.json("POST", path+"/add", tok, map[string]any{"ids": []int64{ids[3], ids[0]}, "end": true}, 200, &al)
	if fmt.Sprint(al.AssetIDs) != fmt.Sprint([]int64{ids[2], ids[0], ids[1], ids[3]}) {
		t.Errorf("add to end keeps existing places: %v", al.AssetIDs)
	}
	e.json("PATCH", path, tok, map[string]string{"name": "Urlaub 2024"}, 200, &al)

	// Bob can neither see the album nor put Alice's photos in his own.
	bob := e.login("bob", "phone")
	e.json("GET", path, bob, nil, 404, nil)
	var bobs Album
	e.json("POST", "/api/v1/albums", bob, map[string]any{"name": "mine"}, 201, &bobs)
	e.json("POST", fmt.Sprintf("/api/v1/albums/%d/add", bobs.ID), bob, map[string]any{"ids": []int64{ids[0]}}, 400, nil)
	var bf feed
	e.json("GET", "/api/v1/changes", bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || bf.Albums[0].ID != bobs.ID {
		t.Errorf("bob's feed albums: %+v", bf.Albums)
	}

	// The feed carries the album once, in its latest state.
	var f feed
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", start.Cursor), tok, nil, 200, &f)
	if len(f.Albums) != 1 || f.Albums[0].Name != "Urlaub 2024" || len(f.Albums[0].AssetIDs) != 4 {
		t.Fatalf("feed: %+v", f.Albums)
	}

	// Deleting a photo for good takes it out of the album, and the album changes.
	e.json("POST", "/api/v1/assets/trash", tok, map[string][]int64{"ids": {ids[1]}}, 204, nil)
	e.json("GET", path, tok, nil, 200, &al)
	if len(al.AssetIDs) != 4 {
		t.Error("trashed photo should stay listed until deleted")
	}
	e.json("POST", "/api/v1/assets/delete", tok, map[string][]int64{"ids": {ids[1]}}, 200, nil)
	var f2 feed
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", f.Cursor), tok, nil, 200, &f2)
	if len(f2.Albums) != 1 || fmt.Sprint(f2.Albums[0].AssetIDs) != fmt.Sprint([]int64{ids[2], ids[0], ids[3]}) {
		t.Errorf("album after delete: %+v", f2.Albums)
	}

	// Paging walks through assets and albums together without losing any.
	for i := range 3 {
		var x Album
		e.json("POST", "/api/v1/albums", tok, map[string]any{"name": fmt.Sprintf("x%d", i), "asset_ids": []int64{ids[0]}}, 201, &x)
	}
	seenAssets, seenAlbums := map[int64]bool{}, map[int64]bool{}
	cur := int64(0)
	for pages := 0; ; pages++ {
		var p feed
		e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d&limit=2", cur), tok, nil, 200, &p)
		if len(p.Assets)+len(p.Albums) > 2 {
			t.Fatalf("page too big: %+v", p)
		}
		// Later pages may carry tombstones of things deleted before; a client just drops them.
		for _, a := range p.Assets {
			seenAssets[a.ID] = !a.Deleted
		}
		for _, a := range p.Albums {
			seenAlbums[a.ID] = !a.Deleted
		}
		cur = p.Cursor
		if !p.More {
			break
		}
		if pages > 20 {
			t.Fatal("paging does not end")
		}
	}
	live := func(m map[int64]bool) (n int) {
		for _, ok := range m {
			if ok {
				n++
			}
		}
		return n
	}
	if live(seenAssets) != 3 || live(seenAlbums) != 4 {
		t.Errorf("paged full sync saw %d assets, %d albums; want 3, 4", live(seenAssets), live(seenAlbums))
	}

	// Deleting the album leaves a tombstone and keeps the photos.
	e.json("DELETE", path, tok, nil, 204, nil)
	var f3 feed
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), tok, nil, 200, &f3)
	if len(f3.Albums) != 1 || !f3.Albums[0].Deleted || f3.Albums[0].ID != al.ID {
		t.Errorf("album tombstone: %+v", f3.Albums)
	}
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", ids[0]), tok, nil, 200, nil)
}

func TestPlacesAndCaptions(t *testing.T) {
	e := newEnv(t)
	tok := e.login("alice", "phone")
	a := e.upload(tok, "IMG_berlin.jpg", testJPEG(t, 64, 48, 51, true), 201) // EXIF GPS 52.52, 13.41
	get := func(lang string) Asset {
		res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d", a.ID), tok, nil, map[string]string{"Accept-Language": lang})
		var out Asset
		json.NewDecoder(res.Body).Decode(&out)
		return out
	}
	if p := get("de-DE,de;q=0.9").Place; p == nil || p.City != "Berlin" || p.Country != "Deutschland" || p.CC != "DE" {
		t.Errorf("german place: %+v", p)
	}
	if p := get("en").Place; p == nil || p.Country != "Germany" {
		t.Errorf("english place: %+v", p)
	}
	if p := e.upload(tok, "nogps.jpg", testJPEG(t, 64, 48, 52, false), 201).Place; p != nil {
		t.Errorf("place without GPS: %+v", p)
	}

	var start feed
	e.json("GET", "/api/v1/changes", tok, nil, 200, &start)
	var patched Asset
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", a.ID), tok, map[string]string{"caption": "  Brandenburger Tor im Regen  "}, 200, &patched)
	if patched.Caption != "Brandenburger Tor im Regen" {
		t.Errorf("caption: %q", patched.Caption)
	}
	var f feed
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", start.Cursor), tok, nil, 200, &f)
	if len(f.Assets) != 1 || f.Assets[0].Caption != "Brandenburger Tor im Regen" {
		t.Errorf("caption change in feed: %+v", f.Assets)
	}
	var cleared Asset
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", a.ID), tok, map[string]string{"caption": ""}, 200, &cleared)
	if cleared.ID != a.ID || cleared.Caption != "" {
		t.Error("caption not cleared")
	}

	// Assets from before places existed get theirs in the background, with a new seq.
	e.srv.db.Exec(`UPDATE assets SET place_id = NULL WHERE id = ?`, a.ID)
	var before feed
	e.json("GET", "/api/v1/changes", tok, nil, 200, &before)
	e.srv.fillPlaces(context.Background())
	var after feed
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", before.Cursor), tok, nil, 200, &after)
	if len(after.Assets) != 1 || after.Assets[0].Place == nil || after.Assets[0].Place.City != "Berlin" {
		t.Errorf("backfilled place: %+v", after.Assets)
	}
}

func TestSharedAlbums(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.login("alice", "phone"), e.login("bob", "phone")
	p1 := e.upload(alice, "familie.jpg", testJPEG(t, 32, 32, 61, false), 201)
	p2 := e.upload(alice, "privat.jpg", testJPEG(t, 32, 32, 62, false), 201)
	var al Album
	e.json("POST", "/api/v1/albums", alice, map[string]any{"name": "Familie", "asset_ids": []int64{p1.ID}}, 201, &al)
	path := fmt.Sprintf("/api/v1/albums/%d", al.ID)

	var users struct{ Users []UserRef }
	e.json("GET", "/api/v1/users", alice, nil, 200, &users)
	if len(users.Users) != 1 || users.Users[0].Name != "bob" {
		t.Fatalf("household: %+v", users)
	}
	bobID := users.Users[0].ID
	member := fmt.Sprintf("%s/members/%d", path, bobID)

	// Not shared yet: Bob sees nothing.
	e.json("GET", path, bob, nil, 404, nil)
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", p1.ID), bob, nil, 404, nil)
	var bf feed
	e.json("GET", "/api/v1/changes", bob, nil, 200, &bf)
	cur := bf.Cursor

	e.json("PUT", member, alice, map[string]any{"can_add": true}, 200, &al)
	if len(al.Members) != 1 || al.Members[0].Name != "bob" || al.Role != "owner" {
		t.Errorf("owner's view: %+v", al)
	}
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || bf.Albums[0].Role != "member" || bf.Albums[0].Owner == nil || bf.Albums[0].Owner.Name != "alice" ||
		fmt.Sprint(bf.Albums[0].AssetIDs) != fmt.Sprint([]int64{p1.ID}) || len(bf.Assets) != 0 {
		t.Fatalf("bob joined: %+v", bf)
	}
	cur = bf.Cursor

	// Bob sees what is in the album, nothing else of Alice's.
	var content struct{ Assets []Asset }
	e.json("GET", path+"/assets", bob, nil, 200, &content)
	if len(content.Assets) != 1 || content.Assets[0].ID != p1.ID || content.Assets[0].Owner != "alice" {
		t.Errorf("album assets for bob: %+v", content)
	}
	for _, p := range []string{"", "/original", "/file", "/thumb/small"} {
		if res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d%s", p1.ID, p), bob, nil, nil); res.StatusCode != 200 {
			t.Errorf("bob GET shared %q: %d", p, res.StatusCode)
		}
	}
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", p2.ID), bob, nil, 404, nil)
	// Looking is all: Alice's photo stays Alice's.
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", p1.ID), bob, map[string]bool{"favorite": true}, 404, nil)
	e.json("POST", "/api/v1/assets/trash", bob, map[string][]int64{"ids": {p1.ID}}, 204, nil)
	var still Asset
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", p1.ID), alice, nil, 200, &still)
	if still.TrashedAt != nil {
		t.Error("bob trashed alice's photo")
	}

	// Bob adds a photo of his own; Alice, the owner, sees it.
	b1 := e.upload(bob, "bobs.jpg", testJPEG(t, 32, 32, 63, false), 201)
	e.json("POST", path+"/add", bob, map[string]any{"ids": []int64{b1.ID}, "end": true}, 200, &al)
	e.json("POST", path+"/add", bob, map[string]any{"ids": []int64{p2.ID}}, 400, nil) // not his
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", b1.ID), alice, nil, 200, nil)
	e.json("GET", path+"/assets", alice, nil, 200, &content)
	if len(content.Assets) != 2 || content.Assets[1].Owner != "bob" {
		t.Errorf("album assets for alice: %+v", content)
	}

	// Only the owner renames and deletes; a member takes out only their own photos.
	e.json("PATCH", path, bob, map[string]string{"name": "Meins"}, 403, nil)
	e.json("DELETE", path, bob, nil, 403, nil)
	e.json("POST", path+"/remove", bob, map[string]any{"ids": []int64{p1.ID, b1.ID}}, 200, &al)
	if fmt.Sprint(al.AssetIDs) != fmt.Sprint([]int64{p1.ID}) {
		t.Errorf("after bob removed: %v", al.AssetIDs)
	}

	// A change to a photo in the album reaches the member through the album.
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	cur = bf.Cursor
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", p1.ID), alice, map[string]string{"caption": "Oma"}, 200, nil)
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || bf.Albums[0].ID != al.ID {
		t.Errorf("caption change did not touch the shared album: %+v", bf.Albums)
	}
	cur = bf.Cursor

	// View only.
	e.json("PUT", member, alice, map[string]any{"can_add": false}, 200, nil)
	e.json("POST", path+"/add", bob, map[string]any{"ids": []int64{b1.ID}}, 403, nil)

	// Removed: gone from Bob's feed and out of reach.
	e.json("DELETE", member, alice, nil, 204, nil)
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || !bf.Albums[0].Deleted {
		t.Errorf("removed member's feed: %+v", bf.Albums)
	}
	cur = bf.Cursor
	e.json("GET", path, bob, nil, 404, nil)
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", p1.ID), bob, nil, 404, nil)

	// Leaving on one's own, and the owner deleting the album.
	e.json("PUT", member, alice, map[string]any{}, 200, nil)
	e.json("DELETE", member, bob, nil, 204, nil)
	e.json("PUT", member, alice, map[string]any{}, 200, nil)
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	cur = bf.Cursor
	e.json("DELETE", path, alice, nil, 204, nil)
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || !bf.Albums[0].Deleted {
		t.Errorf("deleted album in member's feed: %+v", bf.Albums)
	}
	// Bob's own photo is still his.
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", b1.ID), bob, nil, 200, nil)
}
