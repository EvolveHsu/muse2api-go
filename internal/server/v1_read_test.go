package server

import (
	"testing"
	"time"
)

// TestStarletteEtagFormula 的两组期望值是从容器里对**真实媒体文件**取的：
//
//	{"mtime": 1791212303.8032646, "size": 115944,
//	 "base": "1791212303.8032646-115944", "etag": "\"588254098bf073860fddf38f34e314e8\""}
//
// 第二组是媒体目录里的 .gitkeep（14 字节）。它同时钉住两件事：
// md5(str(st_mtime)+"-"+str(st_size)) 这个构造，以及 str(float) 的写法必须走 pyval.Float。
func TestStarletteEtagFormula(t *testing.T) {
	cases := []struct {
		mtime time.Time
		size  int64
		want  string
	}{
		{time.Unix(1791212303, 803264600), 115944, `"588254098bf073860fddf38f34e314e8"`},
		{time.Unix(1791112752, 683688400), 14, `"70424634ba813e962b1a00d211180f6c"`},
	}
	for _, c := range cases {
		if got := starletteEtag(c.mtime, c.size); got != c.want {
			t.Errorf("etag(%v, %d) = %s，Python 给 %s", c.mtime, c.size, got, c.want)
		}
	}
}
