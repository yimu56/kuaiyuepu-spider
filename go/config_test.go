package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 配置优先级：命令行 > 配置文件 > 默认值
// ---------------------------------------------------------------------------

func TestLoadConfigAppliesFileValues(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ConfigFileName)
	body := `{
	  "pages": 7,
	  "workers": 4,
	  "delay": 0.8,
	  "log_file": "",
	  "quiet": true,
	  "state_flush_every": 25
	}`
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	s, found, err := loadConfig(p)
	if err != nil || !found {
		t.Fatalf("loadConfig 失败：found=%v err=%v", found, err)
	}
	if s.Pages != 7 || s.Workers != 4 || s.StateFlushEvery != 25 {
		t.Fatalf("配置未生效：%+v", s)
	}
	if s.Delay != 0.8 {
		t.Fatalf("delay = %v，期望 0.8", s.Delay)
	}
	if !s.Quiet {
		t.Fatal("quiet 未生效")
	}
	// 关键：配置里写空字符串代表"关闭日志"，不能被默认值覆盖回去
	if s.LogFile != "" {
		t.Fatalf("log_file 应为空（关闭日志），实际 %q", s.LogFile)
	}
	// 配置里没写的字段保持默认
	if s.Limit != pageSize {
		t.Fatalf("limit 应保持默认 %d，实际 %d", pageSize, s.Limit)
	}
	if s.Retry != 3 {
		t.Fatalf("retry 应保持默认 3，实际 %d", s.Retry)
	}
	if s.Width != 1280 {
		t.Fatalf("width 应保持默认 1280，实际 %d", s.Width)
	}
}

func TestLoadConfigMissingFileIsNotAnError(t *testing.T) {
	s, found, err := loadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件不存在不应报错：%v", err)
	}
	if found {
		t.Fatal("不应标记为已找到配置文件")
	}
	if s != DefaultSettings() {
		t.Fatalf("应返回默认值，实际 %+v", s)
	}
}

func TestPreScanConfig(t *testing.T) {
	cases := []struct {
		args []string
		want string
		ok   bool
	}{
		{[]string{"--config", "a.json"}, "a.json", true},
		{[]string{"-config", "b.json"}, "b.json", true},
		{[]string{"--config=c.json"}, "c.json", true},
		{[]string{"-config=d.json"}, "d.json", true},
		{[]string{"--workers", "3", "--config", "e.json"}, "e.json", true},
		{[]string{"--workers", "3"}, "", false},
		{[]string{"--"}, "", false},
		{[]string{"--config"}, "", false},
	}
	for _, c := range cases {
		got, ok := preScanConfig(c.args)
		if got != c.want || ok != c.ok {
			t.Fatalf("preScanConfig(%v) = (%q,%v)，期望 (%q,%v)", c.args, got, ok, c.want, c.ok)
		}
	}
}

// TestFlagOverridesConfig 覆盖"命令行显式给值要压过配置文件"这条规则。
// 难点在于 flag 无法区分"显式写了默认值"和"没写"，所以实现里用 flag.Visit。
func TestFlagOverridesConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfg, []byte(`{"pages": 5, "workers": 2, "delay": 2.5}`), 0644); err != nil {
		t.Fatal(err)
	}
	s, _, err := loadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	oldArgs := os.Args
	os.Args = []string{"kuaiyuepu-crawler.exe", "--pages", "9", "--delay", "0.5"}
	defer func() { os.Args = oldArgs }()

	apply := bindFlags(&s)
	apply()
	s.normalize(dir)

	if s.Pages != 9 {
		t.Fatalf("命令行应覆盖 pages 为 9，实际 %d", s.Pages)
	}
	if s.Delay != 0.5 {
		t.Fatalf("命令行应覆盖 delay 为 0.5，实际 %v", s.Delay)
	}
	// workers 没在命令行出现 → 保留配置文件里的 2（不是默认值 3）
	if s.Workers != 2 {
		t.Fatalf("未在命令行出现的 workers 应沿用配置文件值 2，实际 %d", s.Workers)
	}
}

func TestSettingsNormalize(t *testing.T) {
	s := Settings{Pages: -1, Limit: -5, Workers: 99, Retry: 0, Delay: -1, Width: 10, LogMaxMB: 0, StateFlushEvery: 0, AbortAfterFails: 0}
	s.normalize("C:\\work")
	if s.Pages != 0 {
		t.Fatalf("pages 应被收敛为 0（不限），实际 %d", s.Pages)
	}
	if s.Limit != 1 {
		t.Fatalf("limit 下限应为 1，实际 %d", s.Limit)
	}
	if s.Workers != 8 {
		t.Fatalf("workers 上限应为 8，实际 %d", s.Workers)
	}
	if s.Retry != 1 || s.AbortAfterFails != 1 || s.LogMaxMB != 1 || s.StateFlushEvery != 1 {
		t.Fatalf("下限未收敛：%+v", s)
	}
	if s.Delay != 0 {
		t.Fatalf("delay 应被收敛为 0，实际 %v", s.Delay)
	}
	if s.Width != 320 {
		t.Fatalf("width 下限应为 320，实际 %d", s.Width)
	}
	if s.Out != "C:\\work" {
		t.Fatalf("空 out 应落到 exe 目录，实际 %q", s.Out)
	}
	if s.StatePath != filepath.Join("C:\\work", "crawl_state.json") {
		t.Fatalf("空 state 应落到 exe 目录，实际 %q", s.StatePath)
	}
}

// TestNormalizeAbsolutizesPaths 是回归测试：Chrome 拿到相对的 --user-data-dir
// 会照常启动但不监听调试端口，导致"连不上浏览器"这种极难排查的故障。
func TestNormalizeAbsolutizesPaths(t *testing.T) {
	s := Settings{Out: filepath.Join("rel", "out"), StatePath: filepath.Join("rel", "s.json")}
	s.normalize("C:\\work")
	if !filepath.IsAbs(s.Out) {
		t.Fatalf("输出目录应被绝对化，实际 %q", s.Out)
	}
	if !filepath.IsAbs(s.StatePath) {
		t.Fatalf("断点文件路径应被绝对化，实际 %q", s.StatePath)
	}
	if !strings.HasSuffix(s.Out, filepath.Join("rel", "out")) {
		t.Fatalf("绝对化不应改变相对结构，实际 %q", s.Out)
	}
}

func TestNewBrowserProfileDirIsAbsoluteAndIsolated(t *testing.T) {
	b1 := NewBrowser("chrome.exe", filepath.Join("rel", "out"), 1280, false, "", 1)
	b2 := NewBrowser("chrome.exe", filepath.Join("rel", "out"), 1280, false, "", 2)
	if !filepath.IsAbs(b1.profileDir) {
		t.Fatalf("profile 目录必须绝对化，实际 %q", b1.profileDir)
	}
	if b1.profileDir == b2.profileDir {
		t.Fatal("不同 worker 的 profile 目录必须不同，否则 Chrome 会互相抢目录")
	}
	if !strings.Contains(b1.profileDir, ProfilePrefix) {
		t.Fatalf("profile 目录须带 %s 前缀以便启动时清理，实际 %q", ProfilePrefix, b1.profileDir)
	}
	base := func(p string) string { return strings.TrimSuffix(filepath.Base(p), ".del") }
	if !strings.Contains(base(b1.profileDir), "_1_") || !strings.Contains(base(b2.profileDir), "_2_") {
		t.Fatalf("profile 目录须带 worker 序号：%q / %q", b1.profileDir, b2.profileDir)
	}
	// 实例序号必须递增：否则异步清理可能删掉刚建好的新 profile
	b3 := NewBrowser("chrome.exe", "rel/out", 1280, false, "", 1)
	if base(b1.profileDir) == base(b3.profileDir) {
		t.Fatalf("同一 worker 的不同实例也必须拿到不同目录：%q", b1.profileDir)
	}
	// 端口段也必须错开
	if b1.port == b2.port {
		t.Fatalf("不同 worker 的调试端口不应相同：%d", b1.port)
	}
}

// ---------------------------------------------------------------------------
// 断点推进判定 —— 正确性的枢纽
// ---------------------------------------------------------------------------

func TestJudgePageAdvancesOnlyWhenFullyClean(t *testing.T) {
	cur := "2026-05-03 18:42:57,ouSJw1i0m"
	next := "2026-05-02 11:00:00,aaBBcc11x"

	// 全部完成且整页抓完 → 推进，继续下一轮
	v := judgePage(cur, next, 0, 0, 0, 3)
	if !v.Advance || v.Stop || v.Next != next {
		t.Fatalf("全部完成时应推进：%+v", v)
	}

	// 有失败 → 不动、停止
	v = judgePage(cur, next, 1, 0, 0, 3)
	if v.Advance || !v.Stop || v.Next != cur {
		t.Fatalf("有失败时不应推进：%+v", v)
	}
	if !strings.Contains(v.Reason, "1 条失败") {
		t.Fatalf("原因应说明失败条数：%s", v.Reason)
	}

	// 有未完成（中断 / 实例启动失败）→ 不动、停止
	v = judgePage(cur, next, 0, 4, 0, 1)
	if v.Advance || !v.Stop || v.Next != cur {
		t.Fatalf("有未完成时不应推进：%+v", v)
	}

	// limit 截断了本页 → 不推进，但**继续**（下一轮重拉本页接着抓）。
	// 这保证了 --limit 小于一页时 --pages 依然有效，同时不会漏抓。
	v = judgePage(cur, next, 0, 0, 60, 3)
	if v.Advance || v.Stop || !v.SamePage || v.Next != cur {
		t.Fatalf("本页有剩余时应保持游标并继续（漏抓防护）：%+v", v)
	}
	if !strings.Contains(v.RoundNote, "60") {
		t.Fatalf("应提示本页还剩多少条：%s", v.RoundNote)
	}

	// 一个实例都没起来 → 停止
	v = judgePage(cur, next, 0, 0, 0, 0)
	if v.Advance || !v.Stop || v.Next != cur {
		t.Fatalf("无可用实例时应停止：%+v", v)
	}
}

func TestJudgePageAtLastPage(t *testing.T) {
	// 末页且整页抓完 → 清空游标（下次从最新开始），停止
	v := judgePage("cur,x", "", 0, 0, 0, 3)
	if !v.Stop || !v.Advance || v.Next != "" {
		t.Fatalf("末页全部完成时应清空游标：%+v", v)
	}
	// 末页有失败 → 保留原游标，下次还能补抓这一页
	v = judgePage("cur,x", "", 2, 0, 0, 3)
	if !v.Stop || v.Advance || v.Next != "cur,x" {
		t.Fatalf("末页有失败时应保留原游标：%+v", v)
	}
	// 末页且 limit 截断 → 游标不动但继续补本页
	v = judgePage("cur,x", "", 0, 0, 30, 3)
	if v.Stop || v.Advance || !v.SamePage {
		t.Fatalf("末页有剩余时应继续补本页：%+v", v)
	}
	// 从"最新开始"的首次运行，末页有失败 → 游标仍为空
	v = judgePage("", "", 2, 0, 0, 3)
	if v.Next != "" || v.Advance {
		t.Fatalf("游标本就为空时应保持为空：%+v", v)
	}
}

// ---------------------------------------------------------------------------
// 日志轮转与写失败降级
// ---------------------------------------------------------------------------

func TestRotatingWriterRotatesAndBoundsTotalSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	// 上限 1 MB、保留 2 份 → 最多 3 个文件，总量有界
	rw, err := newRotatingWriter(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()

	line := strings.Repeat("a", 4096) + "\n"
	for i := 0; i < 900; i++ { // ≈ 3.6 MB
		if _, err := rw.Write([]byte(line)); err != nil {
			t.Fatalf("写入不应报错：%v", err)
		}
	}
	rw.Flush()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("应保留 run.log / .1 / .2 共 3 个文件，实际 %d 个：%v", len(entries), names)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("超出保留份数的历史文件应被删除")
	}
	// 每个文件都不应超过上限（允许一个 bufio 缓冲块的超出）
	for _, e := range entries {
		info, _ := e.Info()
		if info.Size() > 1<<20+64<<10 {
			t.Fatalf("%s 体积 %d 超出上限", e.Name(), info.Size())
		}
	}
}

func TestRotatingWriterNeverErrorsAfterClose(t *testing.T) {
	dir := t.TempDir()
	rw, err := newRotatingWriter(filepath.Join(dir, "run.log"), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	rw.Close() // 模拟文件被外部占用/句柄失效
	n, err := rw.Write([]byte("x"))
	if err != nil {
		t.Fatalf("日志写失败不得向上抛错，否则会打断抓取：%v", err)
	}
	if n != 1 {
		t.Fatalf("字节数应为 1，实际 %d", n)
	}
}

// TestRotatingWriterWritesThroughWithoutClose 是回归测试。
//
// 曾经的实现用 bufio 缓冲，而程序收尾走 os.Exit —— os.Exit 不执行 defer，
// 于是缓冲区里的日志永远没落盘，run.log 恒为 0 字节。
// 这里刻意**不调用 Close/Flush**，直接读文件，确保日志是直写的。
func TestRotatingWriterWritesThroughWithoutClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	rw, err := newRotatingWriter(path, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()

	if _, err := rw.Write([]byte("第一行\n第二行\n")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "第一行\n第二行\n" {
		t.Fatalf("未 Close 时日志也应已落盘，实际读到 %q", string(b))
	}
}

func TestNewOutWritesToLogWithoutClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	o := NewOut(path, 1, 1, false)
	o.SetWriter(io.Discard) // 测试里不往标准输出灌内容
	defer o.Close()
	o.Important("汇总行 %d\n", 42)
	o.Printf("明细行\n")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "汇总行 42") || !strings.Contains(got, "明细行") {
		t.Fatalf("日志文件内容不完整：%q", got)
	}
}

// TestOutQuietStillWritesToLogFile 校验 --quiet 的语义：
// 控制台安静，但日志文件仍记录全部内容。
func TestOutQuietStillWritesToLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.log")
	o := NewOut(path, 1, 1, true)
	defer o.Close()
	var console strings.Builder
	o.SetWriter(&console)

	o.Printf("这条只该进文件\n")
	o.Important("这条两边都要有\n")

	if strings.Contains(console.String(), "只该进文件") {
		t.Fatalf("quiet 模式下普通日志不应出现在控制台：%q", console.String())
	}
	if !strings.Contains(console.String(), "两边都要有") {
		t.Fatalf("重要日志在 quiet 模式下也必须显示：%q", console.String())
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	if !strings.Contains(got, "只该进文件") || !strings.Contains(got, "两边都要有") {
		t.Fatalf("日志文件应记录全部内容：%q", got)
	}
}

func TestNewOutWithUnwritableLogPathFallsBackToConsole(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// 把日志路径设在一个普通文件下面 → 必定打不开
	o := NewOut(filepath.Join(blocker, "sub", "run.log"), 1, 1, false)
	o.SetWriter(io.Discard)
	if o.LogPath() != "" {
		t.Fatal("打不开日志时应降级为无日志文件")
	}
	o.Printf("仍然可以正常输出 %d\n", 1) // 不应 panic
	o.Close()
}

// ---------------------------------------------------------------------------
// 断点文件：原子写、备份、损坏处理
// ---------------------------------------------------------------------------

func TestStateWriterKeepsBackupAndValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crawl_state.json")
	sw := newStateWriter(path)

	st := newState()
	st.Shots["aaa"] = "A-甲.png"
	if err := sw.save(st); err != nil {
		t.Fatal(err)
	}
	st.Shots["bbb"] = "B-乙.png"
	if err := sw.save(st); err != nil {
		t.Fatal(err)
	}

	// 第二次保存前，原始文件应已备份
	if !fileExists(path + ".bak") {
		t.Fatal("覆盖已有断点文件前应留一份 .bak")
	}
	var back State
	b, _ := os.ReadFile(path + ".bak")
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf(".bak 应为合法 JSON：%v", err)
	}
	if len(back.Shots) != 1 {
		t.Fatalf(".bak 应保留上一次的内容（1 条），实际 %d 条", len(back.Shots))
	}

	var cur State
	b, _ = os.ReadFile(path)
	if err := json.Unmarshal(b, &cur); err != nil {
		t.Fatalf("主文件应为合法 JSON：%v", err)
	}
	if len(cur.Shots) != 2 {
		t.Fatalf("主文件应为最新内容（2 条），实际 %d 条", len(cur.Shots))
	}
	if fileExists(path + ".tmp") {
		t.Fatal("成功落盘后不应残留 .tmp")
	}
	if sw.size() <= 0 {
		t.Fatal("应记录最近一次写入的体积，供体积告警使用")
	}
}

func TestStateWriterConcurrentSaveStaysValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crawl_state.json")
	sw := newStateWriter(path)
	st := newState()
	st.Shots["seed"] = "S-种子.png"

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				sw.save(st)
			}
		}()
	}
	wg.Wait()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got State
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("并发保存后文件必须仍是完整 JSON：%v", err)
	}
	if len(got.Shots) != 1 {
		t.Fatalf("内容应完好，实际 %d 条", len(got.Shots))
	}
}

func TestLoadStateCorruptBacksUpInsteadOfSilentReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crawl_state.json")
	if err := os.WriteFile(path, []byte(`{"shots": {`), 0644); err != nil {
		t.Fatal(err)
	}
	st := loadState(path)
	if !st.corrupt {
		t.Fatal("损坏的断点文件应被标记，而不是静默重置")
	}
	if !fileExists(path + ".bad") {
		t.Fatal("损坏文件应另存为 .bad 供排查")
	}
	if fileExists(path) {
		t.Fatal("损坏文件应被移走")
	}
	if st.NextSortPoint != "" || len(st.Shots) != 0 {
		t.Fatalf("应返回干净状态，实际 %+v", st)
	}
}

func TestLoadStateReadsLegacyVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crawl_state.json")
	// 旧版（version 1，没有 pages 字段）必须还能读
	old := `{"version":1,"next_sort_point":"2026-01-01 00:00:00,abc","runs":3,"total_items":270,"total_shots":268,"shots":{"x1":"X-甲.png"}}`
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	st := loadState(path)
	if st.corrupt {
		t.Fatal("旧版断点文件不应被判为损坏")
	}
	if st.NextSortPoint != "2026-01-01 00:00:00,abc" || st.Runs != 3 || len(st.Shots) != 1 {
		t.Fatalf("旧版数据应完整保留：%+v", st)
	}
	if st.Pages != 0 {
		t.Fatalf("旧版没有 pages 字段，应为 0，实际 %d", st.Pages)
	}
	if st.Version != stateVersion {
		t.Fatalf("版本应升级到 %d，实际 %d", stateVersion, st.Version)
	}
}

func TestPruneMissingShots(t *testing.T) {
	dir := t.TempDir()
	// 只有 A 真的存在
	if err := os.WriteFile(filepath.Join(dir, "A-甲.png"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	st := newState()
	st.Shots["a"] = "A-甲.png"
	st.Shots["b"] = "B-乙.png"
	st.Shots["c"] = "C-丙.png"

	n := pruneMissingShots(st, dir)
	if n != 2 {
		t.Fatalf("应清理 2 条失效记录，实际 %d", n)
	}
	if len(st.Shots) != 1 || st.Shots["a"] != "A-甲.png" {
		t.Fatalf("存在的记录必须保留：%+v", st.Shots)
	}
}

// ---------------------------------------------------------------------------
// 并发派发：同批同名不得撞车
// ---------------------------------------------------------------------------

// TestDispatchAllocatesUniqueNamesSerially 复刻 main 里派发阶段的取名逻辑：
// 同名歌曲必须在**派发前**就拿到互不相同的目标路径，否则并发 worker
// 会同时往同一个文件写，最终留下谁的截图全看运气。
func TestDispatchAllocatesUniqueNamesSerially(t *testing.T) {
	dir := t.TempDir()
	songs := []Song{
		{ID: "id1", Name: "大梦归"},
		{ID: "id2", Name: "大梦归"},
		{ID: "id3", Name: "大梦归"},
		{ID: "id4", Name: "小池"},
	}
	taken := map[string]bool{}
	var targets []string
	for _, s := range songs {
		p := uniquePath(dir, buildFilename(s.Name), taken)
		taken[filepath.Base(p)] = true
		targets = append(targets, filepath.Base(p))
	}
	want := []string{"D-大梦归.png", "D-大梦归_2.png", "D-大梦归_3.png", "X-小池.png"}
	for i, w := range want {
		if targets[i] != w {
			t.Fatalf("第 %d 个目标应为 %s，实际 %s", i+1, w, targets[i])
		}
	}
	// 磁盘上已有的文件也要被让开
	if err := os.WriteFile(filepath.Join(dir, want[3]), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(uniquePath(dir, "X-小池.png", map[string]bool{})); got != "X-小池_2.png" {
		t.Fatalf("应避开磁盘上已存在的同名文件，实际 %s", got)
	}
}

// TestPortIsolationBetweenWorkers 保证并发实例不会抢同一个调试端口。
func TestPortIsolationBetweenWorkers(t *testing.T) {
	seen := map[int]int{}
	for id := 1; id <= 8; id++ {
		p := freePort(basePort + (id-1)*portStride)
		seen[p]++
	}
	// 端口是"探测后释放"，理论上可能撞，但错开 stride 后不应出现同段重复
	if seen[freePort(basePort)] > 1 {
		t.Fatalf("worker 端口段发生重叠：%v", seen)
	}
	if basePort+7*portStride != 9411+140 {
		t.Fatalf("端口步长被意外改动：%d", portStride)
	}
}

// TestPagesDescRendering 只是防止有人把 -pages 0（不限）的描述写错。
func TestPagesDescRendering(t *testing.T) {
	if got := pagesDesc(0, 90); !strings.Contains(got, "不限") {
		t.Fatalf("pages=0 应描述为不限，实际 %q", got)
	}
	if got := pagesDesc(3, 90); !strings.Contains(got, "3 轮") {
		t.Fatalf("pages=3 应描述为 3 轮，实际 %q", got)
	}
}

func TestWriteExampleConfigIsValidAndLoadable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ConfigFileName)
	if err := writeExampleConfig(p); err != nil {
		t.Fatal(err)
	}
	s, found, err := loadConfig(p)
	if err != nil || !found {
		t.Fatalf("生成的配置文件应能被自己读回：found=%v err=%v", found, err)
	}
	if s != DefaultSettings() {
		t.Fatalf("生成的配置应与默认值一致：\n%+v\n%+v", s, DefaultSettings())
	}
}

// 让 gofmt 别把 fmt 判成未使用（断言失败信息里用到）
var _ = fmt.Sprintf

// ---------------------------------------------------------------------------
// 失败归档与"放弃"机制 —— 让 --pages 真正能跑满 N 轮的关键
// ---------------------------------------------------------------------------

// TestStateGiveUpAfterRepeatedFailures 验证"失败够遍数就放过"。
//
// 没有这个机制时的真实故障：一页里有 3 条反复失败的曲谱，
// "--pages 10" 只跑得动第 1 轮，因为游标永远不推进。
func TestStateGiveUpAfterRepeatedFailures(t *testing.T) {
	st := newState()
	const id = "abc123"

	if st.givenUp(id, 3) {
		t.Fatal("没失败过就不该算放弃")
	}
	if got := st.noteFailure(id, "页面被重定向到登录页"); got != 1 {
		t.Fatalf("首次失败计数应为 1，实际 %d", got)
	}
	if st.givenUp(id, 3) {
		t.Fatal("只失败 1 次不该放弃")
	}
	st.noteFailure(id, "页面被重定向到登录页")
	st.noteFailure(id, "页面被重定向到登录页")
	if !st.givenUp(id, 3) {
		t.Fatal("累计失败 3 次后应记为放弃")
	}
	if st.Failed[id].Reason == "" || st.Failed[id].Last == "" {
		t.Fatalf("失败记录应带原因与时间：%+v", st.Failed[id])
	}

	// 阈值 0 = 永不放弃（严格模式）
	if st.givenUp(id, 0) {
		t.Fatal("阈值为 0 时不应放弃任何条目")
	}

	// 抓到之后应清掉历史失败记录
	st.clearFailure(id)
	if _, ok := st.Failed[id]; ok {
		t.Fatal("成功后应清掉失败记录")
	}
	if st.givenUp(id, 3) {
		t.Fatal("清掉之后不该还算放弃")
	}
}

// TestFailureRecordPersistsAcrossRuns 失败次数必须跨运行累计，
// 否则每次启动都从 0 开始，永远达不到放弃阈值，游标照样卡死。
func TestFailureRecordPersistsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crawl_state.json")
	sw := newStateWriter(path)

	st := loadState(path)
	st.noteFailure("song1", "页面被重定向到登录页")
	sw.save(st)

	st2 := loadState(path)
	if got := st2.Failed["song1"].Tries; got != 1 {
		t.Fatalf("失败次数应跨运行保留，实际 %d", got)
	}
	st2.noteFailure("song1", "页面被重定向到登录页")
	st2.noteFailure("song1", "页面被重定向到登录页")
	if !st2.givenUp("song1", 3) {
		t.Fatal("跨运行累计 3 次后应记为放弃")
	}
}

// TestLoadStateReadsVersion2WithoutFailedField 老断点文件（没有 failed 字段）要能读。
func TestLoadStateReadsVersion2WithoutFailedField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crawl_state.json")
	old := `{"version":2,"next_sort_point":"2024-10-06 19:21:59,VpOxs1ETy","runs":9,"pages":2,"total_items":578,"total_shots":630,"shots":{"a1":"A-甲.png"}}`
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}
	st := loadState(path)
	if st.corrupt {
		t.Fatal("v2 文件不该被判为损坏")
	}
	if st.NextSortPoint != "2024-10-06 19:21:59,VpOxs1ETy" || len(st.Shots) != 1 {
		t.Fatalf("数据应完整保留：%+v", st)
	}
	if st.Failed == nil {
		t.Fatal("Failed 应初始化为空 map，避免后续写入 panic")
	}
	if st.Version != stateVersion {
		t.Fatalf("版本应升级到 %d，实际 %d", stateVersion, st.Version)
	}
	if st.givenUp("a1", 3) {
		t.Fatal("老文件里没有失败记录，不该判定为放弃")
	}
}

// TestSelectPendingHandlesGivenUpItems 校验 pending 过滤的真实语义：
// 已放弃的条目要从待抓队列里剔除（不再阻塞游标），但**不能**写进 shots。
func TestSelectPendingHandlesGivenUpItems(t *testing.T) {
	dir := t.TempDir()
	st := newState()
	st.Shots["done1"] = "D-已抓.png"
	// 截图必须真的在磁盘上才会被跳过
	if err := os.WriteFile(filepath.Join(dir, "D-已抓.png"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	items := []Song{
		{ID: "done1", Name: "已抓"},
		{ID: "stuck", Name: "反复失败"},
		{ID: "fresh", Name: "正常"},
	}
	for i := 0; i < 3; i++ {
		st.noteFailure("stuck", "页面被重定向到登录页")
	}

	sel := selectPending(items, st, dir, false, 3)
	if len(sel.Todo) != 1 || sel.Todo[0].ID != "fresh" {
		t.Fatalf("待抓队列应只剩 fresh，实际 %+v", sel.Todo)
	}
	if sel.Skipped != 1 || sel.GaveUp != 1 {
		t.Fatalf("应统计 已抓1/放弃1，实际 %d/%d", sel.Skipped, sel.GaveUp)
	}
	if _, ok := st.Shots["stuck"]; ok {
		t.Fatal("被放弃不等于已抓到，绝不能写进 shots（否则会永久漏抓）")
	}

	// --retry-failed：已放弃的条目要重新纳入
	sel = selectPending(items, st, dir, true, 3)
	if len(sel.Todo) != 2 || sel.GaveUp != 0 {
		t.Fatalf("--retry-failed 时该把 stuck 重新纳入，实际 todo=%d gaveUp=%d", len(sel.Todo), sel.GaveUp)
	}

	// 关闭放弃机制（阈值 0）：永不放行
	sel = selectPending(items, st, dir, false, 0)
	if len(sel.Todo) != 2 || sel.GaveUp != 0 {
		t.Fatalf("阈值 0 时不该放弃任何条目，实际 todo=%d gaveUp=%d", len(sel.Todo), sel.GaveUp)
	}
}

// TestNormalizeClampsFailureKnobs 失败处置相关参数的边界。
func TestNormalizeClampsFailureKnobs(t *testing.T) {
	s := Settings{RetryPasses: -1, GiveUpAfter: -5}
	s.normalize(t.TempDir())
	if s.RetryPasses != 0 {
		t.Fatalf("retry_passes 下限应为 0，实际 %d", s.RetryPasses)
	}
	if s.GiveUpAfter != 0 {
		t.Fatalf("give_up_after 下限应为 0（永不放弃），实际 %d", s.GiveUpAfter)
	}
	s = Settings{RetryPasses: 99}
	s.normalize(t.TempDir())
	if s.RetryPasses != 10 {
		t.Fatalf("retry_passes 上限应为 10，实际 %d", s.RetryPasses)
	}
	// UA 池文件路径应绝对化
	s = Settings{UAPoolFile: filepath.Join("rel", "ua.txt")}
	s.normalize("C://work")
	if s.UAPoolFile != "" && !filepath.IsAbs(s.UAPoolFile) {
		t.Fatalf("UA 池路径应绝对化，实际 %q", s.UAPoolFile)
	}
	// 显式传空字符串 = 关闭持久化，必须原样保留
	s = Settings{UAPoolFile: ""}
	s.normalize("C://work")
	if s.UAPoolFile != "" {
		t.Fatalf("空字符串应表示关闭持久化，实际 %q", s.UAPoolFile)
	}
}

// TestStubbornItemEventuallyStopsBlockingAdvance 把 main 主循环里的判定序列
// 按真实顺序串起来跑一遍：selectPending → 失败 → noteFailure → judgePage。
//
// 它守卫的正是本次修复的那个故障：页面上有一条反复失败的曲谱时，
// "--pages 10" 只跑得动第 1 轮（游标永远不推进）。
func TestStubbornItemEventuallyStopsBlockingAdvance(t *testing.T) {
	const (
		id          = "stuck01"
		giveUpAfter = 3
		cur         = "2024-10-06 19:21:59,VpOxs1ETy"
		next        = "2024-05-29 20:08:45,N1ICs1FQm"
	)
	items := []Song{{ID: id, Name: "反复失败的曲谱"}}

	// run 模拟一次完整运行的主循环，预置的失败遍数由 preTries 给出。
	run := func(preTries int) (advancedAtRound int, tries int) {
		dir := t.TempDir()
		st := newState()
		for i := 0; i < preTries; i++ {
			st.noteFailure(id, "页面被重定向到登录页")
		}
		for round := 1; round <= 10; round++ {
			sel := selectPending(items, st, dir, false, giveUpAfter)
			if len(sel.Todo) == 0 {
				// 上一轮（或上一轮运行）已放弃 → 本页没有阻塞失败
				if sel.GaveUp != 1 {
					t.Fatalf("应统计到 1 条被放弃，实际 %d", sel.GaveUp)
				}
				v := judgePage(cur, next, 0, 0, 0, 1)
				if !v.Advance || v.Next != next {
					t.Fatalf("失败项已放弃后应推进游标，实际 %+v", v)
				}
				return round, st.Failed[id].Tries
			}

			st.noteFailure(id, "页面被重定向到登录页")
			blocking := 0
			if !st.givenUp(id, giveUpAfter) {
				blocking = 1
			}
			v := judgePage(cur, next, blocking, blocking, 0, 1)
			if blocking > 0 {
				if v.Advance || !v.Stop || v.Next != cur {
					t.Fatalf("第 %d 轮：还有阻塞失败时不得推进游标，实际 %+v", round, v)
				}
				continue
			}
			// 本轮刚好跨过放弃阈值：不再阻塞，应立刻放行
			if !v.Advance || v.Next != next {
				t.Fatalf("第 %d 轮：达到放弃阈值后就应放行游标，实际 %+v", round, v)
			}
			return round, st.Failed[id].Tries
		}
		t.Fatal("10 轮之内应靠放弃机制走出来，否则 --pages 永远跑不满")
		return 0, 0
	}

	// 情形一：从一个干净的断点开始，第 3 轮跨过阈值
	round, tries := run(0)
	if round != giveUpAfter || tries != giveUpAfter {
		t.Fatalf("应在第 %d 轮放行（失败遍数 %d），实际第 %d 轮/%d 遍", giveUpAfter, giveUpAfter, round, tries)
	}
	// 情形二：上次运行已经失败到阈值 —— 本次第一轮就该直接放行
	round, tries = run(giveUpAfter)
	if round != 1 || tries != giveUpAfter {
		t.Fatalf("已达阈值的条目应在本轮立刻放行，实际第 %d 轮/%d 遍", round, tries)
	}
}

// TestFailuresMustNeverBeSilentlyMarkedAsDone 守住那条最危险的不变量：
// 无论失败重试多少遍，只要没真抓到，就绝不能出现在 shots 里。
func TestFailuresMustNeverBeSilentlyMarkedAsDone(t *testing.T) {
	dir := t.TempDir()
	st := newState()
	st.Shots["ok1"] = "O-抓到的.png"
	if err := os.WriteFile(filepath.Join(dir, "O-抓到的.png"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	items := []Song{{ID: "ok1", Name: "抓到的"}, {ID: "bad1", Name: "抓不到的"}}
	for i := 0; i < 12; i++ { // 反复失败远超阈值
		st.noteFailure("bad1", "页面被重定向到登录页")
	}
	sel := selectPending(items, st, dir, false, 3)
	if len(sel.Todo) != 0 {
		t.Fatalf("两条都不该再进待抓队列，实际 %+v", sel.Todo)
	}
	if _, ok := st.Shots["bad1"]; ok {
		t.Fatal("抓不到就是抓不到，绝不能写进 shots")
	}
	if len(st.Shots) != 1 {
		t.Fatalf("shots 只该有那条真抓到的，实际 %+v", st.Shots)
	}
	if st.Failed["bad1"].Reason == "" {
		t.Fatal("放弃的条目必须留下失败原因，方便事后用 --cookie 补抓")
	}
}

// ---------------------------------------------------------------------------
// profile 清理：这两条是真实故障的回归
// ---------------------------------------------------------------------------

// TestDirExistsIsNotFileExists 钉住一个真实踩过的坑。
//
// fileExists() 刻意排除目录，对目录永远返回 false。
// 清理 profile 目录时误用了它 → 整段清理逻辑直接短路 →
// 工作目录里堆满清不掉的 _browser_profile_*。
func TestDirExistsIsNotFileExists(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "somedir")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "somefile")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if fileExists(sub) {
		t.Fatal("fileExists 对目录必须返回 false（它就是为此才排除目录的）")
	}
	if !dirExists(sub) {
		t.Fatal("dirExists 对存在的目录必须返回 true")
	}
	if dirExists(file) {
		t.Fatal("dirExists 对普通文件必须返回 false")
	}
	if !fileExists(file) {
		t.Fatal("fileExists 对普通文件必须返回 true")
	}
	if dirExists(filepath.Join(dir, "nope")) || fileExists(filepath.Join(dir, "nope")) {
		t.Fatal("不存在的路径两个函数都该返回 false")
	}
}

// TestCleanupProfileActuallyRemovesDirectory 直接断言清理真的发生了。
//
// 顺带钉住一条设计约定：**截图目录里永远不出现 profile 目录** ——
// profile 一律放系统临时目录，否则删不干净的 _browser_profile_*
// 会长期堆在用户看得到的地方。
func TestCleanupProfileActuallyRemovesDirectory(t *testing.T) {
	work := t.TempDir()
	b := NewBrowser("chrome.exe", work, 1280, false, "", 1)
	prof := b.profileDir
	if !strings.HasPrefix(prof, ProfileRoot()) {
		t.Fatalf("profile 必须放在系统临时目录下，实际 %s", prof)
	}
	if strings.HasPrefix(prof, work) {
		t.Fatalf("profile 不该出现在截图目录里：%s", prof)
	}
	if err := os.MkdirAll(filepath.Join(prof, "Default"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prof, "Default", "Preferences"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if !dirExists(prof) {
		t.Fatal("profile 目录应已创建")
	}

	b.CleanupProfile()

	// 同步部分（改名）必须立刻完成，所以原名字下不能再看到它
	if dirExists(prof) {
		t.Fatalf("清理后原目录名的 profile 目录仍可见：%s", prof)
	}
	// 截图目录里绝不能留下任何 profile 目录
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ProfilePrefix) {
			t.Fatalf("截图目录里不该出现 profile 目录：%s", e.Name())
		}
	}
	b.CleanupProfile() // 重复调用必须安全
}

// TestCleanupStaleProfilesHidesLeftoversImmediately 启动清理要先改名，
// 保证"启动完成后就看不到 _browser_profile_* 了"，即使后台删除很慢。
func TestCleanupStaleProfilesHidesLeftoversImmediately(t *testing.T) {
	if err := os.MkdirAll(ProfileRoot(), 0755); err != nil {
		t.Fatal(err)
	}
	// 模拟上次运行的残留：PID 不是当前进程，而且已经放了很久
	old := filepath.Join(ProfileRoot(), ProfilePrefix+"_999999_1_1")
	os.RemoveAll(old)
	os.RemoveAll(old + profileDelSuffix) // 清掉上一次测试可能留下的
	if err := os.MkdirAll(old, 0755); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	CleanupStaleProfiles()

	if dirExists(old) {
		t.Fatal("启动清理后不该还能看到上次遗留的 profile 目录")
	}
	CleanupStaleProfiles() // 幂等
}

// TestCleanupStaleProfilesKeepsFreshOnes 刚出现的目录可能是并发实例正在用的，
// 不能乱删 —— 否则两个实例同时跑会互相破坏 profile。
func TestCleanupStaleProfilesKeepsFreshOnes(t *testing.T) {
	if err := os.MkdirAll(ProfileRoot(), 0755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(ProfileRoot(), ProfilePrefix+"_888888_1_1")
	if err := os.MkdirAll(fresh, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(fresh)

	CleanupStaleProfiles()

	if !dirExists(fresh) {
		t.Fatal("刚创建的 profile 目录不该被启动清理删掉（可能是并发实例在用）")
	}
}

// TestProfilePathsAreAbsolute 相对路径的 --user-data-dir 会让 Chrome
// 静默不监听调试端口（见 README），所以这条必须绝对化。
func TestProfilePathsAreAbsolute(t *testing.T) {
	b := NewBrowser("chrome.exe", "rel/out", 1280, false, "", 1)
	if !filepath.IsAbs(b.profileDir) {
		t.Fatalf("profile 目录必须绝对化，实际 %q", b.profileDir)
	}
	if !filepath.IsAbs(ProfileRoot()) {
		t.Fatalf("profile 根目录必须绝对化，实际 %q", ProfileRoot())
	}
}

// TestCleanupLegacyProfilesCleansOutputDir 旧版把 profile 建在截图目录、
// 又删不完，留下 _browser_profile_*。新版必须能顺手把它们清掉，
// 否则升级上来的用户目录里那堆垃圾会一直留着。
func TestCleanupLegacyProfilesCleansOutputDir(t *testing.T) {
	work := t.TempDir()
	legacy := filepath.Join(work, ProfilePrefix+"_12345_1_1")
	if err := os.MkdirAll(filepath.Join(legacy, "Default"), 0755); err != nil {
		t.Fatal(err)
	}
	alsoLegacy := filepath.Join(work, ProfilePrefix+"_12345_1_2.del")
	if err := os.MkdirAll(alsoLegacy, 0755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(work, "D-大梦归.png")
	if err := os.WriteFile(keep, []byte("png"), 0644); err != nil {
		t.Fatal(err)
	}

	CleanupLegacyProfiles(work)

	// 改名是同步的 → 立刻就该看不见了
	if dirExists(legacy) {
		t.Fatal("截图目录里的旧版 profile 残留应被同步改名，不该还看得见")
	}
	// 已经是 .del 形态的走后台删除，给它一点时间
	for i := 0; i < 40 && dirExists(alsoLegacy); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if dirExists(alsoLegacy) {
		t.Fatal("后台删除应清掉 .del 残留")
	}
	if !fileExists(keep) {
		t.Fatal("清理 profile 时绝不能碰到曲谱图片")
	}
	CleanupLegacyProfiles(work) // 幂等
}

// TestPruneOptionsAreIndependent 两个清理开关必须互不牵连。
//
// 真实场景：用户把截图挪到了别的目录，于是 outPath 下 900 条"文件缺失"，
// 只有 10 条是真空白图。如果只想清那 10 条，绝不能被顺带清掉 900 条 ——
// 那会让程序把它们全部重抓一遍。
func TestPruneOptionsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	// 10 张正常图、2 张空白图，其余 88 条文件不在磁盘
	st := newState()
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("ok_%02d.png", i)
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 50<<10), 0644); err != nil {
			t.Fatal(err)
		}
		st.Shots[fmt.Sprintf("ok%d", i)] = name
	}
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("blank_%02d.png", i)
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 3522), 0644); err != nil {
			t.Fatal(err)
		}
		st.Shots[fmt.Sprintf("blank%d", i)] = name
	}
	for i := 0; i < 88; i++ {
		st.Shots[fmt.Sprintf("gone%d", i)] = fmt.Sprintf("gone_%02d.png", i)
	}

	// 只开"清空白图"
	res := pruneShots(st, dir, pruneOpts{MinBytes: 8 << 10}, 12)
	if res.Blank != 2 {
		t.Fatalf("应清掉 2 条空白图，实际 %d", res.Blank)
	}
	if res.Missing != 0 {
		t.Fatalf("没开 Missing 就不该统计缺失条目，实际 %d", res.Missing)
	}
	if len(st.Shots) != 98 {
		t.Fatalf("只该少 2 条（100→98），实际剩 %d 条", len(st.Shots))
	}
	if _, ok := st.Shots["gone0"]; !ok {
		t.Fatal("没开 Missing 时，文件缺失的条目必须原样保留")
	}

	// 只开"清缺失"
	res2 := pruneShots(st, dir, pruneOpts{Missing: true}, 0)
	if res2.Missing != 88 || res2.Blank != 0 {
		t.Fatalf("应清 88 条缺失、0 条空白，实际 %d/%d", res2.Missing, res2.Blank)
	}
	if len(st.Shots) != 10 {
		t.Fatalf("应剩下 10 条正常图，实际 %d", len(st.Shots))
	}
}
