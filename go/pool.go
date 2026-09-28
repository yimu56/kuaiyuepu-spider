package main

import (
	"context"
	"fmt"
	"sync"
)

// ============================================================================
// 抓取实例池：让浏览器跨"遍"和"轮"复用
// ============================================================================
//
// 为什么必须复用：`--pages 10` + 轮内补抓 2 遍，一趟最多会走 30 次
// "启动浏览器 → 用一会儿 → 销毁"。每个实例都会在磁盘上留下一份 Chrome profile
// （实测删除一份要 20 秒），于是：
//   - 工作目录里会攒下几十个 _browser_profile_* 目录；
//   - 后台删除根本追不上创建速度；
//   - 每遍还要多花 0.5 秒启动浏览器。
//
// 改成"整趟运行只创建 N 个实例（N = --workers），一直复用到收尾"之后，
// 这些开销都消失了。每个实例在每条截图前都会换 UA + 清 Cookie，
// 所以复用不会带来身份串味的问题。

type poolWorker struct {
	id     int
	cap    Capturer
	closer func()
}

// workerPool 管理复用中的抓取实例。并发安全。
type workerPool struct {
	mu     sync.Mutex
	opt    pageOptions
	idle   []*poolWorker
	nextID int // 只增不减：实例序号唯一，避免端口/profile 被回收后撞车
	live   int // 当前存活实例数
	closed bool
}

func newWorkerPool(opt pageOptions) *workerPool {
	if opt.Workers < 1 {
		opt.Workers = 1
	}
	return &workerPool{opt: opt}
}

// acquire 取一个可用实例：优先复用空闲的，没有就新建。
//
// 同时借用实例的 goroutine 数不会超过 --workers，所以不需要额外的数量闸门；
// 序号只增不减 —— 若失败后回收序号，端口/profile 目录会跟着被复用，
// 而"某个序号启动失败"往往是稳定复现的，会造成一直重试同一个坏序号。
func (p *workerPool) acquire(ctx context.Context) (*poolWorker, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("抓取实例池已关闭")
	}
	if n := len(p.idle); n > 0 {
		w := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return w, nil
	}
	id := p.nextID + 1 // 从 1 开始，日志里显示 W1、W2…
	p.nextID++
	p.mu.Unlock()

	w, err := p.start(id)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.live++
	p.mu.Unlock()
	return w, nil
}

func (p *workerPool) start(id int) (*poolWorker, error) {
	if p.opt.capturerFactory != nil {
		cap, closer, err := p.opt.capturerFactory(id)
		if err != nil {
			return nil, err
		}
		return &poolWorker{id: id, cap: cap, closer: closer}, nil
	}
	br := NewBrowser(p.opt.BrowserExe, p.opt.WorkDir, p.opt.Width, p.opt.Headful, p.opt.Cookie, id)
	if err := br.Start(); err != nil {
		return nil, fmt.Errorf("W%d 浏览器启动失败：%w", id, err)
	}
	return &poolWorker{id: id, cap: br, closer: func() {
		br.Close()
		br.CleanupProfile()
	}}, nil
}

// release 归还实例。健康状态不佳的直接销毁，不复用坏连接。
func (p *workerPool) release(w *poolWorker) {
	if w == nil {
		return
	}
	healthy := true
	if h, ok := w.cap.(interface{ Healthy() bool }); ok {
		healthy = h.Healthy()
	}
	p.mu.Lock()
	if p.closed || !healthy {
		p.live--
		p.mu.Unlock()
		w.closer()
		return
	}
	p.idle = append(p.idle, w)
	p.mu.Unlock()
}

// Live 返回当前存活的实例数（报告用）。
func (p *workerPool) Live() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live
}

// Close 关掉所有实例并收掉它们的 profile。收尾（含 Ctrl+C）时必须调用：
// 进程一旦退出，defer 和 goroutine 都不再有机会干活。
func (p *workerPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.live -= len(idle)
	p.mu.Unlock()

	for _, w := range idle {
		w.closer()
	}
}
