// Package download fetches Dream Cast audio from DASH/HLS with parallel
// segment downloads and resumable caching.
package download

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dreamtui/internal/media"
	"dreamtui/internal/site"
)

// Progress is a snapshot of a download.
type Progress struct {
	Phase         string
	SegmentsDone  int
	SegmentsTotal int
	Bytes         int64
	ExpectedBytes int64
	Speed         float64
	Source        string
}

// Options controls downloads.
type Options struct {
	Workers  int
	Force    bool
	Client   *http.Client
	Progress func(Progress)
}

// Result describes a downloaded audio file.
type Result struct {
	Path     string
	Duration float64
	Codec    string
	Source   string
}

type downloader struct {
	ctx          context.Context
	client       *http.Client
	opts         Options
	ua           string
	progress     func(Progress)
	phase        string
	total        int64
	done         int64
	bytes        int64
	expected     int64
	started      time.Time
	stopProgress func()
}

// Audio downloads the best available audio track. opusHLS and aacHLS are HLS
// master URLs from the release page. DASH Opus is tried first.
func Audio(ctx context.Context, opusHLS, aacHLS, workDir string, opts Options) (*Result, error) {
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.Workers > 16 {
		opts.Workers = 16
	}
	dest := filepath.Join(workDir, "audio.mka")
	if !opts.Force {
		if st, err := os.Stat(dest); err == nil && st.Size() > 1024 {
			if info, err := media.Probe(ctx, dest); err == nil {
				codec := ""
				if a := info.Audio(); len(a) > 0 {
					codec = a[0].Codec
				}
				return &Result{Path: dest, Duration: info.Duration, Codec: codec, Source: "cache"}, nil
			}
		}
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}

	var errs []error
	if durl := dashFromHLS(opusHLS); durl != "" {
		res, err := downloadDASH(ctx, durl, workDir, dest, opts)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("DASH Opus: %w", err))
	}
	if durl := dashFromHLS(aacHLS); durl != "" {
		res, err := downloadDASH(ctx, durl, workDir, dest, opts)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("DASH AAC: %w", err))
	}
	if aacHLS != "" {
		res, err := downloadHLS(ctx, aacHLS, workDir, dest, opts)
		if err == nil {
			return res, nil
		}
		errs = append(errs, fmt.Errorf("HLS AAC: %w", err))
	}
	return nil, fmt.Errorf("не удалось скачать аудио: %w", errors.Join(errs...))
}

func dashFromHLS(u string) string {
	if u == "" {
		return ""
	}
	if strings.Contains(u, "/hls/") && strings.HasSuffix(u, "/master.m3u8") {
		return strings.Replace(strings.Replace(u, "/hls/", "/dash/", 1), "/master.m3u8", "/manifest.mpd", 1)
	}
	return ""
}

func newDownloader(ctx context.Context, opts Options, phase string) *downloader {
	client := opts.Client
	if client == nil {
		tr := &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 24,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
		}
		client = &http.Client{Transport: tr}
	}
	return &downloader{
		ctx:      ctx,
		client:   client,
		opts:     opts,
		ua:       site.UserAgent,
		progress: opts.Progress,
		phase:    phase,
		started:  time.Now(),
	}
}

func (d *downloader) runProgress(total int, expected int64) {
	d.total = int64(total)
	d.expected = expected
	if d.progress == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		emit := func() {
			d.progress(Progress{
				Phase:         d.phase,
				SegmentsDone:  int(atomic.LoadInt64(&d.done)),
				SegmentsTotal: int(atomic.LoadInt64(&d.total)),
				Bytes:         atomic.LoadInt64(&d.bytes),
				ExpectedBytes: expected,
				Speed:         float64(atomic.LoadInt64(&d.bytes)) / math.Max(time.Since(d.started).Seconds(), 0.1),
				Source:        d.phase,
			})
		}
		emit()
		for {
			select {
			case <-done:
				emit()
				return
			case <-d.ctx.Done():
				emit()
				return
			case <-ticker.C:
				emit()
			}
		}
	}()
	d.stopProgress = func() { close(done) }
}

func (d *downloader) fetch(ctx context.Context, rawURL, dest string, count bool) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt*attempt) * 400 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", d.ua)
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Referer", "https://dreamerscast.com/")
		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
				return lastErr
			}
			continue
		}
		tmp := dest + ".part"
		f, err := os.Create(tmp)
		if err != nil {
			_ = resp.Body.Close()
			return err
		}
		n, copyErr := io.Copy(f, resp.Body)
		closeErr := f.Close()
		_ = resp.Body.Close()
		if copyErr != nil || closeErr != nil {
			_ = os.Remove(tmp)
			lastErr = errors.Join(copyErr, closeErr)
			continue
		}
		if n == 0 {
			_ = os.Remove(tmp)
			lastErr = fmt.Errorf("пустой сегмент")
			continue
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if count {
			atomic.AddInt64(&d.bytes, n)
			atomic.AddInt64(&d.done, 1)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("неизвестная ошибка")
	}
	return lastErr
}

func (d *downloader) fetchText(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", d.ua)
	req.Header.Set("Accept", "*/*")
	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(b), err
}

func (d *downloader) parallel(ctx context.Context, total int, work func(context.Context, int) error) error {
	if total == 0 {
		return fmt.Errorf("нет сегментов для скачивания")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr error
	var once sync.Once
	for i := 0; i < d.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if err := work(ctx, idx); err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		}()
	}
	for i := 0; i < total; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (d *downloader) makeMKA(ctx context.Context, input, dest string) error {
	tmp := dest + ".part.mka"
	_ = os.Remove(tmp)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error", "-y",
		"-i", input,
		"-map", "0:a:0", "-vn", "-c:a", "copy",
		"-f", "matroska", tmp,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg remux: %s", strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return nil
}

func (d *downloader) finish(ctx context.Context, dest string) (*Result, error) {
	info, err := media.Probe(ctx, dest)
	if err != nil {
		return nil, fmt.Errorf("проверка скачанного аудио: %w", err)
	}
	codec := ""
	if a := info.Audio(); len(a) > 0 {
		codec = a[0].Codec
	}
	return &Result{Path: dest, Duration: info.Duration, Codec: codec, Source: d.phase}, nil
}

// ---- DASH ----

type mpdXML struct {
	Duration string `xml:"mediaPresentationDuration,attr"`
	Periods  []struct {
		AdaptationSets []struct {
			ContentType string       `xml:"contentType,attr"`
			MimeType    string       `xml:"mimeType,attr"`
			Template    *templateXML `xml:"SegmentTemplate"`
			Reps        []repXML     `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

type repXML struct {
	ID        string       `xml:"id,attr"`
	MimeType  string       `xml:"mimeType,attr"`
	Codecs    string       `xml:"codecs,attr"`
	Bandwidth int64        `xml:"bandwidth,attr"`
	Template  *templateXML `xml:"SegmentTemplate"`
}

type templateXML struct {
	Media          string `xml:"media,attr"`
	Initialization string `xml:"initialization,attr"`
	Timescale      int64  `xml:"timescale,attr"`
	StartNumber    int    `xml:"startNumber,attr"`
	Duration       int64  `xml:"duration,attr"`
	Timeline       []struct {
		D int64 `xml:"d,attr"`
		R int64 `xml:"r,attr"`
	} `xml:"SegmentTimeline>S"`
}

type dashSegment struct {
	Number int
	Time   int64
	URL    string
}

func downloadDASH(ctx context.Context, manifestURL, workDir, dest string, opts Options) (*Result, error) {
	d := newDownloader(ctx, opts, "DASH Opus")
	text, err := d.fetchText(ctx, manifestURL)
	if err != nil {
		return nil, err
	}
	var mpd mpdXML
	if err := xml.Unmarshal([]byte(text), &mpd); err != nil {
		return nil, err
	}
	var chosen *templateXML
	var rep repXML
	bestScore := -1
	for _, p := range mpd.Periods {
		for _, a := range p.AdaptationSets {
			for _, r := range a.Reps {
				mime := strings.ToLower(r.MimeType)
				codecs := strings.ToLower(r.Codecs)
				if !strings.HasPrefix(mime, "audio/") && !strings.Contains(codecs, "opus") &&
					!strings.Contains(codecs, "mp4a") && strings.ToLower(a.ContentType) != "audio" {
					continue
				}
				tmpl := r.Template
				if tmpl == nil || tmpl.Media == "" {
					tmpl = a.Template
				}
				if tmpl == nil || tmpl.Media == "" {
					continue
				}
				score := 0
				if strings.Contains(codecs, "opus") {
					score = 100
				} else if strings.Contains(codecs, "mp4a") {
					score = 50
				}
				if score > bestScore {
					bestScore = score
					chosen = tmpl
					rep = r
				}
			}
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("в DASH не найдена аудиодорожка")
	}
	if chosen.Timescale <= 0 {
		chosen.Timescale = 1000
	}
	startNumber := chosen.StartNumber
	if startNumber <= 0 {
		startNumber = 1
	}
	if strings.Contains(d.phase, "Opus") && !strings.Contains(strings.ToLower(rep.Codecs), "opus") {
		d.phase = "DASH AAC"
	}

	initURL := expandTemplate(chosen.Initialization, rep, startNumber, 0)
	if initURL == "" {
		return nil, fmt.Errorf("в DASH нет initialization")
	}
	segDir := filepath.Join(workDir, "dash-"+safeName(rep.ID))
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}
	initPath := filepath.Join(segDir, "init.mp4")
	if st, err := os.Stat(initPath); err != nil || st.Size() == 0 {
		if err := d.fetch(ctx, initURL, initPath, false); err != nil {
			return nil, fmt.Errorf("init DASH: %w", err)
		}
	}

	var segs []dashSegment
	if len(chosen.Timeline) > 0 {
		number := startNumber
		var t int64
		for _, s := range chosen.Timeline {
			repeats := s.R
			if repeats < 0 {
				repeats = 0
			}
			for k := int64(0); k <= repeats; k++ {
				u := expandTemplate(chosen.Media, rep, number, t)
				segs = append(segs, dashSegment{Number: number, Time: t, URL: u})
				number++
				t += s.D
			}
		}
	} else if chosen.Duration > 0 {
		total := parseISODuration(mpd.Duration)
		n := int(total * float64(chosen.Timescale) / float64(chosen.Duration))
		if n < 1 {
			n = 1
		}
		var t int64
		for i := 0; i < n; i++ {
			number := startNumber + i
			u := expandTemplate(chosen.Media, rep, number, t)
			segs = append(segs, dashSegment{Number: number, Time: t, URL: u})
			t += chosen.Duration
		}
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("DASH: не удалось построить список фрагментов")
	}

	expected := int64(float64(rep.Bandwidth) * parseISODuration(mpd.Duration) / 8)
	d.runProgress(len(segs), expected)
	defer d.stopProgress()

	// Count already-downloaded fragments for resume.
	for i := range segs {
		p := filepath.Join(segDir, fmt.Sprintf("frag-%05d.m4s", segs[i].Number))
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			atomic.AddInt64(&d.bytes, st.Size())
			atomic.AddInt64(&d.done, 1)
			segs[i].URL = "" // marker for cached
		}
	}

	err = d.parallel(ctx, len(segs), func(ctx context.Context, i int) error {
		seg := segs[i]
		p := filepath.Join(segDir, fmt.Sprintf("frag-%05d.m4s", seg.Number))
		if seg.URL == "" {
			if st, err := os.Stat(p); err == nil && st.Size() > 0 {
				return nil
			}
		}
		if err := d.fetch(ctx, seg.URL, p, true); err != nil {
			return fmt.Errorf("фрагмент %d: %w", seg.Number, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	combined := filepath.Join(segDir, "audio.mp4")
	if err := concatFiles(combined, append([]string{initPath}, segmentPaths(segDir, segs)...)); err != nil {
		return nil, err
	}
	if err := d.makeMKA(ctx, combined, dest); err != nil {
		return nil, err
	}
	res, err := d.finish(ctx, dest)
	if err != nil {
		return nil, err
	}
	res.Source = d.phase
	_ = os.RemoveAll(segDir)
	return res, nil
}

func segmentPaths(dir string, segs []dashSegment) []string {
	paths := make([]string, 0, len(segs))
	for _, s := range segs {
		paths = append(paths, filepath.Join(dir, fmt.Sprintf("frag-%05d.m4s", s.Number)))
	}
	return paths
}

func expandTemplate(tmpl string, rep repXML, number int, tm int64) string {
	if tmpl == "" {
		return ""
	}
	out := tmpl
	out = strings.ReplaceAll(out, "$RepresentationID$", rep.ID)
	out = strings.ReplaceAll(out, "$Number$", strconv.Itoa(number))
	out = strings.ReplaceAll(out, "$Bandwidth$", strconv.FormatInt(rep.Bandwidth, 10))
	out = strings.ReplaceAll(out, "$Time$", strconv.FormatInt(tm, 10))
	return out
}

func concatFiles(out string, files []string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(out)
		}
	}()
	for _, p := range files {
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, in)
		_ = in.Close()
		if err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func parseISODuration(s string) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "PT")
	s = strings.TrimSuffix(s, "S")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func safeName(s string) string {
	if s == "" {
		return "audio"
	}
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	return s
}

// ---- HLS fallback ----

type hlsVariant struct {
	URL       string
	Bandwidth int64
	AudioOnly bool
}

func downloadHLS(ctx context.Context, masterURL, workDir, dest string, opts Options) (*Result, error) {
	d := newDownloader(ctx, opts, "HLS AAC")
	text, err := d.fetchText(ctx, masterURL)
	if err != nil {
		return nil, err
	}
	if strings.Contains(text, "#EXT-X-STREAM-INF") {
		v, err := chooseVariant(text, masterURL)
		if err != nil {
			return nil, err
		}
		text, err = d.fetchText(ctx, v.URL)
		if err != nil {
			return nil, err
		}
	}
	var segs []string
	var totalDur float64
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#EXTINF:") {
			continue
		}
		if d, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ","), 64); err == nil {
			totalDur += d
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			if strings.HasPrefix(next, "http") {
				segs = append(segs, next)
			} else if strings.HasPrefix(next, "/") {
				segs = append(segs, "https://play.dreamerscast.com"+next)
			}
			break
		}
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("HLS: не найдены сегменты")
	}
	segDir := filepath.Join(workDir, "hls")
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return nil, err
	}
	expected := int64(totalDur * 144000 / 8)
	d.runProgress(len(segs), expected)
	defer d.stopProgress()
	var paths []string
	for i := range segs {
		paths = append(paths, filepath.Join(segDir, fmt.Sprintf("seg-%05d.ts", i+1)))
	}
	for i, p := range paths {
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			atomic.AddInt64(&d.done, 1)
			atomic.AddInt64(&d.bytes, st.Size())
			segs[i] = ""
		}
	}
	err = d.parallel(ctx, len(segs), func(ctx context.Context, i int) error {
		if segs[i] == "" {
			if st, err := os.Stat(paths[i]); err == nil && st.Size() > 0 {
				return nil
			}
		}
		if err := d.fetch(ctx, segs[i], paths[i], true); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	list := filepath.Join(segDir, "list.txt")
	var b strings.Builder
	for _, p := range paths {
		abs, _ := filepath.Abs(p)
		fmt.Fprintf(&b, "file '%s'\n", strings.ReplaceAll(abs, "'", "'\\''"))
	}
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	tmpMKA := dest + ".hls.part.mka"
	_ = os.Remove(tmpMKA)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-v", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list,
		"-map", "0:a:0", "-c:a", "copy", "-f", "matroska", tmpMKA,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("HLS remux: %s", strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmpMKA, dest); err != nil {
		return nil, err
	}
	res, err := d.finish(ctx, dest)
	if err != nil {
		return nil, err
	}
	res.Source = d.phase
	_ = os.RemoveAll(segDir)
	return res, nil
}

func chooseVariant(text, base string) (hlsVariant, error) {
	var variants []hlsVariant
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#EXT-X-STREAM-INF:") {
			continue
		}
		attrs := strings.TrimPrefix(strings.TrimSpace(line), "#EXT-X-STREAM-INF:")
		var v hlsVariant
		for _, part := range strings.Split(attrs, ",") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch kv[0] {
			case "BANDWIDTH":
				v.Bandwidth, _ = strconv.ParseInt(kv[1], 10, 64)
			case "RESOLUTION":
				v.AudioOnly = false
			case "CODECS":
				v.AudioOnly = !strings.Contains(kv[1], "avc") && !strings.Contains(kv[1], "hev") && !strings.Contains(kv[1], "av01")
			}
		}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if next == "" || strings.HasPrefix(next, "#") {
				continue
			}
			v.URL = next
			break
		}
		if v.URL != "" {
			variants = append(variants, v)
		}
	}
	if len(variants) == 0 {
		return hlsVariant{URL: base, AudioOnly: true}, nil
	}
	best := variants[0]
	for _, v := range variants[1:] {
		if v.AudioOnly && !best.AudioOnly {
			best = v
			continue
		}
		if v.AudioOnly == best.AudioOnly && v.Bandwidth < best.Bandwidth {
			best = v
		}
	}
	return best, nil
}
