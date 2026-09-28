package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 验证 UA 是「动态生成」而不是「从池子里挑」：
// 连续生成 300 个必须互不相同，且格式看起来是正常的浏览器 UA。
func TestRandomUADynamic(t *testing.T) {
	const n = 300
	seen := make(map[string]bool, n)

	var sampleWin, sampleMac, sampleMobile, sampleFF string
	for i := 0; i < n; i++ {
		ua := randomUA()

		if seen[ua] {
			t.Fatalf("第 %d 次生成出现重复 UA：%s（池子/随机空间不够大）", i, ua)
		}
		seen[ua] = true

		if !strings.HasPrefix(ua, "Mozilla/5.0 (") {
			t.Fatalf("UA 格式异常：%s", ua)
		}
		if !strings.Contains(ua, "Chrome/") && !strings.Contains(ua, "Firefox/") &&
			!strings.Contains(ua, "Version/") {
			t.Fatalf("UA 里找不到浏览器标识：%s", ua)
		}

		switch {
		case sampleMac == "" && strings.Contains(ua, "Macintosh"):
			sampleMac = ua
		case sampleMobile == "" && strings.Contains(ua, "Android"):
			sampleMobile = ua
		case sampleFF == "" && strings.Contains(ua, "Firefox"):
			sampleFF = ua
		case sampleWin == "" && strings.Contains(ua, "Windows NT"):
			sampleWin = ua
		}
	}

	if len(seen) != n {
		t.Fatalf("期望 %d 个不同 UA，实际 %d 个", n, len(seen))
	}
	t.Logf("%d 个 UA 全部互不相同", n)
	t.Logf("  Windows : %s", sampleWin)
	t.Logf("  macOS   : %s", sampleMac)
	t.Logf("  Android : %s", sampleMobile)
	t.Logf("  Firefox : %s", sampleFF)
}

// 验证操作系统的加权分布与油猴脚本一致：
// Windows .45 / macOS .20 / Linux .10 / Android .15 / iOS .10
//
// 注意这里直接调 generateByOS 而不走 randomUA —— 后者额外做了
// 「本次运行内不重复」的重摇，会把分布拉平，不适合用来验分布。
func TestUAOSDistribution(t *testing.T) {
	const n = 20000
	counts := map[string]int{}
	want := map[string]float64{
		"windows": 0.45, "macos": 0.20, "linux": 0.10,
		"android": 0.15, "ios": 0.10,
	}

	for i := 0; i < n; i++ {
		// 固定 seed 序列，保证测试可复现
		r := newMulberry32(uint32(i)*2654435761 + 0x9E3779B9)
		ua := generateByOS(r)
		switch {
		case strings.Contains(ua, "Windows NT"):
			counts["windows"]++
		case strings.Contains(ua, "Macintosh"):
			counts["macos"]++
		case strings.Contains(ua, "Android"):
			counts["android"]++
		case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
			counts["ios"]++
		case strings.Contains(ua, "X11"):
			counts["linux"]++
		default:
			t.Fatalf("有 UA 归类不了：%s", ua)
		}
	}

	for osName, exp := range want {
		got := float64(counts[osName]) / n
		// 2 万次采样的统计涨落约 ±0.01，留到 ±0.03 足够宽松
		if math.Abs(got-exp) > 0.03 {
			t.Errorf("%s 占比 %.4f，与期望 %.2f 偏差过大", osName, got, exp)
		}
		t.Logf("  %-8s 实际 %.4f  期望 %.2f", osName, got, exp)
	}
}

// 验证各平台的版本号范围与脚本一致。
//
// 注意 macOS Safari 的 Version/ 是 16-18，而 iOS Safari 是 15-17，
// 两者必须分开判断（iOS 的 UA 里同样含 "like Mac OS X"）。
func TestUAVersionRanges(t *testing.T) {
	for i := 0; i < 3000; i++ {
		r := newMulberry32(uint32(i)*40503 + 7919)
		ua := generateByOS(r)

		for _, bad := range []string{
			"Chrome/109", "Chrome/136", "Chrome/140",
			"Firefox/112", "Firefox/135", "Firefox/137",
		} {
			if strings.Contains(ua, bad) {
				t.Fatalf("版本号越界（%s）：%s", bad, ua)
			}
		}

		switch {
		case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
			if strings.Contains(ua, "Version/14.") || strings.Contains(ua, "Version/18.") {
				t.Fatalf("iOS Safari 版本越界（应为 15-17）：%s", ua)
			}
		case strings.Contains(ua, "Macintosh"):
			if strings.Contains(ua, "Version/15.") || strings.Contains(ua, "Version/19.") {
				t.Fatalf("macOS Safari 版本越界（应为 16-18）：%s", ua)
			}
		}
	}
	t.Log("Chrome 110-135 / Firefox 113-134 / macOS Safari 16-18 / iOS Safari 15-17 全部在范围内")
}

// ---------------------------------------------------------------------------
// UA 池：容量、唯一性、跨运行持久化
// ---------------------------------------------------------------------------

// TestUAPoolSizeIsLarge 守住"池子必须够大"这条底线。
//
// 额度是按 UA 记账的，池子多大就直接决定总共能抓多少条。
// 曾经只有 1935 种，跑满 9 次（约 900 条）后近一半抽签都落在已耗尽的 UA 上。
func TestUAPoolSizeIsLarge(t *testing.T) {
	size := UAPoolSize()
	t.Logf("UA 组合空间：%d 种", size)
	if size < 25000 {
		t.Fatalf("UA 池只剩 %d 种，太小了：池子容量直接限制总抓取量", size)
	}
}

// TestRandomUAUniqueAtScale 用 2 万次抽签验证池子确实能供得上。
func TestRandomUAUniqueAtScale(t *testing.T) {
	const n = 20000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		ua := randomUA()
		if seen[ua] {
			t.Fatalf("第 %d 次抽到重复 UA：%s", i, ua)
		}
		seen[ua] = true
	}
	_, _, added := UAPoolStats()
	if added < n {
		t.Fatalf("本次新增应 >= %d，实际 %d", n, added)
	}
	t.Logf("连续抽取 %d 个 UA 全部互不相同", n)
}

// TestUAPoolPersistsAcrossRuns 验证跨运行去重：第二次打开池子时，
// 上一轮用过的 UA 必须仍然被认得出来。
func TestUAPoolPersistsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, uaPoolFileName)

	p1 := newUAPool(path)
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	if p1.Has(ua) {
		t.Fatal("全新的池子不该认为这个 UA 用过")
	}
	p1.Add(ua)
	p1.Add(ua) // 重复 Add 不应重复计数
	if !p1.Has(ua) {
		t.Fatal("Add 之后应认得出来")
	}
	if _, _, added := p1.Stats(); added != 1 {
		t.Fatalf("去重后应只新增 1 条，实际 %d", added)
	}
	p1.Close()

	p2 := newUAPool(path)
	if !p2.Has(ua) {
		t.Fatal("重新打开池子后，上一轮用过的 UA 必须仍被记住（否则会重复消耗）")
	}
	total, pre, added := p2.Stats()
	if total != 1 || pre != 1 || added != 0 {
		t.Fatalf("统计应为 total=1 pre=1 added=0，实际 %d/%d/%d", total, pre, added)
	}

	// 文件里只存指纹，不该出现 UA 原文
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Mozilla/5.0") {
		t.Fatal("池文件里不该出现 UA 原文，只应存指纹")
	}
	if err := p2.Reset(); err != nil {
		t.Fatal(err)
	}
	if p2.Has(ua) {
		t.Fatal("Reset 之后应回到空池")
	}
	p2.Close()
}

// TestUAPoolDisabledPersistsNothing 空路径 = 不持久化，不应崩也不应写文件。
func TestUAPoolDisabledPersistsNothing(t *testing.T) {
	p := newUAPool("")
	p.Add("test-ua")
	if !p.Has("test-ua") {
		t.Fatal("内存里去重仍应生效")
	}
	p.Close()
}

// TestGlobalRandomUAAvoidsAlreadyUsed 端到端验证 randomUA 会跳过失效 UA。
func TestGlobalRandomUAAvoidsAlreadyUsed(t *testing.T) {
	dir := t.TempDir()
	old := uaPoolRef
	defer func() { uaPoolRef = old }()

	OpenUAPool(filepath.Join(dir, uaPoolFileName))
	defer uaPoolRef.Close()

	first := randomUA()
	if !uaPoolRef.Has(first) {
		t.Fatal("生成的 UA 应立即被记为已用")
	}
	for i := 0; i < 200; i++ {
		if randomUA() == first {
			t.Fatal("randomUA 不该再次返回已经用过的 UA")
		}
	}
}
