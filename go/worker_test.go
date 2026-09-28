package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCapturer 模拟一个抓取实例：不做真实截图，只落一个标记文件。
type fakeCapturer struct {
	id        int
	mu        sync.Mutex
	written   []string // 本实例写过的文件名
	failEvery bool
	err       error
	latency   time.Duration
}

func (f *fakeCapturer) Capture(pageURL, outPath string) (int, int, int, error) {
	if f.latency > 0 {
		time.Sleep(f.latency)
	}
	if f.failEvery {
		return 0, 0, 0, f.err
	}
	if err := os.WriteFile(outPath, []byte("png"), 0644); err != nil {
		return 0, 0, 0, err
	}
	f.mu.Lock()
	f.written = append(f.written, filepath.Base(outPath))
	f.mu.Unlock()
	return 3, 1280, 900, nil
}

// quietOut 返回一个把输出丢进 io.Discard 的输出器（不写日志文件、不刷测试输出）。
func quietOut(t *testing.T) *Out {
	t.Helper()
	o := NewOut("", 1, 1, true)
	o.SetWriter(io.Discard)
	t.Cleanup(o.Close)
	return o
}

func makeTasks(n int, dir string) []captureTask {
	tasks := make([]captureTask, 0, n)
	taken := map[string]bool{}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("曲-%d", i)
		target := uniquePath(dir, buildFilename(name), taken)
		taken[filepath.Base(target)] = true
		tasks = append(tasks, captureTask{Index: i, Song: Song{ID: fmt.Sprintf("id%d", i), Name: name}, Target: target})
	}
	return tasks
}

// TestRunTasksProcessesEveryTaskExactlyOnce 是并发正确性的第一道关：
// 所有任务都要被处理，且每个目标文件只被写一次（不重不漏）。
func TestRunTasksProcessesEveryTaskExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	caps := map[int]*fakeCapturer{}
	var capsMu sync.Mutex
	opt := pageOptions{
		Workers:    4,
		Out:        quietOut(t),
		AbortAfter: 5,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			c := &fakeCapturer{id: id, latency: 2 * time.Millisecond}
			capsMu.Lock()
			caps[id] = c
			capsMu.Unlock()
			return c, func() {}, nil
		},
	}
	tasks := makeTasks(40, dir)
	savedCount := 0
	opt.OnSaved = func(captureTask) { savedCount++ }

	out := runTasks(context.Background(), opt, newWorkerPool(opt), tasks)

	if out.Saved != 40 || out.Failed != 0 || out.Aborted {
		t.Fatalf("应 40 条全部成功，实际 %+v", out)
	}
	if savedCount != 40 {
		t.Fatalf("OnSaved 应被回调 40 次，实际 %d", savedCount)
	}
	if out.Started != 4 {
		t.Fatalf("应有 4 个实例启动成功，实际 %d", out.Started)
	}

	// 每个目标文件存在且只被一个实例写过
	seen := map[string]int{}
	for _, c := range caps {
		for _, n := range c.written {
			seen[n]++
		}
	}
	if len(seen) != 40 {
		t.Fatalf("应有 40 个不同的文件名，实际 %d", len(seen))
	}
	for n, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("%s 被写了 %d 次，命名唯一性被破坏", n, cnt)
		}
	}
	// 任务确实被分摊到了多个实例上（而不是一个 worker 全干了）
	if len(caps) < 2 {
		t.Fatalf("应有多个实例参与，实际 %d", len(caps))
	}
}

// TestRunTasksOnSavedIsSingleWriter 保证 OnSaved 不会被并发调用 ——
// 这是"断点 State 只有一个写者"这一设计前提的直接验证。
func TestRunTasksOnSavedIsSingleWriter(t *testing.T) {
	dir := t.TempDir()
	var inFlight, maxConcurrent, total int32
	opt := pageOptions{
		Workers:    6,
		Out:        quietOut(t),
		AbortAfter: 99,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return &fakeCapturer{id: id, latency: time.Millisecond}, func() {}, nil
		},
		OnSaved: func(captureTask) {
			n := atomic.AddInt32(&inFlight, 1)
			for {
				m := atomic.LoadInt32(&maxConcurrent)
				if n <= m || atomic.CompareAndSwapInt32(&maxConcurrent, m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond) // 拉长窗口，真有并发就会撞上
			atomic.AddInt32(&inFlight, -1)
			atomic.AddInt32(&total, 1)
		},
	}
	out := runTasks(context.Background(), opt, newWorkerPool(opt), makeTasks(60, dir))
	if out.Saved != 60 {
		t.Fatalf("应 60 条成功，实际 %d", out.Saved)
	}
	if maxConcurrent != 1 {
		t.Fatalf("OnSaved 出现并发调用（最多 %d 个同时在跑），断点状态会有数据竞争", maxConcurrent)
	}
	if total != 60 {
		t.Fatalf("回调总数应为 60，实际 %d", total)
	}
}

// TestRunTasksAbortsAfterConsecutiveFailures 验证"连续失败就提前收工"，
// 且中止后未被处理的任务必须如实计入"未完成"，否则断点会被错误推进（= 漏抓）。
func TestRunTasksAbortsAfterConsecutiveFailures(t *testing.T) {
	dir := t.TempDir()
	opt := pageOptions{
		Workers:    3,
		Out:        quietOut(t),
		AbortAfter: 5,
		Retry:      1,
		Delay:      0,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return &fakeCapturer{id: id, failEvery: true, err: errors.New("模拟失败")}, func() {}, nil
		},
	}
	tasks := makeTasks(90, dir)
	out := runTasks(context.Background(), opt, newWorkerPool(opt), tasks)

	if !out.Aborted {
		t.Fatal("连续失败达到阈值后应标记为已中止")
	}
	if out.Failed < 5 {
		t.Fatalf("至少应有 5 条失败，实际 %d", out.Failed)
	}
	notDone := len(tasks) - out.Saved - out.Failed
	if notDone <= 0 {
		t.Fatalf("中止后应存在未完成任务，实际 saved=%d failed=%d", out.Saved, out.Failed)
	}
	// 用未完成数走一遍判定，确认断点不会前进
	v := judgePage("cur,x", "next,y", out.Failed, notDone, 0, out.Started)
	if v.Advance {
		t.Fatalf("有失败与未完成时断点不得推进：%+v", v)
	}
}

// TestRunTasksAllInstancesFailToStart 全挂了要和"跑完了"区分开。
func TestRunTasksAllInstancesFailToStart(t *testing.T) {
	dir := t.TempDir()
	opt := pageOptions{
		Workers:    3,
		Out:        quietOut(t),
		AbortAfter: 5,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return nil, func() {}, errors.New("浏览器起不来")
		},
	}
	tasks := makeTasks(10, dir)
	out := runTasks(context.Background(), opt, newWorkerPool(opt), tasks)

	if out.Started != 0 {
		t.Fatalf("不应有实例启动成功，实际 %d", out.Started)
	}
	if out.startFail == nil {
		t.Fatal("应回报启动失败原因")
	}
	if out.Saved != 0 || out.Failed != 0 {
		t.Fatalf("没有实例可用时不应有成败计数：%+v", out)
	}
	notDone := len(tasks) - out.Saved - out.Failed
	if notDone != 10 {
		t.Fatalf("10 条都应算未完成，实际 %d", notDone)
	}
}

// TestRunTasksPartialStartFailureStillCompletes 部分实例起不来要降级继续，
// 而不是把整批任务丢掉。
func TestRunTasksPartialStartFailureStillCompletes(t *testing.T) {
	dir := t.TempDir()
	opt := pageOptions{
		Workers:    4,
		Out:        quietOut(t),
		AbortAfter: 99,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			if id%2 == 0 {
				return nil, func() {}, errors.New("偶数号实例起不来")
			}
			return &fakeCapturer{id: id, latency: time.Millisecond}, func() {}, nil
		},
	}
	tasks := makeTasks(24, dir)
	out := runTasks(context.Background(), opt, newWorkerPool(opt), tasks)

	if out.Started != 2 {
		t.Fatalf("应有 2 个实例存活，实际 %d", out.Started)
	}
	if out.Saved != 24 {
		t.Fatalf("剩余实例应把 24 条全部做完，实际 %d", out.Saved)
	}
	if out.startFail == nil {
		t.Fatal("应记录启动失败原因，便于提示用户降速")
	}
}

// TestRunTasksCancelledByContext 验证 Ctrl+C：上下文取消后 worker 要尽快退出，
// 未处理的任务原样留在队列里（→ 计为未完成 → 断点不动）。
func TestRunTasksCancelledByContext(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	opt := pageOptions{
		Workers:    2,
		Out:        quietOut(t),
		AbortAfter: 99,
		Retry:      1,
		Delay:      0,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return &fakeCapturer{id: id, latency: 5 * time.Millisecond}, func() {}, nil
		},
	}
	tasks := makeTasks(200, dir)
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	out := runTasks(ctx, opt, newWorkerPool(opt), tasks)
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("取消后应迅速返回，实际耗时 %v", elapsed)
	}
	if out.Saved >= len(tasks) {
		t.Fatalf("取消得够快就不该全跑完（saved=%d）", out.Saved)
	}
	if out.Saved+out.Failed >= len(tasks) {
		t.Fatalf("应有未完成任务：saved=%d failed=%d", out.Saved, out.Failed)
	}
}

// TestRunTasksWorkerCountCappedByTaskCount 任务比 worker 少时不要空转。
func TestRunTasksWorkerCountCappedByTaskCount(t *testing.T) {
	dir := t.TempDir()
	startedIDs := []int{}
	var mu sync.Mutex
	opt := pageOptions{
		Workers:    8,
		Out:        quietOut(t),
		AbortAfter: 99,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			mu.Lock()
			startedIDs = append(startedIDs, id)
			mu.Unlock()
			return &fakeCapturer{id: id}, func() {}, nil
		},
	}
	out := runTasks(context.Background(), opt, newWorkerPool(opt), makeTasks(2, dir))
	if out.Saved != 2 {
		t.Fatalf("应完成 2 条，实际 %d", out.Saved)
	}
	if out.Started != 2 {
		t.Fatalf("只该启动 2 个实例，实际 %d", out.Started)
	}
}

// TestRetryOnAccessBlocked 验证"被登录墙拦下 → 换 UA 重试 → 最终成功"。
func TestRetryOnAccessBlocked(t *testing.T) {
	dir := t.TempDir()
	c := &flakyCapturer{failTimes: 2}
	opt := pageOptions{
		Workers:      1,
		Out:          quietOut(t),
		Retry:        3,
		Delay:        0,
		RetryBackoff: time.Millisecond,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return c, func() {}, nil
		},
	}
	out := runTasks(context.Background(), opt, newWorkerPool(opt), makeTasks(1, dir))
	if out.Saved != 1 || out.Failed != 0 {
		t.Fatalf("重试后应成功：%+v", out)
	}
	if c.calls != 3 {
		t.Fatalf("应调用 3 次（2 次被拦 + 1 次成功），实际 %d", c.calls)
	}
	if !strings.Contains(c.lastURL, "/jianpu/id1.html") {
		t.Fatalf("详情页地址应正确，实际 %s", c.lastURL)
	}
}

type flakyCapturer struct {
	failTimes int
	calls     int
	lastURL   string
}

func (f *flakyCapturer) Capture(pageURL, outPath string) (int, int, int, error) {
	f.calls++
	f.lastURL = pageURL
	if f.calls <= f.failTimes {
		return 0, 0, 0, errAccessBlocked
	}
	if err := os.WriteFile(outPath, []byte("png"), 0644); err != nil {
		return 0, 0, 0, err
	}
	return 3, 1280, 900, nil
}

// ---------------------------------------------------------------------------
// 实例池：复用、淘汰坏实例、幂等关闭
// ---------------------------------------------------------------------------

// TestPoolReusesInstancesAcrossBatches 是"profile 不再堆积"的回归测试。
//
// 每遍都新建/销毁浏览器时，一趟 10 轮会留下几十份 Chrome profile
// （删一份要 20 秒），用户会看到满目录的 _browser_profile_*。
// 复用后，实例数量应固定为 --workers 个。
func TestPoolReusesInstancesAcrossBatches(t *testing.T) {
	dir := t.TempDir()
	made := 0
	var mu sync.Mutex
	opt := pageOptions{
		Workers:    2,
		Out:        quietOut(t),
		AbortAfter: 99,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			mu.Lock()
			made++
			mu.Unlock()
			return &fakeCapturer{id: id}, func() {}, nil
		},
	}
	pool := newWorkerPool(opt)
	defer pool.Close()

	for batch := 0; batch < 4; batch++ {
		out := runTasks(context.Background(), opt, pool, makeTasks(6, dir))
		if out.Saved != 6 {
			t.Fatalf("第 %d 批应有 6 条成功，实际 %d", batch+1, out.Saved)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if made != 2 {
		t.Fatalf("4 批任务只该创建 2 个实例（--workers 2），实际创建了 %d 个", made)
	}
}

// TestPoolDropsUnhealthyInstances 坏掉的实例不能再用，否则会把后续每条都拖垮。
func TestPoolDropsUnhealthyInstances(t *testing.T) {
	dir := t.TempDir()
	closed := 0
	var mu sync.Mutex
	opt := pageOptions{
		Workers:    1,
		Out:        quietOut(t),
		AbortAfter: 99,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return &unhealthyCapturer{}, func() {
				mu.Lock()
				closed++
				mu.Unlock()
			}, nil
		},
	}
	pool := newWorkerPool(opt)
	defer pool.Close()

	runTasks(context.Background(), opt, pool, makeTasks(3, dir))
	mu.Lock()
	got := closed
	mu.Unlock()
	if got == 0 {
		t.Fatal("不健康的实例应被销毁，而不是放回池子复用")
	}
	if pool.Live() != 0 {
		t.Fatalf("销毁后池内不应再有实例，实际 %d", pool.Live())
	}
}

type unhealthyCapturer struct{}

func (u *unhealthyCapturer) Capture(pageURL, outPath string) (int, int, int, error) {
	if err := os.WriteFile(outPath, []byte("png"), 0644); err != nil {
		return 0, 0, 0, err
	}
	return 3, 1280, 900, nil
}
func (u *unhealthyCapturer) Healthy() bool { return false }

func TestPoolCloseIsIdempotent(t *testing.T) {
	opt := pageOptions{Workers: 1, Out: quietOut(t), capturerFactory: func(id int) (Capturer, func(), error) {
		return &fakeCapturer{id: id}, func() {}, nil
	}}
	pool := newWorkerPool(opt)
	pool.Close()
	pool.Close() // 必须安全：收尾路径会调用多次
	if _, err := pool.acquire(context.Background()); err == nil {
		t.Fatal("已关闭的池子不该还能借出实例")
	}
}

// ---------------------------------------------------------------------------
// 未就绪必须判失败 —— 绝不能把空白图当成"抓到了"
// ---------------------------------------------------------------------------

// TestRetryableClassifiesErrors 只有"换 UA 有意义"的错误才重试。
func TestRetryableClassifiesErrors(t *testing.T) {
	if !retryable(errAccessBlocked) {
		t.Fatal("UA 额度用尽应可重试")
	}
	if !retryable(fmt.Errorf("%w：详情", errNotReady)) {
		t.Fatal("未就绪应可重试（换 UA 往往就过了）")
	}
	if retryable(errors.New("CDP 连接已失效")) {
		t.Fatal("连接坏掉重试没意义")
	}
	if retryable(context.Canceled) {
		t.Fatal("上下文取消不该重试")
	}
}

// TestNotReadyIsFailureAndNeverSaved 是本次修复的核心回归：
// 页面渲染不出来时要判失败、要换 UA 重试、**绝不能**写进存档。
func TestNotReadyIsFailureAndNeverSaved(t *testing.T) {
	dir := t.TempDir()
	opt := pageOptions{
		Workers:      2,
		Out:          quietOut(t),
		AbortAfter:   99,
		Retry:        3,
		RetryBackoff: time.Millisecond,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return &notReadyCapturer{}, func() {}, nil
		},
	}
	saved := 0
	opt.OnSaved = func(captureTask) { saved++ }
	out := runTasks(context.Background(), opt, newWorkerPool(opt), makeTasks(4, dir))

	if out.Saved != 0 || saved != 0 {
		t.Fatalf("未就绪的页面绝不能算成功：saved=%d 回调=%d", out.Saved, saved)
	}
	if out.Failed != 4 {
		t.Fatalf("4 条都应记为失败，实际 %d", out.Failed)
	}
	if out.NotReady != 4 {
		t.Fatalf("应识别出 4 条是「未渲染出乐谱」，实际 %d", out.NotReady)
	}
	// 目录里不能出现任何图片
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			t.Fatalf("失败条目不该留下文件：%s", e.Name())
		}
	}
}

type notReadyCapturer struct{ calls int32 }

func (n *notReadyCapturer) Capture(pageURL, outPath string) (int, int, int, error) {
	atomic.AddInt32(&n.calls, 1)
	return 0, 0, 0, fmt.Errorf("%w：25 秒内未加载完（readyState=interactive）", errNotReady)
}

// TestNotReadyCircuitBreakerAbortsRun 连续未就绪要提前中止，
// 而不是把 900 条一口气全跑成 25 秒 × 900 的空转。
func TestNotReadyCircuitBreakerAbortsRun(t *testing.T) {
	dir := t.TempDir()
	opt := pageOptions{
		Workers:    1,
		Out:        quietOut(t),
		AbortAfter: 5,
		Retry:      1,
		capturerFactory: func(id int) (Capturer, func(), error) {
			return &notReadyCapturer{}, func() {}, nil
		},
	}
	tasks := makeTasks(100, dir)
	start := time.Now()
	out := runTasks(context.Background(), opt, newWorkerPool(opt), tasks)
	if !out.Aborted {
		t.Fatal("连续未就绪应触发提前中止")
	}
	if out.Failed >= len(tasks) {
		t.Fatalf("应中止而不是跑完全部 %d 条", len(tasks))
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("中止应迅速，实际耗时 %v", time.Since(start))
	}
}
