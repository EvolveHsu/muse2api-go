package server

// FastAPI 在自己装配时白挂出来的四条路由：/openapi.json、/docs、/redoc、/docs/oauth2-redirect。
// 面板之外它们也是对外契约的一部分（Swagger UI 页 + 它加载的文档），
// 所以这里按 fastapi/openapi/docs.py 的模板逐块拼，不是随手写的一个近似页面：
//   ① /docs 的字节已由线上金样本钉住（routes_golden.json 的 docs 段，sha256 比对）；
//   ② /openapi.json 直接内嵌线上抓下来的那份快照；
//   ③ /redoc 与 /docs/oauth2-redirect 同出自那一个 docs.py，但线上还没录过它们的体，
//     属于「同源转写、待补录」——金样本探测脚本里已经排好了这两条。
//
// 取值口径（与部署环境一致）：uvicorn 不带 --root-path，所以 root_path 恒为 ""，
// openapi_url 就是 "/openapi.json"；app 的 title="muse2api"，version="1.5.3"；
// 未设 swagger_ui_parameters / swagger_ui_init_oauth，所以走 fastapi 的那套默认参数。
//
// DEVIATION(#13): /openapi.json 走内嵌快照，不在运行时生成（Python 是 pydantic 现算）——
// 路由表对账测试 TestRouteTableMatchesOpenAPIDoc 兜漂移。
// DEVIATION(#14): /redoc 与 /docs/oauth2-redirect 的字节还没被线上录制钉住，
// 与已校验通过的 /docs 同源同版本，补录探针已排进 golden_routes.py。

import (
	"net/http"
	"strings"

	"muse2api-go/internal/pyval"
	"muse2api-go/internal/webui"
)

// 与 fastapi/openapi/docs.py 的函数默认值一致（0.142.x；/docs 字节已按这版校验通过）。
const (
	swaggerJSURL    = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"
	swaggerCSSURL   = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"
	swaggerFavURL   = "https://fastapi.tiangolo.com/img/favicon.png"
	redocJSURL      = "https://cdn.jsdelivr.net/npm/redoc@2/bundles/redoc.standalone.js"
	redocFavURL     = "https://fastapi.tiangolo.com/img/favicon.png"
	openAPIJSONPath = "/openapi.json"
	oauth2RedirPath = "/docs/oauth2-redirect"
	// appTitle 是 app.py 里 `FastAPI(title=...)` 的那个字面量，不是环境变量。
	appTitle = "muse2api"
)

// swaggerUIParams 对应 fastapi.openapi.docs.swagger_ui_default_parameters —— 顺序即渲染顺序。
var swaggerUIParams = []pyval.Pair{
	{"dom_id", "#swagger-ui"},
	{"layout", "BaseLayout"},
	{"deepLinking", true},
	{"showExtensions", true},
	{"showCommonExtensions", true},
}

func (a *App) registerDocs() {
	a.add("GET", openAPIJSONPath, false, func(c *reqCtx) (any, error) {
		return rawBytes(http.StatusOK, "application/json", webui.OpenAPIDoc()), nil
	})
	a.add("GET", "/docs", false, func(c *reqCtx) (any, error) {
		// get_swagger_ui_html：title 是 app.title + " - Swagger UI"。
		return rawBytes(http.StatusOK, "text/html; charset=utf-8",
			[]byte(swaggerUIHTML(openAPIJSONPath, appTitle+" - Swagger UI", oauth2RedirPath))), nil
	})
	a.add("GET", "/redoc", false, func(c *reqCtx) (any, error) {
		return rawBytes(http.StatusOK, "text/html; charset=utf-8",
			[]byte(redocUIHTML(openAPIJSONPath, appTitle+" - ReDoc"))), nil
	})
	a.add("GET", oauth2RedirPath, false, func(c *reqCtx) (any, error) {
		return rawBytes(http.StatusOK, "text/html; charset=utf-8", webui.OAuth2RedirectHTML()), nil
	})
}

func rawBytes(status int, contentType string, body []byte) *rawResponse {
	return &rawResponse{Status: status, Headers: map[string]string{"content-type": contentType}, Body: body}
}

// swaggerUIHTML 复刻 get_swagger_ui_html：四段拼接 + 参数逐行 + oauth2 回调行，
// 连块边界的空白都保持原样（那 1007 个字节里每一格都在金样本的 sha256 里）。
func swaggerUIHTML(openapiURL, title, oauth2Redirect string) string {
	var b strings.Builder
	b.WriteString("\n    <!DOCTYPE html>\n    <html>\n    <head>\n")
	b.WriteString("    <meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n")
	b.WriteString("    <link type=\"text/css\" rel=\"stylesheet\" href=\"" + swaggerCSSURL + "\">\n")
	b.WriteString("    <link rel=\"shortcut icon\" href=\"" + swaggerFavURL + "\">\n")
	b.WriteString("    <title>" + title + "</title>\n    </head>\n    <body>\n")
	b.WriteString("    <div id=\"swagger-ui\">\n    </div>\n")
	b.WriteString("    <script src=\"" + swaggerJSURL + "\"></script>\n")
	b.WriteString("    <!-- `SwaggerUIBundle` is now available on the page -->\n    <script>\n")
	b.WriteString("    const ui = SwaggerUIBundle({\n        url: '" + openapiURL + "',\n    ")
	for _, p := range swaggerUIParams {
		b.WriteString(htmlSafeJSON(p.Key) + ": " + htmlSafeJSON(p.Val) + ",\n")
	}
	if oauth2Redirect != "" {
		b.WriteString("oauth2RedirectUrl: window.location.origin + '" + oauth2Redirect + "',")
	}
	b.WriteString("\n    presets: [\n        SwaggerUIBundle.presets.apis,\n")
	b.WriteString("        SwaggerUIBundle.SwaggerUIStandalonePreset\n        ],\n    })")
	b.WriteString("\n    </script>\n    </body>\n    </html>\n    ")
	return b.String()
}

// redocUIHTML 复刻 get_redoc_html（with_google_fonts 默认 True）。
func redocUIHTML(openapiURL, title string) string {
	var b strings.Builder
	b.WriteString("\n    <!DOCTYPE html>\n    <html>\n    <head>\n    <title>" + title + "</title>\n")
	b.WriteString("    <!-- needed for adaptive design -->\n    <meta charset=\"utf-8\"/>\n")
	b.WriteString("    <meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n    ")
	b.WriteString("\n    <link href=\"https://fonts.googleapis.com/css?family=Montserrat:300,400,700|Roboto:300,400,700\" rel=\"stylesheet\">\n    ")
	b.WriteString("\n    <link rel=\"shortcut icon\" href=\"" + redocFavURL + "\">\n    <!--\n")
	b.WriteString("    ReDoc doesn't change outer page styles\n    -->\n    <style>\n")
	b.WriteString("      body {\n        margin: 0;\n        padding: 0;\n      }\n    </style>\n")
	b.WriteString("    </head>\n    <body>\n    <noscript>\n")
	b.WriteString("        ReDoc requires Javascript to function. Please enable it to browse the documentation.\n")
	b.WriteString("    </noscript>\n    <redoc spec-url=\"" + openapiURL + "\"></redoc>\n")
	b.WriteString("    <script src=\"" + redocJSURL + "\"> </script>\n    </body>\n    </html>\n    ")
	return b.String()
}

// htmlSafeJSON 对应 docs.py 的 _html_safe_json：json.dumps 之后再把 < > & 转成 \\uXXXX，
// 免得嵌进 <script> 的值提前把标签关掉。
func htmlSafeJSON(v any) string {
	return strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).
		Replace(pyval.Dumps(v, true, false))
}
