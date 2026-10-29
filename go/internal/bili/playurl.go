// Package bili B站 playurl 客户端：拿 aid/cid/qn/access_key → 真实流地址。
// 在 NVA（哔哩必连）模式下，B站 App 只下发 Play{aid,cid,qn}，
// 我需要自己调 playurl 拿流，才能"自己持有一个不会过期的流"。
package bili

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Client playurl 客户端
type Client struct {
	UA        string
	Referer   string
	Cookie    string
	AccessKey string
	hc        *http.Client
}

// New 默认客户端
func New(accessKey string) *Client {
	return &Client{
		UA:        "Mozilla/5.0 (Linux; Android 13; SM-S9180) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36",
		Referer:   "https://www.bilibili.com/",
		Cookie:    "",
		AccessKey: accessKey,
		hc:        &http.Client{Timeout: 15 * time.Second},
	}
}

// Stream 解析出的流
type Stream struct {
	URL      string // 可播放 URL（DASH 取合并或 mp4）
	MimeType string
}

// PlayURL 调 playurl 拿流（支持 DASH 与 mp4）
func (c *Client) PlayURL(aid, cid string, qn int) (*Stream, error) {
	if qn == 0 {
		qn = 80
	}
	params := url.Values{}
	params.Set("avid", aid)
	params.Set("cid", cid)
	params.Set("qn", fmt.Sprint(qn))
	params.Set("fnval", "16") // DASH
	params.Set("fnver", "0")
	params.Set("fourk", "1")
	if c.AccessKey != "" {
		params.Set("access_key", c.AccessKey)
	}
	api := "https://api.bilibili.com/x/player/playurl?" + params.Encode()
	req, err := http.NewRequest(http.MethodGet, api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UA)
	req.Header.Set("Referer", c.Referer)
	if c.Cookie != "" {
		req.Header.Set("Cookie", c.Cookie)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			URL string `json:"url"` // 非 DASH 时（mp4）
			Dash struct {
				Video []struct {
					ID        int    `json:"id"`
					BaseURL   string `json:"baseUrl"`
					BaseURL2  string `json:"baseUrl_backup"`
					Codecs    string `json:"codecs"`
					Bandwidth int64  `json:"bandwidth"`
				} `json:"video"`
				Audio []struct {
					ID      int    `json:"id"`
					BaseURL string `json:"baseUrl"`
				} `json:"audio"`
			} `json:"dash"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("playurl code=%d msg=%s", parsed.Code, parsed.Message)
	}
	// DASH：选 video+audio 合并（很多电视/DLNA 只吃 mp4，这里返回视频 baseUrl，音频单独处理）
	if len(parsed.Data.Dash.Video) > 0 {
		v := parsed.Data.Dash.Video[0]
		u := v.BaseURL
		if u == "" {
			u = v.BaseURL2
		}
		return &Stream{URL: u, MimeType: "video/mp4"}, nil
	}
	if parsed.Data.URL != "" {
		return &Stream{URL: parsed.Data.URL, MimeType: "video/mp4"}, nil
	}
	return nil, fmt.Errorf("no stream url")
}
