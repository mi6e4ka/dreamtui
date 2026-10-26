// Package media queries streams with ffprobe and selects the reference track.
package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Info is a subset of ffprobe output.
type Info struct {
	URL      string   `json:"url"`
	Duration float64  `json:"duration"`
	Size     int64    `json:"size"`
	BitRate  int64    `json:"bit_rate"`
	Streams  []Stream `json:"streams"`
}

// Stream is one container stream.
type Stream struct {
	Index         int               `json:"index"`
	Type          string            `json:"type"`
	Codec         string            `json:"codec"`
	Channels      int               `json:"channels"`
	ChannelLayout string            `json:"channel_layout"`
	Language      string            `json:"language"`
	Title         string            `json:"title"`
	Tags          map[string]string `json:"tags,omitempty"`
}

type probeJSON struct {
	Format struct {
		Duration string `json:"duration"`
		Size     string `json:"size"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index         int               `json:"index"`
		CodecType     string            `json:"codec_type"`
		CodecName     string            `json:"codec_name"`
		Channels      int               `json:"channels"`
		ChannelLayout string            `json:"channel_layout"`
		Tags          map[string]string `json:"tags"`
	} `json:"streams"`
}

// Probe reads media metadata, including remote HTTP streams.
func Probe(ctx context.Context, mediaURL string) (*Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-rw_timeout", "30000000",
		"-show_entries", "format=duration,size,bit_rate",
		"-show_entries", "stream=index,codec_type,codec_name,channels,channel_layout:stream_tags=language,title",
		"-of", "json",
		mediaURL,
	)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg != "" {
				return nil, fmt.Errorf("ffprobe: %s", msg)
			}
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var raw probeJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("разбор ffprobe: %w", err)
	}
	info := &Info{URL: mediaURL}
	info.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	info.Size, _ = strconv.ParseInt(raw.Format.Size, 10, 64)
	info.BitRate, _ = strconv.ParseInt(raw.Format.BitRate, 10, 64)
	for _, s := range raw.Streams {
		st := Stream{
			Index:         s.Index,
			Type:          s.CodecType,
			Codec:         s.CodecName,
			Channels:      s.Channels,
			ChannelLayout: s.ChannelLayout,
			Tags:          s.Tags,
		}
		if s.Tags != nil {
			for k, v := range s.Tags {
				switch strings.ToLower(k) {
				case "language", "lang":
					st.Language = v
				case "title", "handler_name":
					if st.Title == "" || strings.EqualFold(k, "title") {
						st.Title = v
					}
				}
			}
		}
		info.Streams = append(info.Streams, st)
	}
	if info.Duration <= 0 {
		return nil, fmt.Errorf("не удалось определить длительность потока")
	}
	return info, nil
}

// Audio returns all audio streams.
func (i *Info) Audio() []Stream {
	var out []Stream
	for _, s := range i.Streams {
		if s.Type == "audio" {
			out = append(out, s)
		}
	}
	return out
}

// ChooseReference picks the original/Japanese audio track. override is an
// absolute container stream index, or -1 for automatic selection.
func (i *Info) ChooseReference(override int) (Stream, error) {
	audio := i.Audio()
	if len(audio) == 0 {
		return Stream{}, fmt.Errorf("в видео нет аудиодорожек")
	}
	if override >= 0 {
		for _, s := range audio {
			if s.Index == override {
				return s, nil
			}
		}
		return Stream{}, fmt.Errorf("аудиопоток с индексом %d не найден", override)
	}
	best := audio[0]
	bestScore := -9999
	for _, s := range audio {
		score := s.Channels * 2
		lang := strings.ToLower(strings.TrimSpace(s.Language))
		title := strings.ToLower(s.Title)
		switch lang {
		case "jpn", "ja", "jp", "japanese":
			score += 100
		case "rus", "ru", "russian":
			score -= 60
		}
		if strings.Contains(title, "japanese") || strings.Contains(title, "日本語") || strings.Contains(title, "оригинал") {
			score += 60
		}
		if strings.Contains(title, "commentary") || strings.Contains(title, "комментар") {
			score -= 80
		}
		if strings.Contains(title, "flora") || strings.Contains(title, "lostfilm") ||
			strings.Contains(title, "red head") || strings.Contains(title, "дубляж") {
			score -= 30
		}
		switch strings.ToLower(s.Codec) {
		case "truehd", "flac", "eac3", "dts":
			score += 5
		case "aac":
			score += 1
		}
		if score > bestScore {
			bestScore = score
			best = s
		}
	}
	return best, nil
}

// DescribeStream returns a human-readable audio track label.
func DescribeStream(s Stream) string {
	parts := []string{fmt.Sprintf("#%d", s.Index), s.Codec}
	if s.Channels > 0 {
		parts = append(parts, fmt.Sprintf("%dch", s.Channels))
	}
	if s.Language != "" {
		parts = append(parts, s.Language)
	}
	if s.Title != "" {
		parts = append(parts, s.Title)
	}
	return strings.Join(parts, " · ")
}
