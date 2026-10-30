//go:build !android

package main

import (
	"log"
	"net/http"
)

// 桌面端后端监听：0.0.0.0 让局域网 SSDP/DLNA 可见
func listenBackend(h http.Handler) {
	addr := "0.0.0.0:" + backendPort()
	if err := http.ListenAndServe(addr, logRequest(h)); err != nil {
		log.Printf("[backend] %v", err)
	}
}
