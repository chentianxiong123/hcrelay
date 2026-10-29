// Package renderer 让中间件伪装成一台 DLNA MediaRenderer（接收端）。
// B站 App 会 SSDP 搜到我 → 读 /description.xml → 把播放 URL/命令发到我这里。
package renderer

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"hcrelay/internal/upnp"
)

// UPnP 服务命名空间（SOAP 响应的 xmlns:u 必须与 SCPD 一致）
const (
	nsAVTransport      = "urn:schemas-upnp-org:service:AVTransport:1"
	nsRenderingControl = "urn:schemas-upnp-org:service:RenderingControl:1"
	nsConnectionManager = "urn:schemas-upnp-org:service:ConnectionManager:1"
)

// Event 渲染端发生的事件（供 relay 订阅）
type Event struct {
	Type string // "uri" | "play" | "pause" | "stop" | "seek" | "volume"
	URI  string
	Meta string
	Pos  string
	Vol  int
	// Referer 是发起投屏那个请求的 Referer 头。
	//
	// 关键：App/浏览器在 SetAVTransportURI 时会把自己的 Referer 带上，
	// 那正是用户正在看的网站域名（比如盗版站）。而很多盗版站的 CDN
	// 会校验 Referer 是否属于自己的域名 —— 猜是猜不出来的，只能透传。
	Referer string
	// UA 是发起投屏那个请求的 User-Agent，同样透传给上游更安全。
	UA string
}

// Renderer 接收端
type Renderer struct {
	HostPort string      // 对外 IP:port（B站 App 访问我的 URL 用）
	UUID     string      // uuid:XXXX...
	Name     string      // friendlyName
	Services Services    // 可配置的服务
	mu       sync.Mutex
	state    state
	onEvent  func(Event)
	// 供 relay 设置的状态
	SetEventFn func(func(Event))
}

type state struct {
	URI      string
	Meta     string
	State    string // STOPPED/PLAYING/PAUSED/TRANSITIONING
	Position string
	Duration string
	Volume   int
	Muted    bool
}

// Services 渲染端实现的服务（B站 App 需要的都在）
type Services struct {
	AVTransport    bool
	RenderingCtrl  bool
	ConnMgr        bool
	BiliNirvana    bool // B站私有 NirvanaControl 服务，用于"极速投屏"/高清
}

// New 创建渲染端
func New(hostPort, uuid, name string, svc Services) *Renderer {
	r := &Renderer{
		HostPort: hostPort,
		UUID:     uuid,
		Name:     name,
		Services: svc,
		state: state{
			State:  "STOPPED",
			Volume: 50,
		},
	}
	r.SetEventFn = func(fn func(Event)) { r.onEvent = fn }
	return r
}

// Handler 返回 HTTP handler（挂到 http.Server）
func (r *Renderer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/description.xml", r.handleDescription)
	mux.HandleFunc("/scpd/AVTransport.xml", r.handleAVTransportSCPD)
	mux.HandleFunc("/scpd/RenderingControl.xml", r.handleRenderingSCPD)
	mux.HandleFunc("/scpd/ConnectionManager.xml", r.handleConnMgrSCPD)
	if r.Services.BiliNirvana {
		mux.HandleFunc("/scpd/NirvanaControl.xml", r.handleNirvanaSCPD)
	}
	mux.HandleFunc("/ctl/AVTransport", r.handleAVTransport)
	mux.HandleFunc("/ctl/RenderingControl", r.handleRendering)
	mux.HandleFunc("/ctl/ConnectionManager", r.handleConnMgr)
	if r.Services.BiliNirvana {
		mux.HandleFunc("/ctl/NirvanaControl", r.handleNirvana)
	}
	mux.HandleFunc("/event/", r.handleEvent)
	return mux
}

// handleEvent GENA 事件订阅（简化实现：接受订阅并立即回一个初始 LastChange）
func (r *Renderer) handleEvent(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case "SUBSCRIBE":
		sid := fmt.Sprintf("uuid:%x", time.Now().UnixNano())
		w.Header().Set("SID", sid)
		w.Header().Set("TIMEOUT", "Second-1800")
		w.Header().Set("SERVER", "Linux/5.0 UPnP/1.0 hcrelay/0.1")
		w.WriteHeader(http.StatusOK)
	case "UNSUBSCRIBE":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (r *Renderer) emit(e Event) {
	if r.onEvent != nil {
		r.onEvent(e)
	}
}

// ---- 设备描述 ----

func (r *Renderer) handleDescription(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>HomeCast</manufacturer>
    <manufacturerURL>https://homecast.local</manufacturerURL>
    <modelDescription>Bilibili DLNA relay renderer</modelDescription>
    <modelName>HomeCast Relay</modelName>
    <modelNumber>1.0</modelNumber>
    <modelURL>https://homecast.local</modelURL>
    <serialNumber>%s</serialNumber>
    <UDN>%s</UDN>
    <dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMR-1.50</dlna:X_DLNADOC>
    <dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">playcontainer-1-0</dlna:X_DLNADOC>
    <dlna:X_DLNACAP xmlns:dlna="urn:schemas-dlna-org:device-1-0"></dlna:X_DLNACAP>
    <serviceList>
%s
    </serviceList>
    <presentationURL>/ui</presentationURL>
  </device>
</root>`, r.Name, r.UUID, r.UUID, r.serviceList())
}

func (r *Renderer) serviceList() string {
	var b strings.Builder
	if r.Services.AVTransport {
		b.WriteString(`      <service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><serviceId>urn:upnp-org:serviceId:AVTransport</serviceId><SCPDURL>/scpd/AVTransport.xml</SCPDURL><controlURL>/ctl/AVTransport</controlURL><eventSubURL>/event/AVTransport</eventSubURL></service>`)
	}
	if r.Services.RenderingCtrl {
		b.WriteString(`      <service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType><serviceId>urn:upnp-org:serviceId:RenderingControl</serviceId><SCPDURL>/scpd/RenderingControl.xml</SCPDURL><controlURL>/ctl/RenderingControl</controlURL><eventSubURL>/event/RenderingControl</eventSubURL></service>`)
	}
	if r.Services.ConnMgr {
		b.WriteString(`      <service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType><serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId><SCPDURL>/scpd/ConnectionManager.xml</SCPDURL><controlURL>/ctl/ConnectionManager</controlURL><eventSubURL>/event/ConnectionManager</eventSubURL></service>`)
	}
	if r.Services.BiliNirvana {
		b.WriteString(`      <service><serviceType>urn:schemas-upnp-org:service:NirvanaControl:3</serviceType><serviceId>urn:upnp-org:serviceId:NirvanaControl</serviceId><SCPDURL>/scpd/NirvanaControl.xml</SCPDURL><controlURL>/ctl/NirvanaControl</controlURL><eventSubURL>/event/NirvanaControl</eventSubURL></service>`)
	}
	return b.String()
}

// ---- 控制端点 ----

func (r *Renderer) handleAVTransport(w http.ResponseWriter, req *http.Request) {
	soap, err := upnp.ParseSoap(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch soap.Action {
	case "SetAVTransportURI":
		r.mu.Lock()
		r.state.URI = soap.Args["CurrentURI"]
		r.state.Meta = soap.Args["CurrentURIMetaData"]
		r.state.State = "TRANSITIONING"
		r.mu.Unlock()
		// 透传发起方的 Referer / UA：盗版站等不知名站点的 CDN
		// 会校验 Referer 是否为自己的域名，猜不出来只能原样带过去。
		r.emit(Event{
			Type:     "uri",
			URI:      soap.Args["CurrentURI"],
			Meta:     soap.Args["CurrentURIMetaData"],
			Referer:  req.Header.Get("Referer"),
			UA:       req.Header.Get("User-Agent"),
		})
		r.soapOK(w, soap.Action, map[string]string{})
	case "SetNextAVTransportURI":
		r.soapOK(w, soap.Action, map[string]string{})
	case "Play":
		r.mu.Lock()
		r.state.State = "PLAYING"
		r.mu.Unlock()
		r.emit(Event{Type: "play"})
		r.soapOK(w, soap.Action, map[string]string{})
	case "Pause":
		r.mu.Lock()
		r.state.State = "PAUSED"
		r.mu.Unlock()
		r.emit(Event{Type: "pause"})
		r.soapOK(w, soap.Action, map[string]string{})
	case "Stop":
		r.mu.Lock()
		r.state.State = "STOPPED"
		r.mu.Unlock()
		r.emit(Event{Type: "stop"})
		r.soapOK(w, soap.Action, map[string]string{})
	case "Seek":
		target := soap.Args["Target"]
		r.mu.Lock()
		r.state.Position = target
		r.mu.Unlock()
		r.emit(Event{Type: "seek", Pos: target})
		r.soapOK(w, soap.Action, map[string]string{})
	case "GetTransportInfo":
		r.mu.Lock()
		st := r.state
		r.mu.Unlock()
		r.soapOK(w, soap.Action, map[string]string{
			"CurrentTransportState":    st.State,
			"CurrentTransportStatus":   "OK",
			"CurrentSpeed":             "1",
		})
	case "GetMediaInfo":
		r.mu.Lock()
		st := r.state
		r.mu.Unlock()
		r.soapOK(w, soap.Action, map[string]string{
			"NrTracks":        "1",
			"MediaDuration":   st.Duration,
			"CurrentURI":      st.URI,
			"CurrentURIMetaData": st.Meta,
			"NextURI":         "",
			"NextURIMetaData": "",
			"PlayMedium":      "NONE",
			"RecordMedium":    "NONE",
			"WriteStatus":     "NOT_IMPLEMENTED",
		})
	case "Next", "Previous":
		r.soapOK(w, soap.Action, map[string]string{})
	case "GetPositionInfo":
		r.mu.Lock()
		st := r.state
		r.mu.Unlock()
		r.soapOK(w, soap.Action, map[string]string{
			"Track":            "0",
			"TrackDuration":    st.Duration,
			"TrackMetaData":    st.Meta,
			"TrackURI":         st.URI,
			"RelTime":          st.Position,
			"AbsTime":          st.Position,
			"RelCount":         "0",
			"AbsCount":         "0",
		})
	case "GetTransportSettings":
		r.soapOK(w, soap.Action, map[string]string{"PlayMode": "NORMAL", "RecQualityMode": "1"})
	case "GetDeviceCapabilities":
		r.soapOK(w, soap.Action, map[string]string{
			"PlayMedia":        "NONE",
			"RecMedia":         "NONE",
			"RecQualityModes":  "1",
		})
	case "GetCurrentTransportActions":
		r.soapOK(w, soap.Action, map[string]string{"Actions": "Play,Pause,Stop,Seek"})
	default:
		log.Printf("[renderer] unknown AVTransport action: %q", soap.Action)
		r.soapOK(w, soap.Action, map[string]string{})
	}
}

func (r *Renderer) handleRendering(w http.ResponseWriter, req *http.Request) {
	soap, err := upnp.ParseSoap(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch soap.Action {
	case "GetVolume":
		r.mu.Lock()
		v := r.state.Volume
		r.mu.Unlock()
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{"CurrentVolume": fmt.Sprint(v)})
	case "SetVolume":
		var v int
		fmt.Sscanf(soap.Args["DesiredVolume"], "%d", &v)
		r.mu.Lock()
		r.state.Volume = v
		r.mu.Unlock()
		r.emit(Event{Type: "volume", Vol: v})
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{})
	case "GetMute":
		r.mu.Lock()
		m := r.state.Muted
		r.mu.Unlock()
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{"CurrentMute": boolStr(m)})
	case "SetMute":
		r.mu.Lock()
		r.state.Muted = strings.EqualFold(soap.Args["DesiredMute"], "1")
		r.mu.Unlock()
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{})
	case "ListPresets":
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{"CurrentPresetNameList": ""})
	case "SelectPreset":
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{})
	default:
		r.soapOKNS(w, nsRenderingControl, soap.Action, map[string]string{})
	}
}

func (r *Renderer) handleConnMgr(w http.ResponseWriter, req *http.Request) {
	soap, _ := upnp.ParseSoap(req)
	switch soap.Action {
	case "GetProtocolInfo":
		r.soapOKNS(w, nsConnectionManager, soap.Action, map[string]string{
			"Source":  "http-get:*:*:*",
			"Sink":    "",
		})
	case "GetCurrentConnectionIDs":
		r.soapOKNS(w, nsConnectionManager, soap.Action, map[string]string{"ConnectionIDs": "0"})
	case "GetCurrentConnectionInfo":
		r.soapOKNS(w, nsConnectionManager, soap.Action, map[string]string{
			"RcsID":       "0",
			"AVTransportID": "0",
			"ProtocolInfo":  "http-get:*:*:*",
			"PeerConnectionManager": "/",
			"PeerConnectionID":      "-1",
			"Direction":             "Input",
			"Status":                "OK",
		})
	default:
		r.soapOKNS(w, nsConnectionManager, soap.Action, map[string]string{})
	}
}

// handleNirvana SOAP 兼容（早期 NVA 用 XML 描述），现在几乎不用
func (r *Renderer) handleNirvana(w http.ResponseWriter, req *http.Request) {
	soap, _ := upnp.ParseSoap(req)
	r.soapOK(w, soap.Action, map[string]string{})
}

func (r *Renderer) soapOK(w http.ResponseWriter, action upnp.SoapAction, args map[string]string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(upnp.SoapResponse(action, args)))
}

// soapOKNS 按服务类型返回 SOAP 应答（命名空间必须与 SCPD 一致）
func (r *Renderer) soapOKNS(w http.ResponseWriter, ns string, action upnp.SoapAction, args map[string]string) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(upnp.SoapResponseNS(ns, action, args)))
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---- SCPD ----

func (r *Renderer) handleAVTransportSCPD(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(scpdAVTransport))
}

func (r *Renderer) handleRenderingSCPD(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(scpdRendering))
}

func (r *Renderer) handleConnMgrSCPD(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(scpdConnMgr))
}

func (r *Renderer) handleNirvanaSCPD(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Write([]byte(scpdNirvana))
}

const scpdAVTransport = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
  <action><name>SetAVTransportURI</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>CurrentURI</name><direction>in</direction><relatedStateVariable>AVTransportURI</relatedStateVariable></argument>
    <argument><name>CurrentURIMetaData</name><direction>in</direction><relatedStateVariable>AVTransportURIMetaData</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>SetNextAVTransportURI</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>NextURI</name><direction>in</direction><relatedStateVariable>NextAVTransportURI</relatedStateVariable></argument>
    <argument><name>NextURIMetaData</name><direction>in</direction><relatedStateVariable>NextAVTransportURIMetaData</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetMediaInfo</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>NrTracks</name><direction>out</direction><relatedStateVariable>NumberOfTracks</relatedStateVariable></argument>
    <argument><name>MediaDuration</name><direction>out</direction><relatedStateVariable>CurrentMediaDuration</relatedStateVariable></argument>
    <argument><name>CurrentURI</name><direction>out</direction><relatedStateVariable>AVTransportURI</relatedStateVariable></argument>
    <argument><name>CurrentURIMetaData</name><direction>out</direction><relatedStateVariable>AVTransportURIMetaData</relatedStateVariable></argument>
    <argument><name>NextURI</name><direction>out</direction><relatedStateVariable>NextAVTransportURI</relatedStateVariable></argument>
    <argument><name>NextURIMetaData</name><direction>out</direction><relatedStateVariable>NextAVTransportURIMetaData</relatedStateVariable></argument>
    <argument><name>PlayMedium</name><direction>out</direction><relatedStateVariable>PlaybackStorageMedium</relatedStateVariable></argument>
    <argument><name>RecordMedium</name><direction>out</direction><relatedStateVariable>RecordStorageMedium</relatedStateVariable></argument>
    <argument><name>WriteStatus</name><direction>out</direction><relatedStateVariable>RecordMediumWriteStatus</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetTransportInfo</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>CurrentTransportState</name><direction>out</direction><relatedStateVariable>TransportState</relatedStateVariable></argument>
    <argument><name>CurrentTransportStatus</name><direction>out</direction><relatedStateVariable>TransportStatus</relatedStateVariable></argument>
    <argument><name>CurrentSpeed</name><direction>out</direction><relatedStateVariable>TransportPlaySpeed</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetPositionInfo</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Track</name><direction>out</direction><relatedStateVariable>CurrentTrack</relatedStateVariable></argument>
    <argument><name>TrackDuration</name><direction>out</direction><relatedStateVariable>CurrentTrackDuration</relatedStateVariable></argument>
    <argument><name>TrackMetaData</name><direction>out</direction><relatedStateVariable>CurrentTrackMetaData</relatedStateVariable></argument>
    <argument><name>TrackURI</name><direction>out</direction><relatedStateVariable>CurrentTrackURI</relatedStateVariable></argument>
    <argument><name>RelTime</name><direction>out</direction><relatedStateVariable>RelativeTimePosition</relatedStateVariable></argument>
    <argument><name>AbsTime</name><direction>out</direction><relatedStateVariable>AbsoluteTimePosition</relatedStateVariable></argument>
    <argument><name>RelCount</name><direction>out</direction><relatedStateVariable>RelativeCounterPosition</relatedStateVariable></argument>
    <argument><name>AbsCount</name><direction>out</direction><relatedStateVariable>AbsoluteCounterPosition</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>Play</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Speed</name><direction>in</direction><relatedStateVariable>TransportPlaySpeed</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>Pause</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>Stop</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>Seek</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Unit</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SeekMode</relatedStateVariable></argument>
    <argument><name>Target</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SeekTarget</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>Next</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>Previous</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetTransportSettings</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>PlayMode</name><direction>out</direction><relatedStateVariable>CurrentPlayMode</relatedStateVariable></argument>
    <argument><name>RecQualityMode</name><direction>out</direction><relatedStateVariable>CurrentRecordQualityMode</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetDeviceCapabilities</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>PlayMedia</name><direction>out</direction><relatedStateVariable>PossiblePlaybackStorageMedia</relatedStateVariable></argument>
    <argument><name>RecMedia</name><direction>out</direction><relatedStateVariable>PossibleRecordStorageMedia</relatedStateVariable></argument>
    <argument><name>RecQualityModes</name><direction>out</direction><relatedStateVariable>PossibleRecordQualityModes</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetCurrentTransportActions</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Actions</name><direction>out</direction><relatedStateVariable>CurrentTransportActions</relatedStateVariable></argument>
  </argumentList></action>
</actionList>
<serviceStateTable>
  <stateVariable sendEvents="no"><name>TransportState</name><dataType>string</dataType><allowedValueList><allowedValue>STOPPED</allowedValue><allowedValue>PLAYING</allowedValue><allowedValue>PAUSED</allowedValue><allowedValue>TRANSITIONING</allowedValue></allowedValueList></stateVariable>
  <stateVariable sendEvents="no"><name>TransportStatus</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>TransportPlaySpeed</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>PlaybackStorageMedium</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>RecordStorageMedium</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>RecordMediumWriteStatus</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentPlayMode</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentRecordQualityMode</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>NumberOfTracks</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentTrack</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentTrackDuration</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentMediaDuration</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentTrackMetaData</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentTrackURI</name><dataType>uri</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>AVTransportURI</name><dataType>uri</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>AVTransportURIMetaData</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>NextAVTransportURI</name><dataType>uri</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>NextAVTransportURIMetaData</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>RelativeTimePosition</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>AbsoluteTimePosition</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>RelativeCounterPosition</name><dataType>i4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>AbsoluteCounterPosition</name><dataType>i4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentTransportActions</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>PossiblePlaybackStorageMedia</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>PossibleRecordStorageMedia</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>PossibleRecordQualityModes</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="yes"><name>LastChange</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_InstanceID</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_SeekMode</name><dataType>string</dataType><allowedValueList><allowedValue>TRACK_NR</allowedValue><allowedValue>ABS_TIME</allowedValue><allowedValue>REL_TIME</allowedValue></allowedValueList></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_SeekTarget</name><dataType>string</dataType></stateVariable>
</serviceStateTable>
</scpd>`

const scpdRendering = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
  <action><name>GetVolume</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Channel</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Channel</relatedStateVariable></argument>
    <argument><name>CurrentVolume</name><direction>out</direction><relatedStateVariable>Volume</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>SetVolume</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Channel</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Channel</relatedStateVariable></argument>
    <argument><name>DesiredVolume</name><direction>in</direction><relatedStateVariable>Volume</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetMute</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Channel</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Channel</relatedStateVariable></argument>
    <argument><name>CurrentMute</name><direction>out</direction><relatedStateVariable>Mute</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>SetMute</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>Channel</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Channel</relatedStateVariable></argument>
    <argument><name>DesiredMute</name><direction>in</direction><relatedStateVariable>Mute</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>ListPresets</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>CurrentPresetNameList</name><direction>out</direction><relatedStateVariable>PresetNameList</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>SelectPreset</name><argumentList>
    <argument><name>InstanceID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_InstanceID</relatedStateVariable></argument>
    <argument><name>PresetName</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_PresetName</relatedStateVariable></argument>
  </argumentList></action>
</actionList>
<serviceStateTable>
  <stateVariable sendEvents="no"><name>Volume</name><dataType>ui2</dataType><defaultValue>50</defaultValue><allowedValueRange><minimum>0</minimum><maximum>100</maximum><step>1</step></allowedValueRange></stateVariable>
  <stateVariable sendEvents="no"><name>Mute</name><dataType>boolean</dataType><defaultValue>0</defaultValue></stateVariable>
  <stateVariable sendEvents="yes"><name>LastChange</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>PresetNameList</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_InstanceID</name><dataType>ui4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_Channel</name><dataType>string</dataType><allowedValueList><allowedValue>Master</allowedValue></allowedValueList></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_PresetName</name><dataType>string</dataType></stateVariable>
</serviceStateTable>
</scpd>`

const scpdConnMgr = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion>
<actionList>
  <action><name>GetProtocolInfo</name><argumentList>
    <argument><name>Source</name><direction>out</direction><relatedStateVariable>SourceProtocolInfo</relatedStateVariable></argument>
    <argument><name>Sink</name><direction>out</direction><relatedStateVariable>SinkProtocolInfo</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetCurrentConnectionIDs</name><argumentList>
    <argument><name>ConnectionIDs</name><direction>out</direction><relatedStateVariable>CurrentConnectionIDs</relatedStateVariable></argument>
  </argumentList></action>
  <action><name>GetCurrentConnectionInfo</name><argumentList>
    <argument><name>ConnectionID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
    <argument><name>RcsID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_RcsID</relatedStateVariable></argument>
    <argument><name>AVTransportID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_AVTransportID</relatedStateVariable></argument>
    <argument><name>ProtocolInfo</name><direction>out</direction><relatedStateVariable>ProtocolInfo</relatedStateVariable></argument>
    <argument><name>PeerConnectionManager</name><direction>out</direction><relatedStateVariable>PeerConnectionManager</relatedStateVariable></argument>
    <argument><name>PeerConnectionID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_ConnectionID</relatedStateVariable></argument>
    <argument><name>Direction</name><direction>out</direction><relatedStateVariable>Direction</relatedStateVariable></argument>
    <argument><name>Status</name><direction>out</direction><relatedStateVariable>Status</relatedStateVariable></argument>
  </argumentList></action>
</actionList>
<serviceStateTable>
  <stateVariable sendEvents="no"><name>SourceProtocolInfo</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>SinkProtocolInfo</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>CurrentConnectionIDs</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionID</name><dataType>i4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_RcsID</name><dataType>i4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_AVTransportID</name><dataType>i4</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionManager</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_ConnectionStatus</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_Direction</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>A_ARG_TYPE_ProtocolInfo</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>ProtocolInfo</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>PeerConnectionManager</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>Direction</name><dataType>string</dataType></stateVariable>
  <stateVariable sendEvents="no"><name>Status</name><dataType>string</dataType></stateVariable>
</serviceStateTable>
</scpd>`

const scpdNirvana = `<?xml version="1.0" encoding="utf-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0"><specVersion><major>1</major><minor>0</minor></specVersion>
<actionList></actionList>
<serviceStateTable></serviceStateTable>
</scpd>`
