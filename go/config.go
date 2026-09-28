package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// Settings 是一次运行的全部可调项。
//
// 优先级：命令行显式给出的参数 > 配置文件 > 内置默认值。
// 之所以要"显式"判断，是因为命令行里写 `--workers 1` 与不写，
// 在 flag 看来都是一个值，只有 flag.Visit 才能区分。
type Settings struct {
	// —— 抓取规模 ——
	Pages   int     `json:"pages"`   // 本次运行最多抓多少轮（页）；0 = 不限，抓到末页或出错为止
	Limit   int     `json:"limit"`   // 每轮最多处理多少条"尚未抓过"的条目
	Workers int     `json:"workers"` // 并发浏览器实例数；1 = 原来的串行行为
	Delay   float64 `json:"delay"`   // 每个 worker 内两条之间的间隔秒数
	Retry   int     `json:"retry"`   // 单条命中限制时的换 UA 重试次数

	// —— 失败处置 ——
	//
	// 站点按 UA 记账，抽到已被耗尽的 UA 就会失败 —— 这是常态，
	// 不能让它把整轮甚至整个运行卡住。早期版本"有一条失败就停下、
	// 游标原地不动"，后果是 --pages 10 永远只跑得动第 1 轮。
	RetryPasses int  `json:"retry_passes"`  // 本轮失败的条目**在本轮内**再补抓几遍
	GiveUpAfter int  `json:"give_up_after"` // 累计失败"遍数"达到该值即记为放弃、放行游标；0 = 永不放弃
	RetryFailed bool `json:"retry_failed"`  // 重新尝试已放弃的条目（例如补上登录 Cookie 之后）

	// —— 路径 ——
	Out        string `json:"out"`          // 截图输出目录（空 = 程序所在目录）
	StatePath  string `json:"state"`        // 断点文件（空 = 程序目录下 crawl_state.json）
	LogFile    string `json:"log_file"`     // 运行日志；空字符串 = 不写日志
	UAPoolFile string `json:"ua_pool_file"` // 已用 UA 指纹的持久化文件；空字符串 = 不持久化
	Browser    string `json:"browser"`      // 指定浏览器可执行文件
	Cookie     string `json:"cookie"`       // 可选附加 Cookie

	// —— 渲染 ——
	Width int `json:"width"` // 截图视口宽度

	// —— 日志与状态文件体积控制（对应"文件过大"类问题）——
	LogMaxMB        int  `json:"log_max_mb"`           // 单个日志文件上限（MB），超出即轮转
	LogKeep         int  `json:"log_keep"`             // 轮转后保留的历史份数（run.log.1 …）
	Quiet           bool `json:"quiet"`                // 只记录失败与汇总，显著缩小日志
	StateFlushEvery int  `json:"state_flush_every"`    // 每成功 N 条落盘一次断点（1 = 逐条）
	AbortAfterFails int  `json:"abort_after_failures"` // 连续失败达到该条数就提前中止
	PruneMissing    bool `json:"prune_missing"`        // 启动时清理"截图已不可用"的断点条目
	PruneBlankKB    int  `json:"prune_blank_kb"`       // 配合 --prune-missing：小于该 KB 的图视为空白图并清理；0 = 不判空白

	// —— 一次性动作 ——
	FromPoint string `json:"from_point"`
	Reset     bool   `json:"reset"`
	DryRun    bool   `json:"dry_run"`
	Headful   bool   `json:"headful"`

	// —— 运行期字段，不参与配置持久化 ——
	ConfigPath  string `json:"-"`
	InitConfig  bool   `json:"-"`
	ResetUAPool bool   `json:"-"`
}

// DefaultSettings 返回内置默认值。
//
// Workers 默认 1（串行）不是保守，而是实测结果：本站点按 IP 限速，
// 并发一开，单页加载时间从 0.5 秒涨到 6.7 秒（--workers 6），
// 把并发省下的时间全吃回去还倒亏。详见 README 的「并发抓取实测」一节。
func DefaultSettings() Settings {
	return Settings{
		Pages:       1,
		Limit:       pageSize,
		Workers:     1,
		Delay:       1.2,
		Retry:       3,
		RetryPasses: 2,
		// = RetryPasses(2) + 1：一轮内三遍全失败就放过。
		// 每一遍内部还有 --retry(3) 次换 UA 尝试，所以一条要连败 9 次才被放弃。
		GiveUpAfter:     3,
		RetryFailed:     false,
		Out:             "",
		StatePath:       "",
		LogFile:         "run.log",
		UAPoolFile:      uaPoolFileName,
		Browser:         "",
		Cookie:          "",
		Width:           1280,
		LogMaxMB:        8,
		LogKeep:         5,
		Quiet:           false,
		StateFlushEvery: 1,
		AbortAfterFails: 5,
		PruneMissing:    false,
		PruneBlankKB:    0,
	}
}

// ConfigFileName 是默认配置文件名（放在 exe 同目录）。
const ConfigFileName = "crawler.config.json"

// loadConfig 读取配置文件。文件不存在不算错误（直接用默认值）。
// 只覆盖配置里出现过的字段，未出现的保持调用方传入的默认值。
func loadConfig(path string) (Settings, bool, error) {
	s := DefaultSettings()
	if path == "" {
		return s, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, false, nil
		}
		return s, false, err
	}
	// 用指针字段承接，才能区分"写了 false"与"没写"
	var raw struct {
		Pages           *int     `json:"pages"`
		Limit           *int     `json:"limit"`
		Workers         *int     `json:"workers"`
		Delay           *float64 `json:"delay"`
		Retry           *int     `json:"retry"`
		RetryPasses     *int     `json:"retry_passes"`
		GiveUpAfter     *int     `json:"give_up_after"`
		RetryFailed     *bool    `json:"retry_failed"`
		Out             *string  `json:"out"`
		StatePath       *string  `json:"state"`
		LogFile         *string  `json:"log_file"`
		UAPoolFile      *string  `json:"ua_pool_file"`
		Browser         *string  `json:"browser"`
		Cookie          *string  `json:"cookie"`
		Width           *int     `json:"width"`
		LogMaxMB        *int     `json:"log_max_mb"`
		LogKeep         *int     `json:"log_keep"`
		Quiet           *bool    `json:"quiet"`
		StateFlushEvery *int     `json:"state_flush_every"`
		AbortAfterFails *int     `json:"abort_after_failures"`
		PruneMissing    *bool    `json:"prune_missing"`
		PruneBlankKB    *int     `json:"prune_blank_kb"`
		FromPoint       *string  `json:"from_point"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return s, false, fmt.Errorf("配置文件不是合法 JSON：%w", err)
	}
	setInt(&s.Pages, raw.Pages)
	setInt(&s.Limit, raw.Limit)
	setInt(&s.Workers, raw.Workers)
	setFloat(&s.Delay, raw.Delay)
	setInt(&s.Retry, raw.Retry)
	setInt(&s.RetryPasses, raw.RetryPasses)
	setInt(&s.GiveUpAfter, raw.GiveUpAfter)
	setBool(&s.RetryFailed, raw.RetryFailed)
	setStr(&s.Out, raw.Out)
	setStr(&s.StatePath, raw.StatePath)
	setStr(&s.LogFile, raw.LogFile)
	setStr(&s.UAPoolFile, raw.UAPoolFile)
	setStr(&s.Browser, raw.Browser)
	setStr(&s.Cookie, raw.Cookie)
	setInt(&s.Width, raw.Width)
	setInt(&s.LogMaxMB, raw.LogMaxMB)
	setInt(&s.LogKeep, raw.LogKeep)
	setBool(&s.Quiet, raw.Quiet)
	setInt(&s.StateFlushEvery, raw.StateFlushEvery)
	setInt(&s.AbortAfterFails, raw.AbortAfterFails)
	setBool(&s.PruneMissing, raw.PruneMissing)
	setInt(&s.PruneBlankKB, raw.PruneBlankKB)
	setStr(&s.FromPoint, raw.FromPoint)
	return s, true, nil
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
func setFloat(dst *float64, v *float64) {
	if v != nil {
		*dst = *v
	}
}
func setStr(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}
func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

// bindFlags 把 Settings 绑到命令行参数上，返回一个函数：
// 在 flag.Parse() 之后调用，把"显式给出"的参数写回 Settings。
func bindFlags(s *Settings) func() {
	f := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)

	pages := f.Int("pages", s.Pages, "本次运行最多抓取多少轮（页）；0 = 不限，抓到末页或出错为止")
	limit := f.Int("limit", s.Limit, "每轮最多处理多少条（以\"尚未抓过\"计，非页内前 N 条）")
	workers := f.Int("workers", s.Workers, "并发浏览器实例数；1 = 串行")
	delay := f.Float64("delay", s.Delay, "每个 worker 内两条之间的间隔秒数")
	retry := f.Int("retry", s.Retry, "单条命中访问限制时的最大尝试次数（每次自动换 UA）")
	retryPasses := f.Int("retry-passes", s.RetryPasses, "本轮失败的条目在本轮内再补抓几遍（0 = 不补抓）")
	giveUpAfter := f.Int("give-up-after", s.GiveUpAfter, "累计失败达到该次数即记为放弃并放行游标；0 = 永不放弃")
	retryFailed := f.Bool("retry-failed", s.RetryFailed, "重新尝试已放弃的条目（如补上登录 Cookie 之后）")
	uaPoolFile := f.String("ua-pool-file", s.UAPoolFile, "已用 UA 指纹的持久化文件；空字符串 = 不持久化")
	resetUAPool := f.Bool("reset-ua-pool", false, "清空已用 UA 池（UA 额度告急时才需要）")
	outDir := f.String("out", s.Out, "截图输出目录（默认：程序所在目录）")
	stateP := f.String("state", s.StatePath, "断点记录文件（默认：程序目录下 crawl_state.json）")
	logFile := f.String("log-file", s.LogFile, "运行日志文件；传空字符串则关闭日志")
	logMaxMB := f.Int("log-max-mb", s.LogMaxMB, "单个日志文件上限（MB），超出即轮转")
	logKeep := f.Int("log-keep", s.LogKeep, "日志轮转保留的历史份数")
	quiet := f.Bool("quiet", s.Quiet, "日志只记失败与汇总")
	flushEvery := f.Int("state-flush-every", s.StateFlushEvery, "每成功 N 条落盘一次断点（默认 1，逐条）")
	abortFails := f.Int("abort-after-failures", s.AbortAfterFails, "连续失败达到该条数即提前中止")
	prune := f.Bool("prune-missing", s.PruneMissing, "启动时清理\"截图已不可用\"的断点条目")
	pruneBlank := f.Int("prune-blank-kb", s.PruneBlankKB, "配合 --prune-missing：小于该 KB 的图视为空白图并清理（如 8）；0 = 只清文件缺失的")
	width := f.Int("width", s.Width, "截图视口宽度")
	fromP := f.String("from-point", s.FromPoint, "手动指定起始游标，形如 '2026-04-24 16:14:08,34BGw1eUX'")
	reset := f.Bool("reset", s.Reset, "忽略已有断点，从最新一页重新开始")
	dryRun := f.Bool("dry-run", s.DryRun, "只抓取列表并打印，不截图、不写断点")
	headful := f.Bool("headful", s.Headful, "显示浏览器窗口（调试用）")
	brPath := f.String("browser", s.Browser, "Chrome / Edge 可执行文件路径")
	cookie := f.String("cookie", s.Cookie, "可选：附加 Cookie 字符串（默认匿名访问，无需登录）")
	cfgPath := f.String("config", "", "配置文件路径（默认：程序同目录 "+ConfigFileName+"）")
	initCfg := f.Bool("init-config", false, "生成一份带默认值的配置文件后退出")

	// 先让调用方的 Usage 生效（flag 包默认输出到 stderr，符合原行为）
	f.Usage = func() {
		fmt.Fprintf(os.Stderr, "快乐谱曲谱爬虫（Go 版）：列表抓取 + 详情页截图 + 断点续抓\n\n用法：\n  %s [参数]\n\n参数：\n", filepath.Base(os.Args[0]))
		f.PrintDefaults()
	}
	// 解析 os.Args[1:]（与 flag.Parse 行为一致；-h 走 Usage 后退出）
	if err := f.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}

	return func() {
		f.Visit(func(fl *flag.Flag) {
			switch fl.Name {
			case "pages":
				s.Pages = *pages
			case "limit":
				s.Limit = *limit
			case "workers":
				s.Workers = *workers
			case "delay":
				s.Delay = *delay
			case "retry":
				s.Retry = *retry
			case "retry-passes":
				s.RetryPasses = *retryPasses
			case "give-up-after":
				s.GiveUpAfter = *giveUpAfter
			case "retry-failed":
				s.RetryFailed = *retryFailed
			case "ua-pool-file":
				s.UAPoolFile = *uaPoolFile
			case "reset-ua-pool":
				s.ResetUAPool = *resetUAPool
			case "out":
				s.Out = *outDir
			case "state":
				s.StatePath = *stateP
			case "log-file":
				s.LogFile = *logFile
			case "log-max-mb":
				s.LogMaxMB = *logMaxMB
			case "log-keep":
				s.LogKeep = *logKeep
			case "quiet":
				s.Quiet = *quiet
			case "state-flush-every":
				s.StateFlushEvery = *flushEvery
			case "abort-after-failures":
				s.AbortAfterFails = *abortFails
			case "prune-missing":
				s.PruneMissing = *prune
			case "prune-blank-kb":
				s.PruneBlankKB = *pruneBlank
			case "width":
				s.Width = *width
			case "from-point":
				s.FromPoint = *fromP
			case "reset":
				s.Reset = *reset
			case "dry-run":
				s.DryRun = *dryRun
			case "headful":
				s.Headful = *headful
			case "browser":
				s.Browser = *brPath
			case "cookie":
				s.Cookie = *cookie
			}
		})
		s.ConfigPath = *cfgPath
		s.InitConfig = *initCfg
	}
}

// normalize 把越界/空值收敛到安全范围。
func (s *Settings) normalize(exeDir string) {
	if s.Pages < 0 {
		s.Pages = 0
	}
	if s.Workers < 1 {
		s.Workers = 1
	}
	if s.Workers > 8 {
		s.Workers = 8
	}
	if s.Limit < 1 {
		s.Limit = 1 // 0 条没有意义，且会让多轮逻辑空转
	}
	if s.Retry < 1 {
		s.Retry = 1
	}
	if s.RetryPasses < 0 {
		s.RetryPasses = 0
	}
	if s.RetryPasses > 10 {
		s.RetryPasses = 10
	}
	if s.GiveUpAfter < 0 {
		s.GiveUpAfter = 0
	}
	if s.PruneBlankKB < 0 {
		s.PruneBlankKB = 0
	}
	if s.AbortAfterFails < 1 {
		s.AbortAfterFails = 1
	}
	if s.Delay < 0 {
		s.Delay = 0
	}
	if s.Width < 320 {
		s.Width = 320
	}
	if s.LogMaxMB < 1 {
		s.LogMaxMB = 1
	}
	if s.LogKeep < 0 {
		s.LogKeep = 0
	}
	if s.StateFlushEvery < 1 {
		s.StateFlushEvery = 1
	}
	if s.Out == "" {
		s.Out = exeDir
	}
	if s.StatePath == "" {
		s.StatePath = filepath.Join(exeDir, "crawl_state.json")
	}
	if s.UAPoolFile == "" {
		s.UAPoolFile = "" // 显式关闭持久化
	} else if !filepath.IsAbs(s.UAPoolFile) {
		s.UAPoolFile = filepath.Join(exeDir, s.UAPoolFile)
	}
	// 一律转成绝对路径。这不是洁癖：--user-data-dir 传相对路径时，
	// Chrome 会照常启动但**不监听调试端口**，表现为"卡住 60 秒后报无法连接浏览器"，
	// 极难排查。所以输出目录与断点文件都在这里定死为绝对路径。
	if abs, err := filepath.Abs(s.Out); err == nil {
		s.Out = abs
	}
	if abs, err := filepath.Abs(s.StatePath); err == nil {
		s.StatePath = abs
	}
}

// writeExampleConfig 生成一份可直接改的配置文件（所有键都写全）。
func writeExampleConfig(path string) error {
	s := DefaultSettings()
	s.Out = ""
	s.StatePath = ""
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}
