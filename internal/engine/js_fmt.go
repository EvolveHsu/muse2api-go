package engine

import (
	"fmt"
	"strings"
)

// pyFormat 复刻 Python 的 `tmpl % (args...)` 语义，只支持 %s 与 %%。
//
// 为什么不用 fmt.Sprintf：Go 的动词表比 Python 宽（%v/%d/%q/...），一旦某段 JS 里
// 出现字面 % 后面跟着 s 之外的字符，fmt.Sprintf 会静默产出别的东西，而 Python 是抛
// ValueError。这里显式只认 %s/%%，其余报编译期之外的硬错误，行为与 Python 一致。
func pyFormat(tmpl string, args ...string) (string, error) {
	var b strings.Builder
	ai := 0
	for i := 0; i < len(tmpl); {
		if tmpl[i] != '%' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}
		if i+1 >= len(tmpl) {
			return "", fmt.Errorf("不支持的格式化：%q 以单个 %% 结尾", tmpl[max(0, i-8):i+1])
		}
		switch nxt := tmpl[i+1]; {
		case nxt == '%':
			b.WriteByte('%')
			i += 2
		case nxt == 's':
			if ai >= len(args) {
				return "", fmt.Errorf("not enough arguments for format string（第 %d 个 %%s 缺参数）", ai+1)
			}
			b.WriteString(args[ai])
			ai++
			i += 2
		default:
			return "", fmt.Errorf("unsupported format character %q", string(nxt))
		}
	}
	if ai < len(args) {
		return "", fmt.Errorf("not all arguments converted during string formatting（多余 %d 个参数）", len(args)-ai)
	}
	return b.String(), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
