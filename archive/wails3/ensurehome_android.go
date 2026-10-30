//go:build android

package main

import (
	"os"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// ensureHome 安卓 app 进程 HOME 不可靠：无条件指向私有 filesDir。
// 必须在 nativeInit 之后（JNI bridge 可用）调用。
// 注：main 的注册由 build/android/gen/main_android.gen.go 的 init() 完成（overlay 注入），
// 这里不再重复 RegisterAndroidMain，避免重复注册。
func ensureHome() {
	os.Setenv("HOME", application.Android.StoragePath())
}

// hcDataPath 应用数据目录（日志/配置落盘处）
func hcDataPath() string {
	return application.Android.StoragePath()
}
