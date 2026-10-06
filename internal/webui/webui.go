// Package webui 内嵌 app.py 会读的那几份静态文件（管理面板、浏览器扩展、取 cookie 的助手脚本）。
// 内容与部署中的 Python 版逐字节一致：admin.html 与 get_muse_cookie.py 的 sha256
// 已经和线上实例的响应对过（见 internal/server/testdata/routes_golden.json 的 index / cookie_helper）。
package webui

import (
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed admin.html
var adminHTML []byte

//go:embed extension
var extensionFS embed.FS

//go:embed tools/get_muse_cookie.py
var cookieHelper []byte

func AdminHTML() []byte { return adminHTML }

func CookieHelper() []byte { return cookieHelper }

// ExtensionFiles 列扩展目录里的文件名（对应 os.listdir + 只留文件）。
// 注意：Python 排的是 os.listdir 的结果再 sorted，这里同样按字节序排。
func ExtensionFiles() []string {
	entries, err := extensionFS.ReadDir("extension")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ExtensionFile 读扩展目录里的单个文件。
func ExtensionFile(name string) ([]byte, bool) {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return nil, false
	}
	b, err := extensionFS.ReadFile(path.Join("extension", name))
	if err != nil {
		return nil, false
	}
	return b, true
}

// ExtensionDir 暴露只读的扩展文件系统，供 zip 打包时按同样顺序遍历。
func ExtensionDir() fs.FS { return extensionFS }
