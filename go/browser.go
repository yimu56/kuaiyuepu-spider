package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errAccessBlocked 表示该 UA 的免费额度已用完，页面被重定向到登录页。
var errAccessBlocked = errors.New("页面被重定向到登录页，该 UA 的免费额度已用完")

// errNotReady 表示页面没能渲染出乐谱。
//
// 这种页面截下来就是一张空白图（实测 1280x400 / 3KB），**必须当失败处理**。
// 早期版本"超时也照截"的后果是：站点一限速，整轮 20 条全部存成空白图并被
// 记入 shots，从此再也不会重抓 —— 比直接失败危险得多。
var errNotReady = errors.New("页面未渲染出乐谱")

const (
	// pageReadyTimeout 是"等页面就绪（readyState=complete 且乐谱 SVG 出现）"的总上限。
	pageReadyTimeout = 25 * time.Second
	// pageReadyGrace 是"页面已加载完、但乐谱还没出现"时额外宽限的时间。
	// 正常页面 1~2 秒内就有乐谱；超过宽限期还没出现，就是在服务风控页/错误页，
	// 继续等只是白等，直接判失败去换 UA 重试更划算。
	pageReadyGrace = 4 * time.Second
	// minShotBytes 是"这张图明显是空白"的下限。真实乐谱图最小也有 20+ KB，
	// 空白页约 3 KB。它只是兜底 —— 主判据是上面的就绪检查。
	minShotBytes = 4 << 10
)

// diagJS 一次调用取回"这个页面到底是什么"的全部关键信息，供失败原因使用。
// 排障时最需要的就是这几个字段：最终地址、readyState、标题、有没有乐谱、正文长度。
const diagJS = `(() => {
  const s = document.querySelector('.sheet-svg');
  return JSON.stringify({
    url: location.pathname + location.search,
    rs: document.readyState,
    title: (document.title || '').slice(0, 50),
    svg: !!s,
    len: document.body ? document.body.innerHTML.length : -1,
    h: document.documentElement.scrollHeight
  });
})()`

// 详情页公共脚本 ------------------------------------------------------------

const closeModalJS = `(() => {
  document.querySelectorAll('.modal').forEach(m => {
    m.style.display = 'none';
    m.classList.remove('open');
  });
  document.querySelectorAll('.lean-overlay, .modal-overlay').forEach(o => o.remove());
  document.body.style.overflow = 'auto';
  document.documentElement.style.overflow = 'auto';
  return 1;
})()`

const measureJS = `(() => {
  let maxB = 0;
  document.querySelectorAll('body *').forEach(el => {
    const cs = getComputedStyle(el);
    if (cs.display === 'none' || cs.visibility === 'hidden' || cs.position === 'fixed') return;
    const r = el.getBoundingClientRect();
    if (r.width <= 0 && r.height <= 0) return;
    const b = r.bottom + window.scrollY;
    if (b > maxB) maxB = b;
  });
  return Math.ceil(maxB) || document.documentElement.scrollHeight;
})()`

// hasScoreJS 判断乐谱是否已经渲染出来。乐谱是内联 SVG，是"页面可用"的唯一可靠标志。
const hasScoreJS = `!!document.querySelector('.sheet-svg')`

const isLoginWallJS = `(() => {
  return location.pathname.includes('/web/user.php')
      || !!document.querySelector('form[action*="action=login"]');
})()`

// ProfilePrefix 是浏览器临时 profile 目录的前缀。
const ProfilePrefix = "_browser_profile"

// ProfileRootName 是临时 profile 的根目录名（放在系统临时目录下）。
//
// 为什么不放在工作目录（截图目录）：删一份 Chrome profile 要 18 秒以上
// （280+ 个文件，Windows 上杀软逐个扫描，并行删也一样慢），根本来不及在退出前删完，
// 于是截图目录里会长期堆着 _browser_profile_* 这样的垃圾目录 —— 这才是用户看到的
// "多个 browser_profile 未清理"。把 profile 放到系统临时目录后，
// 截图目录里从头到尾都不会出现它们；临时目录里的残留则由启动清理 + 系统清理负责。
const ProfileRootName = "kuaiyuepu-crawler-profiles"

// ProfileDelSuffix 是"待删除"标记的后缀。清理时先同步改名成它（瞬间完成），
// 再后台删除；这样即使进程被强杀，工作目录里也不会残留 _browser_profile_* 目录。
const profileDelSuffix = ".del"

// Browser 封装一个 headless Chrome 实例，复用它逐条导航 + 截图。
type Browser struct {
	exe        string
	port       int
	profileDir string
	width      int
	height     int
	headful    bool
	cookie     string
	proc       *exec.Cmd
	cdp        *cdpClient
	currentUA  string
	exited     chan error // cmd.Wait() 的结果，用于判断浏览器是否提前退出

	// 上一条的计时观测（只由持有本实例的那个 worker goroutine 读写）
	lastWait time.Duration // 等页面就绪花了多久
	lastShot time.Duration // 截图 + 落盘花了多久
}

// LastTiming 返回上一条的耗时观测，用于日志里显示瓶颈在哪。
func (b *Browser) LastTiming() (wait, shot time.Duration) {
	return b.lastWait, b.lastShot
}

// Healthy 报告这条 CDP 连接是否还能用。用坏的实例不复用，
// 否则会把后续每条都拖垮（表现成一连串莫名其妙的失败）。
func (b *Browser) Healthy() bool {
	return b.cdp != nil && !b.cdp.broken
}

// diagnose 取回当前页面的诊断信息（失败原因里带着它，排障时不用靠猜）。
func (b *Browser) diagnose() string {
	if b.cdp == nil {
		return "CDP 连接已断开"
	}
	if b.cdp.broken {
		return "CDP 连接已失效"
	}
	v := b.cdp.evalString(diagJS)
	if v == "" {
		return "无法读取页面状态"
	}
	var d struct {
		URL   string `json:"url"`
		RS    string `json:"rs"`
		Title string `json:"title"`
		SVG   bool   `json:"svg"`
		Len   int    `json:"len"`
		H     int    `json:"h"`
	}
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return "页面状态：" + v
	}
	return fmt.Sprintf("最终地址=%s readyState=%s 乐谱=%v 标题=%q 正文=%d字节 高度=%d",
		d.URL, d.RS, d.SVG, d.Title, d.Len, d.H)
}

// basePort 是调试端口的起点；第 n 个 worker 从 basePort+n*portStride 开始找空闲端口。
// 必须错开：并发调用 freePort 时"探测到空闲 → 关闭 → 稍后被别人占用"存在竞态，
// 让多个 Chrome 抢同一个端口会直接启动失败。
const (
	basePort   = 9411
	portStride = 20
)

// browserSeq 给每个浏览器实例发一个递增序号，用来让 profile 目录全局唯一。
//
// 为什么需要：profile 的清理是异步的（见 CleanupProfile），如果同一进程里
// 第 2 遍抓取复用了第 1 遍的目录名，异步删除就可能把刚建好的新 profile 干掉。
var browserSeq atomic.Int64

// NewBrowser 创建浏览器句柄。workerID 从 1 开始，用于隔离 profile 目录与调试端口；
// 串行模式（--workers 1）传 1 即可。
func NewBrowser(exe, workDir string, width int, headful bool, cookie string, workerID int) *Browser {
	if workerID < 1 {
		workerID = 1
	}
	// profile 根目录绝对化是必须的：Chrome 拿到相对的 --user-data-dir 时不报错、
	// 也不监听调试端口，最终表现为"连不上浏览器"，排查成本极高。这里兜一道底。
	root := ProfileRoot()
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	// profile 目录带上 PID、worker 序号与实例序号：万一上次是异常退出，残留目录会让
	// Chrome 的单例机制误判「已有实例在跑」，新进程转发命令后立刻退出，表现就是连不上。
	seq := browserSeq.Add(1)
	return &Browser{
		exe:        exe,
		port:       freePort(basePort + (workerID-1)*portStride),
		profileDir: filepath.Join(root, fmt.Sprintf("%s_%d_%d_%d", ProfilePrefix, os.Getpid(), workerID, seq)),
		width:      width,
		height:     900,
		headful:    headful,
		cookie:     cookie,
	}
}

// ProfileRoot 返回临时 profile 的根目录（系统临时目录下）。
//
// 为什么不放截图目录：删一份 Chrome profile 要 18 秒以上（280+ 个文件，
// Windows 上杀软逐个扫描，并行删也一样慢），根本来不及在退出前删完，
// 于是截图目录里会长期堆着 _browser_profile_* 垃圾目录 ——
// 这正是"多个 browser_profile 未清理"的来由。放到系统临时目录后，
// 截图目录从头到尾都不会出现它们，临时目录里的残留由启动清理和系统清理负责。
func ProfileRoot() string {
	return filepath.Join(os.TempDir(), ProfileRootName)
}

// CleanupStaleProfiles 清理上次运行遗留在临时目录里的 profile。
//
// 先改名成 .del（元数据操作，瞬间完成），再丢到后台删除 ——
// 既要"立刻看不见"，又不能阻塞启动。
//
// 安全性：只动临时目录下本程序自己的目录；跳过与当前进程 PID 同名、
// 以及 2 小时内新出现的目录，避免误删并发实例正在使用的 profile。
func CleanupStaleProfiles() {
	root := ProfileRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	myPID := "_" + strconv.Itoa(os.Getpid()) + "_"
	var doomed []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ProfilePrefix) {
			continue
		}
		full := filepath.Join(root, e.Name())
		isDel := strings.HasSuffix(e.Name(), profileDelSuffix)
		if isDel {
			doomed = append(doomed, full) // 上次没删完的，无论多久都可以删
			continue
		}
		if strings.Contains(e.Name(), myPID) {
			continue // 本次运行自己的目录
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < 2*time.Hour {
			continue // 刚出现的，可能是并发实例正在用
		}
		del := full + profileDelSuffix
		// Windows 的 os.Rename 不会覆盖已存在的目标：上一次可能已经留下了同名 .del
		os.RemoveAll(del)
		if err := os.Rename(full, del); err != nil {
			doomed = append(doomed, full) // 改名失败（可能仍被占用）就直接删
			continue
		}
		doomed = append(doomed, del)
	}
	if len(doomed) == 0 {
		return
	}
	go func() {
		for _, d := range doomed {
			removeAllParallel(d)
		}
	}()
}

// CleanupLegacyProfiles 清理**截图目录里**的 profile 残留。
//
// 老版本把 profile 建在截图目录，退出前又删不完（删一份要 18 秒以上），
// 于是留下一堆 _browser_profile_*。新版一律把 profile 放系统临时目录，
// 所以截图目录里出现的这些目录必定是旧版遗留 —— 可以放心清掉。
func CleanupLegacyProfiles(workDir string) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return
	}
	var doomed []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ProfilePrefix) {
			continue
		}
		full := filepath.Join(workDir, e.Name())
		if strings.HasSuffix(e.Name(), profileDelSuffix) {
			doomed = append(doomed, full)
			continue
		}
		del := full + profileDelSuffix
		os.RemoveAll(del)
		if err := os.Rename(full, del); err != nil {
			doomed = append(doomed, full)
			continue
		}
		doomed = append(doomed, del)
	}
	if len(doomed) == 0 {
		return
	}
	go func() {
		for _, d := range doomed {
			removeAllParallel(d)
		}
	}()
}

// removeAllParallel 并发删除目录。
//
// os.RemoveAll 是单线程的，而 Chrome profile 的文件散在几十个子目录里，
// 串行删要 20 秒。按顶层子目录并行删能把它压到几秒 —— 而且这段是后台跑的，
// 快慢都不影响抓取。
func removeAllParallel(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		os.RemoveAll(dir)
		return
	}
	var wg sync.WaitGroup
	for _, e := range entries {
		if !e.IsDir() {
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			os.RemoveAll(filepath.Join(dir, name))
		}(e.Name())
	}
	wg.Wait()
	os.RemoveAll(dir) // 清掉剩下的空目录与残余文件
}

// freePort 从 start 起找一个可用的本地端口。
func freePort(start int) int {
	for port := start; port < start+portStride; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return port
		}
	}
	return start
}

func (b *Browser) Start() error {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", b.port),
		"--remote-allow-origins=*",
		"--user-data-dir=" + b.profileDir,
		"--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--disable-background-networking",
		"--disable-sync", "--disable-component-update",
		"--disable-gpu", "--no-sandbox", "--hide-scrollbars",
		"--disable-logging", "--log-level=3",
		fmt.Sprintf("--window-size=%d,%d", b.width, b.height),
		// 下面这些是为了**少往 profile 写东西**。删一份 Chrome profile 极慢
		// （实测 350+ 个文件要 20 秒，Windows 上杀软逐个扫描是主因），
		// 文件越少删得越快。实测这些开关能把文件数砍掉一大半：
		//   disk-cache-size=1        → 不落 HTTP 缓存（Default/Cache 是最大头）
		//   disable-gpu-shader-disk-cache → 不落 GL 着色器缓存
		//   OptimizationGuide…       → 不下载"优化指南"模型（实测 19 个文件）
		"--disk-cache-size=1",
		"--media-cache-size=1",
		"--disable-gpu-shader-disk-cache",
		"--disable-features=OptimizationGuideModelDownloading,Translate,MediaRouter,InterestFeedContentSuggestions",
	}
	if !b.headful {
		args = append(args, "--headless=new")
	}
	args = append(args, "about:blank")

	cmd := exec.Command(b.exe, args...)
	setHideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	b.proc = cmd
	b.exited = make(chan error, 1)
	go func() { b.exited <- cmd.Wait() }()

	cdp, err := b.connect(60 * time.Second)
	if err != nil {
		return err
	}
	b.cdp = cdp

	if _, err := cdp.call("Page.enable", nil); err != nil {
		return err
	}
	if _, err := cdp.call("Network.enable", nil); err != nil {
		return err
	}
	if _, err := cdp.call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width": b.width, "height": b.height,
		"deviceScaleFactor": 1, "mobile": false,
	}); err != nil {
		return err
	}
	if b.cookie != "" {
		b.applyCookie()
	}
	return nil
}

// connect 等待调试端口就绪。若浏览器进程提前退出就立刻报错，
// 而不是一直傻等到超时 —— 否则用户看到的就是「卡住不动」。
func (b *Browser) connect(timeout time.Duration) (*cdpClient, error) {
	type result struct {
		c   *cdpClient
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := newCDP(b.port, timeout)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case err := <-b.exited:
		return nil, fmt.Errorf(
			"浏览器进程启动后立即退出（%v）。常见原因：还有残留的 headless 浏览器占着 "+
				"profile 目录，请先结束多余的 chrome.exe / msedge.exe 再试", err)
	}
}

// rotateIdentity 换一个 User-Agent 并清空 Cookie。
//
// 站点按 UA 计算免费额度，额度用完就重定向到登录页；
// 换脸 + 清身份即可重新获得额度（与油猴脚本的绕过思路一致）。
func (b *Browser) rotateIdentity() error {
	ua := randomUA()
	if _, err := b.cdp.call("Network.setUserAgentOverride", map[string]any{
		"userAgent":      ua,
		"acceptLanguage": "zh-CN,zh;q=0.9,en;q=0.8",
		"platform":       platformFromUA(ua),
	}); err != nil {
		return err
	}
	b.cdp.call("Network.clearBrowserCookies", nil)
	if b.cookie != "" {
		b.applyCookie() // 换脸后把用户 Cookie 重新写回去
	}
	b.currentUA = ua
	return nil
}

func (b *Browser) applyCookie() {
	for _, pair := range strings.Split(b.cookie, ";") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		b.cdp.call("Network.setCookie", map[string]any{
			"name":   strings.TrimSpace(k),
			"value":  strings.TrimSpace(v),
			"domain": ".kuaiyuepu.com",
			"path":   "/",
		})
	}
}

// Capture 打开详情页并按内容真实高度整页截图。
//
// 关键约定：**只有在确认乐谱渲染出来之后才截图**。页面没就绪就返回
// errNotReady，让上层换 UA 重试 —— 绝不能把一张空白图当成"抓到了"。
func (b *Browser) Capture(pageURL, outPath string) (int, int, int, error) {
	waitStart := time.Now()
	if err := b.rotateIdentity(); err != nil {
		return 0, 0, 0, err
	}
	if _, err := b.cdp.call("Page.navigate", map[string]any{"url": pageURL}); err != nil {
		return 0, 0, 0, err
	}

	// 等 readyState=complete 且乐谱 SVG 出现。
	//
	// 两条快判，都是为了不白等满 25 秒：
	//   - 登录墙：额度耗尽时页面被换成登录页，它永远不会有乐谱（实测每条能省 25 秒）；
	//   - 已加载完但乐谱迟迟不出现：说明服务的是风控页/错误页，超过宽限期直接判失败。
	// 正常页面 1~2 秒就就绪，这两条都不会触发，所以正常路径零额外开销。
	deadline := time.Now().Add(pageReadyTimeout)
	ready, svg := false, false
	var readyAt time.Time
	for time.Now().Before(deadline) {
		if !ready && b.cdp.evalString("document.readyState") == "complete" {
			ready, readyAt = true, time.Now()
		}
		if !svg && b.cdp.evalBool(hasScoreJS) {
			svg = true
		}
		if ready && svg {
			break
		}
		if !svg && (ready || time.Since(waitStart) > time.Second) {
			if b.cdp.evalBool(isLoginWallJS) {
				b.lastWait = time.Since(waitStart)
				return 0, 0, 0, errAccessBlocked
			}
			if ready && time.Since(readyAt) > pageReadyGrace {
				b.lastWait = time.Since(waitStart)
				return 0, 0, 0, fmt.Errorf("%w：页面已加载完但乐谱迟迟不出现（%s）",
					errNotReady, b.diagnose())
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	b.lastWait = time.Since(waitStart)
	if !ready || !svg {
		// 连 readyState 都没到 complete：页面一直加载不完（站点变慢/被限速/资源挂住）。
		return 0, 0, 0, fmt.Errorf("%w：%s 秒内未加载完（%s）",
			errNotReady, strconv.Itoa(int(pageReadyTimeout.Seconds())), b.diagnose())
	}

	if b.cdp.evalBool(isLoginWallJS) {
		return 0, 0, 0, errAccessBlocked
	}

	time.Sleep(500 * time.Millisecond)
	b.cdp.eval(closeModalJS) // 移除「请选择乐谱类型」弹窗与遮罩
	time.Sleep(250 * time.Millisecond)

	h := b.height
	if v := b.cdp.evalNumber(measureJS); v > 0 {
		h = int(v)
	}
	if h < 400 {
		h = 400
	}
	if h > 20000 {
		h = 20000
	}

	shotStart := time.Now()
	res, err := b.cdp.call("Page.captureScreenshot", map[string]any{
		"format":                "png",
		"captureBeyondViewport": true,
		"clip": map[string]any{
			"x": 0, "y": 0, "width": b.width, "height": h, "scale": 1,
		},
	})
	if err != nil {
		return 0, 0, 0, err
	}
	dataStr, _ := res["data"].(string)
	data, err := base64.StdEncoding.DecodeString(dataStr)
	if err != nil {
		return 0, 0, 0, err
	}
	// 兜底：真出现过"就绪了但截出来是空白"的图（3 KB / 400px）。
	// 宁可判失败重试，也不要往存档里塞一张废图 —— 它会被记入 shots 再也不会重抓。
	if len(data) < minShotBytes {
		return 0, 0, 0, fmt.Errorf("%w：截图像是空白（仅 %d 字节，%dx%d，%s）",
			errNotReady, len(data), b.width, h, b.diagnose())
	}
	if err := os.WriteFile(outPath, data, 0644); err != nil {
		return 0, 0, 0, err
	}
	b.lastShot = time.Since(shotStart)
	return len(data), b.width, h, nil
}

// CleanupProfile 收掉本次使用的临时 profile 目录。
//
// 分两步走，是为了兼顾"别拖慢抓取"和"别留下垃圾目录"：
//
//  1. **同步改名成 .del** —— 纯元数据操作，毫秒级。改完名，工作目录里就再也看不到
//     `_browser_profile_*` 了，即使进程随后被 Ctrl+C 掐掉也不会留垃圾。
//  2. **后台真正删除** —— os.RemoveAll 删一个 Chrome profile 要 20 秒（472 个条目，
//     Windows 上杀软逐个扫描是主因）。这点时间绝不能算进抓取耗时，
//     所以丢到后台；没删完也没关系，下次启动时 CleanupStaleProfiles 会接手。
func (b *Browser) CleanupProfile() {
	dir := b.profileDir
	if dir == "" {
		return
	}
	b.profileDir = ""
	if !dirExists(dir) {
		return
	}
	del := dir + profileDelSuffix
	os.RemoveAll(del)

	// 改名会失败：Windows 不允许改名"内部还有文件被打开"的目录，
	// 而 Chrome 的子进程（renderer / GPU）在主进程退出后还会短暂持有里面的文件。
	// 这正是"profile 目录清不掉"的根因 —— 所以这里重试若干次。
	renamed := false
	for i := 0; i < 12; i++ {
		if err := os.Rename(dir, del); err == nil {
			renamed = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !renamed {
		// 实在改不了就照原名后台删，剩下的交给下次启动时的 CleanupStaleProfiles
		del = dir
	}
	go removeAllParallel(del)
}

func (b *Browser) Close() {
	if b.cdp != nil {
		b.cdp.call("Browser.close", nil) // 连接可能随即断开，报错忽略
		b.cdp.close()
		b.cdp = nil
	}
	if b.proc != nil && b.exited != nil {
		select {
		case <-b.exited: // Start 里的 goroutine 已经调用过 Wait
		case <-time.After(8 * time.Second):
			b.killTree()
			select {
			case <-b.exited:
			case <-time.After(5 * time.Second):
			}
		}
	}
	b.killTree() // 兜底：确保子进程也退干净，否则 profile 目录删不掉
}

// killTree 结束浏览器及其全部子进程。
//
// Chrome 会派生 renderer / GPU / utility 一堆子进程，它们持有 profile 里的文件。
// 只杀主进程的话，这些文件会短暂被占用，随后"改名 profile 目录"就会失败，
// 最终表现为工作目录里留下一堆清不掉的 _browser_profile_* 目录。
func (b *Browser) killTree() {
	if b.proc == nil || b.proc.Process == nil {
		return
	}
	pid := b.proc.Process.Pid
	if runtime.GOOS == "windows" {
		// /T 连子进程一起结束；失败无妨，后面还有进程级 Kill
		cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
		setHideWindow(cmd)
		if err := cmd.Run(); err == nil {
			return
		}
	}
	b.proc.Process.Kill()
}
