# 🤖 安卓端构建（wails3 壳）

产物：`wails3/bin/hcrelay.apk`（23MB，arm64-v8a + x86_64）

## 构建命令

```bash
cd /tmp/hcrelay/wails3

# 1. 生成安卓 overlay（注册 main）
wails3 android overlay:gen -out build/android/overlay.json -config build/config.yml

# 2. 用 NDK 交叉编译 Go → libwails.so（arm64）
export ANDROID_NDK_HOME=/usr/local/android-sdk/ndk/28.2.13676358
export CC=$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android21-clang
export CXX=$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android21-clang++
export GOARCH=arm64 CGO_ENABLED=1 GOOS=android
go build -buildmode=c-shared -overlay build/android/overlay.json \
  -tags android,debug -buildvcs=false -gcflags=all="-l" \
  -o build/android/app/src/main/jniLibs/arm64-v8a/libwails.so .

# 3. gradle 打 APK
cd build/android && ./gradlew assembleRelease
cp app/build/outputs/apk/release/app-release.apk ../../bin/hcrelay.apk
```

（也可以直接用 `task android:package ARCH=arm64`，前提装了 go-task）

## 安装
```bash
adb install -r wails3/bin/hcrelay.apk
```

## 已做的安卓适配
- **MulticastLock**：`MainActivity.acquireMulticastLock()` 在 `onCreate` 拿 `WifiManager.MulticastLock`，否则 Android 默认丢多播包，SSDP 发现不了电视
- **权限**：`INTERNET` + `CHANGE_WIFI_MULTICAST_STATE` + `ACCESS_WIFI_STATE`（SSDP 多播必需）
- **cleartext HTTP**：`android:usesCleartextTraffic="true"`（WebView 走 127.0.0.1，电视走局域网 IP 拉 /m/ 流，都是 http）
- **后端监听**：安卓端也绑 `0.0.0.0`（电视必须能拉到本机媒体流，不能只绑 127.0.0.1）
- **包名锁死 `com.wails.app`**：wails3 安卓 JNI export 符号写死，改包名要 fork 依赖 → 保持默认

## 已知约束
- 应用需保持前台（WebView 界面）才能稳定中继；后台被冻结需加前台服务（后续）
- 手机上要有 ffmpeg 才能用"重封装"模式；没有就用"直连代理"
- 数据落盘：`filesDir/.config/hcrelay/`（hcrelay-http.log）
