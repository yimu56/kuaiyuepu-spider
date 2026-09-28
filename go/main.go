package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const banner = "===================================================================="

// interrupted 标记收到过 Ctrl+C：收尾时用它决定退出码，并跳过"推进断点"。
var interrupted atomic.Bool

func main() {
	exePath, err := os.Executable()
	if err != nil {
		exePath = os.Args[0]
	}
	exeDir := filepath.Dir(exePath)

	// ---------------- 0. 配置：命令行 > 配置文件 > 默认值 ----------------
	cfgPath, explicitCfg := preScanConfig(os.Args[1:])
	if !explicitCfg {
		cfgPath = filepath.Join(exeDir, ConfigFileName)
	}
	settings, cfgFound, cfgErr := loadConfig(cfgPath)
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "! 配置文件读取失败：%v（本次改用默认值）\n", cfgErr)
		cfgFound = false
	}
	applyFlags := bindFlags(&settings)
	applyFlags()
	settings.normalize(exeDir)

	if settings.InitConfig {
		if err := writeExampleConfig(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "生成配置文件失败：%v\n", err)
			exitWithPause(1)
		}
		fmt.Printf("已生成配置文件：%s\n", cfgPath)
		fmt.Println("按需修改后再运行本程序即可；命令行参数仍可临时覆盖它。")
		exitWithPause(0)
	}

	outPath := settings.Out
	if err := os.MkdirAll(outPath, 0755); err != nil {
		fmt.Printf("无法创建输出目录：%v\n", err)
		exitWithPause(1)
	}
	logPath := settings.LogFile
	if logPath != "" && !filepath.IsAbs(logPath) {
		logPath = filepath.Join(exeDir, logPath)
	}
	out := NewOut(logPath, settings.LogMaxMB, settings.LogKeep, settings.Quiet)
	defer out.Close()

	// 清掉遗留的临时 profile：新版放在系统临时目录里，
	// 截图目录里的则一律是旧版残留（老版本建在这里，又删不完）。
	// 残留目录会让 Chrome 的单例机制误判"已有实例在跑"，导致启动后立刻退出。
	CleanupStaleProfiles()
	CleanupLegacyProfiles(outPath)

	// ---------------- 0.5 UA 池 ----------------
	// 站点按 UA 记账、用过的 UA 会失效，所以要跨运行记住"哪些用过了"。
	OpenUAPool(settings.UAPoolFile)
	defer uaPoolRef.Close()
	if settings.ResetUAPool {
		if err := ResetUAPool(); err != nil {
			fmt.Fprintf(os.Stderr, "! 清空 UA 池失败：%v\n", err)
		} else {
			fmt.Println("已清空 UA 池：所有 UA 都会重新视为可用")
		}
	}

	out.Important("%s\n", banner)
	out.Important("快乐谱曲谱爬虫（Go 版）\n")
	out.Important("  截图目录 : %s\n", outPath)
	out.Important("  断点文件 : %s\n", settings.StatePath)
	if cfgFound {
		out.Important("  配置文件 : %s\n", cfgPath)
	} else {
		out.Important("  配置文件 : （无，用内置默认值；-init-config 可生成一份）\n")
	}
	out.Important("  抓取轮数 : %s\n", pagesDesc(settings.Pages, settings.Limit))
	if settings.Workers == 1 {
		out.Important("  并发实例 : 1（串行，整趟复用同一个浏览器）\n")
	} else {
		out.Important("  并发实例 : %d（各自独立换 UA，额度互不干扰；整趟复用）\n", settings.Workers)
	}
	if lp := out.LogPath(); lp != "" {
		out.Important("  运行日志 : %s（上限 %d MB × 保留 %d 份）\n", lp, settings.LogMaxMB, settings.LogKeep)
	} else {
		out.Important("  运行日志 : （已关闭）\n")
	}
	uaUsed, _, _ := UAPoolStats()
	uaCap := UAPoolSize()
	out.Important("  UA 额度  : 池容量约 %d 种，已用 %d 种（剩 %.1f%%）%s\n",
		uaCap, uaUsed, (1-float64(uaUsed)/float64(uaCap))*100, uaPoolNote(settings.UAPoolFile))
	out.Important("%s\n", banner)

	// ---------------- 1. 断点 ----------------
	sw := newStateWriter(settings.StatePath)
	st := loadState(settings.StatePath)
	if st.corrupt {
		out.Important("! 断点文件损坏，已把原文件另存为 %s 并重新开始记录。\n"+
			"  注意：本次会把已抓过的条目当作未抓，可能在目录里生成 _2 后缀的重复截图。\n", settings.StatePath+".bad")
	}
	if settings.Reset {
		st.NextSortPoint = ""
		st.Shots = map[string]string{}
		out.Important("已重置断点：本次将从最新一页开始\n\n")
	}
	// 两个清理开关独立生效：只给 --prune-blank-kb 时，
	// 绝不能顺手把"文件缺失"的条目也清掉 —— 那会让程序重抓一大批。
	if settings.PruneMissing || settings.PruneBlankKB > 0 {
		pr := pruneShots(st, outPath, pruneOpts{
			Missing:  settings.PruneMissing,
			MinBytes: int64(settings.PruneBlankKB) << 10,
		}, 12)
		if pr.Missing+pr.Blank > 0 {
			out.Important("已清理断点：文件缺失 %d 条", pr.Missing)
			if settings.PruneBlankKB > 0 {
				out.Important("、疑似空白图 %d 条（<%d KB）", pr.Blank, settings.PruneBlankKB)
			}
			out.Important("；这些条目会重新抓取\n")
			if settings.DryRun {
				// --dry-run 承诺"不写断点"，这里就不能落盘
				out.Important("（--dry-run：以上仅为演示，未写入断点文件）\n")
			} else {
				sw.save(st)
			}
		}
		if len(pr.Files) > 0 {
			// 只清断点记录、不动磁盘文件：删文件必须由用户自己决定。
			// 但要注意顺序 —— 先把空白图删掉再跑，重抓才会用回原来的文件名；
			// 否则 uniquePath 会避开已存在的文件，生成 _2 后缀的新图。
			out.Important("\n  以下文件看起来是空白图，建议核对后自行删除，然后再跑一次补抓：\n")
			for _, f := range pr.Files {
				out.Important("    %s\n", f)
			}
			out.Important("  （请先删掉文件再补抓，否则会生成 _2 后缀的新文件、与旧图并存）\n")
		}
	}

	sortPoint := st.NextSortPoint
	if settings.FromPoint != "" {
		sortPoint = settings.FromPoint
	}
	out.Important("断点位置 : %s\n", orNone(sortPoint))

	browserExe := ""
	if !settings.DryRun {
		browserExe, err = findBrowser(settings.Browser)
		if err != nil {
			out.Important("  %v\n", err)
			exitWithPause(1)
		}
	}

	// pool 在下面装配。先声明是因为信号处理闭包要引用它 ——
	// Go 的词法作用域不允许闭包引用后面才声明的变量。
	var pool *workerPool

	// ---------------- 2. Ctrl+C：取消上下文，让各 worker 自己收尾 ----------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		interrupted.Store(true)
		out.Important("\n已中断，正在关闭浏览器并保存断点 …（再按一次 Ctrl+C 强制退出）\n")
		cancel()
		<-sigCh
		if pool != nil {
			pool.Close() // 收掉浏览器与临时 profile，别留孤儿进程和垃圾目录
		}
		os.Exit(130)
	}()

	// ---------------- 3. 主循环：逐页抓取 ----------------
	var (
		runSaved, runFailed, runGivenUp, runItems, pagesDone, pagesSkipped int
		pendingSaves                                                       int // 距上次落盘的条数
		stopReason                                                         string
		runStart                                                           = time.Now()
	)
	// failReason 记录每条最近一次失败的原因，交给 st.noteFailure 存档
	failReason := map[string]string{}
	taken := map[string]bool{} // 已占用的文件名，派发阶段串行维护
	for _, fn := range st.Shots {
		taken[fn] = true
	}
	// onSaved 由 runTasks 的主循环（单 goroutine）回调，因此 st 始终只有一个写者
	onSaved := func(t captureTask) {
		st.Shots[t.Song.ID] = filepath.Base(t.Target)
		st.clearFailure(t.Song.ID) // 抓到了就把历史失败记录摘掉
		st.TotalShots++
		runSaved++
		pendingSaves++
		if pendingSaves >= settings.StateFlushEvery {
			sw.save(st)
			pendingSaves = 0
		}
	}

	// ---------------- 1.5 抓取实例池 ----------------
	//
	// 整趟运行只建这么多实例，跨"遍"和"轮"一直复用。
	// 如果每遍都新建/销毁，一趟 10 轮会留下几十份 Chrome profile
	// （删一份要 20 秒，后台删除根本追不上创建）。
	poolOpts := pageOptions{
		BrowserExe: browserExe,
		WorkDir:    outPath,
		Width:      settings.Width,
		Headful:    settings.Headful,
		Cookie:     settings.Cookie,
		Workers:    settings.Workers,
		Delay:      settings.Delay,
		Retry:      settings.Retry,
		AbortAfter: settings.AbortAfterFails,
		Out:        out,
		OnSaved:    onSaved,
		OnFailed: func(t captureTask, err error) {
			failReason[t.Song.ID] = err.Error()
		},
	}
	pool = newWorkerPool(poolOpts)
	defer func() {
		// 正常路径的兜底：Ctrl+C 路径会显式调用（os.Exit 不执行 defer）
		if pool != nil {
			pool.Close()
		}
	}()

	nextGlobalIdx := 0
	for pageNo := 1; ; pageNo++ {
		if interrupted.Load() {
			stopReason = "已被用户中断"
			break
		}
		if settings.Pages > 0 && pageNo > settings.Pages {
			if runFailed == 0 && runSaved+pagesSkipped > 0 {
				stopReason = fmt.Sprintf("已完成配置的 %d 轮", settings.Pages)
			} else {
				stopReason = fmt.Sprintf("已达配置的 %d 轮上限", settings.Pages)
			}
			break
		}

		// ---- 3.1 取列表 ----
		out.Important("\n[第 %d 轮] 抓取曲谱列表 …\n", pageNo)
		res, err := fetchPage(sortPoint, settings.Cookie, 3)
		if err != nil {
			out.Important("  抓取失败：%v\n", err)
			if pageNo == 1 {
				exitWithPause(1)
			}
			stopReason = fmt.Sprintf("第 %d 轮列表抓取失败：%v", pageNo, err)
			break
		}
		if res.NextPoint == "" {
			out.Printf("  接口返回 %d 条；下一页游标：（无，已到末页）\n", len(res.Items))
		} else {
			out.Printf("  接口返回 %d 条；下一页游标：%s\n", len(res.Items), res.NextPoint)
		}
		if len(res.Items) == 0 {
			out.Important("  没有取到任何数据，可能是站点结构变化，请检查解析规则。\n")
			if pageNo == 1 {
				exitWithPause(1)
			}
			stopReason = "列表返回空，已停止"
			break
		}

		// ---- 3.2 计算待抓队列 ----
		rawTotal := len(res.Items)
		sel := selectPending(res.Items, st, outPath, settings.RetryFailed, settings.GiveUpAfter)
		pending, gaveUpSkipped, skipped := sel.Todo, sel.GaveUp, sel.Skipped
		pagesSkipped += skipped
		if gaveUpSkipped > 0 {
			out.Printf("  本页 %d 条：已抓 %d 条，放弃（多次失败）%d 条，待抓 %d 条\n",
				rawTotal, skipped, gaveUpSkipped, len(pending))
		} else {
			out.Printf("  本页 %d 条：此前已抓 %d 条，本次待抓 %d 条\n", rawTotal, skipped, len(pending))
		}

		if settings.DryRun {
			n := min(settings.Limit, len(pending))
			out.Important("\n--dry-run：列出接下来将要抓取的前 %d 条 --\n", n)
			for i := 0; i < n; i++ {
				out.Important("  %3d. %s    (%s)\n", i+1, buildFilename(pending[i].Name), pending[i].ID)
			}
			out.Important("\n（dry-run 未截图、未修改断点文件；起始游标：%s）\n", orNone(sortPoint))
			exitWithPause(0)
		}

		// 本页已全部抓过 → 直接推进游标，连浏览器都不用启动
		if len(pending) == 0 {
			if res.NextPoint == "" {
				out.Important("  本页 %d 条均已抓取，且已到末页，停止。\n", rawTotal)
				stopReason = "已到末页且无待抓条目"
				break
			}
			st.NextSortPoint = res.NextPoint
			sortPoint = res.NextPoint
			pagesDone++
			sw.save(st)
			out.Important("  本页 %d 条此前均已抓取，断点推进到：%s\n", rawTotal, res.NextPoint)
			continue
		}

		todo := pending
		if settings.Limit < len(pending) {
			todo = pending[:settings.Limit]
		}
		rest := len(pending) - len(todo)
		if rest > 0 {
			out.Printf("  本轮处理 %d 条，剩余 %d 条留到下次\n", len(todo), rest)
		} else {
			out.Printf("  本轮处理 %d 条\n", len(todo))
		}

		// ---- 3.3 派发任务：文件名在这里串行算好，worker 不再碰 taken ----
		tasks := make([]captureTask, 0, len(todo))
		for _, song := range todo {
			nextGlobalIdx++
			target := uniquePath(outPath, buildFilename(song.Name), taken)
			taken[filepath.Base(target)] = true
			tasks = append(tasks, captureTask{Index: nextGlobalIdx, Song: song, Target: target})
		}

		// ---- 3.4 并发截图（含轮内补抓）----
		//
		// 失败大多只是"抽到了已被耗尽的 UA"，换一批就过了。所以本轮内
		// 先自己补抓几遍，而不是把失败留给下次运行 —— 早期版本正是
		// "有一条失败就停"，导致 --pages 10 只跑得动第 1 轮。
		out.Important("\n[第 %d 轮] 逐条截图（并发 %d）…\n", pageNo, min(settings.Workers, len(tasks)))
		pageStart := time.Now()
		opts := poolOpts
		remaining := tasks
		pageSaved, pageFailEvents := 0, 0
		startFail := error(nil)
		abortedThisPage := false
		workerShort, workerStarted := 0, 0
		for pass := 0; len(remaining) > 0; pass++ {
			if pass > 0 {
				out.Important("  —— 轮内补抓第 %d 遍：%d 条 ——\n", pass, len(remaining))
			}
			oc := runTasks(ctx, opts, pool, remaining)
			pageSaved += oc.Saved
			pageFailEvents += oc.Failed
			if oc.Started == 0 && oc.startFail != nil {
				startFail = oc.startFail
				break
			}
			if oc.Started < min(settings.Workers, len(remaining)) {
				workerShort = oc.Started
			}
			if oc.Started > workerStarted {
				workerStarted = oc.Started
			}
			abortedThisPage = abortedThisPage || oc.Aborted
			remaining = oc.FailedTasks
			if oc.Aborted || interrupted.Load() || pass >= settings.RetryPasses {
				break
			}
		}
		if startFail != nil {
			out.Important("  浏览器全部启动失败：%v\n", startFail)
			if pageNo == 1 && runSaved == 0 {
				exitWithPause(1)
			}
			stopReason = "浏览器启动失败"
			break
		}
		if workerShort > 0 {
			out.Important("  ! 只有 %d 个实例启动成功（预期 %d），本轮已降速继续\n",
				workerShort, min(settings.Workers, len(tasks)))
		}

		runItems += len(todo)
		pagesDone++
		elapsed := time.Since(pageStart).Seconds()
		if pageSaved > 0 {
			out.Important("  本轮完成 %d 条，耗时 %.0fs（%.1f 条/分钟）\n",
				pageSaved, elapsed, float64(pageSaved)/max(elapsed, 0.001)*60)
		}

		// ---- 3.5 失败归档 + 断点推进 ----
		//
		// 补抓完仍失败的条目要累计失败次数；次数够了就记为"放弃"、
		// 不再阻塞游标 —— 否则一条反复失败的曲谱（会员专享、已下架、
		// 或恰好撞上一串死 UA）就能把整站进度永久钉在同一页。
		blockingFailed, givenUpNow := 0, 0
		for _, t := range remaining {
			st.noteFailure(t.Song.ID, failReason[t.Song.ID])
			if st.givenUp(t.Song.ID, settings.GiveUpAfter) {
				givenUpNow++
			} else {
				blockingFailed++
			}
		}
		// 被中止时没跑到的任务也要算进去（它们既没成功也没失败）
		notAttempted := len(todo) - pageSaved - len(remaining)
		if notAttempted < 0 {
			notAttempted = 0
		}
		pageNotDone := blockingFailed + notAttempted

		runFailed += blockingFailed
		runGivenUp += givenUpNow
		if len(remaining) > 0 {
			out.Important("  补抓后仍失败 %d 条：记入放弃 %d 条，继续阻塞 %d 条\n",
				len(remaining), givenUpNow, blockingFailed)
		}
		if pageFailEvents > pageSaved+len(remaining) {
			out.Printf("  （本轮共发生 %d 次失败尝试，补抓已挽回 %d 条）\n",
				pageFailEvents, pageFailEvents-len(remaining))
		}

		v := judgePage(sortPoint, res.NextPoint, blockingFailed, pageNotDone, rest, workerStarted)
		st.NextSortPoint = v.Next
		if v.Advance {
			sortPoint = v.Next
		}
		if v.RoundNote != "" {
			out.Important("  %s\n", v.RoundNote)
		}
		if v.Stop {
			stopReason = v.Reason
		}
		sw.save(st)
		if v.Stop {
			break
		}

		if abortedThisPage {
			stopReason = "连续失败过多，提前中止"
			break
		}
		if interrupted.Load() {
			stopReason = "已被用户中断"
			break
		}
	}

	// ---------------- 4. 收尾 ----------------
	sw.save(st)
	if pool != nil {
		pool.Close() // 关浏览器 + 收临时 profile，避免留下孤儿进程与垃圾目录
	}

	if interrupted.Load() {
		out.Important("\n已中断：本轮已完成 %d 条，断点保持不动，下次运行会补抓缺失条目。\n", runSaved)
		out.Close()
		os.Exit(130)
	}

	st.Runs++
	st.Pages += pagesDone
	st.TotalItems += runItems
	st.LastRun = time.Now().Format("2006-01-02 15:04:05")
	sw.save(st)

	out.Important("\n%s\n", banner)
	out.Important("完成：成功 %d 条，失败 %d 条，放弃（试满上限）%d 条，耗时 %.0fs\n",
		runSaved, runFailed, runGivenUp, time.Since(runStart).Seconds())
	if pagesSkipped > 0 {
		out.Important("（各轮合计跳过 %d 次已抓条目 —— 同一页被多轮重拉时会重复计数）\n", pagesSkipped)
	}
	if runGivenUp > 0 {
		out.Important("! %d 条反复失败已记为放弃并放行游标：这些条目**没有**抓到，也不会重试。\n"+
			"  断点文件 crawl_state.json 的 failed 字段里留有原因与次数；\n"+
			"  想再试一次（比如补上登录 Cookie）：加 --retry-failed 重跑。\n", runGivenUp)
	}
	out.Important("完成轮数：%d\n", pagesDone)
	out.Important("断点状态：%s\n", stopReason)
	out.Important("累计：%d 次运行 / %d 轮 / %d 条列表 / %d 张截图\n",
		st.Runs, st.Pages, st.TotalItems, st.TotalShots)
	uaUsed2, uaPre, uaAdded := UAPoolStats()
	out.Important("本次新消耗 UA %d 个（池内累计 %d/%d 种，剩 %.1f%%）\n",
		uaAdded, uaUsed2, UAPoolSize(), (1-float64(uaUsed2)/float64(UAPoolSize()))*100)
	if uaPre == uaUsed2 && uaAdded == 0 && runSaved > 0 {
		out.Important("! 本次一个可用 UA 都没抽到，可能池子已耗尽（--reset-ua-pool 可重来）\n")
	}
	if runSaved > 0 {
		out.Important("平均速度：%.1f 条/分钟\n",
			float64(runSaved)/max(time.Since(runStart).Seconds(), 1)*60)
	}
	if sz := sw.size(); sz > stateWarnBytes {
		out.Important("\n! 断点文件已增至 %.1f MB。它的体积随已抓条数线性增长，\n"+
			"  而程序每成功一条都会整体重写一次（写放大）。\n"+
			"  建议：把 --state-flush-every 调到 20~50，或用 --prune-missing 清理失效记录。\n",
			float64(sz)/(1<<20))
	}
	out.Important("%s\n", banner)

	if runFailed > 0 || interrupted.Load() {
		exitWithPause(2)
	}
	exitWithPause(0)
}

// uaPoolNote 说明 UA 池是否持久化，避免用户不知道多出来的那个文件是什么。
func uaPoolNote(path string) string {
	if path == "" {
		return "（未持久化，跨运行会重复消耗）"
	}
	return "（已持久化：" + filepath.Base(path) + "）"
}

func pagesDesc(pages, limit int) string {
	if pages <= 0 {
		return fmt.Sprintf("不限（抓到末页或出错为止），每轮最多 %d 条", limit)
	}
	return fmt.Sprintf("%d 轮，每轮最多 %d 条（约 %d 条）", pages, limit, pages*limit)
}

// preScanConfig 在正式解析参数前先捞出 --config 的值。
// 必须先知道配置文件在哪，才能用它的值作为各参数的默认值。
func preScanConfig(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		for _, name := range []string{"-config", "--config"} {
			if a == name {
				if i+1 < len(args) {
					return args[i+1], true
				}
				return "", false
			}
			if strings.HasPrefix(a, name+"=") {
				return strings.TrimPrefix(a, name+"="), true
			}
		}
	}
	return "", false
}

func orNone(s string) string {
	if s == "" {
		return "无 / 从最新开始"
	}
	return s
}

// findBrowser 定位本机 Chrome / Edge。
func findBrowser(explicit string) (string, error) {
	if explicit != "" {
		if fileExists(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("指定的浏览器不存在：%s", explicit)
	}
	var candidates []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
		)
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c, nil
		}
	}
	for _, name := range []string{"chrome.exe", "chrome", "msedge.exe", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("未找到 Chrome 或 Edge。请安装其中之一，或用 -browser 指定路径")
}

// exitWithPause 退出前，若程序是被「双击」启动的，则等待回车，避免窗口一闪而过。
func exitWithPause(code int) {
	if startedByDoubleClick() {
		fmt.Print("\n按回车键退出 …")
		fmt.Scanln()
	}
	os.Exit(code)
}
