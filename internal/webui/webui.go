// Package webui 内嵌 app.py 会读的那几份静态文件（管理面板、浏览器扩展、取 cookie 的助手脚本），
// 以及 FastAPI 自动挂出来的 /openapi.json 与 /docs/oauth2-redirect 两份快照。
// 内容与部署中的 Python 版逐字节一致：admin.html、get_muse_cookie.py、openapi.json 的 sha256
// 已经和线上实例的响应对过（见 internal/server/testdata/routes_golden.json 的
// index / cookie_helper / openapi 三段）。
//
// assets/openapi.json 是 FastAPI 在自己进程里由 pydantic 模型生成后从线上抓下来的快照，
// 不是手写 —— 所以路由表一旦改动它不会跟着变。这条风险由 TestRouteTableMatchesOpenAPIDoc
// 钉住：内嵌文档里的 path×method 必须与 Go 路由表完全一致，多一条少一条都红。
// assets/oauth2-redirect.html 逐字取自部署环境那版 fastapi/openapi/docs.py 里的常量，
// 它本身就是 swagger-ui-dist 的 oauth2-redirect.html，不含任何应用相关取值。
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

//go:embed assets/openapi.json
var openAPIDoc []byte

//go:embed assets/oauth2-redirect.html
var oauth2RedirectHTML []byte

func AdminHTML() []byte { return adminHTML }

func CookieHelper() []byte { return cookieHelper }

// OpenAPIDoc 是 /openapi.json 的响应体（FastAPI 的 JSONResponse 就是这一串字节，
// 紧凑分隔符、无末尾换行）。
func OpenAPIDoc() []byte { return openAPIDoc }

// OAuth2RedirectHTML 是 /docs/oauth2-redirect 的响应体。
func OAuth2RedirectHTML() []byte { return oauth2RedirectHTML }

// ExtensionFiles 递归列扩展目录里的文件（相对路径），按字节序排，
// 对应 Python 的 walk + sorted：1.6.3 起扩展带 _locales 子目录。
func ExtensionFiles() []string {
	var out []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := extensionFS.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			if e.IsDir() {
				if err := walk(p); err != nil {
					return err
				}
			} else {
				out = append(out, strings.TrimPrefix(p, "extension/"))
			}
		}
		return nil
	}
	if err := walk("extension"); err != nil {
		return nil
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
