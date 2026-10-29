package relay

import (
	"fmt"
	"testing"
)

func TestGuessHeadersForUnknownSites(t *testing.T) {
	cases := []string{
		"https://cdn.unknown-site.com/video/playlist?token=xxx",
		"https://vod.999zy.com/2024/movie.m3u8",
		"https://api.example-cdn.net/hls/abc/index.m3u8?sign=1",
		"https://s3.bfllvip.com/video/x/index.m3u8",
		"https://v3-dy-o.zjcdn.com/x/y.m3u8",
	}
	for _, u := range cases {
		ref, _ := guessHeaders(u)
		fmt.Printf("%-58s → Referer=%q\n", u, ref)
	}
}
