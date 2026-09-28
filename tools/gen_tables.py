# -*- coding: utf-8 -*-
"""生成 Go 用的 GB2312 一级汉字索引表（tables.go）。

思路：GB2312 一级汉字（3755 个）本身就是按汉语拼音顺序排列的，
所以把整串按区位顺序内嵌进 Go 源码，用「某字在串中的索引」即可反推拼音首字母，
完全不需要第三方编码库。
"""
from pathlib import Path

# 每个拼音首字母对应的第一个汉字区位码（拼音无 I / U / V 开头的声母）
ZONES = [
    ("A", 0xB0A1), ("B", 0xB0C5), ("C", 0xB2C1), ("D", 0xB4EE), ("E", 0xB6EA),
    ("F", 0xB7A2), ("G", 0xB8C1), ("H", 0xB9FE), ("J", 0xBBF7), ("K", 0xBFA6),
    ("L", 0xC0AC), ("M", 0xC2E8), ("N", 0xC4C3), ("O", 0xC5B6), ("P", 0xC5BE),
    ("Q", 0xC6DA), ("R", 0xC8BB), ("S", 0xC8F6), ("T", 0xCBFA), ("W", 0xCDDA),
    ("X", 0xCEF4), ("Y", 0xD1B9), ("Z", 0xD4D1),
]
LEVEL1_END = 0xD7FE

# 与 Python 版保持一致的补充表（二级汉字 / 繁体 / 日文汉字）
EXTRA = {
    "禅": "C", "祓": "F", "嘀": "D", "嗒": "D", "嘞": "L", "咿": "Y",
    "喏": "N", "嗯": "E", "噢": "O", "啰": "L", "嗦": "S", "囡": "N",
    "彧": "Y", "昱": "Y", "晞": "X", "曦": "X", "榕": "R", "橦": "T",
    "歆": "X", "殇": "S", "淼": "M", "煜": "Y", "熠": "Y", "琛": "C",
    "瑾": "J", "璎": "Y", "筱": "X", "翀": "C", "蕤": "R", "谧": "M",
    "逍": "X", "钰": "Y", "鋆": "Y", "雩": "Y", "雯": "W", "霁": "J",
    "霏": "F", "霓": "N", "霄": "X", "妤": "Y", "嫮": "H", "虓": "X",
    "饕": "T", "餮": "T", "麋": "M", "黛": "D", "婳": "H", "嬅": "H",
    "寮": "L", "岚": "L", "嵘": "R", "桦": "H", "婧": "J", "菁": "J",
    "胭": "Y", "闺": "G", "隅": "Y", "骅": "H",
    "銀": "Y", "龍": "L", "鳳": "F", "雲": "Y", "愛": "A", "見": "J",
    "語": "Y", "樂": "L", "夢": "M", "華": "H", "國": "G", "學": "X",
    "會": "H", "廣": "G", "場": "C", "門": "M", "開": "K", "關": "G",
    "長": "C", "東": "D", "車": "C", "馬": "M", "鳥": "N", "魚": "Y",
    "風": "F", "電": "D", "話": "H", "機": "J", "點": "D", "燈": "D",
    "紅": "H", "綠": "L", "藍": "L", "黃": "H", "靜": "J", "麗": "L",
    "寶": "B", "貴": "G", "興": "X", "業": "Y", "產": "C", "權": "Q",
    "證": "Z", "書": "S", "讀": "D", "寫": "X", "聽": "T", "說": "S",
    "問": "W", "藥": "Y", "醫": "Y", "時": "S", "間": "J", "對": "D",
    "錯": "C", "實": "S", "現": "X", "發": "F", "變": "B", "還": "H",
    "這": "Z", "麼": "M", "樣": "Y", "們": "M", "來": "L", "過": "G",
    "為": "W", "與": "Y", "無": "W", "萬": "W", "個": "G", "頭": "T",
    "裡": "L", "邊": "B", "讓": "R", "覺": "J", "飛": "F", "歸": "G",
    "聲": "S", "響": "X", "記": "J", "憶": "Y", "親": "Q", "傷": "S",
    "別": "B", "離": "L", "亂": "L", "淚": "L", "詩": "S", "詞": "C",
    "琴": "Q", "笛": "D", "鼓": "G", "舞": "W", "畫": "H", "葉": "Y",
    "樹": "S", "橋": "Q", "湖": "H", "海": "H", "島": "D", "灣": "W",
    "軍": "J", "戰": "Z", "約": "Y", "戀": "L", "純": "C", "緣": "Y",
}


def build_level1() -> str:
    out = []
    for hi in range(0xB0, 0xD8):
        for lo in range(0xA1, 0xFF):
            code = (hi << 8) | lo
            if code > LEVEL1_END:
                break
            try:
                out.append(bytes([hi, lo]).decode("gb2312"))
            except UnicodeDecodeError:
                continue
    return "".join(out)


def main() -> None:
    s = build_level1()
    print(f"一级汉字数量: {len(s)}")
    assert len(s) == 3755, len(s)

    zones = []
    for letter, code in ZONES:
        ch = bytes([code >> 8, code & 0xFF]).decode("gb2312")
        zones.append((letter, s.index(ch)))

    # 分区自检：抽样看每个区间的首字
    print("\n各首字母起始索引（抽样首字）：")
    for i, (letter, start) in enumerate(zones):
        sample = s[start:start + 3]
        print(f"  {letter} @ {start:>5}  首字: {sample}")

    # --- 生成 Go 源码 ---
    lines = [
        "package main",
        "",
        "// 本文件由 tools/gen_tables.py 生成，请勿手工修改。",
        "//",
        "// gb2312Level1 是 GB2312 一级汉字（3755 个），严格按 GB2312 区位顺序排列。",
        "// 一级汉字本身就按汉语拼音排序，所以「某字在串中的索引」可直接反推拼音首字母，",
        "// 无需任何第三方编码库。",
        "",
    ]

    # 字符串按每行 40 个汉字断开，便于阅读
    chunk = 40
    pieces = [s[i:i + chunk] for i in range(0, len(s), chunk)]
    lines.append("var gb2312Level1 = " + "\n\t".join(
        '"%s" +' % p if i < len(pieces) - 1 else '"%s"' % p
        for i, p in enumerate(pieces)))
    lines.append("")

    lines.append("// gbInitialZones 记录每个拼音首字母在 gb2312Level1 中的起始索引。")
    lines.append("// 拼音里没有以 I / U / V 开头的声母，故表中不含这三个字母。")
    lines.append("var gbInitialZones = []initialZone{")
    for letter, start in zones:
        lines.append('\t{"%s", %d},' % (letter, start))
    lines.append("}")
    lines.append("")

    lines.append("// extraInitials 收录二级汉字（按部首排序、编码里查不到拼音）")
    lines.append("// 以及曲谱名中常见的繁体 / 日文汉字。")
    lines.append("var extraInitials = map[rune]string{")
    items = sorted(EXTRA.items(), key=lambda kv: kv[0])
    for i in range(0, len(items), 6):
        row = items[i:i + 6]
        lines.append("\t" + " ".join("'%s': \"%s\"," % (k, v) for k, v in row))
    lines.append("}")
    lines.append("")

    out_path = Path(__file__).resolve().parent.parent / "go" / "tables.go"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text("\n".join(lines), encoding="utf-8")
    print(f"\n已生成: {out_path}  ({out_path.stat().st_size} 字节)")


if __name__ == "__main__":
    main()
