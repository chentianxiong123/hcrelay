# mycast — DLNA 投屏中继（手机当中间件）

把「App 投屏 → 电视」这条经常「越播越慢 / 播不了」的链路，改成手机当中间件：

```
App(B站/百度/抖音/盗版站) ──SSDP发现/SOAP/NVA──> mycast(手机上) ──本地HTTP──> 电视(DLNA)
                                                      │
                                                      ├─ 探测上游(Content-Type/防盗链)
                                                      ├─ HLS: 重写 m3u8 分片到本机代理
                                                      └─ 以本地URL喂电视，电视只拉这条干净的流
```

中间件同时扮演两个角色：

- **对 App**：一台 DLNA MediaRenderer（接收端），App 会 SSDP 搜到我、把播放 URL/命令发给我
- **对电视**：一个 DLNA Controller（发送端），我把取到的流 URL 推给电视，电视自己来拉

## 为什么能解决「越来越慢 / 播不了」

普通投屏卡/慢/黑屏，根因通常是这几类，中间件全都能治：

| 病因 | 中间件怎么治 |
|---|---|
| CDN 要 Referer/UA 防盗链，电视没带 → 403 | proxy 反代：补 Referer/UA + Range |
| HLS 分片是相对路径，电视解析成 `/m/000.ts` → 404 | 分片绝对 URL 用 base64 编码，重写到本机代理 |
| 抖音等 URL 不带 `.m3u8` 后缀 → 电视当 MP4 解析失败 | 按上游 Content-Type 探测识别 HLS |
| AES 加密 / fMP4 初始化段（`EXT-X-KEY`/`EXT-X-MAP`） | 密钥/初始化段也走本机代理 |
| master playlist 多码率 | 子列表走 `/pl/` 递归重写 |
| 山寨/网页客户端发畸形 SOAP（未转义 `&`） | SOAP 解析容错 + 正则兜底 |
| 源流 PTS 漂移 / 时间戳错 → 慢动作 | remux：ffmpeg `-c copy` 重封装成 MPEG-TS |

## 目录结构

```
mycast/
├── go/                  # 业务核心（server.New() 全量 handler，可被壳复用）
│   ├── cmd/mycast/     # 独立进程入口
│   └── internal/        # ssdp / renderer / nva / upnp / bili / media / tv / relay / server
├── android/             # 自写原生 WebView 壳（零 Gradle，纯 aapt2 打包）
├── archive/wails3/      # 已弃用的 wails3 壳（归档保留，不再维护）
├── docs/                # 技术分析与排查记录
└── README.md
```

## 运行

### 手机上（Android，推荐）
用 `android/` 壳打包成 APK 装到手机（见「安卓打包」），或 Termux 直跑：

```bash
# Termux
pkg install golang
cd /path/to/mycast/go
go build -o mycast ./cmd/mycast && ./mycast
```

### 电脑/盒子上
```bash
cd go && go build -o mycast ./cmd/mycast && ./mycast
```

### 环境变量
| 变量 | 默认 | 说明 |
|---|---|---|
| `HCRELAY_PORT` | 8080 | 主 HTTP 端口 |
| `HCRELAY_NVA_PORT` | 9958 | 哔哩必连 TCP 端口 |
| `HCRELAY_IP` | 自动 | 对外 IP（电视能访问到的地址） |
| `HCRELAY_ACCESS_KEY` | 空 | B站 access_key（会员高清可选） |

## 安卓打包（自写 WebView 壳）

壳在 `android/`，产物 `android/dist/mycast-all.apk`（arm64-v8a + x86_64，零 Gradle）。

```bash
cd android
./build-apk.sh            # 纯 aapt2 打包，双 ABI
```

> wails3 壳已弃用（setURL no-op / 绑定限制 / 混合内容限制），归档在 `archive/wails3/`。

安卓已适配：
- `MainActivity.acquireMulticastLock()`：Android 默认丢多播包，SSDP 发现电视必须拿 `WifiManager.MulticastLock`
- 权限：`INTERNET` + `CHANGE_WIFI_MULTICAST_STATE` + `ACCESS_WIFI_STATE`
- `usesCleartextTraffic="true"`：WebView 走 127.0.0.1，电视走局域网 IP 拉 /m/ 流
- 安卓端后端绑 `0.0.0.0`（电视要能拉到本机媒体流）
- JS 桥 `window.hc.sniffRender(url)` + `window.hc.renderDone(html,url)`：JS 反爬站 WebView 兜底

细节见 `android/` 源码。

## 打开控制台
浏览器访问 `http://<手机IP>:8080/ui`（移动端四页 SPA）：

- **首页**：状态卡 + 实时状态（目标电视/播放中/速度）
- **操作台**：进度条 + 大播放/暂停按钮 + 音量 + 传输统计
- **设备**：电视气泡选择
- **日志**：实时日志
- 底部 tab 导航，投屏成功后自动进操作台

> 播放模式是**全自动**的（`mode=auto`），后端按上游流自动决定处理方式，
> 普通用户不需要选择。

## 使用流程
1. 手机装 mycast 并打开（SSDP 广播中）
2. 打开 App，投屏 → 应该能搜到「HomeCast Relay」
3. 在 mycast 界面「设备」页把电视设为目标
4. App 开始投屏 → mycast 接住 → 探测/重写 → 推给电视

> 支持的入口：
> - 标准 DLNA（百度、B站普通投屏、抖音等走 SOAP SetAVTransportURI）
> - B站「极速投屏」走哔哩必连(NVA) 私有协议
>
> 不支持的（协议层面，不是中继能解决的）：
> - Google Cast（Chrome 投放按钮，走 Chromecast 协议）
> - Miracast（系统无线投屏 = 屏幕镜像）
> - DRM/Widevine 加密流
> - DASH (.mpd) —— 明确报错，不做假支持

## 测试
```bash
cd go && go test ./...
```
- `TestRelayEndToEnd`：模拟 App SetAVTransportURI → 中间件 → 模拟电视收到本地 URL + Play
- `TestGenericHLSFromAnySource`：7 种任意来源 HLS（无后缀/带query/fMP4/AES/master/绝对路径）
- `TestPirateSiteRefererPassthrough`：盗版站防盗链 Referer 透传端到端
- `TestParseSoapRobust`：畸形 SOAP（未转义 &）容错
- `TestRemuxProducesTS`：真实 mp4 → ffmpeg 重封装 → 有效 MPEG-TS（0x47 开头）

## 文档
见 `docs/`：
- `2026-10-29-DLNA投屏HLS全404排查修复.md` — HLS 分片 404 根因与修复
- `2026-10-29-手机CPU转码能力实测评估.md` — 手机转码能撑多少码率（实测+估算）

## 说明
- 纯 Go 标准库 + goupnp（控制电视）；交叉编译到 Android/arm64 通过
- proxy 模式是默认，零依赖零 CPU（字节搬运）；remux 需 ffmpeg（可选）
- 数据不经过手机解码/转码路径 → 手机只是字节搬运，CPU 占用≈0
