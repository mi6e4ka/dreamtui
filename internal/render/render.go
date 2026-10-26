// Package render builds the final MKA from a synchronized timeline.
package render

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"dreamtui/internal/sync"
)

// Options controls output rendering.
type Options struct {
	Output        string
	VideoURL      string
	RefStream     int
	VideoDuration float64
	Bitrate       string
	Title         string
	TempDir       string
}

// MuxOptions controls remuxing the aligned audio into the source video.
type MuxOptions struct {
	Output        string
	VideoURL      string
	AudioPath     string
	VideoDuration float64
	Title         string
	Force         bool
	Progress      func(percent float64)
}

// Mux copies all source streams into a local MKV and appends the aligned
// Dream Cast track. The video is not re-encoded, but the whole stream is read
// from the remote source, so this takes as long as downloading the episode.
func Mux(ctx context.Context, opts MuxOptions) error {
	if opts.VideoURL == "" {
		return fmt.Errorf("нет URL видео для вшивания")
	}
	if opts.AudioPath == "" {
		return fmt.Errorf("нет дорожки Dream Cast для вшивания")
	}
	if !opts.Force {
		if st, err := os.Stat(opts.Output); err == nil && st.Size() > 0 {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o755); err != nil {
		return err
	}
	tmp := opts.Output + ".part.mkv"
	_ = os.Remove(tmp)
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-rw_timeout", "30000000",
		"-i", opts.VideoURL,
		"-i", opts.AudioPath,
		"-map", "0", "-map", "1:a:0",
		"-c", "copy",
		"-metadata", "title=" + opts.Title,
		"-metadata", "comment=Dream Cast / dreamtui",
		"-progress", "pipe:1", "-nostats",
		"-f", "matroska",
		tmp,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg mux: %w", err)
	}
	lastPercent := -1.0
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "out_time_ms=") {
			continue
		}
		us, err := strconv.ParseFloat(strings.TrimPrefix(line, "out_time_ms="), 64)
		if err != nil || opts.VideoDuration <= 0 {
			continue
		}
		percent := us / 1e6 / opts.VideoDuration * 100
		if percent > 100 {
			percent = 100
		}
		if opts.Progress != nil && percent-lastPercent >= 1 {
			lastPercent = percent
			opts.Progress(percent)
		}
	}
	if err := cmd.Wait(); err != nil {
		_ = os.Remove(tmp)
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffmpeg mux: %s", msg)
	}
	if opts.Progress != nil {
		opts.Progress(100)
	}
	if err := os.Rename(tmp, opts.Output); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Render writes the synchronized audio track to opts.Output.
func Render(ctx context.Context, dubPath string, result *sync.Result, opts Options) error {
	if result == nil || len(result.Segments) == 0 {
		return fmt.Errorf("нет таймлайна для рендера")
	}
	if opts.Bitrate == "" {
		opts.Bitrate = "192k"
	}
	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o755); err != nil {
		return err
	}
	if len(result.Segments) == 1 && result.Segments[0].Kind == sync.Dub {
		seg := result.Segments[0]
		return renderUniform(ctx, dubPath, seg, opts)
	}
	return renderSegments(ctx, dubPath, result, opts)
}

func renderUniform(ctx context.Context, dubPath string, seg sync.Segment, opts Options) error {
	tmp := opts.Output + ".part.mka"
	_ = os.Remove(tmp)
	length := seg.End - seg.Start
	srcStart := math.Max(0, seg.SourceStart)
	lead := math.Max(0, -seg.SourceStart)
	filter := "aresample=48000"
	if lead > 0.001 {
		filter += fmt.Sprintf(",adelay=%d:all=1", int(math.Round(lead*1000)))
	}
	filter += ",apad"
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", srcStart),
		"-i", dubPath,
		"-vn",
		"-af", filter,
		"-t", fmt.Sprintf("%.3f", length),
		"-c:a", "libopus", "-b:a", opts.Bitrate, "-vbr", "on",
		"-map_metadata", "-1",
		"-metadata:s:a:0", "language=rus",
		"-metadata:s:a:0", "title=Dream Cast",
		"-metadata", "title=" + opts.Title,
		"-metadata", "comment=Dream Cast / dreamtui",
		"-f", "matroska", tmp,
	}
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg render: %s", strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, opts.Output); err != nil {
		return err
	}
	return nil
}

func renderSegments(ctx context.Context, dubPath string, result *sync.Result, opts Options) error {
	baseDir := opts.TempDir
	if baseDir == "" {
		baseDir = filepath.Dir(opts.Output)
	}
	work, err := os.MkdirTemp(baseDir, ".dreamtui-render-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	var files []string
	for i, seg := range result.Segments {
		length := seg.End - seg.Start
		if length <= 0.001 {
			continue
		}
		out := filepath.Join(work, fmt.Sprintf("seg-%04d.mka", i))
		switch seg.Kind {
		case sync.Dub:
			if err := renderDubSegment(ctx, dubPath, seg, length, out, opts); err != nil {
				return err
			}
		case sync.Reference:
			if err := renderReferenceSegment(ctx, seg, length, out, opts); err != nil {
				// A failed remote reference fetch must not lose the whole job.
				if err2 := renderSilenceSegment(ctx, length, out, opts); err2 != nil {
					return err
				}
			}
		default:
			if err := renderSilenceSegment(ctx, length, out, opts); err != nil {
				return err
			}
		}
		files = append(files, out)
	}
	if len(files) == 0 {
		return fmt.Errorf("пустой таймлайн")
	}
	list := filepath.Join(work, "list.txt")
	var b strings.Builder
	for _, p := range files {
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(p, "'", "'\\''"))
	}
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		return err
	}
	tmp := opts.Output + ".part.mka"
	_ = os.Remove(tmp)
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list,
		"-c:a", "copy",
		"-map_metadata", "-1",
		"-metadata:s:a:0", "language=rus",
		"-metadata:s:a:0", "title=Dream Cast",
		"-metadata", "title=" + opts.Title,
		"-metadata", "comment=Dream Cast / dreamtui",
		"-f", "matroska", tmp,
	}
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("склейка сегментов: %s", strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, opts.Output)
}

func renderDubSegment(ctx context.Context, dubPath string, seg sync.Segment, length float64, out string, opts Options) error {
	srcStart := math.Max(0, seg.SourceStart)
	lead := math.Max(0, -seg.SourceStart)
	filter := "aresample=48000"
	if lead > 0.001 {
		filter += fmt.Sprintf(",adelay=%d:all=1", int(math.Round(lead*1000)))
	}
	filter += ",apad"
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-ss", fmt.Sprintf("%.3f", srcStart),
		"-i", dubPath,
		"-vn", "-af", filter,
		"-t", fmt.Sprintf("%.3f", length),
		"-c:a", "libopus", "-b:a", opts.Bitrate, "-vbr", "on",
		out,
	}
	if b, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg dub segment: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

func renderReferenceSegment(ctx context.Context, seg sync.Segment, length float64, out string, opts Options) error {
	if opts.VideoURL == "" {
		return fmt.Errorf("нет URL видео для заполнения пропуска")
	}
	filter := fmt.Sprintf("atrim=duration=%.3f,asetpts=PTS-STARTPTS,aresample=48000,apad", length)
	fade := math.Min(0.05, length/4)
	if fade > 0.001 {
		filter += fmt.Sprintf(",afade=t=in:st=0:d=%.3f,afade=t=out:st=%.3f:d=%.3f", fade, length-fade, fade)
	}
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-rw_timeout", "30000000",
		"-ss", fmt.Sprintf("%.3f", seg.Start),
		"-t", fmt.Sprintf("%.3f", length),
		"-i", opts.VideoURL,
		"-map", fmt.Sprintf("0:%d", opts.RefStream),
		"-vn", "-ac", "2", "-af", filter,
		"-t", fmt.Sprintf("%.3f", length),
		"-c:a", "libopus", "-b:a", opts.Bitrate, "-vbr", "on",
		out,
	}
	if b, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg reference segment: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

func renderSilenceSegment(ctx context.Context, length float64, out string, opts Options) error {
	args := []string{
		"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
		"-t", fmt.Sprintf("%.3f", length),
		"-c:a", "libopus", "-b:a", opts.Bitrate, "-vbr", "on",
		out,
	}
	if b, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg silence segment: %s", strings.TrimSpace(string(b)))
	}
	return nil
}
