package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"math"
	"os"
	"strings"
	"unicode"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
)

// The model: CLIP ViT-B/32 for pictures, and the multilingual text model
// sentence-transformers made to match it (50+ languages), so a search in
// German finds what one in English does. Vectors have 512 dimensions and
// unit length.
const (
	modelName = "clip-vit-b32-multilingual-v1"
	dim       = 512
	imageSize = 224
	maxTokens = 128
)

var (
	clipMean = [3]float32{0.48145466, 0.4578275, 0.40821073}
	clipStd  = [3]float32{0.26862954, 0.26130258, 0.27577711}
)

// pixels prepares a picture as CLIP wants it: the short side scaled to 224
// (bicubic), the middle 224×224 cut out, channels first, normalised.
func pixels(r io.Reader, out []float32) error {
	img, _, err := image.Decode(r)
	if err != nil {
		return err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return fmt.Errorf("empty picture")
	}
	nw, nh := imageSize, imageSize
	if w < h {
		nh = int(math.Round(float64(h) * imageSize / float64(w)))
	} else {
		nw = int(math.Round(float64(w) * imageSize / float64(h)))
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	x0, y0 := (nw-imageSize)/2, (nh-imageSize)/2
	plane := imageSize * imageSize
	for y := 0; y < imageSize; y++ {
		for x := 0; x < imageSize; x++ {
			p := dst.Pix[(y0+y)*dst.Stride+(x0+x)*4:]
			for c := 0; c < 3; c++ {
				out[c*plane+y*imageSize+x] = (float32(p[c])/255 - clipMean[c]) / clipStd[c]
			}
		}
	}
	return nil
}

// vision runs the image model on batches of pictures.
type vision struct {
	sess  *ort.AdvancedSession
	in    *ort.Tensor[float32]
	out   *ort.Tensor[float32]
	batch int
}

func newVision(path string, batch, threads int) (*vision, error) {
	in, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(batch), 3, imageSize, imageSize))
	if err != nil {
		return nil, err
	}
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(batch), dim))
	if err != nil {
		return nil, err
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	if threads > 0 {
		opts.SetIntraOpNumThreads(threads)
	}
	sess, err := ort.NewAdvancedSession(path, []string{"pixel_values"}, []string{"image_embeds"},
		[]ort.Value{in}, []ort.Value{out}, opts)
	if err != nil {
		return nil, err
	}
	return &vision{sess, in, out, batch}, nil
}

// slot is where picture i of the next batch goes.
func (v *vision) slot(i int) []float32 {
	n := 3 * imageSize * imageSize
	return v.in.GetData()[i*n : (i+1)*n]
}

// run computes the vectors of the first n pictures put into their slots.
func (v *vision) run(n int) ([][]float32, error) {
	if err := v.sess.Run(); err != nil {
		return nil, err
	}
	res := v.out.GetData()
	out := make([][]float32, n)
	for i := range out {
		out[i] = normalize(append([]float32(nil), res[i*dim:(i+1)*dim]...))
	}
	return out, nil
}

func (v *vision) close() {
	v.sess.Destroy()
	v.in.Destroy()
	v.out.Destroy()
}

func normalize(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(s))
	if n > 0 {
		for i := range v {
			v[i] /= n
		}
	}
	return v
}

// --- text: BERT WordPiece (cased), mean pooling, a linear layer to 512 ---

type tokenizer struct{ vocab map[string]int64 }

func loadVocab(path string) (*tokenizer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	t := &tokenizer{map[string]int64{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var i int64
	for sc.Scan() {
		t.vocab[sc.Text()] = i
		i++
	}
	return t, sc.Err()
}

func isPunct(r rune) bool {
	return (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) || unicode.IsPunct(r)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) || (r >= 0x20000 && r <= 0x2A6DF) ||
		(r >= 0x2A700 && r <= 0x2CEAF) || (r >= 0xF900 && r <= 0xFAFF) || (r >= 0x2F800 && r <= 0x2FA1F)
}

// ids splits text the way BERT's tokenizer does (cased: no lower-casing, no
// accent stripping) into word pieces, between [CLS] and [SEP].
func (t *tokenizer) ids(text string) []int64 {
	var sb strings.Builder
	for _, r := range text {
		switch {
		case r == 0 || r == 0xFFFD || (unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r'):
		case unicode.IsSpace(r):
			sb.WriteRune(' ')
		case isCJK(r) || isPunct(r):
			sb.WriteString(" " + string(r) + " ")
		default:
			sb.WriteRune(r)
		}
	}
	unk := t.vocab["[UNK]"]
	ids := []int64{t.vocab["[CLS]"]}
	for _, word := range strings.Fields(sb.String()) {
		rs := []rune(word)
		if len(rs) > 100 {
			ids = append(ids, unk)
			continue
		}
		var pieces []int64
		for start := 0; start < len(rs); {
			end, found := len(rs), int64(-1)
			for ; end > start; end-- {
				s := string(rs[start:end])
				if start > 0 {
					s = "##" + s
				}
				if id, ok := t.vocab[s]; ok {
					found = id
					break
				}
			}
			if found < 0 {
				pieces = []int64{unk}
				break
			}
			pieces = append(pieces, found)
			start = end
		}
		ids = append(ids, pieces...)
	}
	if len(ids) > maxTokens-1 {
		ids = ids[:maxTokens-1]
	}
	return append(ids, t.vocab["[SEP]"])
}

// loadDense reads the 768→512 projection (safetensors, float32, no bias).
func loadDense(path string) ([]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 8 {
		return nil, fmt.Errorf("%s: too short", path)
	}
	n := binary.LittleEndian.Uint64(b[:8])
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(b[8:8+n], &hdr); err != nil {
		return nil, err
	}
	var w struct {
		Shape   []int  `json:"shape"`
		Offsets [2]int `json:"data_offsets"`
	}
	if err := json.Unmarshal(hdr["linear.weight"], &w); err != nil || len(w.Shape) != 2 || w.Shape[0] != dim {
		return nil, fmt.Errorf("%s: no 512×768 linear.weight", path)
	}
	data := b[8+n:][w.Offsets[0]:w.Offsets[1]]
	out := make([]float32, len(data)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return out, nil
}

// text turns search text into a vector.
type text struct {
	model string
	tok   *tokenizer
	dense []float32
}

func (t *text) embed(q string) ([]float32, error) {
	ids := t.tok.ids(q)
	L := int64(len(ids))
	mask := make([]int64, L)
	for i := range mask {
		mask[i] = 1
	}
	in1, err := ort.NewTensor(ort.NewShape(1, L), ids)
	if err != nil {
		return nil, err
	}
	defer in1.Destroy()
	in2, err := ort.NewTensor(ort.NewShape(1, L), mask)
	if err != nil {
		return nil, err
	}
	defer in2.Destroy()
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, L, 768))
	if err != nil {
		return nil, err
	}
	defer out.Destroy()
	s, err := ort.NewAdvancedSession(t.model, []string{"input_ids", "attention_mask"}, []string{"last_hidden_state"},
		[]ort.Value{in1, in2}, []ort.Value{out}, nil)
	if err != nil {
		return nil, err
	}
	defer s.Destroy()
	if err := s.Run(); err != nil {
		return nil, err
	}
	h := out.GetData()
	pooled := make([]float32, 768)
	for i := 0; i < int(L); i++ {
		for d := range pooled {
			pooled[d] += h[i*768+d]
		}
	}
	v := make([]float32, dim)
	for i := range v {
		var acc float32
		for j := 0; j < 768; j++ {
			acc += t.dense[i*768+j] * pooled[j] / float32(L)
		}
		v[i] = acc
	}
	return normalize(v), nil
}
