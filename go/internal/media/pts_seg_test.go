package media

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSegmentStartPTS 验证：分片时间戳起点 = 前面 EXTINF 累加（忽略源绝对值）
func TestSegmentStartPTS(t *testing.T) {
	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: "http://up/pl.m3u8"}, ModeProxy)

	m3u8 := "#EXTM3U\n#EXTINF:4.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n#EXTINF:5.0,\nseg2.ts\n#EXT-X-ENDLIST\n"
	s.rewritePlaylist([]byte(m3u8), "http://up/pl.m3u8", ses.ID)

	if got := s.segmentStartPTS(ses, "http://up/seg0.ts"); got != 0 {
		t.Fatalf("seg0 start=%d want 0", got)
	}
	if got := s.segmentStartPTS(ses, "http://up/seg1.ts"); got != 4*90000 {
		t.Fatalf("seg1 start=%d want %d", got, 4*90000)
	}
	if got := s.segmentStartPTS(ses, "http://up/seg2.ts"); got != 10*90000 {
		t.Fatalf("seg2 start=%d want %d", got, 10*90000)
	}
}

// TestRecordSegmentDedup 验证：重复拉播放列表不翻倍分片表
func TestRecordSegmentDedup(t *testing.T) {
	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: "http://up/pl.m3u8"}, ModeProxy)

	m3u8 := "#EXTM3U\n#EXTINF:6.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n#EXT-X-ENDLIST\n"
	// 拉两次播放列表（电视可能重复拉）
	s.rewritePlaylist([]byte(m3u8), "http://up/pl.m3u8", ses.ID)
	s.rewritePlaylist([]byte(m3u8), "http://up/pl.m3u8", ses.ID)

	ses.segMu.Lock()
	n := len(ses.segments)
	ses.segMu.Unlock()
	if n != 2 {
		t.Fatalf("segments=%d want 2（去重失败）", n)
	}
	// seg1 起点应仍是 6s（不是 12s）
	if got := s.segmentStartPTS(ses, "http://up/seg1.ts"); got != 6*90000 {
		t.Fatalf("seg1 start=%d want %d", got, 6*90000)
	}
}

// TestPTSRewriteUsesExtinfBase 验证：PTS 重置器用分片 base 生成连续时间戳，
// 即使源分片时间戳是相对（从 0 开始）也不会回退
func TestPTSRewriteUsesExtinfBase(t *testing.T) {
	rw2 := newPTSRewriter(4 * 90000)        // 分片2 base = 4s
	p2 := buildTSPacket(0x100, 0xE0, 0, true) // 源相对时间戳从 0 开始
	rw2.rewrite(p2)
	off2 := tsPayloadOffset(p2)
	ptsOff2, _, _, _ := pesPTSOffset(p2[off2:])
	if got := decodePTS(p2[off2+ptsOff2 : off2+ptsOff2+5]); got != 4*90000 {
		t.Fatalf("seg2 first video PTS=%d want %d（跨分片连续）", got, 4*90000)
	}
}

// TestEndToEndCrossSegment 端到端：两个分片经代理，时间戳全局连续
// （源分片都是相对时间戳从 0 开始，模拟 B站 最坏情况）
func TestEndToEndCrossSegment(t *testing.T) {
	seg0 := buildTSPacket(0x100, 0xE0, 0, true)
	seg1 := buildTSPacket(0x100, 0xE0, 0, true) // 源相对时间戳，从 0 开始

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pl.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Write([]byte("#EXTM3U\n#EXTINF:6.0,\nseg0.ts\n#EXTINF:6.0,\nseg1.ts\n#EXT-X-ENDLIST\n"))
		case "/seg0.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(seg0)
		case "/seg1.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Write(seg1)
		default:
			w.WriteHeader(404)
		}
	}))
	defer up.Close()

	s := NewServer("http://127.0.0.1:9999")
	ses, _ := s.Start(Source{URL: up.URL + "/pl.m3u8"}, ModeProxy)

	// 拉播放列表
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/m/"+ses.ID, nil))
	body := rr.Body.String()
	var segURLs []string
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.Contains(ln, "/seg/") {
			segURLs = append(segURLs, ln)
		}
	}
	if len(segURLs) != 2 {
		t.Fatalf("want 2 segs, got %d\n%s", len(segURLs), body)
	}

	// 分片0
	r0 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r0, httptest.NewRequest("GET", segURLs[0], nil))
	off0 := tsPayloadOffset(r0.Body.Bytes())
	pts0, _, _, _ := pesPTSOffset(r0.Body.Bytes()[off0:])
	if got := decodePTS(r0.Body.Bytes()[off0+pts0 : off0+pts0+5]); got != 0 {
		t.Fatalf("seg0 PTS=%d want 0", got)
	}

	// 分片1（EXTINF 6.0s → base = 6*90000）
	r1 := httptest.NewRecorder()
	s.Handler().ServeHTTP(r1, httptest.NewRequest("GET", segURLs[1], nil))
	off1 := tsPayloadOffset(r1.Body.Bytes())
	pts1, _, _, _ := pesPTSOffset(r1.Body.Bytes()[off1:])
	if got := decodePTS(r1.Body.Bytes()[off1+pts1 : off1+pts1+5]); got != 6*90000 {
		t.Fatalf("seg1 PTS=%d want %d（跨分片连续，源相对时间戳也不回退）", got, 6*90000)
	}
}
