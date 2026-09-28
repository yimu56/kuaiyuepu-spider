package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ============================================================================
// 运行日志：按大小轮转，写失败绝不影响抓取
// ============================================================================
//
// 之所以要专门做一层，是因为"日志/状态文件无限增长"会连带拖垮主流程：
//   磁盘写满 → 断点落盘失败 → 本次进度丢失 → 下次重抓；
//   日志被编辑器/杀软/网盘同步占用 → Write 报错 → 若直接向上抛就会中断抓取。
// 这里的策略是：
//   1. 超过阈值就轮转，最新永远在 run.log，历史依次 .1/.2/…，总占用有上限；
//   2. 单条日志写入失败只记一次到 stderr，之后静默降级，绝不中断抓取；
//   3. 轮转时 Windows 上 rename 不能覆盖已存在文件，先删目标再改名。

// rotatingWriter 是带大小轮转的 io.Writer。
//
// 这里**故意不做缓冲**：缓冲版曾导致 run.log 恒为 0 字节 —— 程序收尾走的是 os.Exit，
// 而 os.Exit 不会执行 defer，缓冲区里的日志永远没机会落盘。
// 日志量本来就不大（一小时也就几千行），直写的系统调用开销可以忽略。
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int

	f      *os.File
	size   int64
	warned bool // 写失败只提醒一次，避免刷屏
}

func newRotatingWriter(path string, maxMB, keep int) (*rotatingWriter, error) {
	rw := &rotatingWriter{
		path:     path,
		maxBytes: int64(maxMB) << 20,
		keep:     keep,
	}
	if err := rw.open(); err != nil {
		return nil, err
	}
	return rw, nil
}

func (rw *rotatingWriter) open() error {
	if dir := filepath.Dir(rw.path); dir != "" {
		os.MkdirAll(dir, 0755)
	}
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	rw.f = f
	rw.size = info.Size()
	return nil
}

// Write 永不返回错误：日志写不进去是次要故障，不能让它打断抓取。
func (rw *rotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f == nil {
		return len(p), nil
	}
	n, err := rw.f.Write(p)
	rw.size += int64(n)
	if err != nil {
		rw.reportOnce(fmt.Sprintf("日志写入失败（%v），已降级为仅控制台输出", err))
		return len(p), nil
	}
	if rw.size >= rw.maxBytes {
		if err := rw.rotateLocked(); err != nil {
			rw.reportOnce(fmt.Sprintf("日志轮转失败（%v），将继续写入当前文件", err))
		}
	}
	return len(p), nil
}

// Flush 保证已写内容落到磁盘。直写下只是 Sync，作为收尾时的保险留着。
func (rw *rotatingWriter) Flush() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f != nil {
		rw.f.Sync()
	}
}

func (rw *rotatingWriter) Close() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f != nil {
		rw.f.Close()
	}
	rw.f = nil
}

// rotateLocked 依次改名：run.log.(keep-1) → run.log.keep，…，run.log → run.log.1。
// 调用方必须已持有锁。
func (rw *rotatingWriter) rotateLocked() error {
	if rw.f != nil {
		rw.f.Close()
		rw.f = nil
	}
	if rw.keep > 0 {
		// 最旧的一份直接丢弃
		os.Remove(rw.path + "." + strconv.Itoa(rw.keep))
		for i := rw.keep - 1; i >= 1; i-- {
			src := rw.path + "." + strconv.Itoa(i)
			dst := rw.path + "." + strconv.Itoa(i+1)
			if !fileExists(src) {
				continue
			}
			// Windows 的 os.Rename 不会覆盖已存在的目标，先删
			os.Remove(dst)
			os.Rename(src, dst)
		}
		os.Remove(rw.path + ".1")
		os.Rename(rw.path, rw.path+".1")
	}
	// 重新打开（O_APPEND + O_CREATE 会新建空文件）
	return rw.open()
}

func (rw *rotatingWriter) reportOnce(msg string) {
	if rw.warned {
		return
	}
	rw.warned = true
	fmt.Fprintf(os.Stderr, "! %s\n", msg)
}

// ============================================================================
// 统一输出：控制台 + 日志，且保证多 worker 下每行不交错
// ============================================================================

// Out 串行化所有输出。并发写同一终端时，多个 goroutine 的 Printf
// 会互相插进对方的行里，导致日志没法读（尤其是多 worker 模式）。
type Out struct {
	mu    sync.Mutex
	w     io.Writer
	rot   *rotatingWriter
	quiet bool
}

// NewOut 构造输出器。logPath 为空则不写日志文件。
func NewOut(logPath string, logMaxMB, logKeep int, quiet bool) *Out {
	o := &Out{w: os.Stdout, quiet: quiet}
	if logPath != "" {
		rw, err := newRotatingWriter(logPath, logMaxMB, logKeep)
		if err != nil {
			fmt.Fprintf(os.Stderr, "! 无法打开日志文件 %s：%v（本次仅输出到控制台）\n", logPath, err)
		} else {
			o.rot = rw
			o.w = io.MultiWriter(os.Stdout, rw)
		}
	}
	return o
}

// Printf 输出一条普通日志（quiet 模式下只写日志文件，不刷屏）。
func (o *Out) Printf(format string, a ...any) {
	o.emit(true, format, a...)
}

// Important 输出必须让用户看到的内容（汇总、警告），quiet 也照常显示。
func (o *Out) Important(format string, a ...any) {
	o.emit(false, format, a...)
}

func (o *Out) emit(respectQuiet bool, format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	o.mu.Lock()
	defer o.mu.Unlock()
	if respectQuiet && o.quiet {
		if o.rot != nil {
			o.rot.Write([]byte(line))
		}
		return
	}
	fmt.Fprint(o.w, line)
}

// LogPath 返回实际使用的日志文件路径（空表示未启用）。
func (o *Out) LogPath() string {
	if o.rot == nil {
		return ""
	}
	return o.rot.path
}

// SetWriter 只替换"控制台"这一路输出（日志文件那一路不受影响）。
// 主要给测试用：把噪音丢进 io.Discard。
func (o *Out) SetWriter(w io.Writer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rot != nil {
		o.w = io.MultiWriter(w, o.rot)
	} else {
		o.w = w
	}
}

func (o *Out) Close() {
	if o.rot != nil {
		o.rot.Close()
	}
}

// CleanupStaleLogs 清理历史轮转文件（换日志路径时用，避免旧文件堆着）。
func CleanupStaleLogs(path string, keep int) {
	if path == "" {
		return
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return
	}
	base := filepath.Base(path) + "."
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), base) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), base))
		if err != nil || n <= 0 || n > keep {
			os.Remove(filepath.Join(filepath.Dir(path), e.Name()))
		}
	}
}
