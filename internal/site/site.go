// Package site parses Dream Cast release pages.
package site

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	titleRE       = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	atobRE        = regexp.MustCompile(`atob\("([A-Za-z0-9+/=]+)"\)`)
	episodeInfoRE = regexp.MustCompile(`(?s)Эпизодов:.*?<span>\s*(\d+)\s*из\s*(\d+)\s*</span>`)
	idRE          = regexp.MustCompile(`(\d+)`)
)

// Release is a parsed Dream Cast release page.
type Release struct {
	URL       string    `json:"url"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Slug      string    `json:"slug"`
	Available int       `json:"available"`
	Total     int       `json:"total"`
	Episodes  []Episode `json:"episodes"`
}

// Episode is one released episode. URLs point at HLS master playlists.
type Episode struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	AACURL    string `json:"aac_url"`
	OpusURL   string `json:"opus_url"`
	MasterURL string `json:"master_url"`
}

// Fetch downloads and parses a release page.
func Fetch(ctx context.Context, client *http.Client, releaseURL string) (*Release, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("загрузка страницы Dream Cast: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Dream Cast ответил HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return Parse(body, resp.Request.URL.String())
}

// Parse extracts release metadata and HLS episode URLs from a release page.
func Parse(body []byte, pageURL string) (*Release, error) {
	page := string(body)
	rel := &Release{URL: pageURL}
	if u, err := url.Parse(pageURL); err == nil {
		rel.Slug = strings.Trim(u.Path, "/")
		if i := strings.LastIndex(rel.Slug, "/"); i >= 0 {
			rel.Slug = rel.Slug[i+1:]
		}
		if m := idRE.FindString(rel.Slug); m != "" {
			rel.ID = m
		}
	}

	if m := titleRE.FindStringSubmatch(page); len(m) > 1 {
		rel.Title = cleanTitle(m[1])
	}
	if rel.Title == "" {
		rel.Title = rel.Slug
	}

	m := atobRE.FindStringSubmatch(page)
	if len(m) < 2 {
		return nil, fmt.Errorf("на странице не найден список серий Dream Cast")
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return nil, fmt.Errorf("не удалось декодировать список серий: %w", err)
	}
	var masters []string
	for _, u := range strings.Split(string(raw), "||||") {
		u = strings.TrimSpace(u)
		if strings.Contains(u, "play.dreamerscast.com") && strings.Contains(u, ".m3u8") {
			masters = append(masters, u)
		}
	}
	if len(masters) == 0 {
		return nil, fmt.Errorf("список серий пуст")
	}

	for i, master := range masters {
		ep := Episode{
			Number:    i + 1,
			Title:     fmt.Sprintf("Серия %d", i+1),
			MasterURL: master,
			AACURL:    master,
			OpusURL:   opusFromAAC(master),
		}
		rel.Episodes = append(rel.Episodes, ep)
	}
	rel.Available = len(rel.Episodes)
	rel.Total = rel.Available
	if m := episodeInfoRE.FindStringSubmatch(page); len(m) == 3 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			rel.Available = n
		}
		if n, err := strconv.Atoi(m[2]); err == nil {
			rel.Total = n
		}
	}
	if rel.Total < rel.Available {
		rel.Total = rel.Available
	}
	return rel, nil
}

func opusFromAAC(u string) string {
	if strings.Contains(u, ",aac,") {
		return strings.ReplaceAll(u, ",aac,", ",opus,")
	}
	return u
}

func cleanTitle(s string) string {
	s = html.UnescapeString(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSuffix(s, " - Dream Cast")
	s = strings.TrimSpace(s)
	return s
}
