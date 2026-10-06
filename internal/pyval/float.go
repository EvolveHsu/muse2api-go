// Package pyval 放那些「Go 默认格式化跟 Python 不一样」的最小复刻件。
package pyval

import (
	"math"
	"strconv"
	"strings"
)

// Float 复刻 Python 的 str(float)/repr(float)：
// 最短可往返十进制；定点写法下整数补 ".0"；只有数量级 <1e-4 或 >=1e16 才切科学计数法，
// 且科学计数法下 Python 不补 ".0"（repr(1e16) == '1e+16'，repr(1.0) == '1.0'）。
//
// 为什么要专门写：Starlette 的 FileResponse 用 str(st_mtime) + "-" + str(st_size) 算 etag，
// Go 的 FormatFloat('g') 会把 1759647503.0 写成 "1.759647503e+09"，
// 于是同一个媒体文件在两个实现下 etag 完全不同，客户端的条件请求全部失效。
func Float(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	neg := math.Signbit(f)
	abs := math.Abs(f)
	if abs == 0 {
		if neg {
			return "-0.0"
		}
		return "0.0"
	}
	s := strconv.FormatFloat(abs, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(s, "e")
	e, _ := strconv.Atoi(expStr)
	digits := strings.ReplaceAll(mant, ".", "")

	if e >= -4 && e < 16 {
		var out string
		switch {
		case e >= 0:
			if len(digits) <= e+1 {
				out = digits + strings.Repeat("0", e+1-len(digits)) + ".0"
			} else {
				out = digits[:e+1] + "." + digits[e+1:]
			}
		default:
			out = "0." + strings.Repeat("0", -e-1) + digits
		}
		return sign(neg) + out
	}
	// 科学计数法：Go 的输出已经是 d[.d]e±XX（指数至少两位、带符号），与 Python 一致。
	return sign(neg) + s
}

func sign(neg bool) string {
	if neg {
		return "-"
	}
	return ""
}
