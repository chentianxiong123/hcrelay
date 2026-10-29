package media

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildTSPacket 构造一个含 PES + PTS 的 188B TS 包
func buildTSPacket(pid int, streamID byte, pts uint64, payloadUnitStart bool) []byte {
	pkt := make([]byte, tsPacketSize)
	pkt[0] = 0x47
	pkt[1] = byte((pid >> 8) & 0x1F)
	if payloadUnitStart {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid & 0xFF)
	pkt[3] = 0x10 | 0x01 // payload only + continuity 1

	// PES 头
	pes := pkt[4:]
	pes[0] = 0
	pes[1] = 0
	pes[2] = 1
	pes[3] = streamID
	binary.BigEndian.PutUint16(pes[4:6], 0) // packet length (0 = unbounded)
	pes[6] = 0x80                           // 10
	pes[7] = 0x80                           // PTS present
	pes[8] = 5                              // header data length
	encodePTS(pes[9:14], pts)
	return pkt
}

func TestPTSRoundTrip(t *testing.T) {
	// 各种 PTS 值（含 33 位边界）
	vals := []uint64{0, 1, 3003, 90000, 0x1FFFFFFFF, 0x123456789}
	for _, v := range vals {
		b := make([]byte, 5)
		encodePTS(b, v)
		if got := decodePTS(b); got != v {
			t.Fatalf("roundtrip %d -> %d", v, got)
		}
	}
}

func TestPESOffset(t *testing.T) {
	pkt := buildTSPacket(0x100, 0xE0, 3003, true)
	off := tsPayloadOffset(pkt)
	if off != 4 {
		t.Fatalf("payload offset = %d, want 4", off)
	}
	pes := pkt[off:]
	ptsOff, dtsOff, hasPTS, hasDTS := pesPTSOffset(pes)
	if !hasPTS || hasDTS {
		t.Fatalf("want PTS only, got hasPTS=%v hasDTS=%v", hasPTS, hasDTS)
	}
	if ptsOff != 9 {
		t.Fatalf("PTS offset = %d, want 9", ptsOff)
	}
	if dtsOff != -1 {
		t.Fatalf("DTS offset = %d, want -1", dtsOff)
	}
}

// TestTSRewriteReaderShortTail 回归：分片不足 188 字节（或非整数倍）时
// 不能死循环，尾部字节必须原样送出。
func TestTSRewriteReaderShortTail(t *testing.T) {
	rw := newPTSRewriter(0)
	pkt1 := buildTSPacket(0x100, 0xE0, 100000, true)
	tail := []byte{0x47, 0x40, 0x11, 0x10, 0xAA, 0xBB} // 6 字节短尾部
	src := bytes.NewReader(append(pkt1, tail...))
	r := newTSRewriteReader(src, rw)

	buf := make([]byte, tsPacketSize+64)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != tsPacketSize+6 {
		t.Fatalf("read %d bytes, want %d", n, tsPacketSize+6)
	}
	// 尾部原样送出
	if !bytes.Equal(buf[tsPacketSize:n], tail) {
		t.Fatalf("tail mismatch: %v", buf[tsPacketSize:n])
	}
}

func TestRewriteVideoPTS(t *testing.T) {
	rw := newPTSRewriter(1000)
	// 第一个视频 PES：out = base
	pkt := buildTSPacket(0x100, 0xE0, 100000, true)
	if !rw.rewrite(pkt) {
		t.Fatal("rewrite returned false")
	}
	off := tsPayloadOffset(pkt)
	ptsOff, _, _, _ := pesPTSOffset(pkt[off:])
	got := decodePTS(pkt[off+ptsOff : off+ptsOff+5])
	if got != 1000 {
		t.Fatalf("first video PTS = %d, want 1000", got)
	}

	// 第二个视频 PES（+3003，假设 29.97fps）：应保持源相对
	rw.setFrameRate(29.97)
	pkt2 := buildTSPacket(0x100, 0xE0, 103003, true)
	rw.rewrite(pkt2)
	off2 := tsPayloadOffset(pkt2)
	ptsOff2, _, _, _ := pesPTSOffset(pkt2[off2:])
	got2 := decodePTS(pkt2[off2+ptsOff2 : off2+ptsOff2+5])
	if got2 != 1000+3003 {
		t.Fatalf("second video PTS = %d, want %d", got2, 1000+3003)
	}
}

func TestRewriteAudioPTS(t *testing.T) {
	rw := newPTSRewriter(5000)
	// 先视频建立基准
	pktV := buildTSPacket(0x100, 0xE0, 200000, true)
	rw.rewrite(pktV)
	// 音频：源 PTS 在视频之后一点（音视频同步）
	pktA := buildTSPacket(0x101, 0xC0, 200000+1920, true)
	if !rw.rewrite(pktA) {
		t.Fatal("audio rewrite failed")
	}
	off := tsPayloadOffset(pktA)
	ptsOff, _, _, _ := pesPTSOffset(pktA[off:])
	got := decodePTS(pktA[off+ptsOff : off+ptsOff+5])
	// 音频基准 = 视频基准 + (源音频起点 - 源视频起点) = 5000 + 1920
	if got != 5000+1920 {
		t.Fatalf("audio PTS = %d, want %d", got, 5000+1920)
	}
}

// TestTSRewriteReader 验证流式包装能逐包重写
func TestTSRewriteReader(t *testing.T) {
	rw := newPTSRewriter(0)
	pkt1 := buildTSPacket(0x100, 0xE0, 100000, true)
	pkt2 := buildTSPacket(0x101, 0xC0, 101920, true)
	src := bytes.NewReader(append(pkt1, pkt2...))
	r := newTSRewriteReader(src, rw)

	buf := make([]byte, tsPacketSize*2)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != tsPacketSize*2 {
		t.Fatalf("read %d bytes, want %d", n, tsPacketSize*2)
	}
	// 验证第一个包被重写（PTS 从 100000 → 0）
	off1 := tsPayloadOffset(buf[:tsPacketSize])
	ptsOff1, _, _, _ := pesPTSOffset(buf[off1:])
	if decodePTS(buf[off1+ptsOff1:off1+ptsOff1+5]) != 0 {
		t.Fatalf("first packet PTS not rewritten")
	}
}
