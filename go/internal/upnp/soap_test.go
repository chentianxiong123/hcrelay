package upnp

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseSoapRobust 验证对真实世界畸形 SOAP 的容错：
//   - URL 里带未转义的 & （大量山寨/网页 DLNA 客户端）
//   - CurrentURIMetaData 里嵌 DIDL-Lite 带 &lt; 转义
//   - 极端情况下用正则兜底
func TestParseSoapRobust(t *testing.T) {
	cases := []struct {
		name     string
		xml      string
		wantURI  string
		wantAct  string
	}{
		{
			name: "正常-转义完整",
			xml: `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<CurrentURI>http://x.com/a.m3u8?t=1&amp;sign=abc</CurrentURI>
</u:SetAVTransportURI></s:Body></s:Envelope>`,
			wantURI: "http://x.com/a.m3u8?t=1&sign=abc",
			wantAct: "SetAVTransportURI",
		},
		{
			name: "裸amp-常见山寨客户端",
			xml: `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<CurrentURI>http://x.com/stream?id=998877&token=abc</CurrentURI>
</u:SetAVTransportURI></s:Body></s:Envelope>`,
			wantURI: "http://x.com/stream?id=998877&token=abc",
			wantAct: "SetAVTransportURI",
		},
		{
			name: "DIDL-metadata带转义",
			xml: `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">
<InstanceID>0</InstanceID>
<CurrentURI>http://x.com/v.m3u8</CurrentURI>
<CurrentURIMetaData>&lt;DIDL-Lite&gt;&lt;dc:title&gt;电影&lt;/dc:title&gt;&lt;/DIDL-Lite&gt;</CurrentURIMetaData>
</u:SetAVTransportURI></s:Body></s:Envelope>`,
			wantURI: "http://x.com/v.m3u8",
			wantAct: "SetAVTransportURI",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/ctl/AVTransport", strings.NewReader(tc.xml))
			req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI"`)
			r, err := ParseSoap(req)
			if err != nil {
				t.Fatalf("parse err: %v", err)
			}
			if r.Action != SoapAction(tc.wantAct) {
				t.Errorf("action=%q want=%q", r.Action, tc.wantAct)
			}
			if got := r.Args["CurrentURI"]; got != tc.wantURI {
				t.Errorf("CurrentURI=%q want=%q", got, tc.wantURI)
			}
		})
	}
}

// TestSanitizeXML 验证裸 & 转义
func TestSanitizeXML(t *testing.T) {
	in := `<a x="1&b=2">&amp;&lt;&#65;</a>`
	out := sanitizeXML(in)
	if !strings.Contains(out, "1&amp;b=2") {
		t.Errorf("裸&未转义: %s", out)
	}
	if !strings.Contains(out, "&amp;") || !strings.Contains(out, "&lt;") || !strings.Contains(out, "&#65;") {
		t.Errorf("合法实体被破坏: %s", out)
	}
}
