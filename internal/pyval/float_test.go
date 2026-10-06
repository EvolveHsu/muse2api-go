package pyval

import (
	"math"
	"testing"
)

// 这些期望值是从 NAS 容器里那份真 Python 取的 str(float) 输出，不是手推的。
func TestFloatMatchesPythonStr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1.0, "1.0"},
		{100.0, "100.0"},
		{255.0, "255.0"},
		{1759647503.0, "1759647503.0"},
		{1759647503.4562244, "1759647503.4562244"},
		{0.5, "0.5"},
		{0.0001, "0.0001"},
		{0.00001, "1e-05"},
		{1.5e-5, "1.5e-05"},
		{-2.5e-7, "-2.5e-07"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1.5e16, "1.5e+16"},
		{1e22, "1e+22"},
		{1.2345678901234567e19, "1.2345678901234567e+19"},
		{3.14, "3.14"},
		{addFloats(0.1, 0.2), "0.30000000000000004"},
	}
	for _, c := range cases {
		if got := Float(c.in); got != c.want {
			t.Errorf("Float(%v) = %q，Python 给 %q", c.in, got, c.want)
		}
	}
}

// addFloats 逼出真正的 float64 加法：写成 0.1+0.2 字面量的话
// Go 会在编译期按精确有理数折叠成 0.3，测不到二进制浮点的那条路径。
func addFloats(a, b float64) float64 { return a + b }

func TestFloatSpecials(t *testing.T) {
	if got := Float(math.NaN()); got != "nan" {
		t.Errorf("nan: %q", got)
	}
	if got := Float(math.Inf(1)); got != "inf" {
		t.Errorf("inf: %q", got)
	}
	if got := Float(math.Inf(-1)); got != "-inf" {
		t.Errorf("-inf: %q", got)
	}
}
