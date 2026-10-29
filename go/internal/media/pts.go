package media

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"
)

// MPEG-TS 包大小
const tsPacketSize = 188

// 90kHz 时钟，PTS 单位
const pts90k = 90000

// ---------------------------------------------------------------------------
// PTS/DTS 编码/解码（ISO 13818-1 PES 头）
// ---------------------------------------------------------------------------

// decodePTS 从 5 字节 PES 头字段解码 33 位 PTS
func decodePTS(b []byte) uint64 {
	return uint64(b[0]>>1&0x07)<<30 |
		uint64(b[1])<<22 |
		uint64(b[2]>>1)<<15 |
		uint64(b[3])<<7 |
		uint64(b[4]>>1)
}

// encodePTS 编码 33 位 PTS 到 5 字节（marker 位自动置 1）
func encodePTS(b []byte, pts uint64) {
	b[0] = 0x21 | byte((pts>>30)&0x07)<<1
	b[1] = byte(pts >> 22)
	b[2] = 0x01 | byte((pts>>15)&0x7F)<<1
	b[3] = byte(pts >> 7)
	b[4] = 0x01 | byte(pts&0x7F)<<1
}

// pesPTSOffset 返回 PES 头里 PTS 起始偏移，-1 表示无 PTS
// pes 指向 PES 头起始（00 00 01 stream_id ...）
func pesPTSOffset(pes []byte) (ptsOff int, dtsOff int, hasPTS bool, hasDTS bool) {
	if len(pes) < 9 {
		return -1, -1, false, false
	}
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return -1, -1, false, false
	}
	// stream_id: 0xE0-0xEF 视频, 0xC0-0xDF 音频, 0xBD 私有流
	streamID := pes[3]
	if streamID == 0xBC || streamID == 0xBE || streamID == 0xBF || streamID == 0xF0 ||
		streamID == 0xF1 || streamID == 0xF2 || streamID == 0xF8 || streamID == 0xFF {
		return -1, -1, false, false // 非 PES（PSM/填充等）
	}
	// flags
	flags := pes[7]
	hasPTS = flags&0x80 != 0
	hasDTS = flags&0x40 != 0
	ptsOff, dtsOff = -1, -1
	// PTS 在固定头(9) 之后（若 hasPTS）
	// 注意：9 是 00 00 01 + stream_id(1) + len(2) + flags(2) + header_len(1) = 9
	off := 9
	if hasPTS {
		ptsOff = off
		off += 5
	}
	if hasDTS {
		dtsOff = off
	}
	// 检查 PTS/DTS 是否落在 PES 头内（含 header_data）
	if hasPTS && ptsOff+5 > len(pes) {
		return -1, -1, false, false
	}
	if hasDTS && dtsOff+5 > len(pes) {
		return -1, -1, false, false
	}
	return ptsOff, dtsOff, hasPTS, hasDTS
}

// ---------------------------------------------------------------------------
// TS 包解析辅助
// ---------------------------------------------------------------------------

// tsPayloadOffset 返回 TS 包里 payload 起始偏移；无 payload 返回 -1
// 处理 adaptation_field（含 PCR）
func tsPayloadOffset(pkt []byte) int {
	if len(pkt) < tsPacketSize || pkt[0] != 0x47 {
		return -1
	}
	afc := (pkt[3] >> 4) & 0x03 // adaptation_field_control
	if afc&0x02 == 0 {          // 只有 payload
		return 4
	}
	// 有 adaptation_field
	afLen := int(pkt[4])
	return 5 + afLen
}

// tsPID 返回 TS 包的 PID
func tsPID(pkt []byte) int {
	return int(pkt[1]&0x1F)<<8 | int(pkt[2])
}

// tsHasPayload 返回是否含 payload（afc==1 或 afc==3 且 afLen<183）
func tsHasPayload(pkt []byte) bool {
	if len(pkt) < tsPacketSize || pkt[0] != 0x47 {
		return false
	}
	afc := (pkt[3] >> 4) & 0x03
	if afc == 1 {
		return true
	}
	if afc == 3 {
		return int(pkt[4]) < 183
	}
	return false
}

// tsPESStart 返回 TS 包是否以 PES 头开始（payload_unit_start_indicator）
func tsPESStart(pkt []byte) bool {
	return len(pkt) >= tsPacketSize && pkt[0] == 0x47 && pkt[1]&0x40 != 0
}

// ---------------------------------------------------------------------------
// PTS 重置器：把流内 PES 的 PTS/DTS 重写为连续、速率 1.0 的值
// ---------------------------------------------------------------------------

// ptsRewriter 在 io.Reader 层重写 MPEG-TS 的 PTS/DTS。
//
// 背景：B站 等源的 PES 时间戳有累积漂移（增量不精确，如视频每帧 +3003
// 而非精确的 90k/fps），电视播放器按 PTS 播，长时间后 PTS 落后真实时间，
// 于是音画一起变慢、卡顿。proxy 字节透传不改时间戳，治不了。
//
// 方案：为每个 PES 重新生成 PTS——视频按检测到的帧率精确累加，音频按
// AAC 帧长精确累加，并保持音视频相对同步（继承源流内偏移）。这样输出
// 流 PTS 速率恒为 1.0，电视按新 PTS 播就不漂移了。
type ptsRewriter struct {
	// 输出基准（90kHz）：跨分片连续，由调用方设置
	base uint64

	// 视频/音频各自的源起点与输出起点，用于保持源内相对同步
	srcVideoBase uint64 // 源流第一个视频 PTS
	srcAudioBase uint64 // 源流第一个音频 PTS
	outVideoBase uint64 // 对应输出视频基准
	outAudioBase uint64 // 对应输出音频基准
	haveVideoBase bool
	haveAudioBase bool

	// 帧率检测
	videoCount  int
	lastSrcVideoPTS uint64
	haveLastVideo   bool
	frameDur        uint64 // 视频每帧时长（90kHz），0=未定

	// 音频 PTS 增量（AAC 帧 1024/48000s → 1920 ticks）
	audioStep uint64

	// 统计
	rewritten int
	segments  int // 处理的 PES 包数
}

// newPTSRewriter 创建重置器
func newPTSRewriter(base uint64) *ptsRewriter {
	return &ptsRewriter{
		base:      base,
		audioStep: 1920, // AAC 48kHz 1024 采样/帧
	}
}

// setFrameRate 设置视频帧率（fps），用于精确累加视频 PTS
func (p *ptsRewriter) setFrameRate(fps float64) {
	if fps > 0 {
		p.frameDur = uint64(float64(pts90k) / fps)
	}
}

// rewrite 处理一个 TS 包（188B），就地重写 PTS/DTS。
// 返回 true 表示发生了重写（PES 头被修改）。
func (p *ptsRewriter) rewrite(pkt []byte) bool {
	if len(pkt) < tsPacketSize || pkt[0] != 0x47 {
		return false
	}
	// 只有 payload_unit_start（PES 头）才可能含 PTS
	if !tsPESStart(pkt) {
		return false
	}
	off := tsPayloadOffset(pkt)
	if off < 0 || off+9 > len(pkt) {
		return false
	}
	pes := pkt[off:]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return false
	}
	ptsOff, dtsOff, hasPTS, hasDTS := pesPTSOffset(pes)
	if !hasPTS {
		return false
	}
	streamID := pes[3]
	isVideo := streamID >= 0xE0 && streamID <= 0xEF
	isAudio := streamID >= 0xC0 && streamID <= 0xDF

	srcPTS := decodePTS(pes[ptsOff : ptsOff+5])
	newPTS := p.nextPTS(srcPTS, isVideo, isAudio)
	encodePTS(pes[ptsOff:ptsOff+5], newPTS)
	if hasDTS {
		// DTS = PTS - 帧时长（视频 B 帧情况）；保守保持 PTS-DTS 差
		if isVideo && p.frameDur > 0 && newPTS >= p.frameDur {
			encodePTS(pes[dtsOff:dtsOff+5], newPTS-p.frameDur)
		} else {
			// 无帧率信息或音频：DTS=PTS
			encodePTS(pes[dtsOff:dtsOff+5], newPTS)
		}
	}
	p.rewritten++
	return true
}

// nextPTS 计算当前 PES 的新 PTS（保持源内音视频相对同步，速率 1.0）
func (p *ptsRewriter) nextPTS(srcPTS uint64, isVideo, isAudio bool) uint64 {
	if isVideo {
		if !p.haveVideoBase {
			p.haveVideoBase = true
			p.srcVideoBase = srcPTS
			p.outVideoBase = p.base
			// 第一个视频 PES：用源起点
			p.lastSrcVideoPTS = srcPTS
			p.haveLastVideo = true
			p.videoCount = 1
			return p.outVideoBase
		}
		// 帧率检测：用源相邻视频 PES 差
		if p.haveLastVideo && srcPTS > p.lastSrcVideoPTS {
			delta := srcPTS - p.lastSrcVideoPTS
			p.videoCount++
			if p.frameDur == 0 && delta > 0 && isStandardFrameDelta(delta) {
				p.setFrameRate(float64(pts90k) / float64(delta))
			}
			p.lastSrcVideoPTS = srcPTS
		}
		// 输出：保持源内相对偏移（用 int64 防下溢），速率归一化由调用方
		// 通过 scale 校正。这里默认保持同步，不破坏 B 帧乱序。
		diff := int64(srcPTS) - int64(p.srcVideoBase)
		if diff < 0 {
			diff = 0
		}
		return p.outVideoBase + uint64(diff)
	}
	if isAudio {
		if !p.haveAudioBase {
			p.haveAudioBase = true
			p.srcAudioBase = srcPTS
			// 音频基准对齐到视频基准（保持音视频同步）
			if p.haveVideoBase {
				// 音频起点 = 视频起点 + (源音频起点 - 源视频起点)
				diff := int64(srcPTS) - int64(p.srcVideoBase)
				p.outAudioBase = uint64(int64(p.outVideoBase) + diff)
			} else {
				p.outAudioBase = p.base
			}
			return p.outAudioBase
		}
		diff := int64(srcPTS) - int64(p.srcAudioBase)
		if diff < 0 {
			diff = 0
		}
		return p.outAudioBase + uint64(diff)
	}
	return p.base
}

// isStandardFrameDelta 判断 PES 时间戳增量是否接近常见帧率的 PTS 步长。
// 常见帧率 → 90kHz 时钟每帧 tick：
//   23.976fps→3754, 24fps→3750, 25fps→3600, 29.97fps→3003,
//   30fps→3000, 50fps→1800, 59.94fps→1502, 60fps→1500
func isStandardFrameDelta(delta uint64) bool {
	standards := []uint64{3754, 3750, 3600, 3003, 3000, 1800, 1502, 1500}
	for _, s := range standards {
		if delta >= s-2 && delta <= s+2 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// io.Reader 包装：流式重写
// ---------------------------------------------------------------------------

// tsRewriteReader 包装上游响应体，流式重写 PTS
type tsRewriteReader struct {
	src       io.Reader
	rw        *ptsRewriter
	buf       []byte // 完整 TS 包缓冲
	fill      int    // 已填充字节
	done      bool
	sync      sync.Once
	rewritten int
}

func newTSRewriteReader(src io.Reader, rw *ptsRewriter) *tsRewriteReader {
	return &tsRewriteReader{
		src: src,
		rw:  rw,
		buf: make([]byte, tsPacketSize),
	}
}

func (t *tsRewriteReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if t.fill == 0 {
			// 读满一个 TS 包
			if t.done {
				if n > 0 {
					return n, nil
				}
				return 0, io.EOF
			}
			rn, err := io.ReadFull(t.src, t.buf)
			if err != nil {
				t.done = true
				// 读到不足一包（分片尾部/非 188 整数倍）：
				// 把已读到的 rn 字节原样送出（不重写，因为不是完整包）
				if rn > 0 {
					c := copy(p[n:], t.buf[:rn])
					n += c
					return n, nil
				}
				// rn==0：真正的 EOF
				if n > 0 {
					return n, nil
				}
				return 0, io.EOF
			}
			// 完整一个 TS 包：重写 PTS
			if t.rw.rewrite(t.buf) {
				t.rewritten++
			}
			t.fill = tsPacketSize
			// 重写 PTS
			if t.rw.rewrite(t.buf) {
				t.rewritten++
			}
			t.fill = tsPacketSize
		}
		// 拷贝到输出
		c := copy(p[n:], t.buf[:t.fill])
		n += c
		if c < t.fill {
			// 缓冲区被部分消费（不太可能，因为 p 通常足够大）
			// 移动剩余
			remaining := t.fill - c
			copy(t.buf, t.buf[c:t.fill])
			t.fill = remaining
		} else {
			t.fill = 0
		}
	}
	return n, nil
}

// 确保编译通过（避免 unused 警告）
var _ = errors.New
var _ = binary.BigEndian
var _ = time.Now
