package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// User-Agent 动态生成
//
// 规则与油猴脚本 kuaiyuepu-access-helper.user.js 保持一致：
//
//   - 操作系统加权分布：Windows .45 / macOS .20 / Linux .10 / Android .15 / iOS .10
//   - Windows：Win10 85% / Win7 15%；Win64;x64 75% / WOW64 25%；
//     浏览器 Chrome 50% / Edge 30% / Firefox 20%
//   - macOS：多种系统版本加权；Chrome 40% / Safari 40% / Firefox 20%
//   - Linux：多种发行版加权；Chrome 60% / Firefox 40%
//   - Android：系统版本、机型与 Chrome 版本组合
//   - iOS：系统版本 + 构建号 + Safari 版本，iPhone 70% / iPad 30%
//
// 为什么必须动态生成：站点按 User-Agent 计算免费额度，每个 UA 只能看几首。
//
// 为什么池子必须**足够大**（这是 2026-09 的实测教训）：
// 额度是记在 UA 字符串上的，且**用过的 UA 会失效** ——
// 实测同一个 `Chrome/131` 的常见 UA 请求 6 个页面全部 302 到登录页，
// 而换一个从未用过的 UA 立刻 200。所以「池子容量」直接决定了
// 总共能抓多少条：池子只有 1935 种时，跑满 9 次（约 900 条）后
// 近一半的抽签都会落在已耗尽的 UA 上，表现为大量"已换 UA 重试"。
// 现在把池子扩到 3 万量级（见 UAPoolSize），并把用过的 UA 指纹持久化到
// ua_pool.txt，保证跨运行也不重复消耗。
// ============================================================================

// mulberry32 与 JS 版使用同一个伪随机数生成器，保证两端行为一致。
type mulberry32 struct{ state uint32 }

func newMulberry32(seed uint32) *mulberry32 { return &mulberry32{state: seed} }

// Next 返回 [0, 1) 之间的浮点数。
func (m *mulberry32) Next() float64 {
	m.state += 0x6D2B79F5
	t := m.state
	t = (t ^ (t >> 15)) * (t | 1)
	t = t + ((t ^ (t >> 7)) * (t | 61)) ^ t
	return float64(t^(t>>14)) / 4294967296.0
}

// randInt 返回 [min, max] 闭区间内的整数。
func randInt(r *mulberry32, min, max int) int {
	return min + int(r.Next()*float64(max-min+1))
}

// pickWeighted 按权重从 items 中挑一个。
func pickWeighted(r *mulberry32, items []string, weights []float64) string {
	total := 0.0
	for _, w := range weights {
		total += w
	}
	x := r.Next() * total
	for i := range items {
		x -= weights[i]
		if x <= 0 {
			return items[i]
		}
	}
	return items[len(items)-1]
}

// ---------------------------------------------------------------- 版本区间
//
// 区间取得比"当前最新"略宽一些，目的是扩大组合空间；
// 上下界都必须落在真实存在过的版本里，避免生成一看就假的版本号。
const (
	chromeMin, chromeMax       = 110, 135
	firefoxMin, firefoxMax     = 113, 134
	safariMin, safariMax       = 16, 18 // macOS Safari
	iosSafariMin, iosSafariMax = 15, 17
)

var winVersions = []string{"Windows NT 10.0", "Windows NT 6.1", "Windows NT 6.2", "Windows NT 6.3"}
var winArchs = []string{"Win64; x64", "WOW64", "ARM64"}
var linuxDistros = []string{
	"X11; Linux x86_64", "X11; Ubuntu; Linux x86_64", "X11; Fedora; Linux x86_64",
	"X11; Linux i686", "X11; Arch Linux x86_64", "X11; CentOS; Linux x86_64",
	"X11; openSUSE; Linux x86_64", "X11; Debian; Linux x86_64",
}
var macVersions = []string{
	"10_15_7", "11_7_10", "12_7_6", "13_6_8", "14_7_2", "14_2_0", "15_1_0",
}

// androidDevices 是真实存在的机型代号。
// 这是整个池子里最大的熵来源（机型 × 系统版本 × Chrome 版本），
// 所以列表要尽量长。
var androidDevices = []string{
	// Samsung
	"SM-S918B", "SM-S928B", "SM-S921B", "SM-S911B", "SM-S908B", "SM-S901B",
	"SM-A546E", "SM-A536B", "SM-A346B", "SM-A146B", "SM-A047F", "SM-A226B",
	"SM-G991B", "SM-G998B", "SM-G996B", "SM-G780G", "SM-M536B", "SM-F946B",
	// Google Pixel
	"Pixel 6", "Pixel 6a", "Pixel 6 Pro", "Pixel 7", "Pixel 7a", "Pixel 7 Pro",
	"Pixel 8", "Pixel 8a", "Pixel 8 Pro", "Pixel Fold", "Pixel 9",
	// Xiaomi / Redmi / POCO
	"2201117TG", "2201123G", "22101316G", "22111317G", "23013RK75C", "23021RAAEG",
	"M2101K7BG", "M2102J20SG", "M2103K19G", "M2007J3SY", "M2011K2G", "M2004J19C",
	"21061119DG", "220333QAG", "22120RN86G", "Redmi Note 8 Pro", "Redmi Note 9S",
	"Redmi Note 10 Pro", "Redmi Note 11", "Redmi Note 12", "Redmi K40", "Redmi K50",
	"POCO F3", "POCO F4", "POCO X3 NFC", "POCO X4 Pro", "POCO M3", "POCO M4 Pro",
	// HONOR / Huawei
	"ANY-LX1", "BVL-N49", "CRT-NX1", "ELP-NX9", "FNE-NX9", "REA-NX9", "RKY-LX1",
	"VNE-N41", "JNY-LX1", "NOH-NX9", "LGE-NX9", "ANE-LX1", "MAR-LX1A", "WAS-LX1A",
	// OPPO / realme / OnePlus
	"CPH2451", "CPH2477", "CPH2525", "CPH2573", "CPH2609", "CPH2385", "CPH2219",
	"RMX3363", "RMX3370", "RMX3474", "RMX3630", "RMX3501", "RMX3269", "RMX3151",
	"ONEPLUS A6013", "ONEPLUS A6003", "IN2013", "LE2113", "NE2213", "CPH2449",
	"GM1901", "KB2003", "AC2001", "DN2103", "EB2103", "CPH2455",
	// vivo / iQOO
	"V2118A", "V2130", "V2134", "V2145", "V2156", "V2165", "V2207", "V2231",
	"V2045", "V2046", "V1938T", "I2012", "I2126", "I2202", "I2220", "V2334",
	// Motorola / Nokia / Sony / Asus / Lenovo
	"moto g(30)", "moto g(50)", "moto g(60)", "moto g82 5G", "XT2201-2", "XT2175-2",
	"Nokia G20", "Nokia G50", "Nokia X20", "Nokia 5.4", "Nokia 8.3 5G",
	"XQ-AT51", "XQ-BC52", "XQ-CC54", "XQ-DQ54", "XQ-AS52", "XQ-CT54",
	"ASUS_I005DA", "ASUS_I006D", "ASUS_AI2201", "ASUS_Z01QD",
	"Lenovo K12", "Lenovo TB-J606F", "Lenovo TB128FU", "Lenovo Z6 Pro",
	// 其它常见型号
	"2207117BPG", "23053RN02A", "24069PC21G", "22127RK46C", "V2311", "V2227A",
	"M2101K9C", "21091116AC", "2107119DC", "2312DRA50C", "24094RAD4C",
	"CPH2581", "CPH2583", "RMX3820", "RMX3785", "V2254A", "I2302",
	"TECNO KI5", "Infinix X6819", "itel A665L", "LAVA Z3",
	"SM-A155F", "SM-A256E", "SM-S921U", "SM-S928U", "SM-G990B",
	"Pixel 9 Pro", "Pixel 9a", "Pixel 8 Pro XL",
	"moto edge 40", "moto g stylus 5G", "XT2343-1", "XT2307-1",
	"V2354", "V2312", "I2216", "I2301", "CPH2569", "CPH2493",
}

// iosBuilds 是各 iOS 大版本对应的真实 Mobile/ 构建号。
// 之前所有 iOS UA 都固定用 15E148，导致 iOS 分支的组合空间被压得极小。
var iosBuilds = map[int][]string{
	15: {"15A372", "15B93", "15C202", "15D60", "15E148", "15F79"},
	16: {"16A366", "16B92", "16C62", "16D39", "16E137", "16F156", "16G29"},
	17: {"17A264c", "17A577", "17B84", "17C54", "17D50", "17E262", "17F80"},
}

// ---------------------------------------------------------------- 各平台生成器

func generateWindowsUA(r *mulberry32) string {
	winVer := "Windows NT 10.0"
	if r.Next() <= 0.15 { // 15% 走旧系统
		winVer = pickWeighted(r, winVersions[1:], []float64{0.5, 0.25, 0.25})
	}
	arch := "Win64; x64"
	if r.Next() <= 0.30 { // 30% 走兼容模式 / ARM
		arch = pickWeighted(r, winArchs[1:], []float64{0.75, 0.25})
	}

	switch pickWeighted(r,
		[]string{"chrome", "edge", "firefox"},
		[]float64{0.5, 0.3, 0.2}) {
	case "chrome":
		v := randInt(r, chromeMin, chromeMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (%s; %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36",
			winVer, arch, v)
	case "edge":
		v := randInt(r, chromeMin, chromeMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (%s; %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36 Edg/%d.0.0.0",
			winVer, arch, v, v)
	default:
		v := randInt(r, firefoxMin, firefoxMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (%s; %s; rv:%d.0) Gecko/20100101 Firefox/%d.0",
			winVer, arch, v, v)
	}
}

func generateMacUA(r *mulberry32) string {
	macVer := pickWeighted(r,
		macVersions,
		[]float64{0.18, 0.14, 0.14, 0.14, 0.14, 0.13, 0.13})

	switch pickWeighted(r,
		[]string{"chrome", "safari", "firefox"},
		[]float64{0.4, 0.4, 0.2}) {
	case "chrome":
		v := randInt(r, chromeMin, chromeMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (Macintosh; Intel Mac OS X %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36",
			macVer, v)
	case "safari":
		ver := randInt(r, safariMin, safariMax)
		sub := randInt(r, 0, 8)
		return fmt.Sprintf(
			"Mozilla/5.0 (Macintosh; Intel Mac OS X %s) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d Safari/605.1.15",
			macVer, ver, sub)
	default:
		v := randInt(r, firefoxMin, firefoxMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (Macintosh; Intel Mac OS X %s; rv:%d.0) Gecko/20100101 Firefox/%d.0",
			macVer, v, v)
	}
}

func generateLinuxUA(r *mulberry32) string {
	distro := pickWeighted(r,
		linuxDistros,
		[]float64{0.26, 0.16, 0.10, 0.08, 0.10, 0.10, 0.10, 0.10})

	switch pickWeighted(r, []string{"chrome", "firefox"}, []float64{0.6, 0.4}) {
	case "chrome":
		v := randInt(r, chromeMin, chromeMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Safari/537.36",
			distro, v)
	default:
		v := randInt(r, firefoxMin, firefoxMax)
		return fmt.Sprintf(
			"Mozilla/5.0 (%s; rv:%d.0) Gecko/20100101 Firefox/%d.0",
			distro, v, v)
	}
}

func generateAndroidUA(r *mulberry32) string {
	ver := randInt(r, 9, 14)
	device := androidDevices[randInt(r, 0, len(androidDevices)-1)]
	v := randInt(r, chromeMin, chromeMax)
	return fmt.Sprintf(
		"Mozilla/5.0 (Linux; Android %d; %s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.0 Mobile Safari/537.36",
		ver, device, v)
}

func generateIOSUA(r *mulberry32) string {
	iosVer := randInt(r, 15, 17)
	iosSubVer := randInt(r, 0, 8)
	builds := iosBuilds[iosVer]
	build := builds[randInt(r, 0, len(builds)-1)]

	device, cpu := "iPhone", "iPhone OS"
	if r.Next() > 0.7 { // 30% 走 iPad
		device, cpu = "iPad", "OS"
	}

	ver := randInt(r, iosSafariMin, iosSafariMax)
	sub := randInt(r, 0, 9)
	return fmt.Sprintf(
		"Mozilla/5.0 (%s; CPU %s %d_%d like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d Mobile/%s Safari/604.1",
		device, cpu, iosVer, iosSubVer, ver, sub, build)
}

// generateByOS 按加权分布挑一个平台并生成 UA。
func generateByOS(r *mulberry32) string {
	switch pickWeighted(r,
		[]string{"windows", "macos", "linux", "android", "ios"},
		[]float64{0.45, 0.20, 0.10, 0.15, 0.10}) {
	case "windows":
		return generateWindowsUA(r)
	case "macos":
		return generateMacUA(r)
	case "linux":
		return generateLinuxUA(r)
	case "android":
		return generateAndroidUA(r)
	default:
		return generateIOSUA(r)
	}
}

// UAPoolSize 按各平台取值表算出理论组合总数。
// 测试用它守住"池子够大"，收尾统计也用它告诉用户剩余额度。
//
// 注意这里刻意按表长计算而不是写死常数：以后往机型/版本表里加一项，
// 这个数字会自动跟着变，不会悄悄失真。
func UAPoolSize() int {
	chromeVers := chromeMax - chromeMin + 1
	firefoxVers := firefoxMax - firefoxMin + 1
	safariMacVers := (safariMax - safariMin + 1) * 9 // Version/x.y，y 取 0-8
	iosSafariVers := (iosSafariMax - iosSafariMin + 1) * 10
	builds := 0
	for _, b := range iosBuilds {
		builds += len(b)
	}

	windows := len(winVersions) * len(winArchs) * (chromeVers + chromeVers + firefoxVers)
	macos := len(macVersions) * (chromeVers + safariMacVers + firefoxVers)
	linux := len(linuxDistros) * (chromeVers + firefoxVers)
	android := 6 * len(androidDevices) * chromeVers // 6 = Android 9-14
	ios := builds * 2 * iosSafariVers               // 2 = iPhone / iPad

	return windows + macos + linux + android + ios
}

// ============================================================================
// 已用 UA 池：跨运行持久化
// ============================================================================
//
// 站点按 UA 记账，用过的 UA 会失效，所以必须记住"这个 UA 已经用过了"。
// 只在本进程内去重是不够的 —— 池子再大也扛不住天天跑。
//
// 文件里只存 UA 的 12 位十六进制指纹（不存原文）：
// 一是省空间，二是避免把一堆浏览器指纹原样落到磁盘上。
// 追加写、不重写，所以文件大小随已用数量线性增长、且天然可断点续。

const (
	uaPoolFileName = "ua_pool.txt"
	uaFPBytes      = 12 // 指纹长度（hex 字符数），48 bit，6 万条下碰撞概率可忽略
)

// uaPool 记录已用过的 UA 指纹。
type uaPool struct {
	mu    sync.Mutex
	used  map[string]bool
	path  string // 空 = 不持久化
	f     *os.File
	seen  int // 启动时已加载的数量
	added int
}

func newUAPool(path string) *uaPool {
	p := &uaPool{used: map[string]bool{}, path: path}
	if path != "" {
		p.load()
	}
	return p
}

func (p *uaPool) load() {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			p.used[line] = true
		}
	}
	p.seen = len(p.used)
}

// openAppend 惰性打开追加句柄。用 O_APPEND 保证多进程也不互相覆盖。
func (p *uaPool) openAppend() {
	if p.f != nil || p.path == "" {
		return
	}
	if dir := filepath.Dir(p.path); dir != "" {
		os.MkdirAll(dir, 0755)
	}
	f, err := os.OpenFile(p.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "! 无法写入 UA 池文件 %s：%v（本次不持久化，可能重复消耗 UA）\n", p.path, err)
		p.path = ""
		return
	}
	p.f = f
}

func (p *uaPool) Has(ua string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used[uaFingerprint(ua)]
}

// Add 记录一个 UA 并立即追加落盘。
// 立即写而不是攒到收尾 —— 收尾走 os.Exit，defer 不会执行（踩过这个坑）。
func (p *uaPool) Add(ua string) {
	fp := uaFingerprint(ua)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used[fp] {
		return
	}
	p.used[fp] = true
	p.added++
	if p.path == "" {
		return
	}
	p.openAppend()
	if p.f == nil {
		return
	}
	if _, err := p.f.WriteString(fp + "\n"); err != nil {
		fmt.Fprintf(os.Stderr, "! UA 池写入失败：%v\n", err)
	}
}

func (p *uaPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used)
}

func (p *uaPool) Stats() (total, preExisting, addedThisRun int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.used), p.seen, p.added
}

// Reset 清空池文件并重新开始（换站点、或想让 UA 额度"重新来过"时用）。
func (p *uaPool) Reset() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f != nil {
		p.f.Close()
		p.f = nil
	}
	if p.path != "" {
		if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	p.used = map[string]bool{}
	p.seen, p.added = 0, 0
	return nil
}

func (p *uaPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f != nil {
		p.f.Close()
		p.f = nil
	}
}

func uaFingerprint(ua string) string {
	sum := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(sum[:])[:uaFPBytes]
}

// ---------------------------------------------------------------- 全局入口

// uaPoolRef 是全局共享的已用 UA 池。main 启动时用 OpenUAPool 装配；
// 测试里可以直接换成内存池（path 为空）。
var uaPoolRef = newUAPool("")

// OpenUAPool 按配置打开 UA 池。path 为空表示不做持久化。
func OpenUAPool(path string) {
	uaPoolRef.Close()
	uaPoolRef = newUAPool(path)
}

// UAPoolStats 供收尾统计使用：已用总数、本次运行前已有的数量、本次新增。
func UAPoolStats() (total, pre, added int) { return uaPoolRef.Stats() }

// ResetUAPool 清空 UA 池。
func ResetUAPool() error { return uaPoolRef.Reset() }

// randomUA 生成一个**从未用过**的 User-Agent。
//
// 重摇上限设得比较宽松：池子接近耗尽时命中率会下降，
// 这里最多试 200 次；真摇不出来说明池子用完了，此时返回最后一个随机值，
// 由调用方通过收尾统计看到"剩余额度告急"的提示。
func randomUA() string {
	var last string
	for i := 0; i < 200; i++ {
		r := newMulberry32(uint32(time.Now().UnixNano()) ^ rand.Uint32())
		last = generateByOS(r)
		if !uaPoolRef.Has(last) {
			uaPoolRef.Add(last)
			return last
		}
	}
	uaPoolRef.Add(last)
	return last
}

// UACount 返回本次运行已生成的互不相同的 UA 数量（收尾统计用）。
func UACount() int {
	_, _, added := uaPoolRef.Stats()
	return added
}

// platformFromUA 由 UA 推断 navigator.platform 的取值（CDP 覆盖用）。
func platformFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"), strings.Contains(ua, "Macintosh"):
		return "MacIntel"
	case strings.Contains(ua, "Android"):
		return "Linux armv8l"
	case strings.Contains(ua, "X11"), strings.Contains(ua, "Linux"):
		return "Linux x86_64"
	default:
		return "Win32"
	}
}
