package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mycast/internal/media"
	"mycast/internal/renderer"
	"mycast/internal/ssdp"
)

func sleep() { time.Sleep(20 * time.Millisecond) }

// fakeTV 模拟一台电视（DLNA AVTransport SOAP 接收端）
type fakeTV struct {
	gotURI  string
	gotMeta string
	playCnt int
}

func (f *fakeTV) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/description.xml", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><device><deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType><friendlyName>FakeTV</friendlyName><serviceList><service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><serviceId>urn:upnp-org:serviceId:AVTransport</serviceId><controlURL>/ctl/AVTransport</controlURL></service><service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType><serviceId>urn:upnp-org:serviceId:RenderingControl</serviceId><controlURL>/ctl/RenderingControl</controlURL></service></serviceList></device></root>`)
	})
	mux.HandleFunc("/ctl/AVTransport", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		if strings.Contains(s, "SetAVTransportURI") {
			f.gotURI = extractXML(s, "CurrentURI")
			f.gotMeta = extractXML(s, "CurrentURIMetaData")
		}
		if strings.Contains(s, "Play") {
			f.playCnt++
		}
		w.Header().Set("Content-Type", "text/xml")
		io.WriteString(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:SetAVTransportURIResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"></u:SetAVTransportURIResponse></s:Body></s:Envelope>`)
	})
	return mux
}

func extractXML(s, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return s[i+len(open) : i+len(open)+j]
}

func TestRelayEndToEnd(t *testing.T) {
	// 模拟电视
	fake := &fakeTV{}
	tvSrv := httptest.NewServer(fake.handler())
	defer tvSrv.Close()

	// 模拟上游（本地 mock，避免真的请求 example.com）
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "4")
		w.Write([]byte{0, 0, 0, 0})
	}))
	defer upSrv.Close()

	// 渲染端（接收 B站）
	renderer := renderer.New("http://127.0.0.1:9999", "uuid:test", "Relay", renderer.Services{AVTransport: true, RenderingCtrl: true, ConnMgr: true})

	// 媒体服务器
	ms := media.NewServer("http://127.0.0.1:9999")

	// 编排器
	rl := New(renderer, ms, nil, "http://127.0.0.1:9999")
	rl.SelectTV(&ssdp.Device{Name: "FakeTV", Location: tvSrv.URL + "/description.xml"})

	// 模拟 B站 App 投屏：SetAVTransportURI + Play
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><InstanceID>0</InstanceID><CurrentURI>` + upSrv.URL + `/bili/video.mp4</CurrentURI><CurrentURIMetaData>&lt;DIDL-Lite&gt;&lt;dc:title&gt;测试视频&lt;/dc:title&gt;&lt;/DIDL-Lite&gt;</CurrentURIMetaData></u:SetAVTransportURI></s:Body></s:Envelope>`
	req := httptest.NewRequest("POST", "/ctl/AVTransport", strings.NewReader(body))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI"`)
	rr := httptest.NewRecorder()
	renderer.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("SetAVTransportURI returned %d: %s", rr.Code, rr.Body.String())
	}

	// 等 relay 异步推送到电视
	waitFor(t, func() bool { return fake.gotURI != "" })
	if !strings.Contains(fake.gotURI, "/m/") {
		t.Fatalf("TV should receive relayed local URL, got: %s", fake.gotURI)
	}
	if fake.playCnt == 0 {
		t.Fatal("TV should be told to Play")
	}
	t.Logf("OK: TV got %s (meta title=%s)", fake.gotURI, extractXML(fake.gotMeta, "dc:title"))
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if cond() {
			return
		}
		sleep()
	}
	t.Fatal("timeout waiting for condition")
}
