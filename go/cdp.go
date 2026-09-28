package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// 极简 WebSocket 客户端（文本帧 + 分片重组），仅用标准库实现。
// ============================================================================

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	id   int
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在 Windows 上不会失败；退化为固定值也不影响协议正确性
		for i := range b {
			b[i] = byte(i)
		}
	}
	return b
}

// dialWS 完成 WebSocket 握手并返回连接。
func dialWS(wsURL string, timeout time.Duration) (*wsConn, error) {
	rest := strings.TrimPrefix(wsURL, "ws://")
	hostPort, path, ok := strings.Cut(rest, "/")
	if !ok {
		return nil, fmt.Errorf("非法 WebSocket 地址：%s", wsURL)
	}
	conn, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return nil, err
	}
	// 握手阶段也必须设超时：DialTimeout 只管连接建立，
	// 遇到「接受连接但不回数据」的对端，ReadString 会永久阻塞。
	conn.SetDeadline(time.Now().Add(timeout))
	key := base64.StdEncoding.EncodeToString(randBytes(16))
	handshake := fmt.Sprintf(
		"GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"+
			"Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\n"+
			"Sec-WebSocket-Version: 13\r\n\r\n", path, hostPort, key)
	if _, err := conn.Write([]byte(handshake)); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(statusLine, "101") {
		conn.Close()
		return nil, fmt.Errorf("WebSocket 握手被拒绝：%s", strings.TrimSpace(statusLine))
	}
	for { // 丢弃剩余响应头
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsConn{conn: conn, br: br}, nil
}

// writeFrame 写一个客户端帧（必须掩码）。
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	var buf bytes.Buffer
	buf.WriteByte(0x80 | opcode) // FIN + opcode
	mask := randBytes(4)
	n := len(payload)
	switch {
	case n < 126:
		buf.WriteByte(byte(0x80 | n))
	case n < 65536:
		buf.WriteByte(0x80 | 126)
		binary.Write(&buf, binary.BigEndian, uint16(n))
	default:
		buf.WriteByte(0x80 | 127)
		binary.Write(&buf, binary.BigEndian, uint64(n))
	}
	buf.Write(mask)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i&3]
	}
	buf.Write(masked)
	_, err := w.conn.Write(buf.Bytes())
	return err
}

func (w *wsConn) send(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.writeFrame(0x1, payload)
}

// recv 读取一条完整消息：自动重组分片、自动应答 ping。
func (w *wsConn) recv() (map[string]any, error) {
	var payload []byte
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(w.br, hdr[:]); err != nil {
			return nil, err
		}
		fin := hdr[0]&0x80 != 0
		opcode := hdr[0] & 0x0F
		masked := hdr[1]&0x80 != 0
		ln := uint64(hdr[1] & 0x7F)

		switch ln {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			ln = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return nil, err
			}
			ln = binary.BigEndian.Uint64(ext[:])
		}

		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, maskKey[:]); err != nil {
				return nil, err
			}
		}
		data := make([]byte, ln)
		if _, err := io.ReadFull(w.br, data); err != nil {
			return nil, err
		}
		if masked {
			for i := range data {
				data[i] ^= maskKey[i&3]
			}
		}

		switch opcode {
		case 0x8: // close
			return nil, errors.New("服务端关闭了连接")
		case 0x9: // ping -> pong
			w.writeFrame(0xA, nil)
			continue
		case 0xA: // pong
			continue
		}

		payload = append(payload, data...)
		if fin {
			var out map[string]any
			if err := json.Unmarshal(payload, &out); err != nil {
				return nil, err
			}
			return out, nil
		}
	}
}

func (w *wsConn) close() { w.conn.Close() }

// ============================================================================
// CDP 会话
// ============================================================================

type cdpClient struct {
	ws      *wsConn
	timeout time.Duration
	broken  bool // 超时过：连接里可能残留半条消息，不能再信
}

func newCDP(port int, timeout time.Duration) (*cdpClient, error) {
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	wsURL, err := findPageTarget(base, timeout)
	if err != nil {
		return nil, err
	}
	ws, err := dialWS(wsURL, timeout)
	if err != nil {
		return nil, err
	}
	return &cdpClient{ws: ws, timeout: timeout}, nil
}

// findPageTarget 轮询 /json/list，取第一个 page 类型的 target。
func findPageTarget(base string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/json/list")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var targets []struct {
				Type              string `json:"type"`
				WebSocketDebugger string `json:"webSocketDebuggerUrl"`
			}
			if json.Unmarshal(body, &targets) == nil {
				for _, t := range targets {
					if t.Type == "page" && t.WebSocketDebugger != "" {
						return t.WebSocketDebugger, nil
					}
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return "", errors.New("无法连接 headless 浏览器，请检查浏览器是否可正常启动")
}

// call 发送 CDP 指令并等待对应 id 的应答；事件消息直接丢弃。
//
// 注意：net.DialTimeout 只约束「连接建立」，不约束后续读写。
// 这里必须每次调用都重设 deadline，否则 CDP 一旦不响应，
// recv 里的 io.ReadFull 会无限期阻塞，整个程序看起来就是「卡死」。
func (c *cdpClient) call(method string, params map[string]any) (map[string]any, error) {
	if c.broken {
		return nil, errors.New("CDP 连接已失效，无法继续（请重新运行程序）")
	}
	if c.timeout > 0 {
		c.ws.conn.SetDeadline(time.Now().Add(c.timeout))
	}
	c.ws.id++
	id := c.ws.id
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.ws.send(msg); err != nil {
		return nil, err
	}
	for {
		resp, err := c.ws.recv()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// 超时说明还卡在半条消息上，直接把连接判死，
				// 免得后续读到错位数据、表现成一堆莫名其妙的失败
				c.broken = true
				c.ws.close()
				return nil, fmt.Errorf("CDP %s 响应超时（%s），连接已断开", method, c.timeout)
			}
			return nil, err
		}
		rid, ok := resp["id"].(float64)
		if !ok || int(rid) != id {
			continue
		}
		if e, ok := resp["error"]; ok {
			return nil, fmt.Errorf("CDP %s 失败：%v", method, e)
		}
		if r, ok := resp["result"].(map[string]any); ok {
			return r, nil
		}
		return map[string]any{}, nil
	}
}

func (c *cdpClient) eval(expr string) (any, error) {
	r, err := c.call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  true,
	})
	if err != nil {
		return nil, err
	}
	if _, bad := r["exceptionDetails"]; bad {
		return nil, errors.New("页面 JS 执行异常")
	}
	if res, ok := r["result"].(map[string]any); ok {
		return res["value"], nil
	}
	return nil, nil
}

func (c *cdpClient) evalBool(expr string) bool {
	v, err := c.eval(expr)
	if err != nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

func (c *cdpClient) evalString(expr string) string {
	v, err := c.eval(expr)
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func (c *cdpClient) evalNumber(expr string) float64 {
	v, err := c.eval(expr)
	if err != nil {
		return 0
	}
	f, _ := v.(float64)
	return f
}

func (c *cdpClient) close() {
	if c.ws != nil {
		c.ws.close()
	}
}
