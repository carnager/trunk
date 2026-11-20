package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"sync"
	"time"
)

// indexFaces looks for faces in every photo not looked at yet and sends
// them to trunk, which groups them into people.
func indexFaces(ctx context.Context, c *client, threads int) error {
	dp, err := fetch("yunet")
	if err != nil {
		return err
	}
	rp, err := fetch("sface")
	if err != nil {
		return err
	}
	m, err := newFaceModels(dp, rp, threads)
	if err != nil {
		return err
	}
	defer m.close()
	start := time.Now()
	total, done, found := -1, 0, 0
	skip := map[string]bool{}
	type todoItem struct {
		ID     int64  `json:"id"`
		SHA256 string `json:"sha256"`
	}
	for ctx.Err() == nil {
		var todo struct {
			Assets    []todoItem `json:"assets"`
			Remaining int        `json:"remaining"`
		}
		b, err := c.get("/api/v1/ai/faces/todo?model=" + facesModel + "&limit=64")
		if err != nil {
			return err
		}
		json.Unmarshal(b, &todo)
		if total < 0 {
			total = todo.Remaining
			if total == 0 {
				fmt.Println("faces: every photo looked at")
				return nil
			}
			fmt.Printf("faces: %d photos to look at\n", total)
		}
		var list []todoItem
		for _, a := range todo.Assets {
			if !skip[a.SHA256] {
				list = append(list, a)
			}
		}
		if len(list) == 0 {
			break
		}
		// Previews come and are decoded in parallel; the models take them one by one.
		imgs := make([]image.Image, len(list))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		for i, a := range list {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				if raw, err := c.get(fmt.Sprintf("/api/v1/ai/thumb/%d?size=preview", a.ID)); err == nil {
					imgs[i], _, _ = image.Decode(bytes.NewReader(raw))
				}
			}()
		}
		wg.Wait()
		var items []map[string]any
		for i, a := range list {
			img := imgs[i]
			if img == nil {
				skip[a.SHA256] = true
				continue
			}
			fs, err := m.find(img)
			if err != nil {
				return err
			}
			W, H := float64(img.Bounds().Dx()), float64(img.Bounds().Dy())
			faces := []map[string]any{}
			for _, f := range fs {
				if f.vec == nil {
					continue
				}
				faces = append(faces, map[string]any{
					"box":     []float64{f.x / W, f.y / H, f.w / W, f.h / H},
					"score":   f.score,
					"frontal": f.frontal(),
					"vec":     encodeVec(f.vec),
				})
			}
			found += len(faces)
			items = append(items, map[string]any{"sha256": a.SHA256, "faces": faces})
		}
		if len(items) > 0 {
			if err := c.send("PUT", "/api/v1/ai/faces", map[string]any{"model": facesModel, "items": items}); err != nil {
				return err
			}
		}
		done += len(items)
		fmt.Printf("  %d/%d photos, %d faces (%.1f photos a second)\n", done, total, found, float64(done)/time.Since(start).Seconds())
	}
	fmt.Printf("faces: %d photos, %d faces in %s\n", done, found, time.Since(start).Round(time.Second))
	return ctx.Err()
}
