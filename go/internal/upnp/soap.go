// Package upnp UPnP/DLNA 通用件：设备描述 XML、SCPD、SOAP 信封。
package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SoapAction 请求里的 action 名（SOAPAction header 或 body 的第一段）
type SoapAction string

// SoapRequest 解析后的 SOAP 请求
type SoapRequest struct {
	Action SoapAction
	Args   map[string]string
	Raw    string
}

// ParseSoap 解析 SOAP body
func ParseSoap(r *http.Request) (*SoapRequest, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	raw := string(body)
	req := &SoapRequest{Args: map[string]string{}, Raw: raw}
	if a := r.Header.Get("SOAPAction"); a != "" {
		a = strings.Trim(strings.TrimSpace(a), `"`)
		if i := strings.LastIndex(a, "#"); i >= 0 {
			a = a[i+1:]
		}
		req.Action = SoapAction(a)
	}

	// 递归遍历：找 Body 的直接子元素（= action），收集其子元素为 key=value
	dec := xml.NewDecoder(strings.NewReader(raw))
	var stack []xml.Name
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name)
			// action 元素：Body 的直接子元素（Envelope, Body, Action → depth 3）
			if len(stack) == 3 {
				if req.Action == "" {
					req.Action = SoapAction(t.Name.Local)
				}
				continue
			}
			// action 的直接子元素（depth 4）= 参数
			if len(stack) == 4 {
				var val string
				if err := dec.DecodeElement(&val, &t); err == nil {
					req.Args[t.Name.Local] = strings.TrimSpace(val)
				}
				stack = stack[:3] // DecodeElement 已消费该元素
				continue
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return req, nil
}

// SoapResponse 生成 SOAP 成功应答
func SoapResponse(action SoapAction, args map[string]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<u:` + string(action) + `Response xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`)
	for k, v := range args {
		fmt.Fprintf(&b, `<%s>%s</%s>`, k, Escape(v), k)
	}
	b.WriteString(`</u:` + string(action) + `Response></s:Body></s:Envelope>`)
	return b.String()
}

// SoapFault 生成 SOAP 故障应答
func SoapFault(action SoapAction, code int, desc string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault>` +
		`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>` +
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>` + fmt.Sprint(code) + `</errorCode><errorDescription>` + Escape(desc) + `</errorDescription></UPnPError>` +
		`</detail></s:Fault></s:Body></s:Envelope>`
}

// Escape XML 转义
func Escape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// DIDLLite 生成投屏元数据（告诉电视这个 URI 是什么内容）
func DIDLLite(uri, title, contentType string) string {
	if contentType == "" {
		contentType = "video/mp4"
	}
	class := "object.item.videoItem"
	if strings.HasPrefix(contentType, "audio") {
		class = "object.item.audioItem"
	}
	proto := "http-get:*:" + contentType + ":DLNA.ORG_PN=;DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"
	return `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/">` +
		`<item id="0" parentID="-1" restricted="1">` +
		`<upnp:class>` + class + `</upnp:class>` +
		`<dc:title>` + Escape(title) + `</dc:title>` +
		`<res protocolInfo="` + proto + `">` + Escape(uri) + `</res>` +
		`</item></DIDL-Lite>`
}
