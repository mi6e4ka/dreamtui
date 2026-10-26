// Package app orchestrates discovery, download, alignment and rendering.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"dreamtui/internal/audio"
	"dreamtui/internal/download"
	"dreamtui/internal/m3u"
	"dreamtui/internal/media"
	"dreamtui/internal/render"
	"dreamtui/internal/site"
	align "dreamtui/internal/sync"
)

// Options controls a full extraction run.
type Options struct {
	ReleaseURL     string
	M3UPath        string
	VideoDir       string
	OutDir         string
	WorkDir        string
	EpisodeNumbers []int
	Limit          int
	Sync           bool
	NoSync         bool
	ManualOffset   *float64
	ApplyAnyway    bool
	Force          bool
	Mux            bool
	RefStream      int
	SegmentWorkers int
	AnchorWorkers  int
	WindowSeconds  float64
	Event          func(Event)
}

// Plan is the reviewed release-to-video mapping.
type Plan struct {
	Release    *site.Release
	Entries    []m3u.Entry
	Matches    []Match
	Source     string // "m3u", "folder" or "" when there is no video source
	SourcePath string
	OutDir     string
	WorkDir    string
}

// Match maps a Dream Cast episode to an m3u video entry.
type Match struct {
	Episode    site.Episode
	Entry      m3u.Entry
	VideoURL   string
	VideoName  string
	OutputPath string
	Selected   bool
}

// EventKind identifies progress notifications.
type EventKind string

const (
	EventLog          EventKind = "log"
	EventEpisodeStart EventKind = "episode-start"
	EventDownload     EventKind = "download"
	EventSyncProgress EventKind = "sync-progress"
	EventAlign        EventKind = "align"
	EventMux          EventKind = "mux"
	EventEpisodeDone  EventKind = "episode-done"
)

// Event is emitted during a run.
type Event struct {
	Kind     EventKind
	Episode  int
	Message  string
	Download *download.Progress
	Current  int
	Total    int
	Result   *align.Result
	Report   *EpisodeReport
}

// Report is the full run result.
type Report struct {
	Started  time.Time       `json:"started"`
	Finished time.Time       `json:"finished"`
	Release  string          `json:"release"`
	OutDir   string          `json:"out_dir"`
	Episodes []EpisodeReport `json:"episodes"`
	Warnings []string        `json:"warnings,omitempty"`
}

// EpisodeReport is the result for one episode.
type EpisodeReport struct {
	Number       int           `json:"number"`
	Title        string        `json:"title"`
	VideoName    string        `json:"video_name,omitempty"`
	VideoURL     string        `json:"video_url,omitempty"`
	RefStream    string        `json:"ref_stream,omitempty"`
	RefStreamIdx int           `json:"ref_stream_index,omitempty"`
	AudioPath    string        `json:"audio_path,omitempty"`
	OutputPath   string        `json:"output_path,omitempty"`
	MuxPath      string        `json:"mux_path,omitempty"`
	MuxError     string        `json:"mux_error,omitempty"`
	Status       string        `json:"status"`
	Error        string        `json:"error,omitempty"`
	Warning      string        `json:"warning,omitempty"`
	DownloadFrom string        `json:"download_from,omitempty"`
	Sync         *align.Result `json:"sync,omitempty"`
	Elapsed      float64       `json:"elapsed_seconds"`
}

// Discover parses the release page and m3u, then builds an output plan.
func Discover(ctx context.Context, opts Options) (*Plan, error) {
	if opts.ReleaseURL == "" {
		return nil, fmt.Errorf("не указана ссылка на релиз Dream Cast")
	}
	if opts.RefStream == 0 {
		opts.RefStream = -1
	}
	rel, err := site.Fetch(ctx, nil, opts.ReleaseURL)
	if err != nil {
		return nil, err
	}
	m3uPath := strings.TrimSpace(opts.M3UPath)
	videoDir := strings.TrimSpace(opts.VideoDir)
	if m3uPath != "" && videoDir != "" {
		return nil, fmt.Errorf("укажите либо m3u, либо папку с видео — не оба сразу")
	}
	// The path decides the source type, whichever field it came from: a
	// directory is a local video folder, a playlist file is an m3u.
	if videoDir == "" && m3uPath != "" {
		if st, statErr := os.Stat(expandHome(m3uPath)); statErr == nil && st.IsDir() {
			videoDir, m3uPath = m3uPath, ""
		}
	}
	if videoDir != "" && m3uPath == "" {
		if st, statErr := os.Stat(expandHome(videoDir)); statErr == nil && !st.IsDir() {
			m3uPath, videoDir = videoDir, ""
		}
	}
	var entries []m3u.Entry
	source, sourcePath := "", ""
	if videoDir != "" {
		sourcePath, _ = filepath.Abs(expandHome(videoDir))
		entries, err = ScanVideoDir(sourcePath)
		if err != nil {
			return nil, err
		}
		source = "folder"
	} else if m3uPath != "" {
		sourcePath, _ = filepath.Abs(expandHome(m3uPath))
		entries, err = m3u.ParseFile(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("чтение m3u: %w", err)
		}
		source = "m3u"
	}
	outDir := strings.TrimSpace(opts.OutDir)
	if outDir == "" {
		switch source {
		case "folder":
			outDir = DefaultDirOutDir(sourcePath)
		case "m3u":
			outDir = DefaultOutDir(sourcePath)
		default:
			return nil, fmt.Errorf("укажите m3u, папку с видео или папку вывода (--out)")
		}
	}
	outDir, _ = filepath.Abs(expandHome(outDir))
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = DefaultWorkDir(rel)
	}
	workDir, _ = filepath.Abs(expandHome(workDir))

	plan := &Plan{Release: rel, Entries: entries, Source: source, SourcePath: sourcePath, OutDir: outDir, WorkDir: workDir}
	byNumber := make(map[int]m3u.Entry)
	for _, e := range entries {
		if e.EpisodeNumber > 0 {
			if _, exists := byNumber[e.EpisodeNumber]; !exists {
				byNumber[e.EpisodeNumber] = e
			}
		}
	}
	usedOutputs := make(map[string]bool)
	for i, ep := range rel.Episodes {
		var entry m3u.Entry
		hasEntry := false
		if e, ok := byNumber[ep.Number]; ok {
			entry, hasEntry = e, true
		} else if i < len(entries) {
			entry, hasEntry = entries[i], true
		}
		videoName := ""
		videoURL := ""
		if hasEntry {
			videoName = entry.Name
			videoURL = entry.URL
		}
		name := outputName(videoName, ep)
		for usedOutputs[name] {
			name = strings.TrimSuffix(name, ".rus.mka") + fmt.Sprintf(" - E%02d.rus.mka", ep.Number)
		}
		usedOutputs[name] = true
		m := Match{
			Episode:    ep,
			Entry:      entry,
			VideoURL:   videoURL,
			VideoName:  videoName,
			OutputPath: filepath.Join(outDir, name),
			Selected:   true,
		}
		explicit := false
		if len(opts.EpisodeNumbers) > 0 {
			m.Selected = false
			for _, n := range opts.EpisodeNumbers {
				if n == ep.Number {
					m.Selected = true
					explicit = true
					break
				}
			}
		}
		// An episode without a matching video can still be extracted, but it
		// cannot be synchronized; do not select it by default.
		if source != "" && videoURL == "" && !explicit {
			m.Selected = false
		}
		plan.Matches = append(plan.Matches, m)
	}
	if opts.Limit > 0 {
		selected := 0
		for i := range plan.Matches {
			if !plan.Matches[i].Selected {
				continue
			}
			selected++
			if selected > opts.Limit {
				plan.Matches[i].Selected = false
			}
		}
	}
	return plan, nil
}

// Execute runs the plan. Episode failures do not abort the whole run.
func Execute(ctx context.Context, plan *Plan, opts Options) *Report {
	if opts.SegmentWorkers <= 0 {
		opts.SegmentWorkers = 8
	}
	if opts.AnchorWorkers <= 0 {
		opts.AnchorWorkers = 4
	}
	if opts.WindowSeconds <= 0 {
		opts.WindowSeconds = 30
	}
	if opts.RefStream == 0 {
		opts.RefStream = -1
	}
	emit := func(e Event) {
		if opts.Event != nil {
			opts.Event(e)
		}
	}
	rep := &Report{Started: time.Now(), Release: plan.Release.Title, OutDir: plan.OutDir}
	for _, m := range plan.Matches {
		if !m.Selected {
			continue
		}
		if ctx.Err() != nil {
			rep.Warnings = append(rep.Warnings, "остановлено пользователем")
			break
		}
		start := time.Now()
		er := EpisodeReport{
			Number:    m.Episode.Number,
			Title:     m.Episode.Title,
			VideoName: m.VideoName,
			VideoURL:  m.VideoURL,
			Status:    "running",
		}
		emit(Event{Kind: EventEpisodeStart, Episode: m.Episode.Number, Message: fmt.Sprintf("E%02d: %s", m.Episode.Number, m.Episode.Title)})
		if _, err := os.Stat(m.OutputPath); err == nil && !opts.Force {
			er.Status = "exists"
			er.OutputPath = m.OutputPath
			title := plan.Release.Title + " — " + m.Episode.Title
			if opts.Mux && m.VideoURL != "" {
				muxOut := muxPath(m.OutputPath)
				if _, statErr := os.Stat(muxOut); statErr != nil || opts.Force {
					er.MuxPath = muxOut
					if _, muxErr := muxEpisode(ctx, m, m.OutputPath, 0, title, opts, emit); muxErr != nil {
						er.MuxError = muxErr.Error()
						er.Warning = strings.TrimSpace(er.Warning + " mux: " + muxErr.Error())
					}
				} else {
					er.MuxPath = muxOut
				}
			}
			er.Elapsed = time.Since(start).Seconds()
			rep.Episodes = append(rep.Episodes, er)
			msg := "уже существует"
			if er.MuxPath != "" && er.MuxError == "" {
				msg += ", MKV готов"
			}
			emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: msg})
			continue
		}
		if err := os.MkdirAll(plan.OutDir, 0o755); err != nil {
			er.Status, er.Error = "error", err.Error()
			er.Elapsed = time.Since(start).Seconds()
			rep.Episodes = append(rep.Episodes, er)
			emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: er.Error})
			continue
		}
		epWork := filepath.Join(plan.WorkDir, fmt.Sprintf("s%02d", m.Episode.Number))
		if err := os.MkdirAll(epWork, 0o755); err != nil {
			er.Status, er.Error = "error", err.Error()
			er.Elapsed = time.Since(start).Seconds()
			rep.Episodes = append(rep.Episodes, er)
			emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: er.Error})
			continue
		}
		emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: "скачивание аудио"})
		dl, err := download.Audio(ctx, m.Episode.OpusURL, m.Episode.AACURL, epWork, download.Options{
			Workers: opts.SegmentWorkers,
			Force:   opts.Force,
			Progress: func(p download.Progress) {
				emit(Event{Kind: EventDownload, Episode: m.Episode.Number, Download: &p})
			},
		})
		if err != nil {
			er.Status, er.Error = "download-error", err.Error()
			er.Elapsed = time.Since(start).Seconds()
			rep.Episodes = append(rep.Episodes, er)
			emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: "ошибка загрузки: " + err.Error()})
			continue
		}
		er.AudioPath = dl.Path
		er.DownloadFrom = dl.Source
		emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: fmt.Sprintf("аудио готово: %s, %.1f c", dl.Codec, dl.Duration)})

		videoDur := dl.Duration
		syncResult, refDesc, refIndex, err := alignEpisode(ctx, m, dl, epWork, opts, emit)
		if err != nil {
			er.Status, er.Error = "sync-error", err.Error()
			er.Elapsed = time.Since(start).Seconds()
			rep.Episodes = append(rep.Episodes, er)
			emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: "ошибка синхронизации: " + err.Error()})
			continue
		}
		er.RefStream = refDesc
		er.RefStreamIdx = refIndex
		er.Sync = syncResult
		if syncResult != nil {
			videoDur = syncResult.VideoDuration
			if syncResult.Confidence < 0.60 && !opts.ApplyAnyway {
				er.Status = "needs-confirmation"
				er.OutputPath = m.OutputPath
				er.Warning = "низкая уверенность синхронизации; файл не создан"
				er.Elapsed = time.Since(start).Seconds()
				rep.Episodes = append(rep.Episodes, er)
				emit(Event{Kind: EventAlign, Episode: m.Episode.Number, Result: syncResult, Report: &er,
					Message: fmt.Sprintf("E%02d: offset %+.2f c, confidence %.2f — нужно подтверждение", m.Episode.Number, syncResult.Offset, syncResult.Confidence)})
				emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: "требуется подтверждение"})
				continue
			}
			for _, w := range syncResult.Warnings {
				emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: "предупреждение: " + w})
				er.Warning = strings.TrimSpace(er.Warning + " " + w)
			}
		}
		title := plan.Release.Title + " — " + m.Episode.Title
		emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: fmt.Sprintf("рендер: offset %+.2f, confidence %.2f", syncResult.Offset, syncResult.Confidence)})
		err = render.Render(ctx, dl.Path, syncResult, render.Options{
			Output:        m.OutputPath,
			VideoURL:      m.VideoURL,
			RefStream:     refIndex,
			VideoDuration: videoDur,
			Title:         title,
			TempDir:       epWork,
		})
		if err != nil {
			er.Status, er.Error = "render-error", err.Error()
			er.Elapsed = time.Since(start).Seconds()
			rep.Episodes = append(rep.Episodes, er)
			emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: "ошибка рендера: " + err.Error()})
			continue
		}
		er.OutputPath = m.OutputPath
		if opts.Mux && m.VideoURL != "" {
			er.MuxPath = muxPath(m.OutputPath)
			if _, muxErr := muxEpisode(ctx, m, m.OutputPath, videoDur, title, opts, emit); muxErr != nil {
				er.MuxError = muxErr.Error()
				er.Warning = strings.TrimSpace(er.Warning + " mux: " + muxErr.Error())
			}
		}
		er.Status = "done"
		er.Elapsed = time.Since(start).Seconds()
		rep.Episodes = append(rep.Episodes, er)
		emit(Event{Kind: EventEpisodeDone, Episode: m.Episode.Number, Report: &er, Message: "готово: " + filepath.Base(m.OutputPath)})
	}
	rep.Finished = time.Now()
	_ = writeReport(plan.OutDir, rep)
	return rep
}

// muxEpisode appends the aligned audio to a local copy of the source video.
// videoDuration is only used for progress reporting and may be zero.
func muxEpisode(ctx context.Context, m Match, audioPath string, videoDuration float64, title string, opts Options, emit func(Event)) (string, error) {
	out := muxPath(audioPath)
	emit(Event{Kind: EventMux, Episode: m.Episode.Number, Message: "вшивание в MKV: читаю видео с TorrServer"})
	emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: "вшивание в MKV: читаю видео с TorrServer (это долго)"})
	lastPct := -10.0
	err := render.Mux(ctx, render.MuxOptions{
		Output:        out,
		VideoURL:      m.VideoURL,
		AudioPath:     audioPath,
		VideoDuration: videoDuration,
		Title:         title,
		Force:         opts.Force,
		Progress: func(p float64) {
			if videoDuration <= 0 || p-lastPct < 10 {
				return
			}
			lastPct = p
			emit(Event{Kind: EventMux, Episode: m.Episode.Number, Current: int(p), Total: 100,
				Message: fmt.Sprintf("вшивание в MKV: %d%%", int(p))})
		},
	})
	if err != nil {
		emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: "mux не удался: " + err.Error()})
		return out, err
	}
	emit(Event{Kind: EventMux, Episode: m.Episode.Number, Current: 100, Total: 100, Message: "MKV готов"})
	return out, nil
}

func alignEpisode(ctx context.Context, m Match, dl *download.Result, epWork string, opts Options, emit func(Event)) (*align.Result, string, int, error) {
	if opts.NoSync || !opts.Sync || m.VideoURL == "" {
		zero := 0.0
		res, err := align.Align(&audio.Envelope{Values: make([]float64, int(dl.Duration*audio.EnvelopeRate)+1), Duration: dl.Duration}, nil, dl.Duration, dl.Duration, &zero)
		if err != nil {
			return nil, "", -1, err
		}
		if m.VideoURL == "" && opts.Sync {
			res.Warnings = append(res.Warnings, "m3u не найден: синхронизация не выполнялась")
		}
		return res, "", -1, nil
	}
	emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: "проверка видео и поиск оригинальной дорожки"})
	info, err := media.Probe(ctx, m.VideoURL)
	if err != nil {
		return nil, "", -1, fmt.Errorf("не удалось прочитать видео: %w", err)
	}
	ref, err := info.ChooseReference(opts.RefStream)
	if err != nil {
		return nil, "", -1, err
	}
	refDesc := media.DescribeStream(ref)
	emit(Event{Kind: EventLog, Episode: m.Episode.Number, Message: "референс: " + refDesc})

	dubEnv, err := audio.FromFile(ctx, dl.Path)
	if err != nil {
		return nil, refDesc, ref.Index, fmt.Errorf("декодирование дубляжа: %w", err)
	}
	var res *align.Result
	if opts.ManualOffset != nil {
		res, err = align.Align(dubEnv, nil, info.Duration, dl.Duration, opts.ManualOffset)
		if err != nil {
			return nil, refDesc, ref.Index, err
		}
	} else {
		times := align.DefaultAnchorTimes(info.Duration, opts.WindowSeconds)
		good := fetchRefWindows(ctx, m.VideoURL, ref.Index, opts.AnchorWorkers, times, opts.WindowSeconds, m.Episode.Number, emit, "референс")
		if len(good) == 0 {
			return nil, refDesc, ref.Index, fmt.Errorf("не удалось получить ни одного референсного окна")
		}
		res, err = align.Align(dubEnv, good, info.Duration, dl.Duration, nil)
		if err != nil {
			return nil, refDesc, ref.Index, err
		}
		if len(res.Cuts) > 0 {
			refineWindow := opts.WindowSeconds
			if refineWindow > 12 {
				refineWindow = 12
			}
			if refineWindow < 6 {
				refineWindow = 6
			}
			extra := cutRefineTimes(res.Cuts, info.Duration, refineWindow, good)
			if len(extra) > 0 {
				emit(Event{Kind: EventLog, Episode: m.Episode.Number,
					Message: fmt.Sprintf("уточняю границы пропуска: %d окон", len(extra))})
				more := fetchRefWindows(ctx, m.VideoURL, ref.Index, opts.AnchorWorkers, extra, refineWindow, m.Episode.Number, emit, "уточнение")
				if len(more) > 0 {
					all := append(append([]align.RefWindow{}, good...), more...)
					if refined, rerr := align.Align(dubEnv, all, info.Duration, dl.Duration, nil); rerr == nil {
						res = refined
						if len(res.Cuts) > 0 {
							emit(Event{Kind: EventLog, Episode: m.Episode.Number,
								Message: fmt.Sprintf("пропуск уточнён: %s–%s (%.1f c)", fmtSec(res.Cuts[0].Start), fmtSec(res.Cuts[0].End), res.Cuts[0].Removed)})
						}
					}
				}
			}
		}
	}
	emit(Event{Kind: EventAlign, Episode: m.Episode.Number, Result: res,
		Message: fmt.Sprintf("E%02d: offset %+.2f c, confidence %.2f", m.Episode.Number, res.Offset, res.Confidence)})
	data, _ := json.MarshalIndent(res, "", "  ")
	_ = os.WriteFile(filepath.Join(epWork, "sync.json"), data, 0o644)
	return res, refDesc, ref.Index, nil
}

// fetchRefWindows downloads reference windows in parallel and returns the
// successful ones. Each goroutine emits its own progress, guarded by a mutex.
func fetchRefWindows(ctx context.Context, videoURL string, streamIndex, workers int, times []float64, window float64, episode int, emit func(Event), label string) []align.RefWindow {
	if len(times) == 0 {
		return nil
	}
	if workers <= 0 {
		workers = 4
	}
	refs := make([]align.RefWindow, len(times))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	var mu sync.Mutex
	completed := 0
	for i, t := range times {
		wg.Add(1)
		go func(i int, t float64) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			env, err := audio.Reference(ctx, videoURL, streamIndex, t, window)
			mu.Lock()
			defer mu.Unlock()
			completed++
			if err != nil {
				emit(Event{Kind: EventLog, Episode: episode, Message: fmt.Sprintf("%s %s: %v", label, fmtSec(t), err)})
				return
			}
			refs[i] = align.RefWindow{VideoTime: t, Envelope: env}
			emit(Event{Kind: EventSyncProgress, Episode: episode, Current: completed, Total: len(times),
				Message: fmt.Sprintf("%s %s", label, fmtSec(t))})
		}(i, t)
	}
	wg.Wait()
	var good []align.RefWindow
	for _, r := range refs {
		if r.Envelope != nil {
			good = append(good, r)
		}
	}
	return good
}

// cutRefineTimes returns dense anchor times around each detected cut, so a
// second alignment pass can narrow the gap to a few seconds.
func cutRefineTimes(cuts []align.Cut, videoDuration, window float64, existing []align.RefWindow) []float64 {
	maxStart := videoDuration - window
	if maxStart < 0 {
		return nil
	}
	seen := func(t float64) bool {
		for _, r := range existing {
			if math.Abs(r.VideoTime-t) < 4 {
				return true
			}
		}
		return false
	}
	var times []float64
	for _, c := range cuts {
		lo := c.SearchStart - 20
		hi := c.SearchEnd + 20
		if c.SearchStart == 0 && c.SearchEnd == 0 {
			lo, hi = c.Start-20, c.End+20
		}
		if lo < 0 {
			lo = 0
		}
		if hi > maxStart {
			hi = maxStart
		}
		for t := lo; t <= hi; t += 8 {
			if seen(t) {
				continue
			}
			times = append(times, t)
		}
	}
	sort.Float64s(times)
	out := times[:0]
	for _, t := range times {
		if len(out) == 0 || t-out[len(out)-1] >= 4 {
			out = append(out, t)
		}
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// VideoExtensions are the container extensions treated as video files when
// scanning a local folder.
var VideoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".mov": true,
	".webm": true, ".avi": true, ".ts": true, ".m2ts": true,
}

// ScanVideoDir lists video files in a folder. Episode numbers are derived from
// file names, so any naming scheme that contains a number works.
func ScanVideoDir(dir string) ([]m3u.Entry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("чтение папки с видео: %w", err)
	}
	var files []string
	for _, it := range items {
		if it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		if !VideoExtensions[strings.ToLower(filepath.Ext(it.Name()))] {
			continue
		}
		files = append(files, it.Name())
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("в папке %s нет видеофайлов", dir)
	}
	entries := make([]m3u.Entry, 0, len(files))
	for i, name := range files {
		entries = append(entries, m3u.Entry{
			Index:         i,
			Name:          name,
			URL:           filepath.Join(dir, name),
			EpisodeNumber: m3u.DetectEpisode(name),
		})
	}
	return entries, nil
}

// DefaultDirOutDir returns the output folder next to a local video folder.
func DefaultDirOutDir(videoDir string) string {
	abs, _ := filepath.Abs(expandHome(videoDir))
	base := filepath.Base(abs)
	if base == "." || base == string(filepath.Separator) || base == "" {
		base = "video"
	}
	return filepath.Join(filepath.Dir(abs), base+" - DreamCast")
}

// DefaultOutDir is the folder next to the m3u.
func DefaultOutDir(m3uPath string) string {
	abs, _ := filepath.Abs(expandHome(m3uPath))
	base := strings.TrimSuffix(filepath.Base(abs), filepath.Ext(abs))
	return filepath.Join(filepath.Dir(abs), base+" - DreamCast")
}

// DefaultWorkDir returns a per-release cache directory.
func DefaultWorkDir(rel *site.Release) string {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		cache = os.TempDir()
	}
	key := rel.ID
	if key == "" {
		key = sanitize(rel.Slug)
	}
	return filepath.Join(cache, "dreamtui", key)
}

// DetectM3U returns the newest .m3u/.m3u8 file in ~/Downloads.
func DetectM3U() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, "Downloads")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".m3u" && ext != ".m3u8" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cands = append(cands, cand{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	return cands[0].path
}

func muxPath(audioOutput string) string {
	base := strings.TrimSuffix(audioOutput, ".rus.mka")
	if base == audioOutput {
		base = strings.TrimSuffix(audioOutput, filepath.Ext(audioOutput))
	}
	return base + ".rus.mkv"
}

func outputName(videoName string, ep site.Episode) string {
	name := strings.TrimSpace(videoName)
	if name == "" {
		name = fmt.Sprintf("Dream Cast - S01E%02d", ep.Number)
	}
	for _, ext := range []string{".mkv", ".mp4", ".m4v", ".avi", ".webm", ".ts"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			name = name[:len(name)-len(ext)]
			break
		}
	}
	name = sanitize(name)
	if name == "" {
		name = fmt.Sprintf("Dream Cast - S01E%02d", ep.Number)
	}
	return name + ".rus.mka"
}

var illegalRE = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

func sanitize(s string) string {
	s = illegalRE.ReplaceAllString(s, "_")
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".")
	return s
}

func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func fmtSec(sec float64) string {
	m := int(sec) / 60
	s := int(sec) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

func writeReport(outDir string, rep *Report) error {
	if outDir == "" {
		return errors.New("no out dir")
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "dreamtui-report.json"), data, 0o644)
}
