package main

import (
	"image"
	"image/color"
	"math"
	"sort"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
)

// Faces: YuNet (OpenCV Zoo, MIT) finds them with five landmarks (eyes, nose,
// mouth corners), SFace (OpenCV Zoo, Apache 2.0) turns each, aligned to
// 112×112, into 128 numbers. Faces of one person lie close together; trunk
// groups them. The decoding and alignment follow OpenCV's FaceDetectorYN and
// FaceRecognizerSF, which these models were made for.
const (
	facesModel   = "faces-yunet-sface-v1" // trunk's name for these faces; must match internal/server/people.go
	detSize      = 640
	faceSize     = 112
	faceDim      = 128
	detThreshold = 0.75 // YuNet's score; lower finds more, with more that are not faces
	nmsIoU       = 0.3
	minFacePx    = 36 // in the preview: smaller faces are too blurry to recognise
)

// face is one found face: box and landmarks in the picture's pixels.
type face struct {
	x, y, w, h float64
	score      float64
	kps        [5][2]float64
	vec        []float32
}

type faceModels struct {
	det, rec     *ort.AdvancedSession
	detIn, recIn *ort.Tensor[float32]
	detOut       []*ort.Tensor[float32]
	recOut       *ort.Tensor[float32]
}

var detOutputs = []string{"cls_8", "cls_16", "cls_32", "obj_8", "obj_16", "obj_32", "bbox_8", "bbox_16", "bbox_32", "kps_8", "kps_16", "kps_32"}

func newFaceModels(detPath, recPath string, threads int) (*faceModels, error) {
	m := &faceModels{}
	var err error
	if m.detIn, err = ort.NewEmptyTensor[float32](ort.NewShape(1, 3, detSize, detSize)); err != nil {
		return nil, err
	}
	var outs []ort.Value
	for _, name := range detOutputs {
		n := int64(detSize / 8 * detSize / 8)
		switch name[len(name)-2:] {
		case "16":
			n /= 4
		case "32":
			n /= 16
		}
		width := int64(1)
		switch name[:3] {
		case "bbo":
			width = 4
		case "kps":
			width = 10
		}
		t, err := ort.NewEmptyTensor[float32](ort.NewShape(1, n, width))
		if err != nil {
			return nil, err
		}
		m.detOut = append(m.detOut, t)
		outs = append(outs, t)
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	if threads > 0 {
		opts.SetIntraOpNumThreads(threads)
	}
	if m.det, err = ort.NewAdvancedSession(detPath, []string{"input"}, detOutputs, []ort.Value{m.detIn}, outs, opts); err != nil {
		return nil, err
	}
	if m.recIn, err = ort.NewEmptyTensor[float32](ort.NewShape(1, 3, faceSize, faceSize)); err != nil {
		return nil, err
	}
	if m.recOut, err = ort.NewEmptyTensor[float32](ort.NewShape(1, faceDim)); err != nil {
		return nil, err
	}
	if m.rec, err = ort.NewAdvancedSession(recPath, []string{"data"}, []string{"fc1"}, []ort.Value{m.recIn}, []ort.Value{m.recOut}, opts); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *faceModels) close() {
	m.det.Destroy()
	m.rec.Destroy()
	m.detIn.Destroy()
	m.recIn.Destroy()
	m.recOut.Destroy()
	for _, t := range m.detOut {
		t.Destroy()
	}
}

// find returns the faces in img with their vectors.
func (m *faceModels) find(img image.Image) ([]face, error) {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	scale := detSize / math.Max(w, h)
	sw, sh := int(math.Round(w*scale)), int(math.Round(h*scale))
	// Letterboxed at the top left, the rest black, as BGR 0–255 (what YuNet takes).
	small := image.NewRGBA(image.Rect(0, 0, detSize, detSize))
	draw.CatmullRom.Scale(small, image.Rect(0, 0, sw, sh), img, b, draw.Src, nil)
	in := m.detIn.GetData()
	plane := detSize * detSize
	for y := 0; y < detSize; y++ {
		for x := 0; x < detSize; x++ {
			p := small.Pix[y*small.Stride+x*4:]
			i := y*detSize + x
			in[i], in[plane+i], in[2*plane+i] = float32(p[2]), float32(p[1]), float32(p[0])
		}
	}
	if err := m.det.Run(); err != nil {
		return nil, err
	}
	var found []face
	for k, stride := range []int{8, 16, 32} {
		cls, obj := m.detOut[k].GetData(), m.detOut[3+k].GetData()
		bbox, kps := m.detOut[6+k].GetData(), m.detOut[9+k].GetData()
		cols := detSize / stride
		for i := range cls {
			c := math.Min(math.Max(float64(cls[i]), 0), 1)
			o := math.Min(math.Max(float64(obj[i]), 0), 1)
			score := math.Sqrt(c * o)
			if score < detThreshold {
				continue
			}
			r, col := float64(i/cols), float64(i%cols)
			s := float64(stride)
			cx := (col + float64(bbox[i*4])) * s
			cy := (r + float64(bbox[i*4+1])) * s
			bw := math.Exp(float64(bbox[i*4+2])) * s
			bh := math.Exp(float64(bbox[i*4+3])) * s
			f := face{x: (cx - bw/2) / scale, y: (cy - bh/2) / scale, w: bw / scale, h: bh / scale, score: score}
			for n := 0; n < 5; n++ {
				f.kps[n] = [2]float64{(float64(kps[i*10+2*n]) + col) * s / scale, (float64(kps[i*10+2*n+1]) + r) * s / scale}
			}
			found = append(found, f)
		}
	}
	found = nms(found)
	var out []face
	for _, f := range found {
		if f.w < minFacePx || f.h < minFacePx {
			continue
		}
		f.vec = m.embed(img, f)
		out = append(out, f)
	}
	return out, nil
}

func nms(fs []face) []face {
	sort.Slice(fs, func(i, j int) bool { return fs[i].score > fs[j].score })
	var keep []face
	for _, f := range fs {
		ok := true
		for _, k := range keep {
			if iou(f, k) > nmsIoU {
				ok = false
				break
			}
		}
		if ok {
			keep = append(keep, f)
		}
	}
	return keep
}

func iou(a, b face) float64 {
	x1, y1 := math.Max(a.x, b.x), math.Max(a.y, b.y)
	x2, y2 := math.Min(a.x+a.w, b.x+b.w), math.Min(a.y+a.h, b.y+b.h)
	inter := math.Max(0, x2-x1) * math.Max(0, y2-y1)
	return inter / (a.w*a.h + b.w*b.h - inter)
}

// Where the five landmarks lie in a 112×112 face, as SFace was trained.
var template = [5][2]float64{{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366}, {41.5493, 92.3655}, {70.7299, 92.2041}}

// align cuts the face out, turned and scaled so its landmarks lie on the
// template: the similarity transform that fits them best (Umeyama).
func align(img image.Image, f face) *image.RGBA {
	// Fit dst = s·R·src + t, then sample the source through its inverse.
	var mx, my, tx, ty float64
	for i := 0; i < 5; i++ {
		mx += f.kps[i][0]
		my += f.kps[i][1]
		tx += template[i][0]
		ty += template[i][1]
	}
	mx, my, tx, ty = mx/5, my/5, tx/5, ty/5
	var sxx, sxy, syx, syy, varSrc float64
	for i := 0; i < 5; i++ {
		ax, ay := f.kps[i][0]-mx, f.kps[i][1]-my
		bx, by := template[i][0]-tx, template[i][1]-ty
		sxx += bx * ax
		sxy += bx * ay
		syx += by * ax
		syy += by * ay
		varSrc += ax*ax + ay*ay
	}
	// For 2-D similarity: rotation angle and scale from the covariance.
	a := sxx + syy
	bb := syx - sxy
	norm := math.Hypot(a, bb)
	cos, sin := a/norm, bb/norm
	s := norm / varSrc
	// dst = s*[cos -sin; sin cos]*src + t
	t0 := tx - s*(cos*mx-sin*my)
	t1 := ty - s*(sin*mx+cos*my)
	out := image.NewRGBA(image.Rect(0, 0, faceSize, faceSize))
	b := img.Bounds()
	for y := 0; y < faceSize; y++ {
		for x := 0; x < faceSize; x++ {
			// Inverse: src = R^T (dst - t) / s
			dx, dy := float64(x)-t0, float64(y)-t1
			sx := (cos*dx + sin*dy) / s
			sy := (-sin*dx + cos*dy) / s
			out.Set(x, y, bilinear(img, b, sx, sy))
		}
	}
	return out
}

func bilinear(img image.Image, b image.Rectangle, x, y float64) color.RGBA {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(x0), y-float64(y0)
	var acc [3]float64
	for _, p := range [4][3]float64{{0, 0, (1 - fx) * (1 - fy)}, {1, 0, fx * (1 - fy)}, {0, 1, (1 - fx) * fy}, {1, 1, fx * fy}} {
		px, py := x0+int(p[0]), y0+int(p[1])
		if px < b.Min.X || py < b.Min.Y || px >= b.Max.X || py >= b.Max.Y {
			continue
		}
		r, g, bl, _ := img.At(px, py).RGBA()
		acc[0] += p[2] * float64(r>>8)
		acc[1] += p[2] * float64(g>>8)
		acc[2] += p[2] * float64(bl>>8)
	}
	return color.RGBA{uint8(acc[0] + 0.5), uint8(acc[1] + 0.5), uint8(acc[2] + 0.5), 255}
}

// embed runs SFace on the aligned face: RGB, 0–255, channels first.
func (m *faceModels) embed(img image.Image, f face) []float32 {
	a := align(img, f)
	in := m.recIn.GetData()
	plane := faceSize * faceSize
	for y := 0; y < faceSize; y++ {
		for x := 0; x < faceSize; x++ {
			p := a.Pix[y*a.Stride+x*4:]
			i := y*faceSize + x
			in[i], in[plane+i], in[2*plane+i] = float32(p[0]), float32(p[1]), float32(p[2])
		}
	}
	if err := m.rec.Run(); err != nil {
		return nil
	}
	return normalize(append([]float32(nil), m.recOut.GetData()...))
}

// frontal says whether the face looks roughly at the camera: the nose lies
// between the eyes. Profiles are recognised poorly, so they only join a
// person that is clear without them.
func (f face) frontal() bool {
	l, r, n := f.kps[0], f.kps[1], f.kps[2]
	eyes := math.Abs(r[0] - l[0])
	if eyes < 1 {
		return false
	}
	lo, hi := math.Min(l[0], r[0]), math.Max(l[0], r[0])
	margin := 0.15 * eyes
	return n[0] > lo+margin && n[0] < hi-margin
}

// dbscan groups vectors: a frontal face with at least minPts frontal
// neighbours (similarity ≥ eps, itself included) is a core; cores near each
// other form a person, and other faces near a core join it. The rest is -1.
func dbscan(vecs [][]float32, frontal []bool, eps float64, minPts int) []int {
	n := len(vecs)
	near := make([][]int, n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			var dot float64
			for k := range vecs[i] {
				dot += float64(vecs[i][k] * vecs[j][k])
			}
			if dot >= eps {
				near[i] = append(near[i], j)
				near[j] = append(near[j], i)
			}
		}
	}
	core := make([]bool, n)
	for i := range core {
		if !frontal[i] {
			continue
		}
		c := 1
		for _, j := range near[i] {
			if frontal[j] {
				c++
			}
		}
		core[i] = c >= minPts
	}
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	next := 0
	for i := 0; i < n; i++ {
		if !core[i] || labels[i] >= 0 {
			continue
		}
		labels[i] = next
		queue := []int{i}
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			for _, q := range near[p] {
				if labels[q] >= 0 {
					continue
				}
				labels[q] = next
				if core[q] {
					queue = append(queue, q)
				}
			}
		}
		next++
	}
	return labels
}
