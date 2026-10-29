# hcrelay — B站投屏中继（手机当中间件）

把「B站 App → 电视」这条经常「越播越慢」的链路，改成：

```
B站 App ──SSDP发现/SOAP/NVA──> hcrelay(手机上) ──本地HTTP──> 电视(DLNA)
                                   │
                                   ├─ 取流(playurl/直链)
                                   ├─ 本地重封装(ffmpeg -c copy 修时间戳)
                                   └─ 以本地URL喂给电视，电视只拉这条干净的流
```

中间件同时扮演两个角色：

- **对 B站**：一台 DLNA MediaRenderer（接收端），B站 App 会 SSDP 搜到我、把播放 URL/命令发给我
- **对电视**：一个 DLNA Controller（发送端），我把取到/重封装好的流 URL 推给电视

## 为什么能解决「越来越慢」

普通投屏卡/慢，根因通常是这三类，中间件全都能治：

| 病因 | 中间件怎么治 |
|---|---|
| 电视拿不到/被鉴权挡（CDN 要 Referer/UA，电视没带） | proxy 模式：本地反代补 Referer/UA + Range |
| HLS 被电视当直播播、延迟线性累积 → 越拖越慢 | hls 模式：m3u8 段 URL 重写到本机，只喂最新窗口 |
| 源流 PTS 漂移 / 每片段时间戳错 → 整体慢动作 | remux 模式：ffmpeg -c copy 重封装成 MPEG-TS，`+genpts` 重置时间戳 |

## 目录结构

```
hcrelay/
├── go/          # 业务核心（server.New() 全量 handler，可被壳复用）
│   ├── cmd/hcrelay/     # 独立进程入口
│   └── internal/        # ssdp / renderer / nva / upnp / bili / media / tv / relay / server
├── wails3/      # 统一壳（安卓/桌面），见 wails3/build-android.md
└── README.md
```

## 运行

### 手机上（Android，推荐）
用 wails3 打包成 APK 装到手机（见下文「wails3 打包」），或 Termux 直跑：

```bash
# Termux
pkg install golang ffmpeg
cd /path/to/hcrelay/go
go build -o hcrelay ./cmd/hcrelay && ./hcrelay
```

> 手机上必须能访问 ffmpeg（`pkg install ffmpeg`），remux 模式才有效；没有 ffmpeg 用 proxy 模式。

### 电脑/盒子上
```bash
cd go && go build -o hcrelay ./cmd/hcrelay && ./hcrelay
```

### 环境变量
| 变量 | 默认 | 说明 |
|---|---|---|
| `HCRELAY_PORT` | 8080 | 主 HTTP 端口 |
| `HCRELAY_NVA_PORT` | 9958 | 哔哩必连 TCP 端口 |
| `HCRELAY_IP` | 自动 | 对外 IP（电视能访问到的地址） |
| `HCRELAY_ACCESS_KEY` | 空 | B站 access_key（会员高清可选） |

## wails3 打包（安卓 APK）

壳在 `wails3/`，产物 `wails3/bin/hcrelay.apk`（arm64-v8a + x86_64）。

```bash
cd wails3
./build-android.sh            # arm64 真机（默认）
./build-android.sh amd64      # 模拟器
# 或 task android:package ARCH=arm64
```

安卓已适配：
- `MainActivity.acquireMulticastLock()`：Android 默认丢多播包，SSDP 发现电视必须拿 `WifiManager.MulticastLock`
- 权限：`INTERNET` + `CHANGE_WIFI_MULTICAST_STATE` + `ACCESS_WIFI_STATE`
- `usesCleartextTraffic="true"`：WebView 走 127.0.0.1，电视走局域网 IP 拉 /m/ 流
- 安卓端后端绑 `0.0.0.0`（电视要能拉到本机媒体流）

细节见 `wails3/build-android.md`。

## 打开控制台
浏览器访问 `http://<手机IP>:8080/ui`（手机端 htmx 界面）：

- **播放模式**：直连代理 / 重封装(推荐) / HLS修复
- **电视设备**：自动扫描，点「设为目标」
- **状态/日志**：每 2.5s 自动刷新

## 使用流程
1. 手机装 hcrelay 并打开（SSDP 广播中）
2. 打开 B站 App，投屏 → 应该能搜到「HomeCast Relay」
3. 在 hcrelay 界面里把电视「设为目标」，选好播放模式
4. B站 开始投屏 → hcrelay 接住 → 取流/重封装 → 推给电视

> B站 App 里「普通投屏/极速投屏」都会进 hcrelay：
> 普通走标准 DLNA URL，极速走哔哩必连(NVA) Play 命令自行取流。

## 测试
```bash
cd go && go test ./...   # SOAP/NVA/媒体(proxy+range+hls+remux)/端到端 全通过
```
- `TestRelayEndToEnd`：模拟 B站 SetAVTransportURI → 中间件 → 模拟电视收到本地 URL + Play
- `TestRemuxProducesTS`：真实 mp4 → ffmpeg 重封装 → 有效 MPEG-TS（0x47 开头）
- `TestHandshakeAndPlayCommand`：NVA 握手 + Play{aid,cid} 解析 + 应答

## 说明
- 纯 Go 标准库 + goupnp（控制电视）；交叉编译到 Android/arm64 通过
- 需要 `ffmpeg` 在 PATH 里才能用 remux 模式；没有则用 proxy 模式
- 仅个人自用，勿公开分发
