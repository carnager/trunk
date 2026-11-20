package server

import (
	"bytes"
	"context"
	"database/sql"
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/carnager/trunk/internal/media"
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

func TestDirectShares(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.login("alice", "phone"), e.login("bob", "phone")
	p1 := e.upload(alice, "a.jpg", testJPEG(t, 32, 32, 71, false), 201)
	p2 := e.upload(alice, "b.jpg", testJPEG(t, 32, 32, 72, false), 201)
	var users struct{ Users []UserRef }
	e.json("GET", "/api/v1/users", alice, nil, 200, &users)
	bobID := users.Users[0].ID

	var al Album
	e.json("POST", "/api/v1/shares", alice, map[string]any{"user_id": bobID, "asset_ids": []int64{p1.ID}}, 200, &al)
	if al.Kind != "direct" || al.Role != "owner" || al.Name != "bob" || len(al.Members) != 1 || al.Members[0].CanAdd {
		t.Fatalf("sender's view: %+v", al)
	}
	var again Album
	e.json("POST", "/api/v1/shares", alice, map[string]any{"user_id": bobID, "asset_ids": []int64{p2.ID}}, 200, &again)
	if again.ID != al.ID || fmt.Sprint(again.AssetIDs) != fmt.Sprint([]int64{p2.ID, p1.ID}) {
		t.Errorf("second send: %+v", again)
	}
	path := fmt.Sprintf("/api/v1/albums/%d", al.ID)

	var bf feed
	e.json("GET", "/api/v1/changes", bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || bf.Albums[0].Kind != "direct" || bf.Albums[0].Owner.Name != "alice" || bf.Albums[0].CanAdd {
		t.Fatalf("recipient's feed: %+v", bf.Albums)
	}
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d/thumb/small", p1.ID), bob, nil, 200, nil)
	b1 := e.upload(bob, "c.jpg", testJPEG(t, 32, 32, 73, false), 201)
	e.json("POST", path+"/add", bob, map[string]any{"ids": []int64{b1.ID}}, 403, nil)

	// Not an ordinary album: no name, no extra members, no sending to oneself.
	e.json("PATCH", path, alice, map[string]string{"name": "x"}, 400, nil)
	e.json("PUT", fmt.Sprintf("%s/members/%d", path, bobID), alice, map[string]any{}, 400, nil)
	var me struct{ ID int64 }
	e.json("GET", "/api/v1/me", alice, nil, 200, &me)
	e.json("POST", "/api/v1/shares", alice, map[string]any{"user_id": me.ID, "asset_ids": []int64{p1.ID}}, 400, nil)
	e.json("POST", "/api/v1/shares", bob, map[string]any{"user_id": me.ID, "asset_ids": []int64{p1.ID}}, 400, nil) // not bob's photo

	// Hidden by the recipient, back with the next photo.
	e.json("DELETE", fmt.Sprintf("%s/members/%d", path, bobID), bob, nil, 204, nil)
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", p1.ID), bob, nil, 404, nil)
	p3 := e.upload(alice, "d.jpg", testJPEG(t, 32, 32, 74, false), 201)
	e.json("POST", "/api/v1/shares", alice, map[string]any{"user_id": bobID, "asset_ids": []int64{p3.ID}}, 200, &again)
	if again.ID != al.ID {
		t.Error("a new direct album instead of the old one")
	}
	e.json("GET", path, bob, nil, 200, nil)

	// The sender ends it.
	e.json("GET", "/api/v1/changes", bob, nil, 200, &bf)
	cur := bf.Cursor
	e.json("DELETE", path, alice, nil, 204, nil)
	e.json("GET", fmt.Sprintf("/api/v1/changes?since=%d", cur), bob, nil, 200, &bf)
	if len(bf.Albums) != 1 || !bf.Albums[0].Deleted {
		t.Errorf("ended share in recipient's feed: %+v", bf.Albums)
	}
}

func TestPublicLinks(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.login("alice", "phone"), e.login("bob", "phone")
	p1 := e.upload(alice, "berlin.jpg", testJPEG(t, 64, 48, 81, true), 201) // EXIF GPS 52.52, 13.41
	p2 := e.upload(alice, "zwei.jpg", testJPEG(t, 64, 48, 82, false), 201)
	p3 := e.upload(alice, "privat.jpg", testJPEG(t, 64, 48, 83, false), 201)
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", p1.ID), alice, map[string]string{"caption": "Am Tor"}, 200, nil)
	var al Album
	e.json("POST", "/api/v1/albums", alice, map[string]any{"name": "Ausflug", "asset_ids": []int64{p1.ID, p2.ID}}, 201, &al)
	// Bob contributes to the album: his photo must not go public with Alice's link.
	var users struct{ Users []UserRef }
	e.json("GET", "/api/v1/users", alice, nil, 200, &users)
	e.json("PUT", fmt.Sprintf("/api/v1/albums/%d/members/%d", al.ID, users.Users[0].ID), alice, map[string]any{}, 200, nil)
	b1 := e.upload(bob, "bob.jpg", testJPEG(t, 64, 48, 84, false), 201)
	e.json("POST", fmt.Sprintf("/api/v1/albums/%d/add", al.ID), bob, map[string]any{"ids": []int64{b1.ID}, "end": true}, 200, nil)

	pageIn := func(lang, path string) (int, string) {
		res := e.do("GET", path, "", nil, map[string]string{"Accept-Language": lang})
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	page := func(path string) (int, string) { return pageIn("de-DE,de;q=0.9,en;q=0.5", path) }
	var link Link
	e.json("POST", "/api/v1/links", alice, map[string]any{"album_id": al.ID}, 201, &link)
	if link.Title != "Ausflug" || !link.StripLocation || link.Path != "/s/"+link.Token || len(link.Token) < 20 {
		t.Fatalf("album link: %+v", link)
	}
	code, body := page(link.Path)
	if code != 200 || !strings.Contains(body, "Ausflug") || !strings.Contains(body, "Am Tor") || !strings.Contains(body, "Berlin") ||
		!strings.Contains(body, "Original laden") {
		t.Fatalf("page: %d %.300s", code, body)
	}
	// In the visitor's language: English unless the browser prefers German.
	if _, en := pageIn("en-US,en;q=0.9", link.Path); !strings.Contains(en, `lang="en"`) || !strings.Contains(en, "Download original") ||
		!strings.Contains(en, "2 photos") || !strings.Contains(en, "shared by") || strings.Contains(en, "Original laden") {
		t.Errorf("English page: %.400s", en)
	}
	if !strings.Contains(body, `lang="de"`) || !strings.Contains(body, "2 Fotos") {
		t.Errorf("German page: %.400s", body)
	}
	if strings.Contains(body, "52.52") || strings.Contains(body, "13.41") {
		t.Error("page shows coordinates")
	}
	for _, id := range []int64{p1.ID, p2.ID} {
		if !strings.Contains(body, fmt.Sprintf("/t/%d/medium", id)) {
			t.Errorf("photo %d missing from page", id)
		}
	}
	if strings.Contains(body, fmt.Sprintf("/t/%d/", b1.ID)) || strings.Contains(body, fmt.Sprintf("/t/%d/", p3.ID)) {
		t.Error("page shows a photo that is not alice's album photo")
	}
	if c, _ := page(fmt.Sprintf("%s/t/%d/preview", link.Path, p1.ID)); c != 200 {
		t.Errorf("thumb: %d", c)
	}
	// The original, by default without where it was taken.
	download := func(path string) media.Info {
		t.Helper()
		res := e.do("GET", path, "", nil, nil)
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("download %s: %d %q", path, res.StatusCode, res.Header.Get("Content-Disposition"))
		}
		f := filepath.Join(t.TempDir(), "dl.jpg")
		b, _ := io.ReadAll(res.Body)
		os.WriteFile(f, b, 0o644)
		info, _ := media.Extract(f, "dl.jpg")
		return info
	}
	if info := download(fmt.Sprintf("%s/f/%d?dl", link.Path, p1.ID)); info.Lat != nil || info.Make != "Google" || info.TakenAt.IsZero() {
		t.Errorf("cleaned original: lat %v make %q taken %v", info.Lat, info.Make, info.TakenAt)
	}
	for _, id := range []int64{b1.ID, p3.ID} {
		if c, _ := page(fmt.Sprintf("%s/t/%d/small", link.Path, id)); c != 404 {
			t.Errorf("photo %d through the link: %d", id, c)
		}
	}
	// Trashed photos leave the link.
	e.json("POST", "/api/v1/assets/trash", alice, map[string][]int64{"ids": {p2.ID}}, 204, nil)
	if _, body := page(link.Path); strings.Contains(body, fmt.Sprintf("/t/%d/", p2.ID)) {
		t.Error("trashed photo still on the page")
	}

	// A selection that keeps the location.
	var sel Link
	e.json("POST", "/api/v1/links", alice, map[string]any{"asset_ids": []int64{p1.ID, p3.ID}, "strip_location": false, "title": "Mit Ort"}, 201, &sel)
	if sel.StripLocation || fmt.Sprint(sel.AssetIDs) != fmt.Sprint([]int64{p1.ID, p3.ID}) {
		t.Errorf("selection link: %+v", sel)
	}
	if c, body := page(sel.Path); c != 200 || !strings.Contains(body, "Original laden") {
		t.Errorf("selection page: %d", c)
	}
	if info := download(fmt.Sprintf("%s/f/%d?dl", sel.Path, p1.ID)); info.Lat == nil {
		t.Error("location removed although the link keeps it")
	}
	// peek 0.11 still sends "download"; it is accepted and means nothing now.
	e.json("POST", "/api/v1/links", alice, map[string]any{"asset_ids": []int64{p3.ID}, "download": false}, 201, nil)

	// Only one's own.
	e.json("POST", "/api/v1/links", bob, map[string]any{"album_id": al.ID}, 404, nil)
	e.json("POST", "/api/v1/links", bob, map[string]any{"asset_ids": []int64{p1.ID}}, 400, nil)

	var list struct{ Links []Link }
	e.json("GET", "/api/v1/links", alice, nil, 200, &list)
	if len(list.Links) != 3 {
		t.Errorf("links: %+v", list)
	}

	// Revoked and expired links show nothing.
	e.json("DELETE", "/api/v1/links/"+sel.Token, alice, nil, 204, nil)
	if c, body := page(sel.Path); c != 404 || !strings.Contains(body, "gilt nicht mehr") {
		t.Errorf("revoked page: %d", c)
	}
	if c, _ := page(fmt.Sprintf("%s/t/%d/small", sel.Path, p1.ID)); c != 404 {
		t.Errorf("revoked thumb: %d", c)
	}
	e.srv.db.Exec(`UPDATE links SET expires_at = 1 WHERE token = ?`, link.Token)
	if c, _ := page(link.Path); c != 404 {
		t.Errorf("expired page: %d", c)
	}
	e.json("GET", "/api/v1/links", alice, nil, 200, &list)
	if len(list.Links) != 1 || list.Links[0].Token == link.Token || list.Links[0].Token == sel.Token {
		t.Errorf("gone links still listed: %+v", list)
	}
	if c, _ := page("/s/nonsense"); c != 404 {
		t.Errorf("unknown link: %d", c)
	}
}

func TestMaintenance(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tok := e.login("alice", "phone")
	a := e.upload(tok, "IMG_20230105_101112.jpg", testJPEG(t, 64, 48, 91, true), 201)
	b := e.upload(tok, "b.jpg", testJPEG(t, 64, 48, 92, false), 201)
	c := e.upload(tok, "c.jpg", testJPEG(t, 64, 48, 93, false), 201)
	res := e.do("PUT", fmt.Sprintf("/api/v1/assets/%d/edit", c.ID), tok, bytes.NewReader(testJPEG(t, 40, 40, 94, false)), nil)
	if res.StatusCode != 200 {
		t.Fatalf("edit: %d", res.StatusCode)
	}

	// Backup: a consistent copy, also while the server runs.
	dir := t.TempDir()
	if err := e.srv.Backup(ctx, filepath.Join(dir, "copy.db")); err != nil {
		t.Fatal(err)
	}
	copyDB, _ := sql.Open("sqlite", filepath.Join(dir, "copy.db"))
	var n int
	copyDB.QueryRow(`SELECT COUNT(*) FROM assets`).Scan(&n)
	copyDB.Close()
	if n != 3 {
		t.Errorf("backup has %d assets", n)
	}
	if err := e.srv.Backup(ctx, filepath.Join(dir, "copy.db")); err == nil {
		t.Error("backup overwrote an existing file")
	}
	backups := filepath.Join(e.srv.cfg.DataDir, "backups")
	os.MkdirAll(backups, 0o755)
	for i := 1; i <= 9; i++ {
		os.WriteFile(filepath.Join(backups, fmt.Sprintf("trunk-2020-01-0%d.db", i)), nil, 0o644)
	}
	e.srv.dailyBackup(ctx)
	e.srv.dailyBackup(ctx) // once a day
	kept, _ := filepath.Glob(filepath.Join(backups, "trunk-*.db"))
	if len(kept) != keepBackups || !strings.Contains(kept[len(kept)-1], time.Now().Format("2006-01-02")) {
		t.Errorf("backups kept: %v", kept)
	}

	// Verify: all well, then damage.
	if p, err := e.srv.Verify(ctx, VerifyOptions{Hash: true}); err != nil || len(p) != 0 {
		t.Fatalf("clean library: %v %+v", err, p)
	}
	orig := func(as Asset) string {
		var rel string
		e.srv.db.QueryRow(`SELECT path FROM assets WHERE id = ?`, as.ID).Scan(&rel)
		return filepath.Join(e.srv.originals, filepath.FromSlash(rel))
	}
	flipped, _ := os.ReadFile(orig(a))
	flipped[len(flipped)/2] ^= 0xff
	os.WriteFile(orig(a), flipped, 0o644)
	os.Truncate(orig(b), 10)
	var firstOfEdit string
	e.srv.db.QueryRow(`SELECT orig_path FROM assets WHERE id = ?`, c.ID).Scan(&firstOfEdit)
	os.Remove(filepath.Join(e.srv.originals, filepath.FromSlash(firstOfEdit)))
	os.WriteFile(filepath.Join(e.srv.originals, "alice", "stray.jpg"), testJPEG(t, 20, 20, 95, false), 0o644)

	kinds := func(p []Problem) string {
		var k []string
		for _, x := range p {
			k = append(k, x.Kind)
		}
		sort.Strings(k)
		return strings.Join(k, " ")
	}
	p, _ := e.srv.Verify(ctx, VerifyOptions{})
	if kinds(p) != "missing size unknown" {
		t.Errorf("quick verify: %s", kinds(p))
	}
	p, _ = e.srv.Verify(ctx, VerifyOptions{Hash: true})
	if kinds(p) != "hash missing size unknown" {
		t.Errorf("full verify: %s", kinds(p))
	}

	// Reindex: the database is gone, the files are not.
	data2 := t.TempDir()
	if err := os.CopyFS(filepath.Join(data2, "originals"), os.DirFS(e.srv.originals)); err != nil {
		t.Fatal(err)
	}
	srv2, err := New(Config{DataDir: data2}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	srv2.db.Exec(`INSERT INTO users (name, pw_hash, created_at) VALUES ('alice', 'x', 0)`)
	os.MkdirAll(filepath.Join(data2, "originals", "carol"), 0o755)
	os.WriteFile(filepath.Join(data2, "originals", "carol", "x.jpg"), testJPEG(t, 20, 20, 96, false), 0o644)
	r, err := srv2.Reindex(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// a (damaged), b's stub (still starts like a JPEG: taken in, damaged), c's edit, the stray file;
	// carol has no account.
	if r.Added != 4 || len(r.Skipped) != 1 {
		t.Errorf("reindex: %+v", r)
	}
	var taken int64
	srv2.db.QueryRow(`SELECT taken_at FROM assets WHERE orig_name = 'IMG_20230105_101112.jpg'`).Scan(&taken)
	if time.UnixMilli(taken).UTC().Year() != 2024 { // from EXIF, not from the name or the file
		t.Errorf("reindexed date: %v", time.UnixMilli(taken).UTC())
	}
	again, _ := srv2.Reindex(ctx, nil)
	if again.Added != 0 {
		t.Errorf("second reindex added %d", again.Added)
	}
	if p, _ := srv2.Verify(ctx, VerifyOptions{}); len(p) != 1 || p[0].Path != "carol/x.jpg" { // only what reindex could not place
		t.Errorf("after reindex: %+v", p)
	}

	// originals/ as a symlink to another disk works the same.
	data3 := t.TempDir()
	os.Symlink(filepath.Join(data2, "originals"), filepath.Join(data3, "originals"))
	srv2.Backup(ctx, filepath.Join(data3, "trunk.db"))
	srv3, err := New(Config{DataDir: data3}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer srv3.Close()
	if p, _ := srv3.Verify(ctx, VerifyOptions{}); len(p) != 1 || p[0].Path != "carol/x.jpg" {
		t.Errorf("verify through a symlink: %+v", p)
	}
}

// scene draws a picture with structure: soft blobs on a gradient, shifted by
// dx (a hand moving between burst shots) and in a palette from seed.
func scene(w, h, dx int, seed uint8, quality int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x+dx)/float64(w), float64(y)/float64(h)
			sd := float64(seed)
			v := 0.5 + 0.2*math.Sin(fx*19+sd)*math.Cos(fy*14+sd*0.7) + 0.15*math.Sin((fx+fy)*27+sd*1.3) +
				0.15*math.Cos(fx*fy*40+sd*0.3)
			img.Set(x, y, color.RGBA{uint8(v * 255), uint8((1 - v) * 200), seed, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return buf.Bytes()
}

func dark(w, h int, seed uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{seed % 3, seed % 2, 1, 255})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

func TestSimilar(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.login("alice", "phone")
	bob := e.login("bob", "phone")

	orig := e.upload(alice, "IMG_20240601_120000.jpg", scene(800, 600, 0, 10, 92), 201)
	copy := e.upload(alice, "IMG-20240603-WA0001.jpg", scene(400, 300, 0, 10, 60), 201) // a WhatsApp copy, days later
	other := e.upload(alice, "IMG_20240601_120500.jpg", scene(800, 600, 0, 200, 92), 201)
	b1 := e.upload(alice, "IMG_20240710_090000.jpg", scene(800, 600, 0, 77, 90), 201)
	b2 := e.upload(alice, "IMG_20240710_090002.jpg", scene(800, 600, 8, 77, 90), 201) // moved a little
	d1 := e.upload(alice, "IMG_20230101_100000.jpg", dark(800, 600, 1), 201)
	d2 := e.upload(alice, "IMG_20240101_100000.jpg", dark(800, 600, 2), 201)
	theirs := e.upload(bob, "IMG_20240601_120000.jpg", scene(400, 300, 0, 10, 70), 201) // bob's copy: not alice's business
	_ = theirs

	type result struct {
		Groups  []SimilarGroup
		Pending int
	}
	var res result
	// The thumbnail workers fingerprint new uploads; the backfill catches the rest.
	e.srv.fillFingerprints(ctx)
	e.json("GET", "/api/v1/similar", alice, nil, 200, &res)
	if res.Pending != 0 {
		t.Fatalf("pending: %d", res.Pending)
	}
	ids := func(g SimilarGroup) string {
		s := append([]int64{}, g.AssetIDs...)
		slices.Sort(s)
		return fmt.Sprint(s)
	}
	got := map[string]SimilarGroup{}
	for _, g := range res.Groups {
		got[ids(g)] = g
	}
	want := func(kind string, keep Asset, as ...Asset) {
		t.Helper()
		var s []int64
		for _, a := range as {
			s = append(s, a.ID)
		}
		slices.Sort(s)
		g, ok := got[fmt.Sprint(s)]
		if !ok {
			t.Errorf("no group %v; got %+v", s, res.Groups)
			return
		}
		if g.Kind != kind || g.Keep != keep.ID {
			t.Errorf("group %v: kind %s keep %d, want %s keep %d", s, g.Kind, g.Keep, kind, keep.ID)
		}
	}
	want("same", orig, orig, copy)            // the larger one
	want("burst", best2(b1, b2, got), b1, b2) // equal shots: either
	if len(res.Groups) != 2 {
		t.Errorf("groups: %+v (other %d, dark %d %d)", res.Groups, other.ID, d1.ID, d2.ID)
	}
	if res.Groups[0].AssetIDs[0] != b2.ID && res.Groups[0].AssetIDs[0] != b1.ID {
		t.Errorf("not newest first: %+v", res.Groups)
	}

	// Favourites and edits weigh after pixels: of two equal shots, the favourite stays.
	fav := b1
	if got[ids(SimilarGroup{AssetIDs: []int64{b1.ID, b2.ID}})].Keep == b1.ID {
		fav = b2
	}
	e.json("PATCH", fmt.Sprintf("/api/v1/assets/%d", fav.ID), alice, map[string]any{"favorite": true}, 200, nil)
	e.json("GET", "/api/v1/similar", alice, nil, 200, &res)
	for _, g := range res.Groups {
		if g.Kind == "burst" && g.Keep != fav.ID {
			t.Errorf("favourite not kept: %+v", g)
		}
	}

	// "Not duplicates" makes a group go away; trashing one of two does too.
	e.json("POST", "/api/v1/similar/ignore", alice, map[string][]int64{"ids": {b1.ID, b2.ID}}, 204, nil)
	e.json("POST", "/api/v1/assets/trash", alice, map[string][]int64{"ids": {copy.ID}}, 204, nil)
	e.json("GET", "/api/v1/similar", alice, nil, 200, &res)
	if len(res.Groups) != 0 {
		t.Errorf("after ignore and trash: %+v", res.Groups)
	}
	var bobs result
	e.json("GET", "/api/v1/similar", bob, nil, 200, &bobs)
	if len(bobs.Groups) != 0 {
		t.Errorf("bob sees groups: %+v", bobs.Groups)
	}
}

// best2 is whichever of a and b the server suggested in their group.
func best2(a, b Asset, got map[string]SimilarGroup) Asset {
	for _, g := range got {
		if g.Keep == b.ID {
			return b
		}
	}
	return a
}

func TestMotionPhotos(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	e := newEnv(t)
	ctx := context.Background()
	alice := e.login("alice", "phone")
	bob := e.login("bob", "phone")

	vp := filepath.Join(t.TempDir(), "v.mp4")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=10",
		"-pix_fmt", "yuv420p", vp).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	video, _ := os.ReadFile(vp)
	jpg := testJPEG(t, 64, 48, 120, false)
	xmp := fmt.Sprintf(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF><rdf:Description GCamera:MotionPhoto="1"><Container:Directory><rdf:Seq>`+
		`<rdf:li><Container:Item Item:Mime="image/jpeg" Item:Semantic="Primary"/></rdf:li>`+
		`<rdf:li><Container:Item Item:Mime="video/mp4" Item:Semantic="MotionPhoto" Item:Length="%d"/></rdf:li>`+
		`</rdf:Seq></Container:Directory></rdf:Description></rdf:RDF></x:xmpmeta>`, len(video))
	body := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), xmp...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(body)+2))
	motion := append(append(append([]byte{0xFF, 0xD8}, seg...), body...), jpg[2:]...)
	motion = append(motion, video...)

	m := e.upload(alice, "PXL_20240601_120000000.MP.jpg", motion, 201)
	still := e.upload(alice, "still.jpg", testJPEG(t, 64, 48, 121, false), 201)
	if !m.Motion || still.Motion {
		t.Fatalf("motion flags: %v %v", m.Motion, still.Motion)
	}
	get := func(path, tok string, hdr map[string]string) (int, []byte, http.Header) {
		res := e.do("GET", path, tok, nil, hdr)
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b, res.Header
	}
	code, b, h := get(fmt.Sprintf("/api/v1/assets/%d/motion", m.ID), alice, nil)
	if code != 200 || !bytes.Equal(b, video) || h.Get("Content-Type") != "video/mp4" {
		t.Fatalf("motion video: %d %d bytes %q", code, len(b), h.Get("Content-Type"))
	}
	code, b, _ = get(fmt.Sprintf("/api/v1/assets/%d/motion", m.ID), alice, map[string]string{"Range": "bytes=4-11"})
	if code != 206 || string(b) != string(video[4:12]) {
		t.Errorf("range: %d %q", code, b)
	}
	if c, _, _ := get(fmt.Sprintf("/api/v1/assets/%d/motion", still.ID), alice, nil); c != 404 {
		t.Errorf("still: %d", c)
	}
	if c, _, _ := get(fmt.Sprintf("/api/v1/assets/%d/motion", m.ID), bob, nil); c != 404 {
		t.Errorf("someone else's: %d", c)
	}

	// An edit is a plain picture; the video still comes from the first upload.
	res := e.do("PUT", fmt.Sprintf("/api/v1/assets/%d/edit", m.ID), alice, bytes.NewReader(testJPEG(t, 40, 40, 122, false)), nil)
	if res.StatusCode != 200 {
		t.Fatalf("edit: %d", res.StatusCode)
	}
	if code, b, _ := get(fmt.Sprintf("/api/v1/assets/%d/motion", m.ID), alice, nil); code != 200 || !bytes.Equal(b, video) {
		t.Errorf("motion after edit: %d %d", code, len(b))
	}

	// Photos from before trunk knew motion photos are looked at once, and clients hear of it.
	e.srv.db.Exec(`UPDATE assets SET motion_offset = NULL, motion_length = NULL`)
	var seqBefore, stillSeq int64
	e.srv.db.QueryRow(`SELECT seq FROM assets WHERE id = ?`, m.ID).Scan(&seqBefore)
	e.srv.db.QueryRow(`SELECT seq FROM assets WHERE id = ?`, still.ID).Scan(&stillSeq)
	e.srv.fillMotion(ctx)
	var after Asset
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", m.ID), alice, nil, 200, &after)
	var seqAfter, stillAfter int64
	var unchecked int
	e.srv.db.QueryRow(`SELECT seq FROM assets WHERE id = ?`, m.ID).Scan(&seqAfter)
	e.srv.db.QueryRow(`SELECT seq FROM assets WHERE id = ?`, still.ID).Scan(&stillAfter)
	e.srv.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE motion_offset IS NULL`).Scan(&unchecked)
	if !after.Motion || seqAfter <= seqBefore || stillAfter != stillSeq || unchecked != 0 {
		t.Errorf("backfill: motion %v seq %d→%d, still seq %d→%d, unchecked %d", after.Motion, seqBefore, seqAfter, stillSeq, stillAfter, unchecked)
	}

	// The same video as a file of its own (Takeout exports it apart too) is a
	// copy: the clean-up offers it, keeping the photo.
	clip := e.upload(alice, "PXL_20240601_120000000.MP4", video, 201)
	e.srv.fillMotionHashes(ctx)
	type result struct{ Groups []SimilarGroup }
	var groups result
	e.json("GET", "/api/v1/similar", alice, nil, 200, &groups)
	if len(groups.Groups) != 1 || groups.Groups[0].Kind != "motion" || groups.Groups[0].Keep != m.ID ||
		fmt.Sprint(groups.Groups[0].AssetIDs) != fmt.Sprint([]int64{m.ID, clip.ID}) {
		t.Fatalf("motion copy: %+v", groups.Groups)
	}
	var bobs result
	e.json("GET", "/api/v1/similar", bob, nil, 200, &bobs)
	if len(bobs.Groups) != 0 {
		t.Errorf("bob sees alice's: %+v", bobs.Groups)
	}
	e.json("POST", "/api/v1/similar/ignore", alice, map[string][]int64{"ids": {m.ID, clip.ID}}, 204, nil)
	e.json("GET", "/api/v1/similar", alice, nil, 200, &groups)
	if len(groups.Groups) != 0 {
		t.Errorf("after ignore: %+v", groups.Groups)
	}
}

func TestMotionFileDeletedWithPhoto(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice := e.login("alice", "phone")
	p := e.upload(alice, "PXL_1.MP.jpg", testJPEG(t, 64, 48, 130, false), 201)
	var uid int64
	e.srv.db.QueryRow(`SELECT user_id FROM assets WHERE id = ?`, p.ID).Scan(&uid)
	a, err := e.srv.getAsset(ctx, uid, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(e.srv.cfg.DataDir, "uploads", "clip")
	os.MkdirAll(filepath.Dir(tmp), 0o755)
	// Not a video: refused, and the file stays where it was.
	os.WriteFile(tmp, []byte("not a video at all"), 0o644)
	if err := e.srv.attachMotion(ctx, a, tmp); err == nil {
		t.Fatal("attached something that is no video")
	}
	vp := filepath.Join(t.TempDir(), "v.mp4")
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=1:size=160x120:rate=10", "-pix_fmt", "yuv420p", vp).Run()
	b, _ := os.ReadFile(vp)
	os.WriteFile(tmp, b, 0o644)
	if err := e.srv.attachMotion(ctx, a, tmp); err != nil {
		t.Fatal(err)
	}
	var got Asset
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", p.ID), alice, nil, 200, &got)
	res := e.do("GET", fmt.Sprintf("/api/v1/assets/%d/motion", p.ID), alice, nil, nil)
	body, _ := io.ReadAll(res.Body)
	if !got.Motion || res.StatusCode != 200 || !bytes.Equal(body, b) {
		t.Fatalf("attached clip: motion %v, %d, %d bytes", got.Motion, res.StatusCode, len(body))
	}
	var rel string
	e.srv.db.QueryRow(`SELECT motion_path FROM assets WHERE id = ?`, p.ID).Scan(&rel)
	clipFile := filepath.Join(e.srv.originals, filepath.FromSlash(rel))
	if _, err := os.Stat(clipFile); err != nil {
		t.Fatal(err)
	}
	e.json("POST", "/api/v1/assets/trash", alice, map[string][]int64{"ids": {p.ID}}, 204, nil)
	e.json("POST", "/api/v1/assets/delete", alice, map[string][]int64{"ids": {p.ID}}, 200, nil)
	if _, err := os.Stat(clipFile); !os.IsNotExist(err) {
		t.Errorf("clip left behind after deleting its photo: %v", err)
	}
}

func TestAdjust(t *testing.T) {
	e := newEnv(t)
	alice := e.login("alice", "phone")
	bob := e.login("bob", "phone")
	// A scanned series: no EXIF, dates from the file names.
	a := e.upload(alice, "IMG_20240101_100000.jpg", testJPEG(t, 64, 48, 140, false), 201)
	b := e.upload(alice, "IMG_20240101_100500.jpg", testJPEG(t, 64, 48, 141, false), 201)
	gps := e.upload(alice, "gps.jpg", testJPEG(t, 64, 48, 142, true), 201) // has a position (and a place)
	theirs := e.upload(bob, "x.jpg", testJPEG(t, 64, 48, 143, false), 201)

	type out struct{ Assets []Asset }
	var res out
	// The earliest gets the new time; the other keeps its five minutes.
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{b.ID, a.ID}, "local_time": "1987-07-12T15:30:00", "tz_offset": 120}, 200, &res)
	byID := map[int64]Asset{}
	for _, x := range res.Assets {
		byID[x.ID] = x
	}
	if byID[a.ID].LocalTime != "1987-07-12T15:30:00" || byID[b.ID].LocalTime != "1987-07-12T15:35:00" ||
		byID[a.ID].TakenAt.Format(time.RFC3339) != "1987-07-12T13:30:00Z" || *byID[a.ID].TZOffset != 120 {
		t.Fatalf("set time: %+v / %+v", byID[a.ID], byID[b.ID])
	}
	// A clock an hour behind.
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{a.ID}, "shift_minutes": 60}, 200, &res)
	if res.Assets[0].LocalTime != "1987-07-12T16:30:00" {
		t.Errorf("shift: %s", res.Assets[0].LocalTime)
	}

	// Where: a town found by name; a wrong position goes.
	var found struct{ Places []PlaceHit }
	e.json("GET", "/api/v1/places?q=wien", alice, nil, 200, &found)
	if len(found.Places) == 0 || found.Places[0].CC != "AT" {
		t.Fatalf("places: %+v", found)
	}
	wien := found.Places[0].ID
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{gps.ID, a.ID}, "place_id": wien}, 200, &res)
	for _, x := range res.Assets {
		if x.Place == nil || x.Place.CC != "AT" || x.Lat != nil {
			t.Errorf("place set: %+v", x)
		}
	}

	// No place at all; the background filler must not bring it back.
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{gps.ID}, "clear_place": true}, 200, &res)
	e.srv.fillPlaces(context.Background())
	var after Asset
	e.json("GET", fmt.Sprintf("/api/v1/assets/%d", gps.ID), alice, nil, 200, &after)
	if after.Place != nil || after.Lat != nil {
		t.Errorf("place cleared: %+v %v", after.Place, after.Lat)
	}

	// The change reaches other devices.
	var changes struct {
		Assets []Asset
	}
	e.json("GET", "/api/v1/changes?since=0", alice, nil, 200, &changes)
	inFeed := false
	for _, x := range changes.Assets {
		if x.ID == b.ID {
			inFeed = true
			if x.LocalTime != "1987-07-12T15:35:00" {
				t.Errorf("feed has old time: %s", x.LocalTime)
			}
		}
	}
	if !inFeed {
		t.Error("adjusted photo not in the feed")
	}

	// Only one's own; nonsense refused.
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{theirs.ID}, "shift_minutes": 5}, 404, nil)
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{a.ID}, "local_time": "yesterday"}, 400, nil)
	e.json("POST", "/api/v1/assets/adjust", alice, map[string]any{"ids": []int64{a.ID}, "place_id": 1}, 400, nil)
}
