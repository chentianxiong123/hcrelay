// Package ssdp 极简 SSDP：既能当接收端广播（让 B站 App 找到我），
// 又能搜索局域网里的电视（我当发送端）。
// 纯标准库实现，手机/盒子/树莓派都能跑。
package ssdp

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	multicastAddr = "239.255.255.250:1900"
	maxAge        = 1800
)

// Device 局域网里发现的一台设备
type Device struct {
	UDN        string `json:"udn"`
	Name       string `json:"name"`
	Location   string `json:"location"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	DeviceType string `json:"device_type"`
	Server     string `json:"server"`
}

// SearchTarget 搜索时的目标服务类型
type SearchTarget string

const (
	MediaRenderer SearchTarget = "urn:schemas-upnp-org:device:MediaRenderer:1"
	RootDevice    SearchTarget = "upnp:rootdevice"
	AVTransport   SearchTarget = "urn:schemas-upnp-org:service:AVTransport:1"
)

// Search 搜局域网设备（多播 M-SEARCH + 单播补一轮，覆盖路由屏蔽多播的路由器）
func Search(ctx context.Context, timeout time.Duration, targets ...SearchTarget) []*Device {
	if len(targets) == 0 {
		targets = []SearchTarget{MediaRenderer}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	seen := map[string]*Device{}
	var mu sync.Mutex
	add := func(d *Device) {
		if d == nil || d.Location == "" {
			return
		}
		mu.Lock()
		if _, ok := seen[d.UDN]; !ok {
			seen[d.UDN] = d
		}
		mu.Unlock()
	}

	// 多播搜索
	conn, err := net.ListenPacket("udp4", ":0")
	if err == nil {
		defer conn.Close()
		dst := &net.UDPAddr{IP: net.ParseIP("239.255.255.250"), Port: 1900}
		for _, t := range targets {
			msg := fmt.Sprintf("M-SEARCH * HTTP/1.1\r\n"+
				"HOST: %s\r\n"+
				"MAN: \"ssdp:discover\"\r\n"+
				"MX: 2\r\n"+
				"ST: %s\r\n\r\n", multicastAddr, t)
			_, _ = conn.WriteTo([]byte(msg), dst)
		}
		go func() {
			buf := make([]byte, 4096)
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				n, _, err := conn.ReadFrom(buf)
				if err != nil {
					continue
				}
				if d := parseSearchReply(string(buf[:n])); d != nil {
					add(d)
				}
			}
		}()
	}

	// 单播补搜：多播被路由挡掉时，把 M-SEARCH 直接发给候选 IP
	for _, ip := range localIPv4s() {
		for _, t := range targets {
			msg := fmt.Sprintf("M-SEARCH * HTTP/1.1\r\n"+
				"HOST: %s\r\n"+
				"MAN: \"ssdp:discover\"\r\n"+
				"MX: 1\r\n"+
				"ST: %s\r\n\r\n", multicastAddr, t)
			c, err := net.DialTimeout("udp4", net.JoinHostPort(ip, "1900"), 400*time.Millisecond)
			if err != nil {
				continue
			}
			_, _ = c.Write([]byte(msg))
			_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			buf := make([]byte, 4096)
			if n, err := c.Read(buf); err == nil {
				if d := parseSearchReply(string(buf[:n])); d != nil {
					add(d)
				}
			}
			c.Close()
		}
	}

	<-ctx.Done()

	out := make([]*Device, 0, len(seen))
	for _, d := range seen {
		out = append(out, d)
	}
	// 补 friendlyName
	for _, d := range out {
		if n := fetchFriendlyName(d.Location); n != "" {
			d.Name = n
		}
		if d.Name == "" {
			d.Name = "Unknown"
		}
	}
	return out
}

func parseSearchReply(raw string) *Device {
	if !strings.HasPrefix(raw, "HTTP/1.1 200") {
		return nil
	}
	h := http.Header{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Scan() // status line
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if i := strings.Index(line, ":"); i > 0 {
			h.Set(strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]))
		}
	}
	loc := h.Get("LOCATION")
	usn := h.Get("USN")
	if loc == "" || usn == "" {
		return nil
	}
	udn := usn
	if i := strings.Index(udn, "::"); i > 0 {
		udn = udn[:i]
	}
	ip, port := parseHostPort(loc)
	return &Device{
		UDN:        strings.TrimSpace(udn),
		Location:   loc,
		IP:         ip,
		Port:       port,
		DeviceType: h.Get("ST"),
		Server:     h.Get("SERVER"),
	}
}

func parseHostPort(location string) (string, int) {
	u := strings.TrimPrefix(location, "http://")
	if i := strings.Index(u, "/"); i > 0 {
		u = u[:i]
	}
	host, portStr, err := net.SplitHostPort(u)
	if err != nil {
		return u, 80
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	if port == 0 {
		port = 80
	}
	return host, port
}

type rootDeviceXML struct {
	XMLName     struct{} `xml:"root"`
	FriendlyName string  `xml:"device>friendlyName"`
	Manufacturer string  `xml:"device>manufacturer"`
	ModelName    string  `xml:"device>modelName"`
}

var descClient = &http.Client{Timeout: 3 * time.Second}

// fetchFriendlyName 拉设备描述 XML 取名字
func fetchFriendlyName(location string) string {
	if location == "" {
		return ""
	}
	resp, err := descClient.Get(location)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var rd rootDeviceXML
	if err := xmlDecode(resp.Body, &rd); err != nil {
		return ""
	}
	return strings.TrimSpace(rd.FriendlyName)
}

// localIPv4s 本机所有 IPv4 地址（用来单播补搜）
func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil {
					out = append(out, v4.String())
				}
			}
		}
	}
	return out
}

// LocalIP 取本机第一个非回环 IPv4（拼电视可访问的 URL 用）
func LocalIP() string {
	ips := localIPv4s()
	if len(ips) == 0 {
		return "127.0.0.1"
	}
	return ips[0]
}
