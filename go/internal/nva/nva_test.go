package nva

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestHandshakeAndPlayCommand(t *testing.T) {
	// 服务端
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	var gotCmd Command
	srv := NewServer(func(cmd Command) (map[string]any, error) {
		gotCmd = cmd
		return map[string]any{"ok": true}, nil
	})
	go srv.Serve(l)

	// 客户端模拟 B站 App
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 握手
	conn.Write([]byte("SETUP /projection NVA/1.0\r\nSession: abc123\r\nNvaVersion: 1\r\nConnection: Keep-Alive\r\nUUID: Y12345678901234567890123456789012345\r\nUser-Agent: Linux/3.0.0 UPnP/1.0 Platinum/1.0.5.13\r\nHost: 127.0.0.1\r\n\r\n"))
	buf := make([]byte, 512)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	if string(buf[:n])[:11] != "NVA/1.0 200" {
		t.Fatalf("handshake resp: %q", string(buf[:n]))
	}

	// 发送 Play 命令帧（按协议格式）
	cmdBody := append([]byte{0xe0, 3}, []byte{0, 0, 0, 1, 1}...) // e0 argcount=3 seq=1 ver=1
	cmdBody = append(cmdBody, 7)
	cmdBody = append(cmdBody, "Command"...)
	cmdBody = append(cmdBody, 4)
	cmdBody = append(cmdBody, "Play"...)
	js := []byte(`{"aid":"123","cid":"456"}`)
	cmdBody = append(cmdBody, 0, 0, 0, byte(len(js)))
	cmdBody = append(cmdBody, js...)
	writeRaw(conn, cmdBody)

	// 等命令到达
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if gotCmd.Name == "Play" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gotCmd.Name != "Play" {
		t.Fatalf("got cmd=%q", gotCmd.Name)
	}
	if gotCmd.Args["aid"] != "123" || gotCmd.Args["cid"] != "456" {
		t.Fatalf("args=%#v", gotCmd.Args)
	}
	t.Logf("OK: got Play aid=%v cid=%v", gotCmd.Args["aid"], gotCmd.Args["cid"])

	// 读应答
	br := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	f, err := readFrame(br)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != opReply {
		t.Fatalf("expected reply, got op=0x%02x", f.Op)
	}
	t.Logf("OK: got reply op=0x%02x", f.Op)
}
