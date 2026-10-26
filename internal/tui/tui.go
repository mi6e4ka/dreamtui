// Package tui implements the interactive dreamtui interface.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"dreamtui/internal/app"
)

type screen int

const (
	screenSetup screen = iota
	screenDiscovering
	screenReview
	screenRunning
	screenDone
	screenBrowser
)

var (
	colAccent = lipgloss.Color("212")
	colGreen  = lipgloss.Color("42")
	colRed    = lipgloss.Color("203")
	colYellow = lipgloss.Color("214")
	colDim    = lipgloss.Color("244")
	colText   = lipgloss.Color("252")

	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	accentBold  = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	greenStyle  = lipgloss.NewStyle().Foreground(colGreen)
	redStyle    = lipgloss.NewStyle().Foreground(colRed)
	yellowStyle = lipgloss.NewStyle().Foreground(colYellow)
	dimStyle    = lipgloss.NewStyle().Foreground(colDim)
	textStyle   = lipgloss.NewStyle().Foreground(colText)
	labelStyle  = lipgloss.NewStyle().Bold(true).Foreground(colText)

	inputBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDim).
			Padding(0, 1)
	focusedInputBoxStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colAccent).
				Padding(0, 1)
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDim).
			Padding(0, 1)
)

const (
	sourceM3U = iota
	sourceFolder
)

var videoLabels = [2]string{"TorrServer m3u", "Папка с MKV"}

func (m Model) inputLabels() [3]string {
	return [3]string{
		"Ссылка на релиз Dream Cast",
		videoLabels[m.source],
		"Папка вывода (пусто — рядом с источником)",
	}
}

type epState struct {
	Number     int
	Title      string
	Status     string
	Phase      string
	Current    int
	Total      int
	Percent    int
	Bytes      int64
	Speed      float64
	ETA        float64
	Offset     float64
	Confidence float64
	HasSync    bool
	Output     string
	Error      string
	Warning    string
}

type browserEntry struct {
	Name          string
	Path          string
	IsDir         bool
	Parent        bool
	SelectCurrent bool
	Videos        int
}

type browserState struct {
	Target  int // input index: 1 = video source, 2 = output folder
	Mode    int // sourceM3U or sourceFolder
	Dir     string
	Entries []browserEntry
	Cursor  int
	Offset  int
	Err     string
}

type discoverMsg struct {
	plan *app.Plan
	err  error
}

type runMsg struct {
	event  *app.Event
	report *app.Report
}

// Model is the Bubble Tea model.
type Model struct {
	width, height int
	screen        screen
	inputs        []textinput.Model
	focus         int
	source        int
	paths         [2]string
	spinner       spinner.Model

	err     string
	plan    *app.Plan
	states  map[int]*epState
	order   []int
	logs    []string
	report  *app.Report
	started time.Time

	cursor     int
	listOffset int

	syncOn      bool
	muxOn       bool
	applyAnyway bool
	browser     browserState

	runCh      chan runMsg
	cancel     context.CancelFunc
	cancelling bool
	toast      string
}

// New creates the initial model.
func New() Model {
	inputs := make([]textinput.Model, 3)
	for i := range inputs {
		in := textinput.New()
		in.Placeholder = ""
		in.CharLimit = 4096
		in.Width = 70
		inputs[i] = in
	}
	inputs[0].Focus()
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colAccent)

	return Model{
		screen:  screenSetup,
		inputs:  inputs,
		spinner: sp,
		states:  make(map[int]*epState),
		syncOn:  true,
	}
}

// Run starts the TUI.
func Run() error {
	p := tea.NewProgram(New(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeInputs()
		return m, nil

	case spinner.TickMsg:
		if m.screen == screenDiscovering || m.screen == screenRunning {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil

	case discoverMsg:
		if msg.err != nil {
			m.err = msg.err.Error()
			m.screen = screenSetup
			return m, nil
		}
		m.plan = msg.plan
		m.err = ""
		m.screen = screenReview
		m.cursor = 0
		m.listOffset = 0
		m.states = make(map[int]*epState)
		m.syncOn = len(m.plan.Entries) > 0
		m.applyAnyway = false
		m.muxOn = false
		for _, match := range m.plan.Matches {
			status := "ожидание"
			if !match.Selected {
				status = "пропуск"
			}
			m.states[match.Episode.Number] = &epState{Number: match.Episode.Number, Title: match.Episode.Title, Status: status}
		}
		return m, nil

	case runMsg:
		if msg.report != nil {
			m.report = msg.report
			m.screen = screenDone
			m.cancelling = false
			if m.cancel != nil {
				m.cancel()
				m.cancel = nil
			}
			return m, nil
		}
		if msg.event != nil {
			m.handleEvent(*msg.event)
			return m, waitRun(m.runCh)
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
		switch m.screen {
		case screenSetup:
			return m.updateSetup(msg)
		case screenReview:
			return m.updateReview(msg)
		case screenRunning:
			return m.updateRunning(msg)
		case screenDone:
			return m.updateDone(msg)
		case screenBrowser:
			return m.updateBrowser(msg)
		}
	}
	return m, nil
}

func (m Model) focusIndex() int {
	switch m.focus {
	case 0:
		return 0
	case 2:
		return 1
	case 3:
		return 2
	}
	return -1
}

func (m Model) moveFocus(delta int) (tea.Model, tea.Cmd) {
	m.focus = (m.focus + delta + 4) % 4
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	if idx := m.focusIndex(); idx >= 0 {
		return m, m.inputs[idx].Focus()
	}
	return m, nil
}

func (m Model) switchSource(src int) Model {
	if src == m.source {
		return m
	}
	m.paths[m.source] = m.inputs[1].Value()
	m.source = src
	m.inputs[1].SetValue(m.paths[src])
	m.inputs[1].CursorEnd()
	return m
}

func (m Model) updateSetup(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key == "ctrl+f" {
		target := 1
		if m.focus == 3 {
			target = 2
		}
		return m.openBrowser(target)
	}
	if m.focus == 1 {
		switch key {
		case "left", "h", "right", "l", " ":
			return m.switchSource(1 - m.source), nil
		case "tab", "down", "enter":
			return m.moveFocus(1)
		case "shift+tab", "up":
			return m.moveFocus(-1)
		}
		return m, nil
	}
	switch key {
	case "tab", "down":
		return m.moveFocus(1)
	case "shift+tab", "up":
		return m.moveFocus(-1)
	case "enter":
		if strings.TrimSpace(m.inputs[0].Value()) == "" {
			m.err = "вставьте ссылку на релиз Dream Cast"
			return m, nil
		}
		if strings.TrimSpace(m.inputs[1].Value()) == "" && strings.TrimSpace(m.inputs[2].Value()) == "" {
			m.err = "укажите m3u, папку с видео или папку вывода"
			return m, nil
		}
		m.err = ""
		m.screen = screenDiscovering
		return m, tea.Batch(m.spinner.Tick, m.discoverCmd())
	}
	idx := m.focusIndex()
	var cmd tea.Cmd
	m.inputs[idx], cmd = m.inputs[idx].Update(msg)
	if idx == 1 {
		m.paths[m.source] = m.inputs[1].Value()
	}
	return m, cmd
}

func expandUserPath(p string) string {
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

func (m Model) browserStartDir(target int) string {
	if v := strings.TrimSpace(m.inputs[target].Value()); v != "" {
		p := expandUserPath(v)
		if st, err := os.Stat(p); err == nil {
			if st.IsDir() {
				return p
			}
			return filepath.Dir(p)
		}
		if d := filepath.Dir(p); d != "" {
			if _, err := os.Stat(d); err == nil {
				return d
			}
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		downloads := filepath.Join(home, "Downloads")
		if st, err := os.Stat(downloads); err == nil && st.IsDir() {
			return downloads
		}
		return home
	}
	return "."
}

func countVideos(dir string) int {
	items, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, it := range items {
		if it.IsDir() {
			continue
		}
		if app.VideoExtensions[strings.ToLower(filepath.Ext(it.Name()))] {
			n++
		}
	}
	return n
}

func listBrowserEntries(dir string, mode int) ([]browserEntry, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dirs, files []browserEntry
	for _, it := range items {
		name := it.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if it.IsDir() {
			e := browserEntry{Name: name, Path: filepath.Join(dir, name), IsDir: true}
			if mode == sourceFolder {
				e.Videos = countVideos(e.Path)
			}
			dirs = append(dirs, e)
			continue
		}
		if mode != sourceM3U {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext == ".m3u" || ext == ".m3u8" {
			files = append(files, browserEntry{Name: name, Path: filepath.Join(dir, name)})
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	var out []browserEntry
	if parent := filepath.Dir(dir); parent != dir {
		out = append(out, browserEntry{Name: "..", Path: parent, IsDir: true, Parent: true})
	}
	if mode == sourceFolder {
		out = append(out, browserEntry{
			Name: "Выбрать эту папку", Path: dir, IsDir: true,
			SelectCurrent: true, Videos: countVideos(dir),
		})
	}
	// Files first: in m3u mode the playlist is the primary target.
	out = append(out, files...)
	out = append(out, dirs...)
	return out, nil
}

func (m Model) browseTo(dir string) Model {
	abs, err := filepath.Abs(expandUserPath(dir))
	if err == nil {
		dir = abs
	}
	entries, listErr := listBrowserEntries(dir, m.browser.Mode)
	m.browser.Dir = dir
	m.browser.Entries = entries
	m.browser.Cursor = firstBrowserCursor(entries)
	m.browser.Offset = 0
	m.browser.Err = ""
	if listErr != nil {
		m.browser.Err = listErr.Error()
	}
	return m
}

func firstBrowserCursor(entries []browserEntry) int {
	for i, e := range entries {
		if e.Parent || e.SelectCurrent {
			continue
		}
		return i
	}
	if len(entries) > 0 {
		return 0
	}
	return 0
}

func (m Model) openBrowser(target int) (tea.Model, tea.Cmd) {
	mode := m.source
	if target == 2 {
		mode = sourceFolder
	}
	m.browser = browserState{Target: target, Mode: mode, Dir: m.browserStartDir(target)}
	m = m.browseTo(m.browser.Dir)
	m.preselectBrowser()
	m.screen = screenBrowser
	return m, nil
}

func (m *Model) preselectBrowser() {
	v := strings.TrimSpace(m.inputs[m.browser.Target].Value())
	if v == "" {
		return
	}
	abs, err := filepath.Abs(expandUserPath(v))
	if err != nil {
		return
	}
	for i, e := range m.browser.Entries {
		if e.Parent || e.SelectCurrent {
			continue
		}
		if e.Path == abs {
			m.browser.Cursor = i
			break
		}
	}
	m.fixBrowserScroll()
}

func (m Model) browserVisibleRows() int {
	n := m.height - 11
	if n < 5 {
		n = 5
	}
	return n
}

func (m *Model) fixBrowserScroll() {
	visible := m.browserVisibleRows()
	if m.browser.Cursor < m.browser.Offset {
		m.browser.Offset = m.browser.Cursor
	}
	if m.browser.Cursor >= m.browser.Offset+visible {
		m.browser.Offset = m.browser.Cursor - visible + 1
	}
}

func (m Model) closeBrowser() (tea.Model, tea.Cmd) {
	m.screen = screenSetup
	m.focus = 2
	if m.browser.Target == 2 {
		m.focus = 3
	}
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	return m, m.inputs[m.focusIndex()].Focus()
}

func (m Model) selectBrowserFolder() (tea.Model, tea.Cmd) {
	m.inputs[m.browser.Target].SetValue(m.browser.Dir)
	m.inputs[m.browser.Target].CursorEnd()
	if m.browser.Target == 1 {
		m.paths[m.source] = m.browser.Dir
	}
	return m.closeBrowser()
}

func (m Model) updateBrowser(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	b := &m.browser
	switch msg.String() {
	case "esc", "q":
		return m.closeBrowser()
	case "up", "k":
		if b.Cursor > 0 {
			b.Cursor--
			m.fixBrowserScroll()
		}
	case "down", "j":
		if b.Cursor < len(b.Entries)-1 {
			b.Cursor++
			m.fixBrowserScroll()
		}
	case "pgup", "ctrl+b":
		b.Cursor -= m.browserVisibleRows()
		if b.Cursor < 0 {
			b.Cursor = 0
		}
		m.fixBrowserScroll()
	case "pgdown", "ctrl+f":
		b.Cursor += m.browserVisibleRows()
		if b.Cursor >= len(b.Entries) {
			b.Cursor = len(b.Entries) - 1
		}
		if b.Cursor < 0 {
			b.Cursor = 0
		}
		m.fixBrowserScroll()
	case "home", "g":
		b.Cursor = 0
		m.fixBrowserScroll()
	case "end", "G":
		if len(b.Entries) > 0 {
			b.Cursor = len(b.Entries) - 1
		}
		m.fixBrowserScroll()
	case "left", "backspace", "h":
		if parent := filepath.Dir(b.Dir); parent != b.Dir {
			m = m.browseTo(parent)
		}
	case "right", "l":
		if len(b.Entries) > 0 {
			e := b.Entries[b.Cursor]
			if e.IsDir && !e.Parent && !e.SelectCurrent {
				m = m.browseTo(e.Path)
			}
		}
	case " ":
		if b.Target == 2 || (b.Target == 1 && b.Mode == sourceFolder) {
			return m.selectBrowserFolder()
		}
	case "enter":
		if len(b.Entries) == 0 {
			return m, nil
		}
		e := b.Entries[b.Cursor]
		switch {
		case e.SelectCurrent:
			return m.selectBrowserFolder()
		case e.IsDir:
			m = m.browseTo(e.Path)
			return m, nil
		default:
			m.inputs[b.Target].SetValue(e.Path)
			m.inputs[b.Target].CursorEnd()
			if b.Target == 1 {
				m.paths[m.source] = e.Path
			}
			return m.closeBrowser()
		}
	default:
		// Type-ahead: a single character jumps to the next matching entry.
		key := msg.String()
		if len([]rune(key)) == 1 && len(b.Entries) > 0 {
			needle := strings.ToLower(key)
			for i := 1; i <= len(b.Entries); i++ {
				idx := (b.Cursor + i) % len(b.Entries)
				e := b.Entries[idx]
				if e.Parent || e.SelectCurrent {
					continue
				}
				if strings.HasPrefix(strings.ToLower(e.Name), needle) {
					b.Cursor = idx
					m.fixBrowserScroll()
					break
				}
			}
		}
	}
	return m, nil
}

func (m Model) viewBrowser() string {
	b := m.browser
	width := m.contentWidth()
	title := "Выбор m3u"
	switch {
	case b.Target == 2:
		title = "Выбор папки вывода"
	case b.Mode == sourceFolder:
		title = "Выбор папки с MKV"
	}
	var out strings.Builder
	out.WriteString(titleStyle.Render("dreamtui"))
	out.WriteString(dimStyle.Render("  ·  " + title))
	out.WriteString("\n")
	out.WriteString(dimStyle.Render(strings.Repeat("─", width)))
	out.WriteString("\n")
	out.WriteString(textStyle.Render(shorten(b.Dir, width-2)))
	out.WriteString("\n\n")

	visible := m.browserVisibleRows()
	end := b.Offset + visible
	if end > len(b.Entries) {
		end = len(b.Entries)
	}
	for i := b.Offset; i < end; i++ {
		e := b.Entries[i]
		cur := "  "
		if i == b.Cursor {
			cur = "▸ "
		}
		var label string
		switch {
		case e.SelectCurrent:
			label = "✓ Выбрать эту папку — " + filepath.Base(b.Dir)
			if e.Videos > 0 {
				label += fmt.Sprintf("  · %d видео", e.Videos)
			}
		case e.Parent:
			label = "../"
		case e.IsDir:
			label = e.Name + "/"
			if e.Videos > 0 {
				label += fmt.Sprintf("  · %d видео", e.Videos)
			}
		default:
			label = e.Name
		}
		line := cur + shorten(label, width-4)
		switch {
		case i == b.Cursor:
			out.WriteString(accentBold.Render(line))
		case e.SelectCurrent:
			out.WriteString(greenStyle.Render(line))
		default:
			out.WriteString(textStyle.Render(line))
		}
		out.WriteString("\n")
	}
	if len(b.Entries) == 0 && b.Err == "" {
		out.WriteString(dimStyle.Render("  пусто"))
		out.WriteString("\n")
	}
	if len(b.Entries) > visible {
		out.WriteString(dimStyle.Render(fmt.Sprintf("  … %d–%d из %d", b.Offset+1, end, len(b.Entries))))
		out.WriteString("\n")
	}
	if b.Err != "" {
		out.WriteString("\n")
		out.WriteString(redStyle.Render("✗ " + b.Err))
		out.WriteString("\n")
	}
	out.WriteString("\n")
	footer := "↑/↓ — выбор   Enter — открыть/выбрать   ← — выше   Esc — отмена"
	if b.Target == 2 || b.Mode == sourceFolder {
		footer = "↑/↓ — выбор   Enter — открыть/выбрать   Space — эта папка   ← — выше   Esc — отмена"
	}
	out.WriteString(dimStyle.Render(footer))
	out.WriteString("\n")
	return out.String()
}

func (m Model) updateReview(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.screen = screenSetup
		return m, nil
	case "q":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.fixScroll()
		}
	case "down", "j":
		if m.cursor < len(m.plan.Matches)-1 {
			m.cursor++
			m.fixScroll()
		}
	case " ":
		if m.cursor < len(m.plan.Matches) {
			m.plan.Matches[m.cursor].Selected = !m.plan.Matches[m.cursor].Selected
			n := m.plan.Matches[m.cursor].Episode.Number
			if st := m.states[n]; st != nil {
				if m.plan.Matches[m.cursor].Selected {
					st.Status = "ожидание"
				} else {
					st.Status = "пропуск"
				}
			}
		}
	case "a":
		all := m.allSelected()
		for i := range m.plan.Matches {
			m.plan.Matches[i].Selected = !all
			n := m.plan.Matches[i].Episode.Number
			if st := m.states[n]; st != nil {
				if !all {
					st.Status = "ожидание"
				} else {
					st.Status = "пропуск"
				}
			}
		}
	case "s":
		m.syncOn = !m.syncOn
	case "m":
		m.muxOn = !m.muxOn
	case "f":
		m.applyAnyway = !m.applyAnyway
	case "enter":
		if !m.anySelected() {
			m.err = "выберите хотя бы одну серию"
			return m, nil
		}
		m.err = ""
		return m.startRun()
	}
	return m, nil
}

func (m Model) updateRunning(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		if !m.cancelling && m.cancel != nil {
			m.cancel()
			m.cancelling = true
			m.appendLog("останавливаю после текущей операции…")
			return m, nil
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) updateDone(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "esc":
		m.screen = screenReview
		m.report = nil
		return m, nil
	case "o":
		if m.report != nil {
			if err := openFolder(m.report.OutDir); err != nil {
				m.toast = "не удалось открыть папку: " + err.Error()
			} else {
				m.toast = "папка открыта"
			}
		}
	case "e":
		m.screen = screenSetup
		m.focus = 2
		for i := range m.inputs {
			m.inputs[i].Blur()
		}
		cmd := m.inputs[1].Focus()
		m.toast = ""
		return m, cmd
	case "f":
		if m.plan != nil {
			m.applyAnyway = true
			return m.startRun()
		}
	}
	return m, nil
}

func (m Model) discoverCmd() tea.Cmd {
	opts := m.buildOptions()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		plan, err := app.Discover(ctx, opts)
		return discoverMsg{plan: plan, err: err}
	}
}

func (m Model) startRun() (tea.Model, tea.Cmd) {
	if m.plan == nil {
		return m, nil
	}
	opts := m.buildOptions()
	opts.EpisodeNumbers = m.selectedNumbers()
	opts.Sync = m.syncOn
	opts.NoSync = !m.syncOn
	opts.ApplyAnyway = m.applyAnyway
	opts.Mux = m.muxOn
	opts.RefStream = -1
	opts.SegmentWorkers = 8
	opts.AnchorWorkers = 4
	opts.WindowSeconds = 30
	opts.OutDir = m.plan.OutDir

	// The plan was discovered before the user toggled episodes. Apply the
	// current selection, because Execute reads Match.Selected.
	selected := make(map[int]bool, len(opts.EpisodeNumbers))
	for _, n := range opts.EpisodeNumbers {
		selected[n] = true
	}
	for i := range m.plan.Matches {
		m.plan.Matches[i].Selected = selected[m.plan.Matches[i].Episode.Number]
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.started = time.Now()
	m.cancelling = false
	m.report = nil
	m.logs = nil
	m.states = make(map[int]*epState)
	m.order = opts.EpisodeNumbers
	for _, n := range m.order {
		m.states[n] = &epState{Number: n, Title: fmt.Sprintf("Серия %d", n), Status: "ожидание"}
	}
	for _, match := range m.plan.Matches {
		if st := m.states[match.Episode.Number]; st != nil {
			st.Title = match.Episode.Title
		}
	}
	ch := make(chan runMsg, 512)
	m.runCh = ch
	opts.Event = func(e app.Event) {
		select {
		case ch <- runMsg{event: &e}:
		default:
			// Progress is frequent; never block the runner on UI redraws.
		}
	}
	go func() {
		rep := app.Execute(ctx, m.plan, opts)
		ch <- runMsg{report: rep}
	}()
	m.screen = screenRunning
	m.cursor = 0
	m.listOffset = 0
	return m, tea.Batch(m.spinner.Tick, waitRun(ch))
}

func waitRun(ch chan runMsg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

func (m *Model) handleEvent(e app.Event) {
	if e.Episode > 0 {
		st := m.states[e.Episode]
		if st == nil {
			st = &epState{Number: e.Episode, Title: fmt.Sprintf("Серия %d", e.Episode)}
			m.states[e.Episode] = st
		}
		switch e.Kind {
		case app.EventEpisodeStart:
			st.Status = "работа"
			st.Phase = "подготовка"
			st.Percent = 0
		case app.EventDownload:
			if e.Download != nil {
				p := e.Download
				st.Status = "скачивание"
				st.Phase = p.Phase
				st.Current = p.SegmentsDone
				st.Total = p.SegmentsTotal
				st.Bytes = p.Bytes
				st.Speed = p.Speed
				if p.SegmentsTotal > 0 {
					st.Percent = p.SegmentsDone * 100 / p.SegmentsTotal
				}
				if p.ExpectedBytes > 0 && p.Speed > 0 && p.ExpectedBytes > p.Bytes {
					st.ETA = float64(p.ExpectedBytes-p.Bytes) / p.Speed
				} else {
					st.ETA = 0
				}
			}
		case app.EventSyncProgress:
			st.Status = "синхронизация"
			st.Phase = "референс"
			st.Current = e.Current
			st.Total = e.Total
			if e.Total > 0 {
				st.Percent = e.Current * 100 / e.Total
			}
		case app.EventMux:
			st.Status = "вшивание"
			st.Phase = e.Message
			st.Current = e.Current
			st.Total = e.Total
			if e.Total > 0 {
				st.Percent = e.Current * 100 / e.Total
			}
		case app.EventAlign:
			if e.Result != nil {
				st.Offset = e.Result.Offset
				st.Confidence = e.Result.Confidence
				st.HasSync = true
				st.Status = "синхронизация"
			}
		case app.EventEpisodeDone:
			if e.Report != nil {
				st.Status = statusLabel(e.Report.Status)
				st.Output = e.Report.OutputPath
				st.Error = e.Report.Error
				st.Warning = e.Report.Warning
				st.Percent = 100
				st.Speed = 0
				st.ETA = 0
				if e.Report.Sync != nil {
					st.Offset = e.Report.Sync.Offset
					st.Confidence = e.Report.Sync.Confidence
					st.HasSync = true
				}
			}
		}
		if e.Kind == app.EventLog || e.Kind == app.EventEpisodeDone {
			if e.Message != "" {
				m.appendLog(fmt.Sprintf("E%02d  %s", e.Episode, e.Message))
			}
		}
		if e.Kind == app.EventLog && strings.Contains(e.Message, "рендер") {
			st.Status = "рендер"
			st.Phase = "сборка mka"
		}
	}
}

func (m *Model) appendLog(s string) {
	s = strings.ReplaceAll(s, "\n", " ")
	m.logs = append(m.logs, s)
	if len(m.logs) > 200 {
		m.logs = m.logs[len(m.logs)-200:]
	}
}

func (m Model) buildOptions() app.Options {
	opts := app.Options{
		ReleaseURL:  strings.TrimSpace(m.inputs[0].Value()),
		OutDir:      strings.TrimSpace(m.inputs[2].Value()),
		Sync:        m.syncOn,
		NoSync:      !m.syncOn,
		Mux:         m.muxOn,
		ApplyAnyway: m.applyAnyway,
		RefStream:   -1,
	}
	path := strings.TrimSpace(m.inputs[1].Value())
	if m.source == sourceFolder {
		opts.VideoDir = path
	} else {
		opts.M3UPath = path
	}
	return opts
}

func (m Model) selectedNumbers() []int {
	var out []int
	for _, match := range m.plan.Matches {
		if match.Selected {
			out = append(out, match.Episode.Number)
		}
	}
	sort.Ints(out)
	return out
}

func (m Model) anySelected() bool {
	for _, match := range m.plan.Matches {
		if match.Selected {
			return true
		}
	}
	return false
}

func (m Model) allSelected() bool {
	for _, match := range m.plan.Matches {
		if !match.Selected {
			return false
		}
	}
	return true
}

func (m *Model) fixScroll() {
	visible := m.visibleRows()
	if m.cursor < m.listOffset {
		m.listOffset = m.cursor
	}
	if m.cursor >= m.listOffset+visible {
		m.listOffset = m.cursor - visible + 1
	}
}

func (m Model) visibleRows() int {
	n := m.height - 12
	if n < 4 {
		n = 4
	}
	return n
}

func (m *Model) resizeInputs() {
	w := m.contentWidth() - 6
	if w < 20 {
		w = 20
	}
	for i := range m.inputs {
		m.inputs[i].Width = w
	}
}

func (m Model) contentWidth() int {
	w := m.width - 4
	if w > 110 {
		w = 110
	}
	if w < 50 {
		w = 50
	}
	return w
}

func (m Model) View() string {
	if m.width == 0 {
		return "dreamtui"
	}
	switch m.screen {
	case screenSetup:
		return m.viewSetup()
	case screenDiscovering:
		return m.viewDiscovering()
	case screenReview:
		return m.viewReview()
	case screenRunning:
		return m.viewRunning()
	case screenDone:
		return m.viewDone()
	case screenBrowser:
		return m.viewBrowser()
	}
	return ""
}

func fieldLabel(text string, active bool) string {
	if active {
		return accentBold.Render("▸ " + text)
	}
	return labelStyle.Render(text)
}

func inputBox(active bool, view string, width int) string {
	style := inputBoxStyle
	if active {
		style = focusedInputBoxStyle
	}
	return style.Width(width - 2).Render(view)
}

func sourceTabs(source int) string {
	active := lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color("232")).Background(colAccent).Padding(0, 1)
	var tabs [2]string
	for i, label := range videoLabels {
		if i == source {
			tabs[i] = active.Render(label)
		} else {
			tabs[i] = dimStyle.Padding(0, 1).Render(label)
		}
	}
	return "  " + tabs[0] + "  " + tabs[1]
}

func (m Model) viewSetup() string {
	var b strings.Builder
	width := m.contentWidth()
	labels := m.inputLabels()
	b.WriteString(titleStyle.Render("dreamtui"))
	b.WriteString(dimStyle.Render("  ·  Dream Cast → твой релиз"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", width)))
	b.WriteString("\n\n")

	b.WriteString(fieldLabel(labels[0], m.focus == 0))
	b.WriteString("\n")
	b.WriteString(inputBox(m.focus == 0, m.inputs[0].View(), width))
	b.WriteString("\n")

	b.WriteString(fieldLabel("Источник видео (←/→)", m.focus == 1))
	b.WriteString("\n")
	b.WriteString(sourceTabs(m.source))
	b.WriteString("\n")

	b.WriteString(fieldLabel(labels[1], m.focus == 2))
	b.WriteString("\n")
	b.WriteString(inputBox(m.focus == 2, m.inputs[1].View(), width))
	b.WriteString("\n")
	if m.inputs[1].Value() == "" {
		hint := "Ctrl+F — выбрать m3u"
		if m.source == sourceFolder {
			hint = "Ctrl+F — выбрать папку с MKV"
		}
		b.WriteString(dimStyle.Render("  " + hint))
		b.WriteString("\n")
	}

	b.WriteString(fieldLabel(labels[2], m.focus == 3))
	b.WriteString("\n")
	b.WriteString(inputBox(m.focus == 3, m.inputs[2].View(), width))
	b.WriteString("\n")

	if m.err != "" {
		b.WriteString("\n")
		b.WriteString(redStyle.Render("✗ " + m.err))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Enter — проверить релиз   Tab — поле   ←/→ — источник   Ctrl+F — обзор   Ctrl+C — выход"))
	b.WriteString("\n")
	return b.String()
}

func (m Model) viewDiscovering() string {
	source := "источник видео"
	if m.source == sourceFolder {
		source = "папку с видео"
	} else {
		source = "m3u"
	}
	return fmt.Sprintf("\n  %s  Загружаю релиз и %s…\n\n%s",
		m.spinner.View(), source, dimStyle.Render("  разбираю страницу Dream Cast и сопоставляю серии"))
}

func (m Model) viewReview() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("dreamtui"))
	b.WriteString(dimStyle.Render("  ·  проверка"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", m.contentWidth())))
	b.WriteString("\n")
	if m.plan == nil {
		return b.String() + "\nнет плана\n"
	}
	b.WriteString("\n")
	b.WriteString(textStyle.Render(shorten(m.plan.Release.Title, m.contentWidth())))
	b.WriteString("\n")
	source := "без видео"
	switch m.plan.Source {
	case "m3u":
		source = "TorrServer m3u"
	case "folder":
		source = "папка с MKV"
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("Серий: %d из %d   ·   %s: %d   ·   вывод: %s",
		m.plan.Release.Available, m.plan.Release.Total, source, len(m.plan.Entries), shorten(m.plan.OutDir, 50))))
	b.WriteString("\n")
	if m.plan.SourcePath != "" {
		b.WriteString(dimStyle.Render("источник: " + shorten(m.plan.SourcePath, m.contentWidth()-12)))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(renderToggle("авто-синк", m.syncOn, true))
	b.WriteString("  ")
	b.WriteString(renderToggle("вшить в MKV (долго, +место)", m.muxOn, false))
	b.WriteString("  ")
	b.WriteString(renderToggle("применять при низкой уверенности", m.applyAnyway, false))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("offset для каждой серии подбирается автоматически по звуку"))
	b.WriteString("\n")
	if m.plan.Source == "" {
		b.WriteString(yellowStyle.Render("! источник видео не выбран — синхронизации не будет"))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	visible := m.visibleRows()
	end := m.listOffset + visible
	if end > len(m.plan.Matches) {
		end = len(m.plan.Matches)
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("   %-3s %-4s %-10s %s", "#", "выб", "серия", "файл")))
	b.WriteString("\n")
	for i := m.listOffset; i < end; i++ {
		match := m.plan.Matches[i]
		cur := "  "
		if i == m.cursor {
			cur = "▸ "
		}
		check := "[ ]"
		if match.Selected {
			check = greenStyle.Render("[x]")
		}
		file := filepath.Base(match.OutputPath)
		if match.VideoName == "" {
			file = "нет видео"
		}
		line := fmt.Sprintf("%s%s E%02d  %-10s %s", cur, check, match.Episode.Number,
			shorten(match.Episode.Title, 10), shorten(file, m.contentWidth()-26))
		if i == m.cursor {
			b.WriteString(accentBold.Render(line))
		} else if !match.Selected {
			b.WriteString(dimStyle.Render(line))
		} else {
			b.WriteString(textStyle.Render(line))
		}
		b.WriteString("\n")
	}
	if len(m.plan.Matches) > visible {
		b.WriteString(dimStyle.Render(fmt.Sprintf("   … %d–%d из %d", m.listOffset+1, end, len(m.plan.Matches))))
		b.WriteString("\n")
	}
	if m.err != "" {
		b.WriteString("\n")
		b.WriteString(redStyle.Render("✗ " + m.err))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	selected := 0
	for _, match := range m.plan.Matches {
		if match.Selected {
			selected++
		}
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf(
		"Space — выбрать   A — все   S — синк   M — mux   F — low-conf   Enter — начать (%d)   Esc — назад",
		selected)))
	b.WriteString("\n")
	return b.String()
}

func (m Model) viewRunning() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("dreamtui"))
	b.WriteString(dimStyle.Render("  ·  извлечение"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", m.contentWidth())))
	b.WriteString("\n")
	elapsed := time.Since(m.started).Seconds()
	finished := 0
	current := 0
	for i, n := range m.order {
		st := m.states[n]
		if st == nil {
			continue
		}
		if st.Status == "готово" || st.Status == "уже есть" || st.Status == "ошибка" ||
			strings.Contains(st.Status, "ошибка") || st.Status == "нужна проверка" {
			finished++
		}
		if current == 0 && !isFinished(st.Status) {
			current = i + 1
		}
	}
	if current == 0 {
		current = len(m.order)
	}
	header := fmt.Sprintf("%d из %d", current, len(m.order))
	if current > 0 && current <= len(m.order) {
		header = fmt.Sprintf("E%02d · %d из %d", m.order[current-1], current, len(m.order))
	}
	b.WriteString(fmt.Sprintf("\n  %s  %s   %s\n\n", m.spinner.View(),
		textStyle.Render(header),
		dimStyle.Render(fmt.Sprintf("готово %d · прошло %s", finished, fmtClock(elapsed)))))
	if m.cancelling {
		b.WriteString(yellowStyle.Render("  останавливаю… (Esc ещё раз — выйти)"))
		b.WriteString("\n\n")
	}

	for _, n := range m.order {
		st := m.states[n]
		if st == nil {
			continue
		}
		icon := "·"
		style := dimStyle
		switch {
		case st.Status == "готово" || st.Status == "уже есть":
			icon, style = "✓", greenStyle
		case st.Status == "ошибка" || strings.Contains(st.Status, "ошибка"):
			icon, style = "✗", redStyle
		case st.Status == "нужна проверка":
			icon, style = "!", yellowStyle
		case st.Status == "пропуск":
			continue
		case st.Status == "работа" || st.Status == "скачивание" || st.Status == "синхронизация" || st.Status == "рендер" || st.Status == "вшивание":
			icon, style = "▸", accentBold
		}
		line := fmt.Sprintf("  %s E%02d  %-16s", icon, st.Number, st.Status)
		if st.HasSync {
			line += fmt.Sprintf("  offset %+.2f  conf %.2f", st.Offset, st.Confidence)
		}
		b.WriteString(style.Render(line))
		b.WriteString("\n")
		if st.Status == "скачивание" || st.Status == "синхронизация" || st.Status == "рендер" || st.Status == "работа" || st.Status == "вшивание" {
			b.WriteString("      ")
			b.WriteString(progressBar(st.Percent, 28))
			b.WriteString("  ")
			detail := fmt.Sprintf("%3d%%", st.Percent)
			if st.Total > 0 {
				detail += fmt.Sprintf("  %d/%d", st.Current, st.Total)
			}
			if st.Status == "скачивание" {
				detail += "  " + fmtBytes(st.Bytes)
				if st.Speed > 0 {
					detail += "  " + fmtSpeed(st.Speed)
				}
				if st.ETA > 0 {
					detail += "  ETA " + fmtClock(st.ETA)
				}
			}
			b.WriteString(dimStyle.Render(detail))
			b.WriteString("\n")
			if st.Phase != "" {
				b.WriteString(dimStyle.Render("      " + st.Phase))
				b.WriteString("\n")
			}
		}
	}
	b.WriteString("\n")
	logs := m.logs
	if len(logs) > 7 {
		logs = logs[len(logs)-7:]
	}
	if len(logs) > 0 {
		b.WriteString(dimStyle.Render(strings.Repeat("─", m.contentWidth())))
		b.WriteString("\n")
		for _, l := range logs {
			b.WriteString(dimStyle.Render("  " + shorten(l, m.contentWidth()-4)))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("Esc — остановить   Ctrl+C — выйти"))
	b.WriteString("\n")
	return b.String()
}

func (m Model) viewDone() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("dreamtui"))
	b.WriteString(dimStyle.Render("  ·  готово"))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", m.contentWidth())))
	b.WriteString("\n\n")
	if m.report == nil {
		return b.String() + "нет отчёта\n"
	}
	done, exists, needs, errs := 0, 0, 0, 0
	for _, e := range m.report.Episodes {
		switch {
		case e.Status == "done":
			done++
		case e.Status == "exists":
			exists++
		case e.Status == "needs-confirmation":
			needs++
		case strings.HasSuffix(e.Status, "error"):
			errs++
		}
	}
	b.WriteString(fmt.Sprintf("  %s   %s\n\n",
		greenStyle.Render(fmt.Sprintf("✓ %d готово", done+exists)),
		dimStyle.Render(fmt.Sprintf("%d требуют проверки · %d ошибок · %s",
			needs, errs, fmtClock(time.Since(m.started).Seconds())))))
	b.WriteString(dimStyle.Render("  " + shorten(m.report.OutDir, m.contentWidth()-4)))
	b.WriteString("\n\n")
	for _, e := range m.report.Episodes {
		icon, style := "·", dimStyle
		switch {
		case e.Status == "done":
			icon, style = "✓", greenStyle
		case e.Status == "exists":
			icon, style = "·", dimStyle
		case e.Status == "needs-confirmation":
			icon, style = "!", yellowStyle
		case strings.HasSuffix(e.Status, "error"):
			icon, style = "✗", redStyle
		}
		line := fmt.Sprintf("  %s E%02d  %s", icon, e.Number, statusLabel(e.Status))
		if e.Sync != nil {
			line += fmt.Sprintf("  offset %+.2f  conf %.2f", e.Sync.Offset, e.Sync.Confidence)
		}
		if e.OutputPath != "" {
			if st, err := os.Stat(e.OutputPath); err == nil {
				line += "  " + fmtBytes(st.Size())
			}
		}
		if e.MuxPath != "" && e.MuxError == "" {
			line += "  +MKV"
		}
		b.WriteString(style.Render(shorten(line, m.contentWidth())))
		b.WriteString("\n")
		if e.Warning != "" {
			b.WriteString(yellowStyle.Render("      " + shorten(e.Warning, m.contentWidth()-8)))
			b.WriteString("\n")
		}
		if e.Error != "" {
			b.WriteString(redStyle.Render("      " + shorten(e.Error, m.contentWidth()-8)))
			b.WriteString("\n")
		}
	}
	if needs > 0 {
		b.WriteString("\n")
		b.WriteString(panelStyle.Width(m.contentWidth() - 2).Render(
			yellowStyle.Render("Низкая уверенность синка. Файлы для этих серий не созданы.\n") +
				textStyle.Render("Esc — назад и задать offset, либо F — применить всё равно.")))
		b.WriteString("\n")
	}
	if len(m.report.Warnings) > 0 {
		b.WriteString("\n")
		for _, w := range m.report.Warnings {
			b.WriteString(yellowStyle.Render("  ! " + shorten(w, m.contentWidth()-4)))
			b.WriteString("\n")
		}
	}
	if m.toast != "" {
		b.WriteString("\n")
		b.WriteString(greenStyle.Render("  " + m.toast))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("O — открыть папку   E — задать offset   F — применить всё равно   Esc — назад   Q — выход"))
	b.WriteString("\n")
	return b.String()
}

func renderToggle(label string, on, defaultValue bool) string {
	mark := "[ ]"
	if on {
		mark = greenStyle.Render("[x]")
	}
	text := label
	if on != defaultValue {
		text += "*"
	}
	return mark + " " + textStyle.Render(text)
}

func progressBar(pct, width int) string {
	if width < 8 {
		width = 8
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * width / 100
	return greenStyle.Render(strings.Repeat("█", filled)) +
		dimStyle.Render(strings.Repeat("░", width-filled))
}

func isFinished(status string) bool {
	switch status {
	case "готово", "уже есть", "ошибка", "ошибка загрузки", "ошибка синка", "ошибка рендера", "нужна проверка", "пропуск":
		return true
	}
	return false
}

func statusLabel(s string) string {
	switch s {
	case "done":
		return "готово"
	case "exists":
		return "уже есть"
	case "needs-confirmation":
		return "нужна проверка"
	case "download-error":
		return "ошибка загрузки"
	case "sync-error":
		return "ошибка синка"
	case "render-error":
		return "ошибка рендера"
	case "running":
		return "работа"
	case "пропуск":
		return "пропуск"
	default:
		return s
	}
}

func shorten(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
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

func fmtSpeed(bps float64) string {
	return fmtBytes(int64(bps)) + "/s"
}

func fmtClock(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	s := int(sec)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func openFolder(path string) error {
	if path == "" {
		return fmt.Errorf("пустой путь")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}
