// Package tv 中间件当"发送端"：用 goupnp 控制电视（DLNA AVTransport）。
package tv

import (
	"fmt"
	"net/url"
	"sync"

	"github.com/huin/goupnp/dcps/av1"

	"hcrelay/internal/ssdp"
	"hcrelay/internal/upnp"
)

// Controller 电视控制句柄
type Controller struct {
	mu  sync.Mutex
	dev *ssdp.Device
	av  *av1.AVTransport1
	rc  *av1.RenderingControl1
}

// NewController 根据发现的设备创建控制句柄
func NewController(dev *ssdp.Device) (*Controller, error) {
	u, err := url.Parse(dev.Location)
	if err != nil {
		return nil, err
	}
	avs, err := av1.NewAVTransport1ClientsByURL(u)
	if err != nil || len(avs) == 0 {
		return nil, fmt.Errorf("no AVTransport service on %s: %w", dev.Name, err)
	}
	c := &Controller{dev: dev, av: avs[0]}
	if rcs, err := av1.NewRenderingControl1ClientsByURL(u); err == nil && len(rcs) > 0 {
		c.rc = rcs[0]
	}
	return c, nil
}

// Push 设置播放地址并开始播放
func (c *Controller) Push(uri, title, contentType string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.av.SetAVTransportURI(0, uri, upnp.DIDLLite(uri, title, contentType)); err != nil {
		return err
	}
	return c.av.Play(0, "1")
}

// Control 播放控制
func (c *Controller) Control(action, target string, volume int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch action {
	case "play":
		return c.av.Play(0, "1")
	case "pause":
		return c.av.Pause(0)
	case "stop":
		return c.av.Stop(0)
	case "seek":
		return c.av.Seek(0, "REL_TIME", target)
	case "volume":
		if c.rc == nil {
			return fmt.Errorf("device has no RenderingControl")
		}
		return c.rc.SetVolume(0, "Master", uint16(volume))
	}
	return fmt.Errorf("unknown action: %s", action)
}

// Status 查播放状态
func (c *Controller) Status() map[string]string {
	out := map[string]string{"device": c.dev.Name}
	state, status, _, err := c.av.GetTransportInfo(0)
	if err == nil {
		out["state"] = state
		out["status"] = status
	}
	if _, dur, _, uri, rel, _, _, _, err := c.av.GetPositionInfo(0); err == nil {
		out["duration"] = dur
		out["uri"] = uri
		out["position"] = rel
	}
	if c.rc != nil {
		if v, err := c.rc.GetVolume(0, "Master"); err == nil {
			out["volume"] = fmt.Sprint(v)
		}
	}
	return out
}
