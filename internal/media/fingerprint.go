package media

import (
	"image"
	"image/draw"
	"image/jpeg"
	"math"
	"math/bits"
	"os"
)

// FingerprintBits is the size of a fingerprint.
const FingerprintBits = 512

// Hash is a picture's fingerprint: a difference hash (dHash) on a 16×16 grid
// of grey cells, one bit per pair of neighbours, set when the first is
// brighter: 256 bits along the rows, 256 down the columns. Re-encoding,
// resizing and small edits change few bits; another picture changes about
// half of them. 16×16 rather than the usual 8×8, so documents, screenshots
// and hazy landscapes, which are all alike at 8×8, can be told apart.
type Hash [FingerprintBits / 64]uint64

// Fingerprint computes the fingerprint of a JPEG, normally a thumbnail (cheap,
// and the orientation is already applied), and how much detail the picture
// has: the mean brightness step between neighbouring cells, 0–255. A night
// sky or a wall has almost none, and such pictures have alike fingerprints
// whatever they show.
func Fingerprint(path string) (Hash, float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return Hash{}, 0, err
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return Hash{}, 0, err
	}
	h, detail := dHash(img)
	return h, detail, nil
}

const grid = 16

func dHash(img image.Image) (Hash, float64) {
	// Grey cells, (grid+1)² so both directions have grid differences per line.
	const n = grid + 1
	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)
	bw, bh := b.Dx(), b.Dy()
	var cells [n][n]float64
	for y := 0; y < n; y++ {
		y0, y1 := y*bh/n, max((y+1)*bh/n, y*bh/n+1)
		for x := 0; x < n; x++ {
			x0, x1 := x*bw/n, max((x+1)*bw/n, x*bw/n+1)
			var sum float64
			for py := y0; py < y1; py++ {
				row := rgba.Pix[py*rgba.Stride:]
				for px := x0; px < x1; px++ {
					p := row[px*4:]
					sum += 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2])
				}
			}
			cells[y][x] = sum / float64((y1-y0)*(x1-x0))
		}
	}
	var h Hash
	var steps float64
	bit := 0
	set := func(on bool) {
		if on {
			h[bit/64] |= 1 << (bit % 64)
		}
		bit++
	}
	for y := 0; y < grid; y++ {
		for x := 0; x < grid; x++ {
			set(cells[y][x] > cells[y][x+1])
			steps += math.Abs(cells[y][x] - cells[y][x+1])
		}
	}
	for x := 0; x < grid; x++ {
		for y := 0; y < grid; y++ {
			set(cells[y][x] > cells[y+1][x])
			steps += math.Abs(cells[y][x] - cells[y+1][x])
		}
	}
	return h, steps / FingerprintBits
}

// Distance is how many bits two fingerprints differ in: 0 for the same
// picture, a few percent for a re-encoded or slightly changed one, about
// half for unrelated pictures.
func Distance(a, b Hash) int {
	d := 0
	for i := range a {
		d += bits.OnesCount64(a[i] ^ b[i])
	}
	return d
}

// Ones counts the set bits: an even, featureless picture has few.
func (h Hash) Ones() int {
	n := 0
	for _, w := range h {
		n += bits.OnesCount64(w)
	}
	return n
}

// Bytes and HashFrom store a fingerprint as 64 bytes.
func (h Hash) Bytes() []byte {
	out := make([]byte, 0, len(h)*8)
	for _, w := range h {
		for i := 0; i < 8; i++ {
			out = append(out, byte(w>>(8*i)))
		}
	}
	return out
}

func HashFrom(b []byte) (Hash, bool) {
	var h Hash
	if len(b) != len(h)*8 {
		return h, false
	}
	for i := range h {
		for j := 0; j < 8; j++ {
			h[i] |= uint64(b[i*8+j]) << (8 * j)
		}
	}
	return h, true
}
