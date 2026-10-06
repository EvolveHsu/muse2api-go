package server

import "strings"

// pyJSONErr 复刻 CPython Lib/json/decoder.py 解析失败时抛的 (msg, pos)。
//
// FastAPI 在 body 是合法 Content-Type 但 JSON 读不通时，捕获 json.JSONDecodeError 后
// 报的是 422 {'type':'json_invalid','loc':('body', e.pos),'ctx':{'error': e.msg}} ——
// pos 与 msg 都要逐字对上，所以这里不能借用 encoding/json 的报错（它的措辞和偏移都不同）。
//
// pos 数的是**字符**下标而不是字节（`{坏 json` 线上报 1，按字节会是 3），所以全程按 rune 走。
// 分支与 CPython 一一对应：
//
//	Expecting value                                值的位置不是值
//	Expecting property name enclosed in double quotes	对象键位不是字符串
//	Expecting ':' delimiter				键后面不是冒号
//	Expecting ',' delimiter			元素/成员之间缺逗号
//	Unterminated string starting at	字符串没闭合（pos 是**字符串开头**）
//	Extra data					顶层值之后还有东西
func pyJSONErr(s string) (string, int) {
	p := &jsonScan{r: []rune(s)}
	msg, pos := p.value(0, false)
	if msg != "" {
		return msg, pos
	}
	// 顶层值读完了，剩下非空白的就是多余内容
	i := p.skipWS(p.i)
	if i < len(p.r) {
		return "Extra data", i
	}
	return "", 0
}

type jsonScan struct {
	r []rune
	i int // value() 走过后停下的位置（仅在没有报错时有效）
}

func (p *jsonScan) skipWS(i int) int {
	for i < len(p.r) {
		switch p.r[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// value 解析 s[i] 处的一个 JSON 值；返回 (errMsg, errPos)，errMsg 为空表示成功（i 已推进）。
func (p *jsonScan) value(i int, inContainer bool) (string, int) {
	i = p.skipWS(i)
	p.i = i
	if i >= len(p.r) {
		return "Expecting value", i
	}
	switch c := p.r[i]; {
	case c == '"':
		return p.str(i)
	case c == '{':
		return p.object(i + 1)
	case c == '[':
		return p.array(i + 1)
	case strings.HasPrefix(string(p.r[i:]), "true"):
		return "", p.advance(i, 4, inContainer)
	case strings.HasPrefix(string(p.r[i:]), "false"):
		return "", p.advance(i, 5, inContainer)
	case strings.HasPrefix(string(p.r[i:]), "null"):
		return "", p.advance(i, 4, inContainer)
	case c == '-' || (c >= '0' && c <= '9'):
		return "", p.number(i)
	default:
		return "Expecting value", i
	}
}

// CPython 的 JSONObject 允许 `true`/`false`/`null` 后面直接跟分隔符，
// 但也允许 `truex` 这种被 NUMBER_RE/关键词正则吃掉前缀后剩下垃圾 —— 交给 Extra data 处理。
func (p *jsonScan) advance(i, n int, _ bool) int {
	p.i = i + n
	return i + n
}

func (p *jsonScan) number(i int) int {
	j := i
	if j < len(p.r) && p.r[j] == '-' {
		j++
	}
	for j < len(p.r) && p.r[j] >= '0' && p.r[j] <= '9' {
		j++
	}
	if j < len(p.r) && p.r[j] == '.' {
		j++
		for j < len(p.r) && p.r[j] >= '0' && p.r[j] <= '9' {
			j++
		}
	}
	if j < len(p.r) && (p.r[j] == 'e' || p.r[j] == 'E') {
		k := j + 1
		if k < len(p.r) && (p.r[k] == '+' || p.r[k] == '-') {
			k++
		}
		digits := k
		for k < len(p.r) && p.r[k] >= '0' && p.r[k] <= '9' {
			k++
		}
		if k > digits {
			j = k
		}
	}
	p.i = j
	return j
}

// str 从开引号处扫描；未闭合时报的 pos 是**开头**那个引号的下标。
// 反斜杠后面只允许 `"\/bfnrtu` 八个字符（CPython 的 C 扫描器），否则 "Invalid \escape"；
// 裸控制字符报 "Invalid control character at"。两句措辞都由金样本钉着（gen_bad_escape / gen_bad_control）。
func (p *jsonScan) str(i int) (string, int) {
	j := i + 1
	for j < len(p.r) {
		c := p.r[j]
		switch {
		case c == '\\':
			if j+1 >= len(p.r) || !strings.ContainsRune(`"\/bfnrtu`, p.r[j+1]) {
				return "Invalid \\escape", j
			}
			if p.r[j+1] == 'u' {
				// 注意位置与 \q 那条不同：CPython 报的是 **反斜杠后一个字符**（'u'）的下标，
				// 而非法转义报的是反斜杠本身的下标 —— gen_bad_escape 与 gen_bad_uni 各钉一条。
				for k := j + 2; k < j+6; k++ {
					if k >= len(p.r) || !isHexDigit(p.r[k]) {
						return "Invalid \\uXXXX escape", j + 1
					}
				}
			}
			j += 2
		case c == '"':
			p.i = j + 1
			return "", 0
		case c < 0x20:
			return "Invalid control character at", j
		default:
			j++
		}
	}
	return "Unterminated string starting at", i
}

func (p *jsonScan) object(i int) (string, int) {
	i = p.skipWS(i)
	if i < len(p.r) && p.r[i] == '}' {
		p.i = i + 1
		return "", 0
	}
	for {
		i = p.skipWS(i)
		if i >= len(p.r) || p.r[i] != '"' {
			return "Expecting property name enclosed in double quotes", i
		}
		if msg, pos := p.str(i); msg != "" {
			return msg, pos
		}
		i = p.skipWS(p.i)
		if i >= len(p.r) || p.r[i] != ':' {
			return "Expecting ':' delimiter", i
		}
		if msg, pos := p.value(i+1, true); msg != "" {
			return msg, pos
		}
		i = p.skipWS(p.i)
		if i < len(p.r) && p.r[i] == ',' {
			i++
			continue
		}
		if i < len(p.r) && p.r[i] == '}' {
			p.i = i + 1
			return "", 0
		}
		return "Expecting ',' delimiter", i
	}
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func (p *jsonScan) array(i int) (string, int) {
	i = p.skipWS(i)
	if i < len(p.r) && p.r[i] == ']' {
		p.i = i + 1
		return "", 0
	}
	for {
		if msg, pos := p.value(i, true); msg != "" {
			return msg, pos
		}
		i = p.skipWS(p.i)
		if i < len(p.r) && p.r[i] == ',' {
			i++
			continue
		}
		if i < len(p.r) && p.r[i] == ']' {
			p.i = i + 1
			return "", 0
		}
		return "Expecting ',' delimiter", i
	}
}
