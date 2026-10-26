package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	stdsync "sync"
	"syscall"

	"dreamtui/internal/app"
	"dreamtui/internal/sync"
	"dreamtui/internal/tui"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		if err := tui.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "dreamtui:", err)
			os.Exit(1)
		}
		return
	}
	switch os.Args[1] {
	case "extract":
		os.Exit(runExtract(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("dreamtui", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`dreamtui — Dream Cast audio extractor + auto-sync

  dreamtui                      запустить TUI
  dreamtui extract [flags] URL  извлечь аудио из релиза
  dreamtui version              версия

Источник видео — на выбор:
  --m3u PATH          TorrServer m3u
  --video-dir DIR     папка с MKV/MP4

Примеры:
  dreamtui extract --video-dir "$HOME/Downloads/Шоу [2160p]" \
    https://dreamerscast.com/home/release/629-cyberpunk-edgerunners-2

  dreamtui extract --m3u "$HOME/Downloads/Шоу [2160p].m3u" \
    --limit 1 \
    https://dreamerscast.com/home/release/629-cyberpunk-edgerunners-2

Флаги extract:
  --m3u PATH          TorrServer m3u (по умолчанию — свежий в ~/Downloads)
  --video-dir DIR     папка с видеофайлами вместо TorrServer
  --out DIR           папка вывода (по умолчанию рядом с источником)
  --episodes 1,2      только выбранные серии
  --limit N           первые N серий
  --offset SEC        ручной offset (по умолчанию подбирается автоматически)
  --no-sync           не синхронизировать
  --apply-anyway      применять синк даже при низкой уверенности
  --force             перезаписать/перекачать
  --mux               дополнительно вшить дорожку в MKV (скачивает видео целиком)
  --ref-index N       индекс оригинальной аудиодорожки в контейнере
  --workers N         параллельных сегментов (по умолчанию 8)
  --anchors N         параллельных референсных окон (по умолчанию 4)
  --window SEC        длина окна синка (по умолчанию 30)
  --work-dir DIR      папка кэша
  --dry-run           показать план и выйти
`)
}

func runExtract(args []string) int {
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	m3uPath := fs.String("m3u", "", "TorrServer m3u")
	videoDir := fs.String("video-dir", "", "folder with local video files")
	outDir := fs.String("out", "", "output directory")
	workDir := fs.String("work-dir", "", "cache directory")
	episodes := fs.String("episodes", "", "comma-separated episode numbers")
	limit := fs.Int("limit", 0, "first N episodes")
	offsetStr := fs.String("offset", "", "manual offset in seconds")
	noSync := fs.Bool("no-sync", false, "disable synchronization")
	applyAnyway := fs.Bool("apply-anyway", false, "apply low-confidence result")
	force := fs.Bool("force", false, "overwrite existing files")
	mux := fs.Bool("mux", false, "also mux into MKV")
	refIndex := fs.Int("ref-index", -1, "reference audio stream index")
	workers := fs.Int("workers", 8, "parallel segment downloads")
	anchors := fs.Int("anchors", 4, "parallel reference windows")
	window := fs.Float64("window", 30, "sync window seconds")
	dryRun := fs.Bool("dry-run", false, "print plan and exit")
	urlFlag := fs.String("url", "", "release URL (alternative to positional)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	releaseURL := strings.TrimSpace(*urlFlag)
	if releaseURL == "" && fs.NArg() > 0 {
		releaseURL = strings.TrimSpace(fs.Arg(0))
	}
	if releaseURL == "" {
		fmt.Fprintln(os.Stderr, "укажите ссылку на релиз Dream Cast")
		return 1
	}
	if *m3uPath != "" && *videoDir != "" {
		fmt.Fprintln(os.Stderr, "укажите либо --m3u, либо --video-dir")
		return 1
	}
	if *videoDir == "" && *m3uPath == "" {
		*m3uPath = app.DetectM3U()
	}
	if *outDir == "" && *m3uPath == "" && *videoDir == "" {
		fmt.Fprintln(os.Stderr, "нужен --m3u, --video-dir или --out")
		return 1
	}
	var manual *float64
	if *offsetStr != "" {
		v, err := strconv.ParseFloat(strings.ReplaceAll(*offsetStr, ",", "."), 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "неверный --offset: %v\n", err)
			return 1
		}
		manual = &v
	}
	var epNums []int
	if *episodes != "" {
		for _, p := range strings.Split(*episodes, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "неверный номер серии %q\n", p)
				return 1
			}
			epNums = append(epNums, n)
		}
	}
	opts := app.Options{
		ReleaseURL:     releaseURL,
		M3UPath:        *m3uPath,
		VideoDir:       *videoDir,
		OutDir:         *outDir,
		WorkDir:        *workDir,
		EpisodeNumbers: epNums,
		Limit:          *limit,
		Sync:           !*noSync,
		NoSync:         *noSync,
		ManualOffset:   manual,
		ApplyAnyway:    *applyAnyway,
		Force:          *force,
		Mux:            *mux,
		RefStream:      *refIndex,
		SegmentWorkers: *workers,
		AnchorWorkers:  *anchors,
		WindowSeconds:  *window,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	plan, err := app.Discover(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		return 1
	}
	printPlan(plan)
	if *dryRun {
		return 0
	}
	printer := newCLIPrinter()
	opts.Event = printer.event
	rep := app.Execute(ctx, plan, opts)
	printer.finish()
	printReport(rep)
	for _, e := range rep.Episodes {
		if e.Status == "needs-confirmation" {
			return 2
		}
	}
	for _, e := range rep.Episodes {
		if strings.HasSuffix(e.Status, "error") {
			return 1
		}
	}
	return 0
}

func printPlan(plan *app.Plan) {
	fmt.Printf("Dream Cast: %s\n", plan.Release.Title)
	fmt.Printf("Серий на сайте: %d из %d\n", plan.Release.Available, plan.Release.Total)
	switch plan.Source {
	case "folder":
		fmt.Printf("Источник: папка %s (%d)\n", plan.SourcePath, len(plan.Entries))
	case "m3u":
		fmt.Printf("Источник: m3u %s (%d)\n", plan.SourcePath, len(plan.Entries))
	default:
		fmt.Println("Источник: нет — синхронизация не выполнялась")
	}
	fmt.Printf("Вывод: %s\n", plan.OutDir)
	selected := 0
	for _, m := range plan.Matches {
		if m.Selected {
			selected++
		}
	}
	fmt.Printf("Выбрано: %d из %d\n", selected, len(plan.Matches))
	for _, m := range plan.Matches {
		if !m.Selected && m.VideoURL == "" {
			fmt.Printf("  E%02d  пропуск: нет видео в источнике\n", m.Episode.Number)
		}
		if !m.Selected {
			continue
		}
		fmt.Printf("  E%02d  %s\n", m.Episode.Number, m.OutputPath)
	}
	fmt.Println()
}

func printReport(rep *app.Report) {
	fmt.Println()
	for _, e := range rep.Episodes {
		switch e.Status {
		case "done":
			if e.Sync != nil {
				fmt.Printf("✓ E%02d  %s  (offset %+.2f, confidence %.2f, %.1f c)\n",
					e.Number, e.OutputPath, e.Sync.Offset, e.Sync.Confidence, e.Elapsed)
			} else {
				fmt.Printf("✓ E%02d  %s  (%.1f c)\n", e.Number, e.OutputPath, e.Elapsed)
			}
			if e.MuxPath != "" && e.MuxError == "" {
				fmt.Printf("   MKV: %s\n", e.MuxPath)
			}
		case "exists":
			fmt.Printf("· E%02d  уже существует: %s\n", e.Number, e.OutputPath)
		case "needs-confirmation":
			if e.Sync != nil {
				fmt.Printf("! E%02d  нужна проверка синка (confidence %.2f, offset %+.2f). Файл не создан.\n",
					e.Number, e.Sync.Confidence, e.Sync.Offset)
				printAnchors(e.Sync)
			} else {
				fmt.Printf("! E%02d  нужна проверка синка. Файл не создан.\n", e.Number)
			}
			fmt.Println("  Повторите с --offset <сек> или --apply-anyway.")
		default:
			fmt.Printf("✗ E%02d  %s: %s\n", e.Number, e.Status, e.Error)
		}
		if e.Warning != "" {
			fmt.Printf("   предупреждение: %s\n", e.Warning)
		}
	}
	if len(rep.Warnings) > 0 {
		for _, w := range rep.Warnings {
			fmt.Println("предупреждение:", w)
		}
	}
}

func printAnchors(res *sync.Result) {
	if res == nil || len(res.Anchors) == 0 {
		return
	}
	fmt.Println("   время   dub     offset  score")
	for _, a := range res.Anchors {
		fmt.Printf("   %s  %s  %+6.2f  %.2f%s\n", fmtTime(a.VideoTime), fmtTime(a.DubTime), a.Offset, a.Score, outlierMark(a))
	}
}

func outlierMark(a sync.Anchor) string {
	if a.Outlier {
		return " (outlier)"
	}
	return ""
}

func fmtTime(sec float64) string {
	m := int(sec) / 60
	s := int(sec) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

func newCLIPrinter() *cliPrinter { return &cliPrinter{} }

type cliPrinter struct {
	mu      stdsync.Mutex
	lastLen int
}

func (p *cliPrinter) event(e app.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Kind {
	case app.EventDownload:
		if e.Download == nil {
			return
		}
		pr := e.Download
		pct := 0
		if pr.SegmentsTotal > 0 && pr.SegmentsDone >= 0 {
			pct = pr.SegmentsDone * 100 / pr.SegmentsTotal
		}
		eta := ""
		if pr.ExpectedBytes > 0 && pr.Speed > 0 {
			rem := float64(pr.ExpectedBytes-pr.Bytes) / pr.Speed
			if rem > 0 && rem < 3600 {
				eta = " ETA " + fmtTime(rem)
			}
		}
		line := fmt.Sprintf("E%02d  %-9s %3d/%d %3d%%  %s%s",
			e.Episode, pr.Phase, pr.SegmentsDone, pr.SegmentsTotal, pct, fmtBytes(pr.Bytes), eta)
		p.printLine(line)
	case app.EventSyncProgress:
		p.printLine(fmt.Sprintf("E%02d  sync      %d/%d  %s", e.Episode, e.Current, e.Total, e.Message))
	case app.EventLog:
		p.println(fmt.Sprintf("E%02d  %s", e.Episode, e.Message))
	case app.EventEpisodeStart:
		p.println("")
		p.println("── " + e.Message + " ──")
	case app.EventAlign:
		if e.Result != nil {
			p.println(fmt.Sprintf("E%02d  offset %+.2f c, confidence %.2f", e.Episode, e.Result.Offset, e.Result.Confidence))
		}
	case app.EventEpisodeDone:
		if e.Report != nil && (e.Report.Status == "needs-confirmation" || strings.HasSuffix(e.Report.Status, "error")) {
			p.println(fmt.Sprintf("E%02d  %s", e.Episode, e.Message))
		}
	}
}

func (p *cliPrinter) printLine(s string) {
	pad := ""
	if n := p.lastLen - len([]rune(s)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Printf("\r\x1b[K%s%s", s, pad)
	p.lastLen = len([]rune(s))
}

func (p *cliPrinter) println(s string) {
	fmt.Print("\r\x1b[K")
	fmt.Println(s)
	p.lastLen = 0
}

func (p *cliPrinter) finish() {
	fmt.Print("\r\x1b[K")
}

func fmtBytes(n int64) string {
	f := float64(n)
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%.1f GB", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1f MB", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.0f KB", f/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
