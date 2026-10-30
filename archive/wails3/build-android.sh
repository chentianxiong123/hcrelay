#!/usr/bin/env bash
# 一键打安卓 APK（wails3 壳）
# 用法：./build-android.sh [arch]   arch = arm64（默认，真机）| amd64（模拟器）
set -e
cd "$(dirname "$0")"

export PATH="$HOME/go/bin:$PATH"
ARCH="${1:-arm64}"
ANDROID_NDK_HOME="${ANDROID_NDK_HOME:-/usr/local/android-sdk/ndk/28.2.13676358}"
SDK_ROOT="${ANDROID_HOME:-/usr/local/android-sdk}"
MIN_SDK=21
HOST_TAG=linux-x86_64
TOOLCHAIN="$ANDROID_NDK_HOME/toolchains/llvm/prebuilt/$HOST_TAG"

case "$ARCH" in
  arm64) ABI=arm64-v8a; TC=aarch64; JNI=arm64-v8a ;;
  amd64) ABI=x86_64;    TC=x86_64;  JNI=x86_64 ;;
  *) echo "arch must be arm64|amd64"; exit 1 ;;
esac

echo "==> overlay"
wails3 android overlay:gen -out build/android/overlay.json -config build/config.yml

echo "==> NDK 编译 libwails.so ($ABI)"
export CC="$TOOLCHAIN/bin/${TC}-linux-android${MIN_SDK}-clang"
export CXX="$TOOLCHAIN/bin/${TC}-linux-android${MIN_SDK}-clang++"
export GOARCH=$ARCH CGO_ENABLED=1 GOOS=android
export GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off
go mod tidy
go build -buildmode=c-shared -overlay build/android/overlay.json \
  -tags android,debug -buildvcs=false -gcflags=all="-l" \
  -o "build/android/app/src/main/jniLibs/$JNI/libwails.so" .

echo "==> gradle assembleRelease"
cd build/android
export ANDROID_HOME="$SDK_ROOT" ANDROID_SDK_ROOT="$SDK_ROOT"
./gradlew assembleRelease
cp app/build/outputs/apk/release/app-release.apk ../../bin/hcrelay.apk
echo "==> 完成: bin/hcrelay.apk"
