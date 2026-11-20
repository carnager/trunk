package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"regexp"
	"strconv"
)

// Motion photos (Pixel, Samsung and others) are JPEGs with a short MP4
// appended after the image data. The XMP says how long it is, in one of two
// ways:
//   - Motion Photo format: GCamera:MotionPhoto="1" and a Container:Directory
//     whose item with Item:Semantic="MotionPhoto" has the Item:Length;
//   - the older MicroVideo format: GCamera:MicroVideoOffset, the video's
//     length counted from the end of the file.
// Either way the video runs to the end of the file.

// The prefixes vary (Container:Item with Item:Length, GContainer:Item with
// GContainerItem:Length), so they are matched loosely.
var (
	reContainerItem = regexp.MustCompile(`<\w*:Item\b[^>]*>`)
	reSemantic      = regexp.MustCompile(`:Semantic="MotionPhoto"`)
	reItemLength    = regexp.MustCompile(`:Length="(\d+)"`)
	reMicroOffset   = regexp.MustCompile(`MicroVideoOffset(?:="|>)(\d+)`)
	reMotionFlag    = regexp.MustCompile(`MotionPhoto(?:="1"|>1<)|MicroVideo(?:="1"|>1<)|:Semantic="MotionPhoto"`)
)

// xmpHead is how much of a JPEG is searched for the XMP, which sits in the
// header segments; Pixels put it within the first few kilobytes.
const xmpHead = 256 << 10

// MotionVideo finds the video in a motion photo: where it starts and how
// long it is. ok is false for anything else. The video is checked to start
// with an MP4 file type box, so a wrong length in the XMP is not trusted.
func MotionVideo(path string) (offset, length int64, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, false, err
	}
	head := make([]byte, min(int64(xmpHead), st.Size()))
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, 0, false, err
	}
	if !bytes.HasPrefix(head, []byte{0xFF, 0xD8}) {
		return 0, 0, false, nil
	}
	length = 0
	for _, item := range reContainerItem.FindAll(head, -1) {
		if reSemantic.Match(item) {
			if m := reItemLength.FindSubmatch(item); m != nil {
				length, _ = strconv.ParseInt(string(m[1]), 10, 64)
			}
		}
	}
	if length == 0 {
		if m := reMicroOffset.FindSubmatch(head); m != nil {
			length, _ = strconv.ParseInt(string(m[1]), 10, 64)
		}
	}
	if length > 8 && length < st.Size() && mp4ToEnd(f, st.Size()-length, st.Size()) {
		return st.Size() - length, length, true, nil
	}
	if !reMotionFlag.Match(head) {
		return 0, 0, false, nil
	}
	// Marked as a motion photo, but the length is missing or stale (Google
	// Photos keeps the original's when it saves an edit): look for the MP4
	// itself. Exporters that drop the video keep the mark too; then there
	// is none.
	data, err := io.ReadAll(io.NewSectionReader(f, 0, st.Size()))
	if err != nil {
		return 0, 0, false, err
	}
	for i := 4; ; {
		j := bytes.Index(data[i:], []byte("ftyp"))
		if j < 0 {
			return 0, 0, false, nil
		}
		off := int64(i + j - 4)
		if mp4ToEnd(f, off, st.Size()) {
			return off, st.Size() - off, true, nil
		}
		i += j + 4
	}
}

// mp4ToEnd says whether an MP4 starts at off and runs exactly to the end of
// the file: a file type box first, top-level boxes of sane sizes all the
// way, one of them the movie header. Picture data does not pass this.
func mp4ToEnd(f io.ReaderAt, off, end int64) bool {
	pos, first, moov := off, true, false
	for pos < end {
		var h [16]byte
		if _, err := f.ReadAt(h[:8], pos); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(h[:4]))
		typ := string(h[4:8])
		if size == 1 {
			if _, err := f.ReadAt(h[8:16], pos+8); err != nil {
				return false
			}
			size = int64(binary.BigEndian.Uint64(h[8:16]))
		} else if size == 0 {
			size = end - pos // runs to the end
		}
		if size < 8 || pos+size > end || (first && typ != "ftyp") {
			return false
		}
		for _, c := range typ {
			if c < 0x20 || c > 0x7e {
				return false
			}
		}
		moov = moov || typ == "moov"
		first = false
		pos += size
	}
	return pos == end && moov
}
