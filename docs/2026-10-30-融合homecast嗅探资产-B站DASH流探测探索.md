# 探索记录：融合 homecast 嗅探资产到 hcrelay（B站 DASH 流探测）

> 2026-10-30 · 探索型任务 · 无标准答案，靠翻历史项目摸资产

## ① 目标与动机

hcrelay 目前只能靠 B站 App 投屏给的**现成 URL** 拉流，URL 带 `deadline`（约 30 分钟）到期后 403 → 电视卡死。要续期，必须**重新调 playurl 拿新 URL**，但 playurl 需要 `aid/cid`，而投屏 URL 里只有 `cid` 没有 `aid`。

用户提示：`/home/a1/homecast` 是以前的项目，可能留有"嗅探阶段"的资产。目标是**把能拿到 aid/cid 并重新解析 DASH 流的能力摸出来，融进 hcrelay**。

## ② 资源拓扑（最重要的成果）

### /home/a1/homecast 是什么
**小米音箱投送项目**（speaker/lyric/htmx），不是 DLNA 项目。但它的 `go/internal/bilibili/` 包是**完整的 B站 API 客户端**，正是我们要的嗅探资产。

### 关键资产（homecast → hcrelay）

| 原文件 | 内容 | 融进 hcrelay |
|--------|------|-------------|
| `bilibili/client.go` | `Client`/`NewClient`/`defaultHeaders`/`GetJSON`/`GetRaw`/`newUUID`/`decodeData`/`BilibiliAPIError` | `bili/client_api.go` |
| `bilibili/audio.go` | `GetAudioStream`/`GetBestAudioURL`/`GetBestVideoURL`/`GetVideoInfo` + `DashVideoItem`/`DashInfo`/`VideoInfo` | `bili/dash.go` |
| `bilibili/search.go` | `Search`/`SearchSuggest`/`CleanHTML` | `bili/search_api.go` |
| `service/cast/cast.go` | `Sniff`/`sniffBilibili`/`resolveStream`/`PlayURL`/`buildDIDLLite`（页面→DASH 流→token 代理） | 暂未融（依赖 goupnp/token 服务层） |
| `api/proxy.go` | token 化流代理：带 `Referer`+`UA`+`Range` 透传 | 与 hcrelay 现有 proxy 思路一致，暂不融 |
| `dlna/discovery.go` | SSDP 发现 | hcrelay 已有 ssdp，暂不融 |

### B站 流探测关键结构（摸出来的核心）

```
页面 URL（bvid）──→ GetVideoInfo(bvid) ──→ cid/aid
                                      │
                        GetBestVideoURL(bvid, cid)
                                      │
                            playurl (fnval=16 DASH)
                                      │
                        Dash.Video[]（baseUrl / backupUrl / bandwidth / codecs）
                                      │
                      按 Bandwidth 降序取最高 → baseUrl（带 deadline 签名）
```

**续期关键路径**：`cid`（从投屏 URL 提取）→ 但 playurl 要 bvid/aid → 需要 `GetVideoInfo` 反查。**aid 不在投屏 URL 里，必须反查**。

## ③ 工具链

- Go 1.26.0（两边 go.mod 一致）
- homecast module 名：`homecast`；hcrelay module 名：`hcrelay`（**不同，融合要改包名+去掉跨 module 引用**）
- homecast `bilibili` 包**无 goupnp 依赖**（纯标准库 + net/http）→ 可无缝并包

## ④ 成功路径（可复现）

```
cd /home/a1/homecast/go/internal/bilibili
# 1. 复制 client.go/audio.go/search.go 为 .import 临时
# 2. 改包名 bilibili → bili
# 3. 删掉重复的 Client struct + NewClient（合并进 playurl.go 的 Client，取字段并集）
# 4. playurl.go 的 Client 扩展：+BaseURL/Timeout/buvid3，New 初始化它们；新增 NewClient 兼容构造
# 5. go build ./... && go test ./internal/bili/ → 全过
```

关键坑：`NewClient` 移走后 `time` import 未使用 → 删。**homecast 的 audio_test.go/search_test.go 原样可用**（改包名即可），5 个 DASH 测试 + 5 个 search 测试全过。

## ⑤ 踩坑记录

| 现象 | 根因 | 解法 |
|------|------|------|
| `"time" imported and not used` | NewClient 移走，`time` 只剩 NewClient 用 | 删掉 `time` import |
| edit 匹配失败 | 文件实际无包注释，assistant 记错原文 | 先 `read` 看实际内容再精确匹配 |
| `TestEndToEndCrossSegment` FAIL | **PTS 重置器自己的测试（弯路）**，seg1 PTS=1080000 期望 540000 | **与融合无关**，之前就存在。PTS 已判定方向错，不修 |
| `Client` 重复定义 | homecast 与 hcrelay 各有一个 `Client` | 合并成一个，字段取并集（UA/Referer/Cookie/AccessKey/BaseURL/Timeout/buvid3/hc） |

## ⑥ 验证成果

```
go build ./...                → 成功
go test ./internal/bili/ -v   → 11 个 PASS（DASH 5 + search 6）
go test ./...                 → 仅 PTS 弯路 TestEndToEndCrossSegment 失败（与融合无关）
```

提交 `b599b01`。

## ⑦ 决策记录

- **为什么融合 homecast 的 `bilibili` 包而不是重写**：它是完整自洽的 B站 API 客户端（buvid3 防风控 + GetRaw 带 www.bilibili.com Referer 直转 DASH + GetVideoInfo 反查 aid/cid），**续期需要的能力它全有**。重写浪费且易错。
- **为什么选 DASH 视频流（GetBestVideoURL）而不是 playurl 的 mp4**：DASH `baseUrl` 是动态签名的，每次拿都是新 deadline；mp4 durl 是旧方案。续期用 DASH。
- **为什么不融 service/cast + api/proxy**：它们依赖 goupnp/token 服务层，且 hcrelay 已有等价的 relay/media 代理层。**只融 bilibili 纯 API 包**，避免重复。
- **PTS 重置器（c15597b）确认是弯路**：症状"突发爆破音+一直卡"≠"渐进变慢"，PTS 漂移不是主因。**不修它的测试**，主方案是 URL 续期。

## ⑧ 未解之谜 / 下一步岔路

- [ ] **URL 续期主逻辑还没接**：现在只是"有了 GetVideoInfo/GetBestVideoURL 能力"，还没接进 relay 的续期调度（到期前 N 秒重调 playurl 换新 URL）。
- [ ] **登录状态续期需要凭证**：`access_key` 只有 B站 App 有。匿名 playurl 会降清晰度。需 B站 网页版 cookie（SESSDATA）配置项。
- [ ] **B站 投屏给的是 mp4 还是 m3u8**：待真机确认。决定续期是"换新 URL"还是"重拉 m3u8"。
- [ ] **电视 192.168.31.3 关机**：无法真机端到端验证，需开机后投 20+ 分钟 B站长视频。

## 下一步建议

接入续期：`cid`（投屏 URL 提取）→ `GetVideoInfo` 反查 bvid → 到期前 `GetBestVideoURL` 拿新 `baseUrl` → 无缝切换。cookie 可配置（有则高清、无则降级保底）。

---

# 附：盗版网站嗅探资产（很早以前的记录，homecast 历史）

> 用户提示：不是 B站 专用，是**盗版网站**的正则匹配 + 过滤页面 + 提取集数链接（123/45 这种）。
> 来源：`backend/app/sniffer/extractor.py`（homecast git 历史，4月28日 `647bf17` 初始版起，10月2日 `28ac258` 砍 Playwright，后 `ee302ba` 收进 explore/）。

## ① 页面类型正则识别（通用盗版站）

```python
def _is_detail_page(self, url):
    detail_patterns = [
        r"/vod/", r"/voddetail/", r"/detail/",
        r"/video/\d+", r"/movie/", r"/tv/",
    ]
    return any(re.search(p, url, re.IGNORECASE) for p in detail_patterns)

def _is_play_page(self, url):
    play_patterns = [
        r"/play/", r"/vodplay/", r"/episode/", r"/ep/",
        r"play.*\.html",
    ]
    return any(re.search(p, url, re.IGNORECASE) for p in play_patterns)
```

覆盖盗版站常见路由：`/vod/`、`/vodplay/`（苹果CMS/momovod 系）、`/detail/`、`/episode/`、`/video/\d+` 等。

## ② 集数列表提取（核心，123/45 这类）

`_parse_episode_list`：加载详情页 → `li > a` 过滤 → 只取 `/play/` 或 `/vodplay/` 链接 + 含"集"字的 → 集数列表。

```python
# 关键过滤逻辑（li > a 结构）
for item in li_links:
    text, href = item['text'], item['href']
    if not ('/play/' in href or '/vodplay/' in href):   # 只取播放链接
        continue
    if '集' not in text:                                 # 过滤"立即播放"等按钮
        continue
    full_url = base_url + href if href.startswith('/') else href
    episodes.append(VideoEpisode(index=len(episodes)+1, title=text, url=full_url))
# 去重 + 按 index 排序
```

## ③ 播放页流提取

`_sniff_play_page`：监听网络请求拿 m3u8/video URL + 用正则从 `page.content()` 提取 m3u8（过滤 segment `.ts`、`hls`、`/1000k/` 等）。

## ④ 兜底 yt-dlp

`_try_ytdlp`：`yt-dlp --dump-json --playlist-items 1:100`，解析剧集/单集。

## 坑与现状

- **坑**：这套依赖 **Playwright 无头浏览器**（重、慢、需装浏览器），`28ac258` 已砍。`_parse_episode_list` 后来变成"已禁用"。
- **要"直接吃"的建议**：把①（页面正则识别）+②（集数过滤规则）**纯逻辑提炼**（不依赖 Playwright），集数提取改用 **HTTP GET 详情页 + 正则/HTML 解析**（`li > a` 规则可映射成正则或简单 HTML 标签扫描），避免无头浏览器。③ 的 m3u8 正则可保留。④ yt-dlp 是外部依赖，可选。
- **与 hcrelay 的衔接**：hcrelay 现在只接"App 投屏的现成 URL"，没做"输入盗版站页面 URL → 提取集数 → 选一集 → 拿流"。这份资产正好补这个入口。

## 未实现（待探索）

- [ ] 盗版站集数提取（纯 Go，无浏览器）：页面正则 + `li > a` 过滤 → 集数列表
- [ ] 盗版站播放页 m3u8 提取（纯 Go 正则）
- [ ] 前端"粘贴页面 URL → 嗅探 → 选集数 → 投屏"入口
