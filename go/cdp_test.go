package main

import (
	"net"
	"testing"
	"time"
)

// 回归测试：确认「对端接受连接但不回数据」时不会永久阻塞。
//
// 这是「程序卡死」这类问题最隐蔽的来源 ——
// net.DialTimeout 只管连接建立，读写不设 deadline 的话，
// io.ReadFull / ReadString 会无限期等下去。
func TestDialWSHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法建立测试监听：%v", err)
	}
	defer ln.Close()

	// 假服务器：接受连接，但既不读也不写，保持连接挂着
	go func() {
		var conns []net.Conn // 持有引用，避免被 GC 关闭
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()

	start := time.Now()
	_, err = dialWS("ws://"+ln.Addr().String()+"/devtools/page/fake", 2*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("对端毫无响应，dialWS 却返回成功 —— 预期应当超时失败")
	}
	if elapsed > 6*time.Second {
		t.Fatalf("超时保护失效：等了 %v 才返回（预期约 2s）", elapsed)
	}
	t.Logf("按预期在 %v 后返回错误：%v", elapsed.Round(time.Millisecond), err)
}

// 回归测试：确认 pinyinInitial 对几个典型字给出的首字母正确。
func TestPinyinInitial(t *testing.T) {
	cases := []struct {
		ch   rune
		want string
	}{
		{'我', "W"}, {'大', "D"}, {'心', "X"}, {'轻', "Q"}, {'禅', "C"},
		{'銀', "Y"}, {'腕', "W"}, {'祓', "F"}, {'啊', "A"}, {'座', "Z"},
		{'1', "1"}, {'S', "S"}, {'k', "K"},
	}
	for _, c := range cases {
		if got := pinyinInitial(c.ch); got != c.want {
			t.Errorf("pinyinInitial(%q) = %q，期望 %q", c.ch, got, c.want)
		}
	}
}

// 回归测试：日文假名应被跳过，落到其后的汉字上。
func TestSongInitialSkipsKana(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"この腕で、优しく包み込む", "W"},    // 跳过平假名 → 腕(w)
		{"カムラ祓え歌（炎火村祓除之歌）", "F"}, // 跳过片假名 → 祓(f)
		{"銀の意志", "Y"},
		{"Saikai", "S"},
		{"1000 个伤心的理由", "1"},
	}
	for _, c := range cases {
		if got := songInitial(c.name); got != c.want {
			t.Errorf("songInitial(%q) = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// 回归测试：文件名清洗与 Windows 保留名规避。
func TestBuildFilename(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"我要你（精品）", "W-我要你（精品）.png"},
		{"AC/DC 摇滚: 精选？", "A-AC_DC 摇滚_ 精选？.png"},
		{"  带空格 .", "D-带空格.png"},
	}
	for _, c := range cases {
		if got := buildFilename(c.name); got != c.want {
			t.Errorf("buildFilename(%q) = %q，期望 %q", c.name, got, c.want)
		}
	}
	if got := safeFilename("CON"); got != "_CON" {
		t.Errorf("safeFilename(\"CON\") = %q，期望 \"_CON\"", got)
	}
}
