// Command trunk-ai gives a trunk server content search and people: it
// computes a vector for every photo and video (from its thumbnail) with an
// image model, finds the faces in every photo with a vector each, and sends
// them to trunk; it uploads the matching text model for phones, which turn
// search text into vectors themselves. trunk only stores, compares and
// groups.
//
// It runs where there is computing power (a desktop: about 70 photos a
// second on a modern CPU), whenever convenient; photos that arrived since
// are searchable by content after the next run. It needs the ONNX Runtime
// library (Arch: onnxruntime-cpu) and an admin account on the server.
//
//	trunk-ai index -server https://photos.example.org -user alice
//	trunk-ai search -server … "Hund am Strand"
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/term"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  trunk-ai index  -server URL -user NAME [-batch 32] [-threads N] [-content=false] [-faces=false]
                  vectors for content search and faces for people, for what has none yet
  trunk-ai search -server URL -user NAME TEXT                     try a search, as a phone would
flags may also come from TRUNK_SERVER and TRUNK_USER; the login is remembered in
`+tokenDir()+`
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	server := fs.String("server", os.Getenv("TRUNK_SERVER"), "trunk's address")
	user := fs.String("user", os.Getenv("TRUNK_USER"), "an admin account (for index)")
	lib := fs.String("lib", "", "path of libonnxruntime.so (default: the system's)")
	batch := fs.Int("batch", 32, "pictures per model run")
	threads := fs.Int("threads", runtime.NumCPU(), "CPU threads for the model")
	doContent := fs.Bool("content", true, "index: vectors for content search")
	doFaces := fs.Bool("faces", true, "index: find faces, for people")
	fs.Parse(os.Args[2:])
	if *server == "" || *user == "" {
		usage()
	}
	if err := initRuntime(*lib); err != nil {
		fmt.Fprintln(os.Stderr, "trunk-ai:", err)
		os.Exit(1)
	}
	defer ort.DestroyEnvironment()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c, err := login(strings.TrimRight(*server, "/"), *user)
	if err == nil {
		switch cmd {
		case "index":
			if *doContent {
				err = index(ctx, c, *batch, *threads)
			}
			if err == nil && *doFaces {
				err = indexFaces(ctx, c, *threads)
			}
		case "search":
			err = search(c, strings.Join(fs.Args(), " "))
		default:
			usage()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "trunk-ai:", err)
		os.Exit(1)
	}
}

func initRuntime(lib string) error {
	if lib == "" {
		for _, p := range []string{"/usr/lib/libonnxruntime.so", "/usr/local/lib/libonnxruntime.so", "/usr/lib/x86_64-linux-gnu/libonnxruntime.so"} {
			if _, err := os.Stat(p); err == nil {
				lib = p
				break
			}
		}
	}
	if lib == "" {
		return errors.New("ONNX Runtime not found; install it (Arch: onnxruntime-cpu) or give -lib")
	}
	ort.SetSharedLibraryPath(lib)
	return ort.InitializeEnvironment()
}

// --- the server ---

type client struct {
	base, token string
	http        *http.Client
}

func tokenDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "trunk-ai")
}

func login(base, user string) (*client, error) {
	c := &client{base: base, http: &http.Client{Timeout: 5 * time.Minute}}
	u, _ := url.Parse(base)
	file := filepath.Join(tokenDir(), u.Host+"-"+user+".token")
	if b, err := os.ReadFile(file); err == nil {
		c.token = strings.TrimSpace(string(b))
		if _, err := c.get("/api/v1/me"); err == nil {
			return c, nil
		}
	}
	fmt.Printf("password for %s on %s: ", user, u.Host)
	pw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	body, _ := json.Marshal(map[string]string{"user": user, "password": string(pw), "device": "trunk-ai on " + host})
	res, err := c.http.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct{ Token, Error string }
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("login: %s", out.Error)
	}
	c.token = out.Token
	os.MkdirAll(tokenDir(), 0o700)
	os.WriteFile(file, []byte(out.Token), 0o600)
	return c, nil
}

func (c *client) do(method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		var e struct{ Error string }
		json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, e.Error)
	}
	return res, nil
}

func (c *client) get(path string) ([]byte, error) {
	res, err := c.do("GET", path, nil, "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
}

func (c *client) send(method, path string, v any) error {
	b, _ := json.Marshal(v)
	res, err := c.do(method, path, bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func encodeVec(v []float32) string {
	b := make([]byte, 0, len(v)*4)
	for _, x := range v {
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// --- index ---

func index(ctx context.Context, c *client, batch, threads int) error {
	if err := uploadTextModel(c); err != nil {
		return err
	}
	vp, err := fetch("vision")
	if err != nil {
		return err
	}
	v, err := newVision(vp, batch, threads)
	if err != nil {
		return err
	}
	defer v.close()
	start := time.Now()
	total, done, failed := -1, 0, 0
	skip := map[string]bool{} // files whose thumbnail cannot be had: not asked for again this run
	for ctx.Err() == nil {
		var todo struct {
			Assets []struct {
				ID     int64  `json:"id"`
				SHA256 string `json:"sha256"`
			} `json:"assets"`
			Remaining int `json:"remaining"`
		}
		b, err := c.get("/api/v1/ai/todo?model=" + modelName + "&limit=" + fmt.Sprint(batch*8))
		if err != nil {
			return err
		}
		json.Unmarshal(b, &todo)
		if total < 0 {
			total = todo.Remaining
			if total == 0 {
				fmt.Println("everything has its vector")
				return nil
			}
			fmt.Printf("%d to do\n", total)
		}
		var list []struct {
			ID     int64  `json:"id"`
			SHA256 string `json:"sha256"`
		}
		for _, a := range todo.Assets {
			if !skip[a.SHA256] {
				list = append(list, a)
			}
		}
		if len(list) == 0 {
			break
		}
		// Thumbnails come in parallel; the model takes them a batch at a time.
		thumbs := make([][]byte, len(list))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i, a := range list {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				thumbs[i], _ = c.get(fmt.Sprintf("/api/v1/ai/thumb/%d", a.ID))
			}()
		}
		wg.Wait()
		var items []map[string]string
		for s := 0; s < len(list); s += batch {
			var shas []string
			for i := s; i < min(s+batch, len(list)); i++ {
				if thumbs[i] == nil || pixels(bytes.NewReader(thumbs[i]), v.slot(len(shas))) != nil {
					skip[list[i].SHA256] = true
					failed++
					continue
				}
				shas = append(shas, list[i].SHA256)
			}
			if len(shas) == 0 {
				continue
			}
			vecs, err := v.run(len(shas))
			if err != nil {
				return err
			}
			for i, sha := range shas {
				items = append(items, map[string]string{"sha256": sha, "vec": encodeVec(vecs[i])})
			}
		}
		if len(items) > 0 {
			if err := c.send("PUT", "/api/v1/ai/embeddings", map[string]any{"model": modelName, "items": items}); err != nil {
				return err
			}
		}
		done += len(items)
		el := time.Since(start).Seconds()
		fmt.Printf("  %d/%d (%.0f a second)\n", done, total, float64(done)/el)
	}
	fmt.Printf("done: %d vectors in %s", done, time.Since(start).Round(time.Second))
	if failed > 0 {
		fmt.Printf(", %d without a usable thumbnail", failed)
	}
	fmt.Println()
	return ctx.Err()
}

// uploadTextModel puts the phone's text model on the server, unless it is
// there already.
func uploadTextModel(c *client) error {
	want := map[string]string{"text.onnx": "text-arm64", "vocab.txt": "vocab", "dense.safetensors": "dense"}
	meta, _ := json.Marshal(map[string]any{
		"model": modelName, "dim": dim, "max_tokens": maxTokens, "cased": true,
		"pooling": "mean", "dense": "linear.weight 512x768",
		"source": "sentence-transformers/clip-ViT-B-32-multilingual-v1 (qint8 arm64)",
	})
	have := map[string]string{}
	if b, err := c.get("/api/v1/ai/model"); err == nil {
		var m struct {
			Model string `json:"model"`
			Files []struct {
				Name   string `json:"name"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		}
		json.Unmarshal(b, &m)
		if m.Model == modelName {
			for _, f := range m.Files {
				have[f.Name] = f.SHA256
			}
		}
	}
	for name, key := range want {
		if have[name] == files[key].sha256 {
			continue
		}
		p, err := fetch(key)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		fmt.Printf("uploading %s for phones\n", name)
		res, err := c.do("PUT", "/api/v1/ai/model/"+modelName+"/"+name, f, "application/octet-stream")
		f.Close()
		if err != nil {
			return err
		}
		res.Body.Close()
	}
	// model.json last: with it, the model is complete and offered to clients.
	res, err := c.do("PUT", "/api/v1/ai/model/"+modelName+"/model.json", bytes.NewReader(meta), "application/json")
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// --- search ---

func search(c *client, q string) error {
	if q == "" {
		return errors.New("what to search for?")
	}
	tp, err := fetch("text")
	if err != nil {
		return err
	}
	vp, err := fetch("vocab")
	if err != nil {
		return err
	}
	dp, err := fetch("dense")
	if err != nil {
		return err
	}
	tok, err := loadVocab(vp)
	if err != nil {
		return err
	}
	dense, err := loadDense(dp)
	if err != nil {
		return err
	}
	vec, err := (&text{tp, tok, dense}).embed(q)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"model": modelName, "vec": encodeVec(vec), "limit": 20})
	res, err := c.do("POST", "/api/v1/ai/search", bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		Results []struct {
			ID    int64   `json:"id"`
			Score float32 `json:"score"`
		} `json:"results"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	for _, r := range out.Results {
		var a struct {
			Name      string `json:"name"`
			LocalTime string `json:"local_time"`
		}
		if b, err := c.get(fmt.Sprintf("/api/v1/assets/%d", r.ID)); err == nil {
			json.Unmarshal(b, &a)
		}
		fmt.Printf("%.3f  %-36s %s\n", r.Score, a.Name, a.LocalTime)
	}
	if len(out.Results) == 0 {
		fmt.Println("nothing found")
	}
	return nil
}
