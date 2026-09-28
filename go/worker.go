package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 并发抓取：多浏览器实例 + 实例池
// ============================================================================
//
// 为什么是"多个浏览器进程"而不是"一个浏览器开多个标签页"：
//   - 站点按 User-Agent 计免费额度，每个实例独立持有一条 CDP 连接、
//     自己 rotateIdentity()，等价于多个互不干扰的访客，额度天然分散；
//   - 截图是渲染密集型任务，独立的 Chrome 进程才能吃到多核；
//   - 复用同一浏览器多标签页需要 Target.attachToTarget 的扁平会话，
//     会让 cdp.go 的"一问一答 + id 匹配"模型变复杂，风险大于收益。
//
// 并发安全边界（这是本改动最容易出错的地方，逐条守住）：
//   1. 文件名去重（taken / uniquePath）只在"派发阶段"由主 goroutine 串行完成，
//      每个任务的目标路径提前算好，worker 不再碰 taken；
//   2. 断点文件（State）只由主 goroutine 在收集结果时读写，天然不存在并发写；
//      saveState 内部再加锁与重试兜底；
//   3. 所有打印都走 Out 的互斥锁，避免多 worker 的行互相插入；
//   4. profile 目录与调试端口按实例序号隔离，否则多个 Chrome 会抢同一个 profile；
//   5. 浏览器实例由 workerPool 统一借用/归还，跨"遍"和"轮"复用（见 pool.go）。

// captureTask 一个待抓任务（目标路径已由主 goroutine 定好，worker 只负责干活）。
type captureTask struct {
	Index  int // 本次运行的全局序号，从 1 开始
	Song   Song
	Target string
}

// captureResult 一条任务的执行结果。
type captureResult struct {
	Task   captureTask
	Worker int
	Size   int
	W, H   int
	Err    error

	// 分阶段耗时，用于在日志里指出瓶颈（等页面 vs 截图渲染）
	Wait time.Duration
	Shot time.Duration
}

// timingReporter 由真正使用浏览器的实现提供，便于把耗时透传到日志。
type timingReporter interface {
	LastTiming() (wait, shot time.Duration)
}

// Capturer 是"把一条任务变成一张截图"的能力。
// 生产实现是 *Browser；测试可注入假实现，从而在没有 Chrome 的环境下
// 验证并发行为（回调次数、中止传播、命名唯一性、单写者保证）。
type Capturer interface {
	Capture(pageURL, outPath string) (size, width, height int, err error)
}

// pageOptions 一次并发执行的参数。
type pageOptions struct {
	BrowserExe string
	WorkDir    string
	Width      int
	Headful    bool
	Cookie     string
	Workers    int
	Delay      float64
	Retry      int
	AbortAfter int
	Out        *Out
	// OnSaved 在"某条成功落盘"时被回调。它由收集协程（即调用 runTasks 的那个
	// goroutine）串行调用，因此回调里可以直接改 State，无需加锁。
	OnSaved func(t captureTask)
	// OnFailed 在"某条这一遍没抓成"时被回调，同样由收集协程串行调用。
	OnFailed func(t captureTask, err error)
	// RetryBackoff 命中访问限制后的等待时长；<=0 时用默认的 1200ms。
	RetryBackoff time.Duration
	// capturerFactory 仅用于测试注入；为 nil 时走真实 headless 浏览器。
	capturerFactory func(workerID int) (Capturer, func(), error)
}

// taskOutcome 一批任务的执行结果。
type taskOutcome struct {
	Saved    int
	Failed   int
	Aborted  bool // 连续失败达阈值，主动中止
	Started  int  // 本轮实际参与工作的实例数
	NotReady int  // 其中因"页面没渲染出乐谱"而失败的条数（站点变慢/被限速的信号）
	// FailedTasks 是这一遍失败的条目，供调用方**在同一轮内**再补抓一遍。
	// 抽到已耗尽的 UA、或站点一时抖动导致的失败都是暂时的，
	// 换一批 UA 往往就过了，没必要留到下次运行。
	FailedTasks []captureTask
	startFail   error
}

// runTasks 并发执行一批截图任务。
//
// 任务队列在开始时一次性填满并关闭，因此不存在"派发 goroutine 卡住"的泄漏；
// 被中止时没被领走的任务自然留在 channel 里，调用方用
// len(tasks) - Saved - Failed 即可得到"未完成"的数量。
//
// 浏览器实例来自传入的实例池（跨遍、跨轮复用），本函数只负责"借用—干活—归还"，
// 不负责创建与销毁 —— 否则一趟 10 轮会在磁盘上建出几十份 Chrome profile。
func runTasks(ctx context.Context, opt pageOptions, pool *workerPool, tasks []captureTask) taskOutcome {
	var out taskOutcome
	if len(tasks) == 0 {
		return out
	}
	workers := opt.Workers
	if workers > len(tasks) {
		workers = len(tasks)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	taskCh := make(chan captureTask, len(tasks))
	for _, t := range tasks {
		taskCh <- t
	}
	close(taskCh)

	resultCh := make(chan captureResult, workers)

	var startedCnt int32
	var startErrMu sync.Mutex
	var startErr error

	var wg sync.WaitGroup
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runWorker(ctx, opt, pool, taskCh, resultCh); err != nil {
				startErrMu.Lock()
				if startErr == nil {
					startErr = err
				}
				startErrMu.Unlock()
				return
			}
			atomic.AddInt32(&startedCnt, 1)
		}()
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	failStreak, notReadyStreak := 0, 0
	for r := range resultCh {
		if r.Err != nil {
			out.Failed++
			out.FailedTasks = append(out.FailedTasks, r.Task)
			failStreak++
			if errors.Is(r.Err, errNotReady) {
				out.NotReady++
				notReadyStreak++
			} else {
				notReadyStreak = 0
			}
			if opt.OnFailed != nil {
				opt.OnFailed(r.Task, r.Err)
			}
			opt.Out.Printf("  [%3d] W%d ✗ %s 失败：%v\n", r.Task.Index, r.Worker, r.Task.Song.Name, r.Err)
			if !out.Aborted && notReadyStreak >= opt.AbortAfter {
				out.Aborted = true
				opt.Out.Important("\n  连续 %d 条页面都没能渲染出乐谱，已提前中止。\n"+
					"  这说明站点当前处于变慢/限速状态，继续跑只会一批批失败。\n"+
					"  建议：等几分钟再跑，并把 --delay 调大（如 3）。断点未推进，本次成绩不会丢。\n",
					notReadyStreak)
				cancel()
			} else if !out.Aborted && failStreak >= opt.AbortAfter {
				out.Aborted = true
				opt.Out.Important("\n  连续 %d 条失败，已提前中止本页。\n"+
					"  请检查网络是否正常，或先结束残留的 chrome.exe / msedge.exe 再重试。\n", failStreak)
				cancel()
			}
			continue
		}
		out.Saved++
		failStreak, notReadyStreak = 0, 0
		if opt.OnSaved != nil {
			opt.OnSaved(r.Task)
		}
		opt.Out.Printf("  [%3d] W%d ✓ %s  (%dx%d, %d KB, 加载 %.1fs + 截图 %.1fs)\n",
			r.Task.Index, r.Worker, filepath.Base(r.Task.Target), r.W, r.H, r.Size/1024,
			r.Wait.Seconds(), r.Shot.Seconds())
	}
	out.Started = int(atomic.LoadInt32(&startedCnt))
	startErrMu.Lock()
	out.startFail = startErr
	startErrMu.Unlock()

	// 页面渲染不出来（errNotReady）不是"这几条曲谱的问题"，而是站点整体变慢/限速。
	// 必须显式告警：否则用户看到的是"大量失败 + 卡住"，很容易误以为是程序坏了。
	// 关键区别：这些条目**没有**被抓到、也没写进存档，下次运行会自动重试。
	if out.NotReady > 0 {
		opt.Out.Important("\n  ! 本轮有 %d 条因为「页面没渲染出乐谱」而失败。\n"+
			"    这通常是站点变慢或被限速，不是这些曲谱本身有问题。\n"+
			"    它们没有写入存档，下次运行会自动重试（不会留下空白图）。\n"+
			"    建议：稍等几分钟，并把 --delay 调大（如 3）。\n", out.NotReady)
	}
	return out
}

// runWorker 一个 worker：向实例池借一个抓取实例，循环领取任务，结束时归还。
// 返回非 nil 表示实例启动失败（该 worker 直接退出，任务留给其他 worker）。
func runWorker(ctx context.Context, opt pageOptions, pool *workerPool, taskCh <-chan captureTask, resultCh chan<- captureResult) error {
	w, err := pool.acquire(ctx)
	if err != nil {
		return err
	}
	defer pool.release(w)
	cap := w.cap

	for {
		var t captureTask
		select {
		case <-ctx.Done():
			return nil
		case v, ok := <-taskCh:
			if !ok {
				return nil
			}
			t = v
		}
		size, hw, hh, err := captureWithRetry(ctx, cap, t, w.id, opt)
		var wait, shot time.Duration
		if tr, ok := cap.(timingReporter); ok {
			wait, shot = tr.LastTiming()
		}
		select {
		case resultCh <- captureResult{
			Task: t, Worker: w.id, Size: size, W: hw, H: hh, Err: err,
			Wait: wait, Shot: shot,
		}:
		case <-ctx.Done():
			return nil
		}
		if err == nil && opt.Delay > 0 {
			select {
			case <-time.After(time.Duration(opt.Delay * float64(time.Second))):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// retryable 判断这个错误是否值得换 UA 再试一次。
//
// 值得重试的两类：
//   - errAccessBlocked：这条 UA 的额度用完了 —— 换脸就能继续；
//   - errNotReady：页面没渲染出乐谱 —— 多半是站点抖动或这条连接不好。
//
// 其余（CDP 断了、写盘失败等）重试也是白搭。
func retryable(err error) bool {
	return errors.Is(err, errAccessBlocked) || errors.Is(err, errNotReady)
}

// captureWithRetry 单条截图 + 可重试错误时换 UA 重试。
func captureWithRetry(ctx context.Context, cap Capturer, t captureTask, workerID int, opt pageOptions) (int, int, int, error) {
	var size, w, h int
	var lastErr error
	pageURL := siteBase + "/jianpu/" + t.Song.ID + ".html"
	for attempt := 1; attempt <= opt.Retry; attempt++ {
		size, w, h, lastErr = cap.Capture(pageURL, t.Target)
		if lastErr == nil {
			return size, w, h, nil
		}
		if retryable(lastErr) && attempt < opt.Retry {
			backoff := opt.RetryBackoff
			if backoff <= 0 {
				backoff = 1200 * time.Millisecond
			}
			if errors.Is(lastErr, errNotReady) {
				opt.Out.Printf("  [%3d] W%d 页面未渲染出乐谱，换 UA 重试 (%d/%d)\n",
					t.Index, workerID, attempt, opt.Retry-1)
			} else {
				opt.Out.Printf("  [%3d] W%d %v；已换 UA 重试 (%d/%d)\n",
					t.Index, workerID, lastErr, attempt, opt.Retry-1)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return 0, 0, 0, ctx.Err()
			}
			continue
		}
		break
	}
	return size, w, h, lastErr
}

// summarizeFailures 把一批失败原因压缩成一行，供收尾/日志展示。
func summarizeFailures(msgs []string) string {
	if len(msgs) == 0 {
		return ""
	}
	seen := map[string]int{}
	var order []string
	for _, m := range msgs {
		key := m
		if len(key) > 60 {
			key = key[:60] + "…"
		}
		if _, ok := seen[key]; !ok {
			order = append(order, key)
		}
		seen[key]++
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if seen[k] > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", k, seen[k]))
		} else {
			parts = append(parts, k)
		}
	}
	return strings.Join(parts, "；")
}
