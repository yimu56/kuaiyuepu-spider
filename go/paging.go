package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// pendingResult 描述"本页里哪些要抓、哪些跳过"。
type pendingResult struct {
	Todo    []Song
	Skipped int // 已抓过且截图仍在磁盘上
	GaveUp  int // 反复失败、已记为放弃
}

// selectPending 从一页列表里挑出真正要抓的条目。
//
// 抽出来当纯函数是为了让"已放弃不等于已抓"这条不变量有地方被钉住：
// 被放弃的条目必须从待抓队列里剔除（否则游标永远卡着、--pages 白设），
// 同时**绝不能**写进 shots（它并没有抓到，写进去就是永久漏抓）。
func selectPending(items []Song, st *State, outPath string, retryFailed bool, giveUpAfter int) pendingResult {
	var out pendingResult
	for _, s := range items {
		if prev, ok := st.Shots[s.ID]; ok && fileExists(filepath.Join(outPath, prev)) {
			out.Skipped++
			continue
		}
		if !retryFailed && st.givenUp(s.ID, giveUpAfter) {
			out.GaveUp++
			continue
		}
		out.Todo = append(out.Todo, s)
	}
	return out
}

// pageVerdict 描述"一轮抓取结束后该怎么走"。
//
// 抽成纯函数是因为这是整个程序正确性的枢纽：判错的后果只有两种 ——
// 要么漏抓（游标跳过去了），要么重复抓（游标不动）。所以它必须能被独立测试。
type pageVerdict struct {
	Next      string // 要写入 st.NextSortPoint 的值
	Stop      bool   // 是否结束整个运行
	Advance   bool   // 游标是否前进了
	SamePage  bool   // 是否继续处理同一页（游标不动，下一轮重拉本页）
	Reason    string // 收尾时展示给用户的说明
	RoundNote string // 本轮结束后的即时说明（可为空）
}

// judgePage 依据本轮结果给出下一轮该怎么走。
//
//	curPoint 本轮使用的游标（原值，可能为空表示"从最新开始"）
//	apiNext  接口回传的下一页游标（空 = 已到末页）
//	failed   本轮失败条数
//	notDone  本轮未完成条数（被中断 / 实例启动失败导致没跑到的）
//	rest     本轮 limit 之外、本页还剩的条数
//	started  成功启动的抓取实例数
//
// 推进游标的条件（缺一不可）：
//  1. 本轮任务全部有结果（notDone == 0）；
//  2. 没有任何失败（failed == 0）；
//  3. 本页没有剩余（rest == 0），即整页都处理完了。
//
// 三种结局：
//   - 有失败 / 有未完成 / 没有可用实例 → **就地停止**，游标不动，下次补抓；
//   - 本页没抓完（rest > 0）→ 游标不动，但**继续**：下一轮重拉本页，
//     靠 shots 清单跳过已抓的接着往下抓。这样 --limit 小于一页时
//     --pages 才有意义（否则第一轮就会因"本页还有剩余"而中止，
//     多轮参数形同虚设）；
//   - 已到末页且整页处理完 → 游标清空，停止。
func judgePage(curPoint, apiNext string, failed, notDone, rest, started int) pageVerdict {
	hardStop := failed > 0 || notDone > 0 || started == 0

	if hardStop {
		var reasons []string
		if failed > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 条失败", failed))
		}
		if notDone > 0 {
			reasons = append(reasons, fmt.Sprintf("%d 条未完成", notDone))
		}
		if started == 0 {
			reasons = append(reasons, "没有可用浏览器实例")
		}
		return pageVerdict{
			Next: curPoint,
			Stop: true,
			Reason: fmt.Sprintf("%s，断点保持不动（%s）；下次运行会重拉本页并补抓缺失条目",
				strings.Join(reasons, "、"), orNone(curPoint)),
		}
	}

	if rest > 0 {
		return pageVerdict{
			Next:      curPoint,
			SamePage:  true,
			RoundNote: fmt.Sprintf("本页还剩 %d 条，游标保持不动，下一轮重拉本页继续抓", rest),
		}
	}

	if apiNext == "" {
		return pageVerdict{
			Next:      "",
			Stop:      true,
			Advance:   true,
			Reason:    "接口未回传新游标（已到末页），下次仍从最新开始",
			RoundNote: "已到末页，本页已抓完",
		}
	}

	return pageVerdict{
		Next:      apiNext,
		Advance:   true,
		Reason:    "本轮已抓完，断点推进到：" + apiNext,
		RoundNote: "本轮已抓完，断点推进到：" + apiNext,
	}
}
