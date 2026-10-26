// Package m3u parses TorrServer-style M3U playlists.
package m3u

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Entry is one playlist item.
type Entry struct {
	Index         int
	Name          string
	URL           string
	EpisodeNumber int // 0 when it could not be detected
	Duration      int
}

var episodePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\s-\s(\d{1,3})\s`),
	regexp.MustCompile(`(?i)(?:^|[\s._-])S\d{1,2}E(\d{1,3})(?:[\s._\-\[]|$)`),
	regexp.MustCompile(`(?i)(?:^|[\s._-])E(?:P)?(\d{1,3})(?:[\s._\-\[]|$)`),
	regexp.MustCompile(`(?i)(?:Серия|Эпизод|Episode|Ep\.?)\s*(\d{1,3})`),
	regexp.MustCompile(`(?i)(?:^|[\s._-])(\d{1,3})(?:[\s._\-\[]|$)`),
}

// ParseFile reads an M3U/M3U8 playlist from disk.
func ParseFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse reads an M3U/M3U8 playlist.
func Parse(r io.Reader) ([]Entry, error) {
	var entries []Entry
	var pendingName string
	var pendingDuration int
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			rest := strings.TrimPrefix(line, "#EXTINF:")
			if i := strings.Index(rest, ","); i >= 0 {
				if d, err := strconv.ParseFloat(strings.TrimSpace(rest[:i]), 64); err == nil {
					pendingDuration = int(d)
				}
				pendingName = strings.TrimSpace(rest[i+1:])
			} else {
				pendingName = strings.TrimSpace(rest)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		e := Entry{
			Index:    len(entries),
			Name:     pendingName,
			URL:      line,
			Duration: pendingDuration,
		}
		if e.Name == "" {
			e.Name = fmt.Sprintf("item %d", e.Index+1)
		}
		e.EpisodeNumber = DetectEpisode(e.Name)
		entries = append(entries, e)
		pendingName = ""
		pendingDuration = 0
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("в m3u не найдено ни одного элемента")
	}
	return entries, nil
}

// DetectEpisode extracts an episode number from a file or playlist name.
// It returns 0 when no number can be detected.
func DetectEpisode(name string) int {
	for _, re := range episodePatterns {
		m := re.FindStringSubmatch(name)
		if len(m) != 2 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 && n < 1000 {
			return n
		}
	}
	return 0
}
