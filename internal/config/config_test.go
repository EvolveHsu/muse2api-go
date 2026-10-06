package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvBoolMatchesPythonStripNotZero(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"0", false},
		{" 0 ", false},
		{"1", true},
		{"abc", true},
		{"", true}, // Python: "".strip() != "0" -> True
	}
	for _, c := range cases {
		t.Setenv("MUSE2API_TOOL_PROTOCOL", c.raw)
		if got := envBool("MUSE2API_TOOL_PROTOCOL", "0"); got != c.want {
			t.Fatalf("raw=%q 期望 %v 得到 %v", c.raw, c.want, got)
		}
	}
}

func TestEnvReadsDefaults(t *testing.T) {
	t.Setenv("MUSE2API_HOME", "/app")
	for _, k := range []string{"MUSE2API_HOST", "MUSE2API_PORT", "MUSE2API_CDP_PORT",
		"MUSE2API_LOGIN_WAIT", "MUSE2API_IMAGE_TIMEOUT", "MUSE2API_VIDEO_TIMEOUT",
		"MUSE2API_CHAT_TIMEOUT", "MUSE2API_EXTRA_PATH", "MUSE2API_SITE",
		"MUSE2API_PUBLIC_BASE", "MUSE2API_KEY", "MUSE2API_PROFILE_ROOT",
		"MUSE2API_CORS_ORIGINS", "MUSE2API_TOOL_PROTOCOL",
		"MUSE2API_KEEPALIVE_DISABLED_ACCOUNTS", "MUSE2API_WARM_TAB",
		"MUSE2API_BROWSER_IDLE_MINUTES", "MUSE2API_CHROMIUM"} {
		os.Unsetenv(k)
	}
	c := Load()
	if c.Host != "0.0.0.0" || c.Port != 18610 || c.CdpPort != 19210 {
		t.Fatalf("默认端口/host 变了: %+v", c)
	}
	if c.LoginWait != 60 || c.ImageTimeout != 240 || c.VideoTimeout != 600 || c.ChatTimeout != 300 {
		t.Fatalf("默认超时变了: %+v", c)
	}
	if c.SiteURL != "https://muse.ai/" || c.ExtraPath != "/snap/bin" {
		t.Fatalf("默认站点/路径变了: %+v", c)
	}
	if c.CORSOrigins != "https://muse.ai,https://www.muse.ai" {
		t.Fatalf("默认 CORS 源变了: %q", c.CORSOrigins)
	}
	if c.ToolProtocol || c.KeepaliveDisabledAccounts {
		t.Fatal("两个协议/保活开关默认应为关闭")
	}
	// DEVIATION #1：Go 版默认不常驻预热、30 分钟空闲后释放浏览器。
	if c.WarmTab {
		t.Fatal("WarmTab 默认应为 false（Python 是无条件预热）")
	}
	if c.BrowserIdleMinutes != 30 {
		t.Fatalf("空闲阈值默认应为 30，得到 %d", c.BrowserIdleMinutes)
	}
}

func TestDerivedPaths(t *testing.T) {
	t.Setenv("MUSE2API_HOME", "/app")
	t.Setenv("MUSE2API_PROFILE_ROOT", filepath.Join(string(os.PathSeparator)+"root", "muse2api-profiles"))
	c := Load()
	cases := []struct{ name, got, want string }{
		{"DataDir", c.DataDir(), filepath.Join("/app", "data")},
		{"MediaDir", c.MediaDir(), filepath.Join("/app", "data", "media")},
		{"DownloadDir", c.DownloadDir(), filepath.Join("/app", "data", "downloads")},
		{"AccountsFile", c.AccountsFile(), filepath.Join("/app", "data", "accounts.json")},
		{"TasksFile", c.TasksFile(), filepath.Join("/app", "data", "tasks.json")},
		{"ProfileDir", c.ProfileDir(), filepath.Join(c.ProfileRoot, "generate")},
	}
	for _, x := range cases {
		if x.got != x.want {
			t.Fatalf("%s: got=%q want=%q", x.name, x.got, x.want)
		}
	}
}

func TestLoadDotenvDoesNotOverrideExisting(t *testing.T) {
	home := t.TempDir()
	env := filepath.Join(home, ".env")
	os.WriteFile(env, []byte(
		"# comment\n\nMUSE2API_KEY=from-file\nTEST_DOTENV_NEW=brand-new\nTEST_DOTENV_QUOTED=\"quoted\"\n"), 0o600)
	t.Setenv("MUSE2API_HOME", home)
	t.Setenv("MUSE2API_KEY", "from-environment")
	defer os.Unsetenv("TEST_DOTENV_NEW")
	defer os.Unsetenv("TEST_DOTENV_QUOTED")

	loadDotenv()

	if os.Getenv("MUSE2API_KEY") != "from-environment" {
		t.Fatal(".env 不应覆盖已存在的系统环境变量")
	}
	if os.Getenv("TEST_DOTENV_NEW") != "brand-new" {
		t.Fatalf(".env 新键未生效: %q", os.Getenv("TEST_DOTENV_NEW"))
	}
	if os.Getenv("TEST_DOTENV_QUOTED") != "quoted" {
		t.Fatalf("引号应被剥掉: %q", os.Getenv("TEST_DOTENV_QUOTED"))
	}
}

func TestDetectChromium(t *testing.T) {
	real := filepath.Join(t.TempDir(), "chromium")
	os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755)

	t.Setenv("MUSE2API_CHROMIUM", real)
	if got := DetectChromium(); got != real {
		t.Fatalf("已存在的路径应被采用: %q", got)
	}

	t.Setenv("MUSE2API_CHROMIUM", "/nonexistent/chromium-binary")
	if got := DetectChromium(); got == "/nonexistent/chromium-binary" {
		t.Fatal("不存在的路径应回落到探测/默认值")
	}

	// 没配置时：本机装了任一候选浏览器就用它，否则回落到裸 "chromium"。
	// （这台机器装了 Edge，候选会命中 Edge，所以不能只断言回落串。）
	t.Setenv("MUSE2API_CHROMIUM", "")
	if got := DetectChromium(); got != "chromium" && !isFile(got) {
		t.Fatalf("结果应是存在的浏览器或默认串，得到 %q", got)
	}
}
