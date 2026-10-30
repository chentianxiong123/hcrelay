// Package nva 实现 B站私有投屏协议"哔哩必连"（NVA/Nirvana）。
// 参考 xfangfang《分析Bilibili客户端的"哔哩必连"协议》：
//   - TCP 长连接，客户端发 "SETUP /projection NVA/1.0" 升级握手
//   - 之后是二进制帧：0xe0=指令 / 0xc0=应答 / 0xe4=ping(服务端每秒)
//   - Play 指令带 {aid,cid,current_qn,desire_qn,access_key,...}
package nva

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	opCommand = 0xe0
	opReply   = 0xc0
	opPing    = 0xe4
)

// Command B站客户端下发的指令
type Command struct {
	Name string
	Args map[string]any
}

// Reply 服务端回执
type Reply struct {
	Seq  uint32
	Args map[string]any
}

// Server NVA 协议服务端（我当接收端）
type Server struct {
	mu       sync.Mutex
	conns    map[uint64]*conn
	nextID   uint64
	onCmd    func(cmd Command) (map[string]any, error) // 返回应答 json
	onClose  func()
	started  time.Time
}

type conn struct {
	id      uint64
	nc      net.Conn
	seq     uint32
	mu      sync.Mutex
	closed  chan struct{}
}

// NewServer 创建
func NewServer(onCmd func(cmd Command) (map[string]any, error)) *Server {
	return &Server{
		conns:  map[uint64]*conn{},
		onCmd:  onCmd,
		started: time.Now(),
	}
}

// Serve 在 l 上 accept（阻塞，放 goroutine 跑）
func (s *Server) Serve(l net.Listener) {
	for {
		nc, err := l.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.nextID++
		c := &conn{id: s.nextID, nc: nc, closed: make(chan struct{})}
		s.conns[c.id] = c
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *Server) handle(c *conn) {
	defer func() {
		c.nc.Close()
		s.mu.Lock()
		delete(s.conns, c.id)
		s.mu.Unlock()
		close(c.closed)
		if s.onClose != nil {
			s.onClose()
		}
	}()

	br := bufio.NewReader(c.nc)
	// 握手：SETUP /projection NVA/1.0
	req, err := readHandshake(br)
	if err != nil {
		return
	}
	log.Printf("[nva] connect %s: %s %s", c.nc.RemoteAddr(), req.Method, req.URI)
	if err := writeHandshake(c.nc, req.Session, c.id); err != nil {
		return
	}

	// 进入帧循环
	go s.pingLoop(c)
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		_ = c.nc.SetReadDeadline(time.Now().Add(90 * time.Second))
		frame, err := readFrame(br)
		if err != nil {
			return
		}
		if frame.Op == opCommand {
			cmd := decodeCommand(frame)
			resp, err := s.onCmd(cmd)
			_ = err
			c.sendReply(frame.Seq, resp)
		}
		// 其他帧忽略
	}
}

func (s *Server) pingLoop(c *conn) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
			c.mu.Lock()
			c.seq++
			seq := c.seq
			c.mu.Unlock()
			writeFrame(c.nc, opPing, seq, nil)
		}
	}
}

func (c *conn) sendReply(seq uint32, args map[string]any) {
	payload, _ := json.Marshal(args)
	c.mu.Lock()
	defer c.mu.Unlock()
	var b bytes.Buffer
	b.WriteByte(opReply)
	b.WriteByte(1)
	binary.Write(&b, binary.BigEndian, seq)
	binary.Write(&b, binary.BigEndian, uint32(len(payload)))
	b.Write(payload)
	writeRaw(c.nc, b.Bytes())
}

// ---- 帧编解码 ----

type frame struct {
	Op   byte
	Seq  uint32
	Body []byte
}

func readFrame(br *bufio.Reader) (*frame, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(br, lenBuf); err != nil {
		return nil, err
	}
	flen := binary.BigEndian.Uint32(lenBuf)
	body := make([]byte, flen)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	f := &frame{}
	switch body[0] {
	case opCommand:
		f.Op = opCommand
		// e0 argcount seq(4) 0x01 ...（指令长度前缀 1 字节）
		// 简化：直接解析出 seq
		if len(body) >= 6 {
			f.Seq = binary.BigEndian.Uint32(body[2:6])
		}
		f.Body = body
	case opReply, opPing:
		if len(body) >= 5 {
			f.Op = body[0]
			f.Seq = binary.BigEndian.Uint32(body[1:5])
		} else {
			f.Op = body[0]
		}
		f.Body = body
	default:
		f.Op = body[0]
		f.Body = body
	}
	return f, nil
}

// decodeCommand 解析 0xe0 指令帧 → Command
func decodeCommand(f *frame) Command {
	body := f.Body
	if len(body) < 2 {
		return Command{}
	}
	argCount := int(body[1])
	pos := 7 // e0 argcount seq(4) 0x01(version) → 参数从字节 7 开始
	var name string
	idx := 0
	for idx < argCount && pos < len(body) {
		l := int(body[pos])
		pos++
		if pos+l > len(body) {
			break
		}
		val := string(body[pos : pos+l])
		pos += l
		if idx == 1 {
			name = val
		}
		idx++
		if idx >= 2 {
			// 第三个参数是 json
			if pos+4 <= len(body) {
				jlen := int(binary.BigEndian.Uint32(body[pos : pos+4]))
				pos += 4
				if pos+jlen <= len(body) {
					var args map[string]any
					if json.Unmarshal(body[pos:pos+jlen], &args) == nil {
						return Command{Name: name, Args: args}
					}
				}
			}
			break
		}
	}
	return Command{Name: name}
}

// ---- 握手 ----

type handshakeReq struct {
	Method  string
	URI     string
	Session string
	UUID    string
}

func readHandshake(br *bufio.Reader) (*handshakeReq, error) {
	var req handshakeReq
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) >= 2 {
		req.Method = parts[0]
		req.URI = parts[1]
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if i := strings.Index(line, ":"); i > 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.TrimSpace(line[i+1:])
			switch k {
			case "Session":
				req.Session = v
			case "UUID":
				req.UUID = v
			}
		}
	}
	return &req, nil
}

func writeHandshake(nc net.Conn, session string, id uint64) error {
	body := "NVA/1.0 200 OK\r\n" +
		"NvaVersion: 1\r\n" +
		"Session: " + session + "\r\n" +
		"Connection: Keep-Alive\r\n" +
		"UUID: XY0000000000000000000000000000000001\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123) + "\r\n" +
		"Content-Length: 0\r\n" +
		"Server: Linux/5.0, UPnP/1.0, mycast/0.1\r\n\r\n"
	_, err := nc.Write([]byte(body))
	return err
}

func writeFrame(nc net.Conn, op byte, seq uint32, payload []byte) {
	var b bytes.Buffer
	body := []byte{op, 0}
	binary.Write(&b, binary.BigEndian, seq)
	if len(payload) > 0 {
		body[1] = 1
	}
	b.Write(body)
	if len(payload) > 0 {
		binary.Write(&b, binary.BigEndian, uint32(len(payload)))
		b.Write(payload)
	}
	writeRaw(nc, b.Bytes())
}

func writeRaw(nc net.Conn, data []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	nc.SetWriteDeadline(time.Now().Add(5 * time.Second))
	nc.Write(lenBuf[:])
	nc.Write(data)
}
