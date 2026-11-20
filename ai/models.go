package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A model file: where it comes from and what it must be. Pinned, so a
// changed file upstream is noticed instead of silently used.
type modelFile struct {
	local, url, sha256 string
	size               int64
}

const hf = "https://huggingface.co/"

var files = map[string]modelFile{
	// Pictures: CLIP ViT-B/32 (OpenAI), as ONNX by Xenova.
	"vision": {"vision.onnx", hf + "Xenova/clip-vit-base-patch32/resolve/main/onnx/vision_model.onnx",
		"fd6e1402a588279d1723c7534d4bcba5bc0b14b47dfab0e46f8c47b8270d7d40", 351685709},
	// Text: the multilingual model made to match it, full precision here …
	"text": {"text.onnx", hf + "sentence-transformers/clip-ViT-B-32-multilingual-v1/resolve/main/onnx/model.onnx",
		"ca09969eb6e0f36fb1622654d1b47ac2025f576794f48b249a50b78d4b70373f", 539049415},
	// … and quantised for phones (same vectors to 0.998).
	"text-arm64": {"text-arm64.onnx", hf + "sentence-transformers/clip-ViT-B-32-multilingual-v1/resolve/main/onnx/model_qint8_arm64.onnx",
		"3bed77e83926519660b5c0833cb3321cfde952331edec4f7d501c7d384b8a8a7", 135336307},
	"vocab": {"vocab.txt", hf + "sentence-transformers/clip-ViT-B-32-multilingual-v1/resolve/main/vocab.txt",
		"fe0fda7c425b48c516fc8f160d594c8022a0808447475c1a7c6d6479763f310c", 995526},
	"dense": {"dense.safetensors", hf + "sentence-transformers/clip-ViT-B-32-multilingual-v1/resolve/main/2_Dense/model.safetensors",
		"d12568dc7300970a4d3dbb49068ad16cd89b99840b74b026f8e48071e9414f74", 1572984},
	// Faces: YuNet finds them (MIT), SFace recognises them (Apache 2.0), from OpenCV Zoo.
	"yunet": {"yunet.onnx", hf + "opencv/face_detection_yunet/resolve/main/face_detection_yunet_2023mar.onnx",
		"8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4", 232589},
	"sface": {"sface.onnx", hf + "opencv/face_recognition_sface/resolve/main/face_recognition_sface_2021dec.onnx",
		"0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79", 38696353},
}

func modelDir() string {
	if d := os.Getenv("TRUNK_AI_MODELS"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "trunk-ai", modelName)
}

// fetch returns the path of a model file, downloading it the first time.
func fetch(key string) (string, error) {
	f := files[key]
	dir := modelDir()
	p := filepath.Join(dir, f.local)
	if st, err := os.Stat(p); err == nil && st.Size() == f.size {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	fmt.Printf("downloading %s (%.0f MB)\n", f.local, float64(f.size)/(1<<20))
	client := &http.Client{Timeout: 30 * time.Minute}
	res, err := client.Get(f.url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("%s: %s", f.url, res.Status)
	}
	tmp, err := os.CreateTemp(dir, ".download-")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), res.Body); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.sha256 {
		return "", fmt.Errorf("%s: checksum %s, expected %s", f.local, sum, f.sha256)
	}
	return p, os.Rename(tmp.Name(), p)
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
