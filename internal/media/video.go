package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ffOnce                sync.Once
	ffmpegBin, ffprobeBin string
)

func lookupFF() {
	ffOnce.Do(func() {
		ffmpegBin, _ = exec.LookPath("ffmpeg")
		ffprobeBin, _ = exec.LookPath("ffprobe")
	})
}

// HaveFFmpeg reports whether video posters and video metadata are available.
func HaveFFmpeg() bool {
	lookupFF()
	return ffmpegBin != "" && ffprobeBin != ""
}

type probeOut struct {
	Streams []struct {
		CodecType    string            `json:"codec_type"`
		Width        int               `json:"width"`
		Height       int               `json:"height"`
		Tags         map[string]string `json:"tags"`
		SideDataList []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}

func probeVideo(path string, in *Info) {
	lookupFF()
	if ffprobeBin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffprobeBin, "-v", "quiet", "-print_format", "json",
		"-show_format", "-show_streams", path).Output()
	if err != nil {
		return
	}
	var p probeOut
	if json.Unmarshal(out, &p) != nil {
		return
	}
	if d, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil {
		in.DurationMS = int64(d * 1000)
	}
	tags := lowerKeys(p.Format.Tags)
	for _, s := range p.Streams {
		if s.CodecType != "video" || in.Width != 0 {
			continue
		}
		in.Width, in.Height = s.Width, s.Height
		rot := 0.0
		if r, err := strconv.ParseFloat(s.Tags["rotate"], 64); err == nil {
			rot = r
		}
		for _, sd := range s.SideDataList {
			if sd.Rotation != 0 {
				rot = sd.Rotation
			}
		}
		if r := int(rot) % 180; r == 90 || r == -90 {
			in.Width, in.Height = in.Height, in.Width
		}
		for k, v := range lowerKeys(s.Tags) {
			if _, ok := tags[k]; !ok {
				tags[k] = v
			}
		}
	}
	// QuickTime and MP4 store creation_time in UTC; the local offset is lost.
	if t, err := time.Parse(time.RFC3339Nano, tags["creation_time"]); err == nil && t.Year() > 1970 {
		in.TakenAt, in.TakenFrom = t.UTC(), "video"
	}
	// Apple writes a local time with offset here, which is better when present.
	if t, err := time.Parse("2006-01-02T15:04:05-0700", tags["com.apple.quicktime.creationdate"]); err == nil {
		in.TakenAt, in.TakenFrom = t, "video"
		_, off := t.Zone()
		m := off / 60
		in.TZOffset = &m
	}
	for _, k := range []string{"location", "com.apple.quicktime.location.iso6709", "location-eng"} {
		if lat, lon, alt, ok := parseISO6709(tags[k]); ok {
			in.Lat, in.Lon, in.Alt = &lat, &lon, alt
			break
		}
	}
	in.Make = firstNonEmpty(tags["com.apple.quicktime.make"], tags["com.android.manufacturer"], tags["make"])
	in.Model = firstNonEmpty(tags["com.apple.quicktime.model"], tags["com.android.model"], tags["model"])
}

var reISO6709 = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)?`)

func parseISO6709(s string) (lat, lon float64, alt *float64, ok bool) {
	m := reISO6709.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, nil, false
	}
	lat, _ = strconv.ParseFloat(m[1], 64)
	lon, _ = strconv.ParseFloat(m[2], 64)
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 || (lat == 0 && lon == 0) {
		return 0, 0, nil, false
	}
	if m[3] != "" {
		if a, err := strconv.ParseFloat(m[3], 64); err == nil {
			alt = &a
		}
	}
	return lat, lon, alt, true
}

// videoFrame grabs an upright frame that fits in max×max.
func videoFrame(path string, durationMS int64, max int) (image.Image, error) {
	lookupFF()
	if ffmpegBin == "" {
		return nil, fmt.Errorf("ffmpeg not available")
	}
	// A second in is usually past fade-ins and black first frames.
	at := 1.0
	if durationMS > 0 && durationMS < 2000 {
		at = float64(durationMS) / 2000
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Fit inside max×max but never upscale small clips.
	scale := fmt.Sprintf("scale=w='min(iw,%d)':h='min(ih,%d)':force_original_aspect_ratio=decrease", max, max)
	seek := []string{"-ss", strconv.FormatFloat(at, 'f', 3, 64)}
	first := []string{"-map", "0:v:0"}
	// The first video stream is the clip. ffmpeg on its own picks the largest,
	// which in Pixel motion photos exported as MP4 is a still without usable
	// frames. Seeking past the end of very short clips yields nothing, hence
	// the attempts from the start; ffmpeg's own choice comes last.
	var errs []string
	for _, try := range [][]string{append(seek, first...), first, nil} {
		args := []string{"-v", "error"}
		if len(try) > 0 && try[0] == "-ss" {
			args = append(args, try[:2]...)
			try = try[2:]
		}
		args = append(args, "-i", path)
		args = append(args, try...)
		args = append(args, "-frames:v", "1", "-vf", scale, "-f", "image2", "-c:v", "png", "pipe:1")
		cmd := exec.CommandContext(ctx, ffmpegBin, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err == nil && len(out) > 0 {
			img, _, err := image.Decode(bytes.NewReader(out))
			return img, err
		}
		errs = append(errs, fmt.Sprintf("%v %s", err, strings.TrimSpace(stderr.String())))
	}
	return nil, fmt.Errorf("ffmpeg found no frame: %s", strings.Join(errs, "; "))
}

func lowerKeys(m map[string]string) map[string]string {
	r := make(map[string]string, len(m))
	for k, v := range m {
		r[strings.ToLower(k)] = v
	}
	return r
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
