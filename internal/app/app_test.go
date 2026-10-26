package app

import (
	"os"
	"path/filepath"
	"testing"

	align "dreamtui/internal/sync"
)

func TestCutRefineTimes(t *testing.T) {
	cuts := []align.Cut{{Start: 155, End: 160, SearchStart: 120, SearchEnd: 210}}
	existing := []align.RefWindow{{VideoTime: 120}, {VideoTime: 210}, {VideoTime: 240}}
	times := cutRefineTimes(cuts, 400, 12, existing)
	if len(times) == 0 {
		t.Fatal("no refine times")
	}
	var hasBefore, hasAfter bool
	for _, v := range times {
		if v > 180 && v < 196 {
			hasBefore = true
		}
		if v > 200 && v < 228 {
			hasAfter = true
		}
		for _, e := range existing {
			if v == e.VideoTime {
				t.Fatalf("duplicate existing anchor at %.0f", v)
			}
		}
		if v < 0 || v > 388 {
			t.Fatalf("out of range: %.0f", v)
		}
	}
	if !hasBefore || !hasAfter {
		t.Fatalf("times do not cover both sides: %v", times)
	}
}

func TestScanVideoDir(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"Show - 01 [2160p].mkv",
		"Show.S01E02.2160p.mkv",
		"Show - 10 [2160p].mp4",
		"notes.txt",
		"poster.jpg",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ScanVideoDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	want := map[string]int{
		"Show - 01 [2160p].mkv": 1,
		"Show.S01E02.2160p.mkv": 2,
		"Show - 10 [2160p].mp4": 10,
	}
	for i, e := range entries {
		if e.EpisodeNumber != want[e.Name] {
			t.Errorf("entry %d (%s): episode %d, want %d", i, e.Name, e.EpisodeNumber, want[e.Name])
		}
		if e.URL != filepath.Join(dir, e.Name) {
			t.Errorf("entry %d URL = %q", i, e.URL)
		}
	}
	if got := DefaultDirOutDir(dir); got != dir+" - DreamCast" {
		t.Errorf("out dir = %q", got)
	}
}
