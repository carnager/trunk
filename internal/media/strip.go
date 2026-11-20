package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"regexp"
)

// Patch overwrites Data at Offset; patches never change a file's length, so
// a patched file can be served with Range requests like the original.
type Patch struct {
	Offset int64
	Data   []byte
}

// LocationPatches finds where a file keeps where it was taken and returns the
// patches that blank it out, leaving everything else (pixels, date, camera)
// as it is. ok is false for formats this cannot clean; such files should not
// be handed out as they are if they have a location.
func LocationPatches(path, mime string) (patches []Patch, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	switch mime {
	case "image/jpeg":
		p, err := jpegLocation(f)
		if err != nil {
			return nil, false, err
		}
		// A motion photo's video keeps a location of its own.
		if off, n, ok, _ := MotionVideo(path); ok {
			vp, err := mp4Location(io.NewSectionReader(f, off, n))
			if err != nil {
				return nil, false, err
			}
			for _, q := range vp {
				p = append(p, Patch{q.Offset + off, q.Data})
			}
		}
		return p, true, nil
	case "video/mp4", "video/quicktime", "video/3gpp":
		p, err := mp4Location(f)
		return p, err == nil, err
	}
	return nil, false, nil
}

// --- JPEG ---

var reXMPGPS = regexp.MustCompile(`(exif:GPS[A-Za-z]+)(="[^"]*"|>[^<]*<)`)

// jpegLocation empties the GPS directory of the EXIF data and blanks GPS
// fields in XMP. Both sit in the header segments before the image data.
func jpegLocation(f io.ReadSeeker) ([]Patch, error) {
	var head [2]byte
	if _, err := io.ReadFull(f, head[:]); err != nil || head != [2]byte{0xFF, 0xD8} {
		return nil, errors.New("not a JPEG")
	}
	var patches []Patch
	pos := int64(2)
	for {
		var m [4]byte
		if _, err := io.ReadFull(f, m[:]); err != nil {
			return patches, nil
		}
		if m[0] != 0xFF {
			return nil, errors.New("broken JPEG segment")
		}
		marker := m[1]
		if marker == 0xDA || marker == 0xD9 { // image data starts: no more metadata
			return patches, nil
		}
		size := int64(binary.BigEndian.Uint16(m[2:]))
		body := make([]byte, size-2)
		if _, err := io.ReadFull(f, body); err != nil {
			return nil, err
		}
		start := pos + 4
		if marker == 0xE1 {
			switch {
			case bytes.HasPrefix(body, []byte("Exif\x00\x00")):
				p, err := tiffGPS(body[6:], start+6)
				if err != nil {
					return nil, err
				}
				patches = append(patches, p...)
			case bytes.HasPrefix(body, []byte("http://ns.adobe.com/xap/1.0/")):
				for _, loc := range reXMPGPS.FindAllSubmatchIndex(body, -1) {
					// Keep the attribute or element, blank its value with spaces.
					v0, v1 := loc[4]+2, loc[5]-1
					if v1 > v0 {
						patches = append(patches, Patch{start + int64(v0), bytes.Repeat([]byte(" "), v1-v0)})
					}
				}
			}
		}
		pos += 2 + size
	}
}

// tiffGPS empties the GPS IFD of a TIFF block (EXIF) at file offset base: its
// entry count becomes 0 and its entries and their values are zeroed, so no
// reader finds a position, not even 0,0.
func tiffGPS(t []byte, base int64) ([]Patch, error) {
	if len(t) < 8 {
		return nil, errors.New("short EXIF")
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return nil, errors.New("bad EXIF byte order")
	}
	ifd0 := int(bo.Uint32(t[4:]))
	gps := -1
	if ifd0+2 > len(t) {
		return nil, errors.New("bad IFD0")
	}
	n := int(bo.Uint16(t[ifd0:]))
	for i := 0; i < n; i++ {
		e := ifd0 + 2 + 12*i
		if e+12 > len(t) {
			break
		}
		if bo.Uint16(t[e:]) == 0x8825 {
			gps = int(bo.Uint32(t[e+8:]))
		}
	}
	if gps < 0 || gps+2 > len(t) {
		return nil, nil // no position in the EXIF data
	}
	var patches []Patch
	count := int(bo.Uint16(t[gps:]))
	for i := 0; i < count; i++ {
		e := gps + 2 + 12*i
		if e+12 > len(t) {
			break
		}
		typ, cnt := bo.Uint16(t[e+2:]), int(bo.Uint32(t[e+4:]))
		if size := cnt * typeSize(typ); size > 4 {
			if off := int(bo.Uint32(t[e+8:])); off >= 0 && off+size <= len(t) {
				patches = append(patches, Patch{base + int64(off), make([]byte, size)})
			}
		}
		patches = append(patches, Patch{base + int64(e), make([]byte, 12)})
	}
	patches = append(patches, Patch{base + int64(gps), make([]byte, 2)}) // count 0
	return patches, nil
}

func typeSize(t uint16) int {
	switch t {
	case 1, 2, 6, 7:
		return 1
	case 3, 8:
		return 2
	case 4, 9, 11:
		return 4
	case 5, 10, 12:
		return 8
	}
	return 1
}

// --- MP4 / QuickTime ---

// ISO 6709 positions as phones write them in videos: "+48.2085+016.3721+171.000/".
var reISO6709Bytes = regexp.MustCompile(`[+-]\d{1,2}\.\d+[+-]\d{1,3}\.\d+(?:[+-]\d+(?:\.\d+)?)?/?`)

// mp4Location blanks every ISO 6709 position in the movie header (moov),
// where both the ©xyz atom and Apple's location key keep it. The media data
// is not touched.
func mp4Location(f io.ReadSeeker) ([]Patch, error) {
	var patches []Patch
	var pos int64
	for {
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
		var h [8]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			return patches, nil // end of file
		}
		size := int64(binary.BigEndian.Uint32(h[:4]))
		hdr := int64(8)
		if size == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(f, ext[:]); err != nil {
				return nil, err
			}
			size, hdr = int64(binary.BigEndian.Uint64(ext[:])), 16
		}
		if size == 0 {
			return patches, nil // runs to the end of the file: media data, not moov
		}
		if size < hdr {
			return nil, errors.New("broken MP4 box")
		}
		if string(h[4:8]) == "moov" {
			if size > 64<<20 {
				return nil, errors.New("movie header too large")
			}
			body := make([]byte, size-hdr)
			if _, err := io.ReadFull(f, body); err != nil {
				return nil, err
			}
			patches = append(patches, moovLocation(body, pos+hdr)...)
		}
		pos += size
	}
}

// moovLocation finds the location in a moov body at file offset base. Phones
// and cameras keep it in one of three places: ©xyz in udta (Android, text),
// loci in udta (3GPP, binary) and a key in meta (Apple, text). The first two
// become "free" boxes, which every reader skips; the text in meta is blanked.
// Nothing else in moov is touched: it holds binary tables a stray match of
// the pattern must not change.
func moovLocation(b []byte, base int64) []Patch {
	var patches []Patch
	for _, c := range children(b, 0, len(b)) {
		switch c.typ {
		case "udta":
			for _, u := range children(b, c.body, c.end) {
				switch u.typ {
				case "\xa9xyz", "loci":
					patches = append(patches, Patch{base + int64(u.start+4), []byte("free")})
				case "meta":
					patches = append(patches, blankISO6709(b, u.body, u.end, base)...)
				}
			}
		case "meta":
			patches = append(patches, blankISO6709(b, c.body, c.end, base)...)
		}
	}
	return patches
}

func blankISO6709(b []byte, from, to int, base int64) []Patch {
	var patches []Patch
	for _, loc := range reISO6709Bytes.FindAllIndex(b[from:to], -1) {
		patches = append(patches, Patch{base + int64(from+loc[0]), bytes.Repeat([]byte(" "), loc[1]-loc[0])})
	}
	return patches
}

type box struct {
	typ              string
	start, body, end int
}

// children lists the boxes in b[from:to].
func children(b []byte, from, to int) []box {
	var out []box
	for i := from; i+8 <= to; {
		size := int(binary.BigEndian.Uint32(b[i:]))
		if size < 8 || i+size > to {
			break
		}
		out = append(out, box{string(b[i+4 : i+8]), i, i + 8, i + size})
		i += size
	}
	return out
}

// --- serving ---

// Patched reads like the file underneath, with the patches applied.
type Patched struct {
	io.ReadSeeker
	patches []Patch
	pos     int64
}

func NewPatched(r io.ReadSeeker, patches []Patch) *Patched {
	return &Patched{ReadSeeker: r, patches: patches}
}

func (p *Patched) Read(b []byte) (int, error) {
	n, err := p.ReadSeeker.Read(b)
	for _, pt := range p.patches {
		lo, hi := max(pt.Offset, p.pos), min(pt.Offset+int64(len(pt.Data)), p.pos+int64(n))
		if lo < hi {
			copy(b[lo-p.pos:hi-p.pos], pt.Data[lo-pt.Offset:hi-pt.Offset])
		}
	}
	p.pos += int64(n)
	return n, err
}

func (p *Patched) Seek(off int64, whence int) (int64, error) {
	n, err := p.ReadSeeker.Seek(off, whence)
	if err == nil {
		p.pos = n
	}
	return n, err
}
