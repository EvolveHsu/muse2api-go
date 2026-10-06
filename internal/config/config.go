// Package config 是 muse2api 的配置，逐字段对应 Python 的 config.py。
package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DEVIATION: Python 用 config.py 所在目录作 base_dir；Go 二进制没有 __file__，
// 改用可执行文件所在目录（在镜像里同为 /app，语义一致）。
var defaultBaseDir = func() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}()

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envBool 照抄 Python 的 `_env(...).strip() != "0"`：除 "0" 外任何值都为真。
func envBool(key, def string) bool {
	return strings.TrimSpace(env(key, def)) != "0"
}

func envInt(key, def string) int {
	raw := env(key, def)
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		// Python 在这里会 ValueError 崩在导入期，等价于启动失败。
		log.Fatalf("muse2api: 环境变量 %s=%q 不是整数", key, raw)
	}
	return n
}

func loadDotenv() {
	base := env("MUSE2API_HOME", defaultBaseDir)
	data, err := os.ReadFile(filepath.Join(base, ".env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") || !strings.Contains(s, "=") {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), "'\"")
		if k == "" {
			continue
		}
		if _, ok := os.LookupEnv(k); !ok {
			os.Setenv(k, v)
		}
	}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// whichLike 对应 shutil.which：绝对/带分隔符的路径按文件判断，裸名字在 PATH 里找。
func whichLike(name string) bool {
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return isFile(name)
	}
	exts := []string{name}
	if os.PathSeparator == '\\' {
		for _, e := range strings.Split(os.Getenv("PATHEXT"), ";") {
			if e != "" {
				exts = append(exts, name+e)
			}
		}
	}
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		for _, cand := range exts {
			if isFile(filepath.Join(dir, cand)) {
				return true
			}
		}
	}
	return false
}

// DetectChromium 优先用 MUSE2API_CHROMIUM；未设置或路径不存在时探测系统已装的 Chromium/Chrome。
func DetectChromium() string {
	configured := strings.TrimSpace(os.Getenv("MUSE2API_CHROMIUM"))
	if configured != "" && (isFile(configured) || whichLike(configured)) {
		return configured
	}
	candidates := []string{
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"chromium",
		"chromium-browser",
		"google-chrome",
		"chrome",
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, c := range candidates {
		if isFile(c) || whichLike(c) {
			return c
		}
	}
	if configured != "" {
		return configured
	}
	return "chromium"
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}

type Config struct {
	BaseDir string
	Host    string
	Port    int

	Chromium  string
	CdpPort   int
	HomeDir   string
	ExtraPath string
	SiteURL   string
	LoginWait int

	// profile_root 必须落在 $HOME 下：snap 版 chromium 受 AppArmor 约束只能读写 $HOME，
	// 传 /opt 或 /tmp 不报错但被静默忽略；也不能用点开头的目录（snap home 接口排除隐藏文件）。
	ProfileRoot string

	APIKey      string
	PublicBase  string
	CORSOrigins string

	ImageTimeout int
	VideoTimeout int
	ChatTimeout  int

	// muse.ai 助手会明确拒绝输出"伪工具调用"，强行注入协议反而污染正常回答，
	// 所以默认只做参数兼容（传 tools 不 422），不注入也不解析。
	ToolProtocol bool

	// 开启后被管理员禁用的账号仍参与自动保活与 VM 唤醒，但不参与生图/生视频/对话的轮转调度。
	KeepaliveDisabledAccounts bool

	// DEVIATION(#1): 下面两项 Python 没有，用于空闲释放浏览器以达成 <100MB 常驻；
	// WarmTab=true 时完全恢复 Python 的常驻行为。
	WarmTab            bool
	BrowserIdleMinutes int
}

func Load() *Config {
	return &Config{
		BaseDir: env("MUSE2API_HOME", defaultBaseDir),
		Host:    env("MUSE2API_HOST", "0.0.0.0"),
		Port:    envInt("MUSE2API_PORT", "18610"),

		Chromium:  DetectChromium(),
		CdpPort:   envInt("MUSE2API_CDP_PORT", "19210"),
		HomeDir:   env("MUSE2API_HOME_DIR", homeDir()),
		ExtraPath: env("MUSE2API_EXTRA_PATH", "/snap/bin"),
		SiteURL:   env("MUSE2API_SITE", "https://muse.ai/"),
		LoginWait: envInt("MUSE2API_LOGIN_WAIT", "60"),

		ProfileRoot: env("MUSE2API_PROFILE_ROOT", filepath.Join(homeDir(), "muse2api-profiles")),

		APIKey:      env("MUSE2API_KEY", ""),
		PublicBase:  env("MUSE2API_PUBLIC_BASE", ""),
		CORSOrigins: env("MUSE2API_CORS_ORIGINS", "https://muse.ai,https://www.muse.ai"),

		ImageTimeout: envInt("MUSE2API_IMAGE_TIMEOUT", "240"),
		VideoTimeout: envInt("MUSE2API_VIDEO_TIMEOUT", "600"),
		ChatTimeout:  envInt("MUSE2API_CHAT_TIMEOUT", "300"),

		ToolProtocol:              envBool("MUSE2API_TOOL_PROTOCOL", "0"),
		KeepaliveDisabledAccounts: envBool("MUSE2API_KEEPALIVE_DISABLED_ACCOUNTS", "0"),

		WarmTab:            envBool("MUSE2API_WARM_TAB", "0"),
		BrowserIdleMinutes: envInt("MUSE2API_BROWSER_IDLE_MINUTES", "30"),
	}
}

func (c *Config) DataDir() string      { return filepath.Join(c.BaseDir, "data") }
func (c *Config) MediaDir() string     { return filepath.Join(c.DataDir(), "media") }
func (c *Config) DownloadDir() string  { return filepath.Join(c.DataDir(), "downloads") }
func (c *Config) ProfileDir() string   { return filepath.Join(c.ProfileRoot, "generate") }
func (c *Config) AccountsFile() string { return filepath.Join(c.DataDir(), "accounts.json") }
func (c *Config) TasksFile() string    { return filepath.Join(c.DataDir(), "tasks.json") }

func (c *Config) EnsureDirs() {
	for _, d := range []string{c.BaseDir, c.DataDir(), c.MediaDir(), c.DownloadDir(), c.ProfileDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatalf("muse2api: 创建目录 %s 失败: %v", d, err)
		}
	}
}

// CFG 对应 Python 模块级的 CFG = Config()：导入即加载 .env 并求值一次。
var CFG = func() *Config {
	loadDotenv()
	return Load()
}()
