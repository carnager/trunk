// Package takeout imports a Google Photos export from Google Takeout.
//
// A Takeout is one or more zip or tgz files (or their unpacked folders) holding
// "Takeout/Google Photos/<folder>/". Year folders ("Photos from 2023", "Fotos
// von 2023") hold everything; album folders hold copies of the album's photos
// plus a JSON file with the album title. Next to most photos sits a JSON
// sidecar with what Google knows: the exact time, the location, whether it is
// a favourite. Edited photos come as "<name>-edited.jpg" (localised) beside
// the original.
//
// The import uses the files as they are, fills in time and place from the
// sidecars where the files lack them, turns "-edited" copies into edits of
// their originals, and recreates the albums. Content already in the library
// is skipped, so it is safe to run again, e.g. after a failed zip.
package takeout

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/gzip"
)

// Entry is one file of the export, from a folder, a zip or a tar archive.
type Entry struct {
	Path string // slash-separated, relative to the export root
	Size int64
	// Open reads the file. It is nil for media inside a tar archive (.tgz),
	// which can only be read front to back: Run streams those instead.
	Open func() (io.ReadCloser, error)

	archive *tarArchive
}

// Sources reads the given folders, zip files and tar archives (.tgz, .tar).
// Tar archives are read through once here to list them, keeping only the
// small JSON files; progress goes to log. Close the returned closer when done.
func Sources(inputs []string, log io.Writer) ([]Entry, io.Closer, error) {
	var entries []Entry
	var zips multiCloser
	fail := func(err error) ([]Entry, io.Closer, error) {
		zips.Close()
		return nil, nil, err
	}
	for _, in := range inputs {
		st, err := os.Stat(in)
		if err != nil {
			return fail(err)
		}
		if st.IsDir() {
			err := filepath.WalkDir(in, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(in, p)
				entries = append(entries, Entry{
					Path: filepath.ToSlash(rel),
					Size: info.Size(),
					Open: func() (io.ReadCloser, error) { return os.Open(p) },
				})
				return nil
			})
			if err != nil {
				return fail(err)
			}
			continue
		}
		kind, err := sniffArchive(in)
		if err != nil {
			return fail(err)
		}
		if kind == "zip" {
			z, err := zip.OpenReader(in)
			if err != nil {
				return fail(fmt.Errorf("%s: %w", in, err))
			}
			zips = append(zips, z)
			for _, f := range z.File {
				if !f.FileInfo().IsDir() {
					entries = append(entries, Entry{Path: f.Name, Size: int64(f.UncompressedSize64), Open: f.Open})
				}
			}
			continue
		}
		a := &tarArchive{path: in, gzip: kind == "tgz"}
		list, err := a.index(log)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", in, err))
		}
		entries = append(entries, list...)
	}
	return entries, zips, nil
}

func sniffArchive(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return "zip", nil
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return "tgz", nil
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return "tar", nil
	}
	return "", fmt.Errorf("%s: not a folder, zip or tgz", path)
}

// tarArchive is a .tgz or .tar export, read front to back.
type tarArchive struct {
	path string
	gzip bool
}

// walk calls fn for each regular file, with a reader for its content.
func (a *tarArchive) walk(fn func(h *tar.Header, r io.Reader) error) error {
	f, err := os.Open(a.path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if a.gzip {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// index lists the archive, keeping the JSON files in memory so the plan can
// read them without another pass.
func (a *tarArchive) index(log io.Writer) ([]Entry, error) {
	var list []Entry
	var bytesSeen int64
	last := time.Now()
	err := a.walk(func(h *tar.Header, r io.Reader) error {
		e := Entry{Path: h.Name, Size: h.Size, archive: a}
		if strings.EqualFold(path.Ext(h.Name), ".json") && h.Size <= 1<<20 {
			data, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			e.Open = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
		}
		list = append(list, e)
		bytesSeen += h.Size
		if time.Since(last) > 3*time.Second {
			last = time.Now()
			fmt.Fprintf(log, "  listing %s: %d files, %.1f GB so far\n", filepath.Base(a.path), len(list), float64(bytesSeen)/(1<<30))
		}
		return nil
	})
	if err == nil {
		fmt.Fprintf(log, "  %s: %d files, %.1f GB\n", filepath.Base(a.path), len(list), float64(bytesSeen)/(1<<30))
	}
	return list, err
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	for _, c := range m {
		c.Close()
	}
	return nil
}

// Sidecar is the part of Google's per-photo JSON that matters here.
type Sidecar struct {
	Title          string `json:"title"`
	Description    string `json:"description"`
	PhotoTakenTime *struct {
		Timestamp string `json:"timestamp"`
	} `json:"photoTakenTime"`
	GeoData     geo  `json:"geoData"`
	GeoDataExif geo  `json:"geoDataExif"`
	Favorited   bool `json:"favorited"`
	Trashed     bool `json:"trashed"`
}

type geo struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Altitude  float64 `json:"altitude"`
}

// Taken is the exact capture time, or zero.
func (s *Sidecar) Taken() time.Time {
	if s == nil || s.PhotoTakenTime == nil {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(s.PhotoTakenTime.Timestamp, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// Location is where the photo was taken, as Google has it; ok is false for
// none (Google writes zeros).
func (s *Sidecar) Location() (lat, lon, alt float64, ok bool) {
	if s == nil {
		return 0, 0, 0, false
	}
	for _, g := range []geo{s.GeoData, s.GeoDataExif} {
		if g.Latitude != 0 || g.Longitude != 0 {
			return g.Latitude, g.Longitude, g.Altitude, true
		}
	}
	return 0, 0, 0, false
}

// Item is one photo or video to import.
type Item struct {
	Entry
	Name    string   // file name
	Sidecar *Sidecar // nil if none was found
	EditOf  *Item    // set for "-edited" copies whose original is in the same folder
}

// Album is a folder Google marked as an album.
type Album struct {
	Name  string
	Items []*Item
}

// Plan is what an import will do.
type Plan struct {
	Items   []*Item // originals first, then edits
	Albums  []*Album
	Skipped []string // files that are neither media nor metadata, or trashed
}

var mediaExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".heif": true,
	".avif": true, ".bmp": true, ".dng": true,
	".mp4": true, ".m4v": true, ".mov": true, ".3gp": true, ".mkv": true, ".webm": true, ".avi": true,
}

// Folders Google uses for the trash, in some languages; their content is not imported.
var trashDirs = map[string]bool{"trash": true, "bin": true, "papierkorb": true, "corbeille": true, "papelera": true, "cestino": true, "prullenbak": true}

// Suffixes Google gives edited copies, by export language.
var editSuffixes = []string{"-edited", "-bearbeitet", "-modifié", "-editado", "-bewerkt", "-modificato", "-redigerad", "-edytowane", "-muokattu", "-redigeret"}

// NewPlan groups the entries by folder, pairs media with their sidecars and
// finds edits and albums. Unreadable sidecars are treated as missing.
func NewPlan(entries []Entry) *Plan {
	dirs := map[string][]Entry{}
	var order []string
	for _, e := range entries {
		d := path.Dir(e.Path)
		if _, ok := dirs[d]; !ok {
			order = append(order, d)
		}
		dirs[d] = append(dirs[d], e)
	}
	slices.Sort(order)

	p := &Plan{}
	var edits []*Item
	for _, d := range order {
		if trashDirs[strings.ToLower(path.Base(d))] {
			for _, e := range dirs[d] {
				p.Skipped = append(p.Skipped, e.Path+" (trash)")
			}
			continue
		}
		sidecars := map[string]*Sidecar{} // by JSON name without ".json"
		albumName := ""
		var media []Entry
		for _, e := range dirs[d] {
			name := path.Base(e.Path)
			ext := strings.ToLower(path.Ext(name))
			switch {
			case ext == ".json":
				sc := readSidecar(e)
				if sc == nil {
					continue
				}
				if sc.PhotoTakenTime == nil {
					// Album metadata (metadata.json, Metadaten.json, …) has a title but no capture time.
					if sc.Title != "" {
						albumName = sc.Title
					}
					continue
				}
				sidecars[strings.TrimSuffix(name, path.Ext(name))] = sc
			case mediaExt[ext]:
				media = append(media, e)
			default:
				p.Skipped = append(p.Skipped, e.Path)
			}
		}

		byName := map[string]*Item{}
		var items, dirEdits []*Item
		for _, e := range media {
			it := &Item{Entry: e, Name: path.Base(e.Path)}
			byName[strings.ToLower(it.Name)] = it
			if orig, ok := editedOriginal(it.Name); ok {
				it.Sidecar = MatchSidecar(orig, sidecars)
				dirEdits = append(dirEdits, it)
				continue
			}
			it.Sidecar = MatchSidecar(it.Name, sidecars)
			if it.Sidecar != nil && it.Sidecar.Trashed {
				p.Skipped = append(p.Skipped, e.Path+" (trash)")
				continue
			}
			items = append(items, it)
		}
		for _, it := range dirEdits {
			orig, _ := editedOriginal(it.Name)
			if o := byName[strings.ToLower(orig)]; o != nil {
				it.EditOf = o
				edits = append(edits, it)
			} else if o := findByStem(orig, items); o != nil {
				it.EditOf = o
				edits = append(edits, it)
			} else {
				items = append(items, it) // no original here: import the edit as a photo of its own
			}
		}
		p.Items = append(p.Items, items...)
		if albumName != "" {
			al := &Album{Name: albumName}
			al.Items = append(al.Items, items...)
			p.Albums = append(p.Albums, al)
		}
	}
	p.Items = append(p.Items, edits...)
	shareSidecars(p.Items)
	return p
}

// shareSidecars gives copies without a sidecar the one of their twin. Album
// folders hold byte-identical copies of photos in the year folders, not
// always with a sidecar of their own; whichever copy is stored first must not
// decide whether the photo gets Google's metadata. Same name and size is
// taken as the same file.
func shareSidecars(items []*Item) {
	type key struct {
		name string
		size int64
	}
	known := map[key]*Sidecar{}
	for _, it := range items {
		if it.Sidecar != nil {
			k := key{strings.ToLower(it.Name), it.Size}
			if known[k] == nil {
				known[k] = it.Sidecar
			}
		}
	}
	for _, it := range items {
		if it.Sidecar == nil {
			it.Sidecar = known[key{strings.ToLower(it.Name), it.Size}]
		}
	}
}

func readSidecar(e Entry) *Sidecar {
	if e.Size > 1<<20 {
		return nil
	}
	r, err := e.Open()
	if err != nil {
		return nil
	}
	defer r.Close()
	var sc Sidecar
	if json.NewDecoder(r).Decode(&sc) != nil {
		return nil
	}
	return &sc
}

// editedOriginal returns the original's name for an edited copy:
// "IMG_1234-edited.jpg" → "IMG_1234.jpg".
func editedOriginal(name string) (string, bool) {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	lower := strings.ToLower(stem)
	for _, suf := range editSuffixes {
		if strings.HasSuffix(lower, suf) {
			return stem[:len(stem)-len(suf)] + ext, true
		}
	}
	return "", false
}

// findByStem finds an item with the same name apart from the extension: an
// edit saved as JPEG of a HEIC original.
func findByStem(name string, items []*Item) *Item {
	stem := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))
	for _, it := range items {
		if strings.ToLower(strings.TrimSuffix(it.Name, path.Ext(it.Name))) == stem {
			return it
		}
	}
	return nil
}

var (
	reMediaCounter = regexp.MustCompile(`^(.*)(\(\d+\))(\.[^.]+)$`) // IMG(1).jpg
	reJSONCounter  = regexp.MustCompile(`^(.*)(\(\d+\))$`)          // IMG.jpg(1)
)

const supplemental = ".supplemental-metadata"

// MatchSidecar finds the sidecar of the media file called name among the
// sidecars of its folder, keyed by JSON file name without ".json". It knows
// Google's naming quirks: the old "IMG.jpg.json" and newer
// "IMG.jpg.supplemental-metadata.json", names cut off at 51 characters, the
// "(1)" of duplicate names moving behind the extension, and Live Photo videos
// sharing the still's sidecar.
func MatchSidecar(name string, sidecars map[string]*Sidecar) *Sidecar {
	base, n := name, ""
	if m := reMediaCounter.FindStringSubmatch(name); m != nil {
		base, n = m[1]+m[3], m[2]
	}
	for _, c := range []string{base + n, base + supplemental + n} {
		if sc := sidecars[c]; sc != nil {
			return sc
		}
	}
	split := func(js string) (string, string) {
		if m := reJSONCounter.FindStringSubmatch(js); m != nil {
			return m[1], m[2]
		}
		return js, ""
	}
	// Truncated: the JSON name is a prefix of what it should be. Either it
	// still holds the whole media name (cut inside ".supplemental-metadata"),
	// or it is cut inside the name, which Google only does to long names.
	full := base + supplemental
	var best *Sidecar
	bestLen := 0
	for js, sc := range sidecars {
		jb, jn := split(js)
		long := len(jb) > len(base) || len(jb) >= 40
		if jn == n && long && len(jb) > bestLen && strings.HasPrefix(full, jb) {
			best, bestLen = sc, len(jb)
		}
	}
	if best != nil {
		return best
	}
	// Live Photos: IMG_1234.MP4 next to IMG_1234.HEIC and its sidecar.
	stem := strings.TrimSuffix(base, path.Ext(base))
	for js, sc := range sidecars {
		jb, jn := split(js)
		jb = strings.TrimSuffix(jb, supplemental)
		if jn == n && strings.EqualFold(strings.TrimSuffix(jb, path.Ext(jb)), stem) {
			return sc
		}
	}
	// Last resort: the title, if exactly one sidecar has it.
	var byTitle *Sidecar
	for _, sc := range sidecars {
		if sc.Title == name {
			if byTitle != nil {
				return nil
			}
			byTitle = sc
		}
	}
	return byTitle
}
