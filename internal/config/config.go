// Package config 负责读取 uitest/config.json 并用环境变量覆盖。
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"

	"uitest-server/internal/model"
)

// Config 服务端配置。
type Config struct {
	Server struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"server"`
	Storage struct {
		Dir      string `json:"dir"`
		KeepRuns int    `json:"keep_runs"`
	} `json:"storage"`
	Browser model.BrowserSettings `json:"browser"`
	AI      model.AISettings      `json:"ai"`
	Assist  model.AssistSettings  `json:"assist"`
}

// Default 返回默认配置。
func Default() Config {
	var c Config
	c.Server.Host = "127.0.0.1"
	c.Server.Port = 8490
	c.Storage.Dir = "data"
	c.Storage.KeepRuns = 200
	c.Browser = model.DefaultSettings().Browser
	c.AI = model.DefaultSettings().AI
	c.Assist = model.DefaultSettings().Assist
	return c
}

// Load 读取配置文件（不存在则用默认值），随后应用环境变量覆盖。
func Load(path string) Config {
	c := Default()
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &c)
		}
	}
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8490
	}
	if c.Storage.Dir == "" {
		c.Storage.Dir = "data"
	}
	if c.Storage.KeepRuns <= 0 {
		c.Storage.KeepRuns = 200
	}
	if c.Browser.StepDelayMS <= 0 {
		c.Browser.StepDelayMS = 260
	}
	if c.Browser.ActionTimeoutMS <= 0 {
		c.Browser.ActionTimeoutMS = 15000
	}
	if c.AI.TimeoutSec <= 0 {
		c.AI.TimeoutSec = 60
	}

	// 环境变量覆盖：UITEST_*
	if v := os.Getenv("UITEST_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := os.Getenv("UITEST_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Server.Port = n
		}
	}
	if v := os.Getenv("UITEST_DATA_DIR"); v != "" {
		c.Storage.Dir = v
	}
	if v := os.Getenv("UITEST_BROWSER"); v != "" {
		c.Browser.Executable = v
	}
	if v := os.Getenv("UITEST_HEADLESS"); v == "1" || v == "true" {
		c.Browser.Headless = true
	}
	if v := os.Getenv("UITEST_AI_BASE_URL"); v != "" {
		c.AI.BaseURL = v
	}
	if v := os.Getenv("UITEST_AI_KEY"); v != "" {
		c.AI.APIKey = v
	}
	if v := os.Getenv("UITEST_AI_MODEL"); v != "" {
		c.AI.Model = v
	}
	return c
}

// AbsDataDir 把数据目录解析成绝对路径（相对于可执行文件所在目录）。
func AbsDataDir(dir string) string {
	if filepath.IsAbs(dir) {
		return dir
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), dir)
		if _, statErr := os.Stat(filepath.Dir(cand)); statErr == nil {
			return cand
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, dir)
	}
	return dir
}
