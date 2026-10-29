package ssdp

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Advertiser 把自己伪装成一台 DLNA MediaRenderer，让 B站 App 能搜到我。
//
// 关键点：
//   - 必须响应 M-SEARCH（App 是主动搜的）
//   - 必须周期发 NOTIFY ssdp:alive（有些 App 只认 alive 广播）
//   - USN/UUID 要带 B站私有格式，才能让 B站 认为是"高能力接收端"
type Advertiser struct {
	UUID       string // 设备 UDN，形如 uuid:XXXX...
	Name       string // friendlyName
	Location   string // 设备描述 XML 的绝对地址
	ExtraTypes []string

	mu      sync.Mutex
	conn    *net.UDPConn
	stopped chan struct{}
	once    sync.Once
}

// NewAdvertiser 造一个广播器
func NewAdvertiser(uuid, name, location string, extraTypes ...string) *Advertiser {
	return &Advertiser{
		UUID:       uuid,
		Name:       name,
		Location:   location,
		ExtraTypes: extraTypes,
		stopped:    make(chan struct{}),
	}
}

// Start 开始监听并广播
func (a *Advertiser) Start() error {
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		// 退路：只监听单播端口（部分环境禁多播监听）
		log.Printf("[ssdp] multicast listen failed (%v), fallback unicast :1900", err)
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 1900})
		if err != nil {
			return err
		}
	}
	_ = conn.SetReadBuffer(1 << 20)
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	go a.serve()
	go a.notifyLoop()
	return nil
}

// Stop 停止并发 byebye
func (a *Advertiser) Stop() {
	a.once.Do(func() {
		close(a.stopped)
		a.mu.Lock()
		conn := a.conn
		a.mu.Unlock()
		if conn != nil {
			a.sendByebye()
			conn.Close()
		}
	})
}

// serve 处理进来的 M-SEARCH
func (a *Advertiser) serve() {
	buf := make([]byte, 8192)
	for {
		select {
		case <-a.stopped:
			return
		default:
		}
		a.mu.Lock()
		conn := a.conn
		a.mu.Unlock()
		if conn == nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		req := string(buf[:n])
		if !strings.HasPrefix(req, "M-SEARCH") {
			continue
		}
		st := headerValue(req, "ST")
		if st == "" || !a.matchesST(st) {
			continue
		}
		a.replySearch(src, st)
	}
}

func (a *Advertiser) matchesST(st string) bool {
	types := append([]string{
		"upnp:rootdevice",
		"urn:schemas-upnp-org:device:MediaRenderer:1",
		"urn:schemas-upnp-org:service:AVTransport:1",
		"urn:schemas-upnp-org:service:RenderingControl:1",
	}, a.ExtraTypes...)
	for _, t := range types {
		if st == t {
			return true
		}
	}
	return false
}

// replySearch 回 M-SEARCH 的 200 OK
func (a *Advertiser) replySearch(src *net.UDPAddr, st string) {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	usn := a.usnFor(st)
	body := fmt.Sprintf("HTTP/1.1 200 OK\r\n"+
		"CACHE-CONTROL: max-age=%d\r\n"+
		"EXT:\r\n"+
		"LOCATION: %s\r\n"+
		"SERVER: Linux/5.0 UPnP/1.0 hcrelay/0.1\r\n"+
		"ST: %s\r\n"+
		"USN: %s\r\n"+
		"BOOTID.UPNP.ORG: 1\r\n"+
		"CONFIGID.UPNP.ORG: 1\r\n\r\n", maxAge, a.Location, st, usn)
	_, _ = conn.WriteToUDP([]byte(body), src)
}

// notifyLoop 周期发 ssdp:alive
func (a *Advertiser) notifyLoop() {
	a.sendAlive()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.stopped:
			return
		case <-t.C:
			a.sendAlive()
		}
	}
}

func (a *Advertiser) sendAlive() {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	dst := &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}
	types := append([]string{
		"upnp:rootdevice",
		"urn:schemas-upnp-org:device:MediaRenderer:1",
	}, a.ExtraTypes...)
	for _, st := range types {
		body := fmt.Sprintf("NOTIFY * HTTP/1.1\r\n"+
			"HOST: %s\r\n"+
			"CACHE-CONTROL: max-age=%d\r\n"+
			"LOCATION: %s\r\n"+
			"NT: %s\r\n"+
			"NTS: ssdp:alive\r\n"+
			"SERVER: Linux/5.0 UPnP/1.0 hcrelay/0.1\r\n"+
			"USN: %s\r\n\r\n", multicastAddr, maxAge, a.Location, st, a.usnFor(st))
		_, _ = conn.WriteToUDP([]byte(body), dst)
	}
}

func (a *Advertiser) sendByebye() {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	dst := &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}
	for _, st := range []string{"upnp:rootdevice", "urn:schemas-upnp-org:device:MediaRenderer:1"} {
		body := fmt.Sprintf("NOTIFY * HTTP/1.1\r\n"+
			"HOST: %s\r\n"+
			"NT: %s\r\n"+
			"NTS: ssdp:byebye\r\n"+
			"USN: %s\r\n\r\n", multicastAddr, st, a.usnFor(st))
		_, _ = conn.WriteToUDP([]byte(body), dst)
	}
}

func (a *Advertiser) usnFor(st string) string {
	switch st {
	case "upnp:rootdevice":
		return a.UUID + "::upnp:rootdevice"
	case "urn:schemas-upnp-org:device:MediaRenderer:1":
		return a.UUID + "::urn:schemas-upnp-org:device:MediaRenderer:1"
	case "urn:schemas-upnp-org:service:AVTransport:1":
		return a.UUID + "::urn:schemas-upnp-org:service:AVTransport:1"
	case "urn:schemas-upnp-org:service:RenderingControl:1":
		return a.UUID + "::urn:schemas-upnp-org:service:RenderingControl:1"
	default:
		return a.UUID + "::" + st
	}
}

func headerValue(raw, key string) string {
	for _, line := range strings.Split(raw, "\r\n") {
		if i := strings.Index(line, ":"); i > 0 {
			if strings.EqualFold(strings.TrimSpace(line[:i]), key) {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}
