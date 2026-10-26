package m3u

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := `#EXTM3U
#EXTINF:-1,Cyberpunk Edgerunners 2 - 01 [2160p WEB-DL DV HDR10].mkv
http://127.0.0.1:8090/stream/one
#EXTINF:-1,Cyberpunk Edgerunners 2 - 02 [2160p WEB-DL DV HDR10].mkv
http://127.0.0.1:8090/stream/two
#EXTINF:24,Show S01E10
http://127.0.0.1:8090/stream/ten
`
	entries, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries", len(entries))
	}
	want := []int{1, 2, 10}
	for i, n := range want {
		if entries[i].EpisodeNumber != n {
			t.Errorf("entry %d: episode %d, want %d", i, entries[i].EpisodeNumber, n)
		}
	}
	if entries[2].Duration != 24 {
		t.Errorf("duration = %d", entries[2].Duration)
	}
	if entries[1].Name == "" || entries[1].URL == "" {
		t.Errorf("entry not populated: %+v", entries[1])
	}
}
