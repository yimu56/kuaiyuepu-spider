package main

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// initialZone 记录某个拼音首字母在 gb2312Level1 中的起始索引。
type initialZone struct {
	Letter string
	Start  int
}

// reservedNames 是 Windows 保留设备名，作为文件名时需要加前缀规避。
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// pinyinInitial 返回单个字符的拼音首字母（大写）；无法判定时返回 "#"。
//
// GB2312 一级汉字按汉语拼音排序，所以先在一级汉字表里定位该字，
// 再按区位索引落到对应的首字母区间；查不到则回退到手工补充表。
func pinyinInitial(ch rune) string {
	switch {
	case ch >= '0' && ch <= '9':
		return string(ch)
	case ch >= 'a' && ch <= 'z':
		return strings.ToUpper(string(ch))
	case ch >= 'A' && ch <= 'Z':
		return string(ch)
	}
	if !isHan(ch) {
		return "#"
	}
	if idx := runeIndexOf(gb2312Level1, ch); idx >= 0 {
		letter := "#"
		for _, z := range gbInitialZones {
			if idx >= z.Start {
				letter = z.Letter
			} else {
				break
			}
		}
		return letter
	}
	if l, ok := extraInitials[ch]; ok {
		return l
	}
	return "#"
}

// songInitial 取歌名首个「有效字符」的拼音首字母。
//
// 只看汉字与 ASCII 字母数字：日文假名、标点、符号一律跳过，
// 因此「この腕で…」会正确落到汉字「腕」上。
// 首字实在认不出（极生僻字）时继续往后找，避免退化成 "#-xxx"。
func songInitial(name string) string {
	for _, ch := range name {
		switch {
		case isHan(ch):
			if l := pinyinInitial(ch); l != "#" {
				return l
			}
		case ch < 128 && (ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'):
			return pinyinInitial(ch)
		}
	}
	return "#"
}

// isHan 判断是否为汉字。日文假名（平假名/片假名）不属于 Han，会被跳过。
func isHan(ch rune) bool {
	return unicode.Is(unicode.Han, ch)
}

// runeIndexOf 返回 target 在 s 中的 rune 序号（不是字节偏移），未找到返回 -1。
func runeIndexOf(s string, target rune) int {
	i := 0
	for _, r := range s {
		if r == target {
			return i
		}
		i++
	}
	return -1
}

var illegalRe = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// safeFilename 把歌名清洗成 Windows 合法文件名片段。
func safeFilename(name string) string {
	cleaned := illegalRe.ReplaceAllString(name, "_")
	cleaned = strings.Join(strings.Fields(cleaned), " ") // 折叠连续空白
	cleaned = strings.Trim(cleaned, " .")                // 去首尾空白与点
	if cleaned == "" {
		return "未命名"
	}
	if reservedNames[strings.ToUpper(cleaned)] {
		cleaned = "_" + cleaned
	}
	return cleaned
}

// buildFilename 按 `X-歌名.png` 规则生成文件名（不含重名处理）。
func buildFilename(name string) string {
	clean := safeFilename(name)
	return songInitial(clean) + "-" + clean + ".png"
}

// uniquePath 避免同一目录内重名：冲突时依次尝试 _2、_3 …
func uniquePath(dir, filename string, taken map[string]bool) string {
	stem := strings.TrimSuffix(filename, ".png")
	candidate := filename
	for i := 2; ; i++ {
		full := filepath.Join(dir, candidate)
		if !taken[candidate] && !fileExists(full) {
			return full
		}
		candidate = stem + "_" + strconv.Itoa(i) + ".png"
	}
}
