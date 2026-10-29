package upnp

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSoap(t *testing.T) {
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:SetAVTransportURI xmlns:u="urn:schemas-upnp-org:service:AVTransport:1"><InstanceID>0</InstanceID><CurrentURI>http://example.com/bili/video.mp4</CurrentURI><CurrentURIMetaData>&lt;DIDL-Lite&gt;&lt;dc:title&gt;测试&lt;/dc:title&gt;&lt;/DIDL-Lite&gt;</CurrentURIMetaData></u:SetAVTransportURI></s:Body></s:Envelope>`
	r := httptest.NewRequest("POST", "/ctl", strings.NewReader(body))
	r.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI"`)
	req, err := ParseSoap(r)
	if err != nil {
		t.Fatal(err)
	}
	if req.Action != "SetAVTransportURI" {
		t.Fatalf("action = %q", req.Action)
	}
	if req.Args["CurrentURI"] != "http://example.com/bili/video.mp4" {
		t.Fatalf("CurrentURI = %q", req.Args["CurrentURI"])
	}
	t.Logf("args=%#v", req.Args)
}
