package main

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	apiURL   = "https://www.kuaiyuepu.com/web/song.php?action=index_more"
	siteBase = "https://www.kuaiyuepu.com"
	pageSize = 90
)

// 列表项：<a href="/jianpu/XXXX.html"><div class="item-content">歌名</div></a>
var (
	itemRe = regexp.MustCompile(
		`(?s)<a\s+href="/jianpu/([A-Za-z0-9]+)\.html">\s*` +
			`<div class="item-content">\s*(.*?)\s*</div>`)
	tagRe = regexp.MustCompile(`<[^>]+>`)
)

// Song 是一条曲谱记录。
type Song struct {
	ID   string
	Name string
}

// listResult 是一页列表的抓取结果。
type listResult struct {
	Items     []Song
	NextPoint string
}

// fetchPage 请求一页曲谱列表。sortPoint 为空表示从最新开始。
func fetchPage(sortPoint, cookie string, retries int) (*listResult, error) {
	form := url.Values{}
	form.Set("sort_by", "new")
	form.Set("subsite", "wind")
	if sortPoint != "" {
		form.Set("sort_point", sortPoint)
	}
	body := form.Encode()

	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error

	for attempt := 1; attempt <= retries; attempt++ {
		result, err := doFetch(client, body, cookie)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt < retries {
			wait := attempt * 2
			fmt.Printf("  请求失败（%v），%d 秒后重试 [%d/%d]\n", err, wait, attempt, retries-1)
			time.Sleep(time.Duration(wait) * time.Second)
		}
	}
	return nil, fmt.Errorf("列表请求连续失败：%w", lastErr)
}

func doFetch(client *http.Client, body, cookie string) (*listResult, error) {
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("User-Agent", randomUA())
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", siteBase)
	req.Header.Set("Referer", siteBase+"/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parseList(raw)
}

func parseList(raw []byte) (*listResult, error) {
	var obj struct {
		ErrCode int    `json:"errCode"`
		ErrMsg  string `json:"errMsg"`
		Result  struct {
			DataList  string `json:"data_list"`
			Count     int    `json:"count"`
			Limit     int    `json:"limit"`
			SortPoint string `json:"sort_point"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("响应不是合法 JSON：%w", err)
	}
	if obj.ErrCode != 0 {
		return nil, fmt.Errorf("接口返回错误：errCode=%d errMsg=%q", obj.ErrCode, obj.ErrMsg)
	}

	res := &listResult{NextPoint: obj.Result.SortPoint}
	for _, m := range itemRe.FindAllStringSubmatch(obj.Result.DataList, -1) {
		name := html.UnescapeString(strings.TrimSpace(tagRe.ReplaceAllString(m[2], "")))
		res.Items = append(res.Items, Song{ID: m[1], Name: name})
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// 断点状态
// ---------------------------------------------------------------------------

// FailedItem 记录一条"抓不动"的曲谱。
//
// 存在的理由：站点按 UA 记账，抽到已耗尽的 UA 会失败。如果"只要有一条失败
// 就不推进游标"，那么只要页面上存在一条**反复失败**的曲谱（会员专享、已下架、
// 或恰好碰上一串死 UA），游标就会永远卡在这一页 —— 表现为
// `--pages 10` 只跑得动第 1 轮。所以失败要计数，试够次数就记为"放弃"并放行。
type FailedItem struct {
	Tries  int    `json:"tries"`            // 累计失败的"遍"数（跨运行累计）；一遍含 --retry 次换 UA 尝试
	Reason string `json:"reason,omitempty"` // 最后一次失败的原因
	Last   string `json:"last,omitempty"`   // 最后一次失败的时间
}

// State 是断点文件的结构。
// NextSortPoint 是页级游标（下一次请求的起点）；
// Shots 是条级清单（曲谱 ID -> 已保存的文件名），两者配合实现不重不漏。
//
// 关于体积：Shots 每抓一条就多一项（约 35 字节 JSON），
// 且程序默认每成功一条就整体重写一次文件 —— 条目越多，单次写入越大。
// 因此 State 提供了 Pages/TotalShots 等累计字段，配合 --state-flush-every
// 与 --prune-missing 控制写放大，见 stateWriter。
type State struct {
	Version       int                   `json:"version"`
	NextSortPoint string                `json:"next_sort_point"`
	Runs          int                   `json:"runs"`
	Pages         int                   `json:"pages"`
	TotalItems    int                   `json:"total_items"`
	TotalShots    int                   `json:"total_shots"`
	LastRun       string                `json:"last_run"`
	Shots         map[string]string     `json:"shots"`
	Failed        map[string]FailedItem `json:"failed,omitempty"`

	corrupt bool `json:"-"` // 本次加载时文件损坏（已备份并重置）
}

const stateVersion = 3

func newState() *State {
	return &State{Version: stateVersion, Shots: map[string]string{}, Failed: map[string]FailedItem{}}
}

// givenUp 判断某条是否已经"失败够遍数、可以放过"。
// threshold <= 0 表示永不放弃（严格模式，游标会一直卡着直到该条成功）。
func (st *State) givenUp(id string, threshold int) bool {
	if threshold <= 0 {
		return false
	}
	f, ok := st.Failed[id]
	return ok && f.Tries >= threshold
}

// noteFailure 记一次失败并返回累计次数。
func (st *State) noteFailure(id, reason string) int {
	if st.Failed == nil {
		st.Failed = map[string]FailedItem{}
	}
	f := st.Failed[id]
	f.Tries++
	f.Reason = reason
	f.Last = time.Now().Format("2006-01-02 15:04:05")
	st.Failed[id] = f
	return f.Tries
}

// clearFailure 在成功抓到之后清掉历史失败记录。
func (st *State) clearFailure(id string) {
	if st.Failed != nil {
		delete(st.Failed, id)
	}
}

// loadState 读取断点文件。
// 文件损坏时**不再静默重置**：先把原文件另存为 .bad 供事后排查，
// 再把 corrupt 置位让调用方明确告知用户（否则用户只会看到莫名其妙的重复抓取）。
func loadState(path string) *State {
	data, err := os.ReadFile(path)
	if err != nil {
		return newState()
	}
	st := newState()
	if err := json.Unmarshal(data, st); err != nil {
		os.Rename(path, path+".bad")
		bad := newState()
		bad.corrupt = true
		return bad
	}
	if st.Shots == nil {
		st.Shots = map[string]string{}
	}
	if st.Failed == nil {
		st.Failed = map[string]FailedItem{}
	}
	if st.Version < stateVersion {
		st.Version = stateVersion
	}
	return st
}

// stateWarnBytes 是断点文件体积的告警阈值（超过就在收尾时提示用户）。
const stateWarnBytes = 4 << 20 // 4 MB

// stateWriter 负责把 State 落盘，集中处理"文件变大之后"才会出现的问题：
//   - 原子写：先写 .tmp 再 rename，中途崩溃不会留下半个 JSON；
//   - rename 重试：Windows 上杀软 / 索引服务 / 网盘同步可能短暂占用目标文件，
//     一次失败就报错会让用户白丢一整轮进度；
//   - 首次覆盖前留一份 .bak：主文件被外部改坏时还有得救；
//   - 记录最近一次写入体积，供收尾时判断是否需要提示"该调参数了"。
type stateWriter struct {
	path string

	mu         sync.Mutex
	bakDone    bool
	failStreak int
	lastSize   int64
}

func newStateWriter(path string) *stateWriter {
	return &stateWriter{path: path}
}

func (sw *stateWriter) save(st *State) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := sw.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		sw.noteFail(err)
		return err
	}
	// 本次运行首次覆盖已有文件时留底（只做一次，不做每条的额外 IO）。
	// 注意 bakDone 只在"确实备份过"时才置位：首次运行时文件还不存在，
	// 不能就此认为备份任务已完成，否则第二次保存就把上一份内容覆盖掉了。
	if !sw.bakDone {
		if _, err := os.Stat(sw.path); err == nil {
			if b, err := os.ReadFile(sw.path); err == nil {
				if err := os.WriteFile(sw.path+".bak", b, 0644); err == nil {
					sw.bakDone = true
				}
			}
		}
	}

	var lastErr error
	for i := 0; i < 3; i++ {
		if err := os.Rename(tmp, sw.path); err == nil {
			sw.failStreak = 0
			sw.lastSize = int64(len(data))
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
	sw.noteFail(lastErr)
	return lastErr
}

// noteFail 连续失败时只提醒有限次数，避免磁盘写满后把控制台刷爆
// （真实故障现场：日志刷满屏幕反而把关键错误顶出视野）。
func (sw *stateWriter) noteFail(err error) {
	sw.failStreak++
	if sw.failStreak <= 3 {
		fmt.Printf("! 断点保存失败（%d 次）：%v\n", sw.failStreak, err)
		if sw.failStreak == 3 {
			fmt.Println("  ! 后续同类失败不再重复提示。请检查磁盘空间是否已满、文件是否被占用。")
		}
	}
}

func (sw *stateWriter) size() int64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.lastSize
}

// pruneResult 是一次断点清理的结果。
type pruneResult struct {
	Missing int      // 截图已不在磁盘
	Blank   int      // 截图还在，但小得像空白图
	Files   []string // 疑似空白图的文件名（供用户核对后自行删除）
}

// pruneOpts 控制清理范围。两项**互相独立**：
// 只想清空白图时，不该顺手把"文件缺失"的 900 条也清掉 ——
// 那会让程序把它们全部重抓一遍。
type pruneOpts struct {
	Missing  bool  // 清理"文件已不在磁盘"的条目
	MinBytes int64 // >0 时，清理小于该字节数的条目（疑似空白图）
}

// pruneShots 清掉断点里"截图已经不可用"的记录。
//
// 两种情形都要清：
//   - 文件已不在磁盘（用户手工删过截图）—— 这类条目只会让断点文件白胖，留着没意义；
//   - 文件还在但小得不正常 —— 站点限速时曾存下过 3 KB 的空白图（正常图 20 KB 起），
//     它们被记成"已抓"就永远不会重抓了，必须清掉才能补回来。
//
// 注意只清**断点记录**，不动磁盘上的文件：删用户文件这种事必须由用户自己决定，
// 所以这里把可疑文件列出来，让用户核对后再删（程序不替他从磁盘上删东西）。
// 两项开关独立生效，见 pruneOpts。
func pruneShots(st *State, outPath string, opt pruneOpts, maxList int) pruneResult {
	var res pruneResult
	for id, name := range st.Shots {
		full := filepath.Join(outPath, name)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			if opt.Missing {
				delete(st.Shots, id)
				res.Missing++
			}
			continue
		}
		if opt.MinBytes > 0 && info.Size() < opt.MinBytes {
			delete(st.Shots, id)
			res.Blank++
			if len(res.Files) < maxList {
				res.Files = append(res.Files, fmt.Sprintf("%s (%d 字节)", name, info.Size()))
			}
		}
	}
	return res
}

// pruneMissingShots 只清"文件缺失"的条目。
func pruneMissingShots(st *State, outPath string) int {
	return pruneShots(st, outPath, pruneOpts{Missing: true}, 0).Missing
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirExists 判断目录是否存在。
//
// 注意别拿 fileExists 当它用 —— 那个函数刻意排除了目录，
// 对目录永远返回 false。曾经就因为这一处混用，导致"清理 profile 目录"
// 整段逻辑直接短路，工作目录里堆满了清不掉的 _browser_profile_*。
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func dirOf(path string) string {
	d := filepath.Dir(path)
	if d == "" {
		return "."
	}
	return d
}
