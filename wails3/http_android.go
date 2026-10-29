//go:build android

package main

import (
	"log"
	"net/http"
)

// 安卓端后端监听：0.0.0.0 —— 电视必须能拉到本机 /m/<id> 的流，
// 所以不能只绑 127.0.0.1（WebView 走 127.0.0.1，电视走局域网 IP，都通）。
func listenBackend(h http.Handler) {
	addr := "0.0.0.0:" + backendPort()
	if err := http.ListenAndServe(addr, logRequest(h)); err != nil {
		log.Printf("[backend] %v", err)
	}
}
