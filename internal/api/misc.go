package api

import (
	"net/http"
	"os"
	"strings"

	"uitest-server/internal/browser"
	"uitest-server/internal/model"
)

/* ---------------------------------------------------------------- 设置 */

type aiSettingsView struct {
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	HasKey     bool   `json:"has_key"`
	KeyHint    string `json:"key_hint"`
	TimeoutSec int    `json:"timeout_sec"`
}

type settingsView struct {
	AI      aiSettingsView        `json:"ai"`
	Browser model.BrowserSettings `json:"browser"`
	Assist  model.AssistSettings  `json:"assist"`
	Server  map[string]any        `json:"server"`
}

func (s *Server) settingsView() settingsView {
	st := s.DB.Settings()
	hint := ""
	if st.AI.APIKey != "" {
		if len(st.AI.APIKey) <= 8 {
			hint = "****"
		} else {
			hint = st.AI.APIKey[:4] + "****" + st.AI.APIKey[len(st.AI.APIKey)-4:]
		}
	}
	return settingsView{
		AI: aiSettingsView{
			BaseURL:    st.AI.BaseURL,
			Model:      st.AI.Model,
			HasKey:     st.AI.APIKey != "",
			KeyHint:    hint,
			TimeoutSec: st.AI.TimeoutSec,
		},
		Browser: st.Browser,
		Assist:  st.Assist,
		Server:  map[string]any{"port": s.Port, "version": s.Version},
	}
}

// getSettings GET /api/settings
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

type putSettingsReq struct {
	AI *struct {
		BaseURL    *string `json:"base_url"`
		APIKey     *string `json:"api_key"`
		Model      *string `json:"model"`
		TimeoutSec *int    `json:"timeout_sec"`
	} `json:"ai"`
	Browser *struct {
		Executable      *string   `json:"executable"`
		Headless        *bool     `json:"headless"`
		StepDelayMS     *int      `json:"step_delay_ms"`
		ActionTimeoutMS *int      `json:"action_timeout_ms"`
		NoSandbox       *bool     `json:"no_sandbox"`
		DebugURL        *string   `json:"debug_url"`
		KernelDir       *string   `json:"kernel_dir"`
		ExtraArgs       *[]string `json:"extra_args"`
		AfterRun        *string   `json:"after_run"`
	} `json:"browser"`
	Assist *struct {
		AutoDataKey   *bool `json:"auto_data_key"`
		StableIDHint  *bool `json:"stable_id_hint"`
		AutoRebindUID *bool `json:"auto_rebind_uid"`
	} `json:"assist"`
}

// putSettings PUT /api/settings
// 语义：只覆盖显式提交的字段；ai.api_key 传空字符串表示保持原值。
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req putSettingsReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	st := s.DB.Settings()
	if req.AI != nil {
		if req.AI.BaseURL != nil {
			st.AI.BaseURL = strings.TrimSpace(*req.AI.BaseURL)
		}
		if req.AI.Model != nil {
			st.AI.Model = strings.TrimSpace(*req.AI.Model)
		}
		if req.AI.APIKey != nil && strings.TrimSpace(*req.AI.APIKey) != "" {
			st.AI.APIKey = strings.TrimSpace(*req.AI.APIKey)
		}
		if req.AI.TimeoutSec != nil && *req.AI.TimeoutSec > 0 {
			st.AI.TimeoutSec = *req.AI.TimeoutSec
		}
	}
	if req.Browser != nil {
		if req.Browser.Executable != nil {
			st.Browser.Executable = strings.TrimSpace(*req.Browser.Executable)
		}
		if req.Browser.Headless != nil {
			st.Browser.Headless = *req.Browser.Headless
		}
		if req.Browser.StepDelayMS != nil && *req.Browser.StepDelayMS >= 0 {
			st.Browser.StepDelayMS = *req.Browser.StepDelayMS
		}
		if req.Browser.ActionTimeoutMS != nil && *req.Browser.ActionTimeoutMS > 0 {
			st.Browser.ActionTimeoutMS = *req.Browser.ActionTimeoutMS
		}
		if req.Browser.NoSandbox != nil {
			st.Browser.NoSandbox = *req.Browser.NoSandbox
		}
		if req.Browser.DebugURL != nil {
			st.Browser.DebugURL = strings.TrimRight(strings.TrimSpace(*req.Browser.DebugURL), "/")
		}
		if req.Browser.KernelDir != nil {
			st.Browser.KernelDir = strings.TrimSpace(*req.Browser.KernelDir)
		}
		if req.Browser.AfterRun != nil {
			v := strings.TrimSpace(*req.Browser.AfterRun)
			switch v {
			case "auto", "close_tab", "close_browser", "keep":
				st.Browser.AfterRun = v
			case "":
				// 传空表示恢复默认
				st.Browser.AfterRun = "auto"
			}
		}
		if req.Browser.ExtraArgs != nil {
			args := make([]string, 0, len(*req.Browser.ExtraArgs))
			for _, a := range *req.Browser.ExtraArgs {
				if a = strings.TrimSpace(a); a != "" {
					args = append(args, a)
				}
			}
			st.Browser.ExtraArgs = args
		}
		// 浏览器相关配置一改，之前探测到的启动参数就不再可信
		st.Browser.ResolvedFlags = nil
	}
	if req.Assist != nil {
		if req.Assist.AutoDataKey != nil {
			st.Assist.AutoDataKey = *req.Assist.AutoDataKey
		}
		if req.Assist.StableIDHint != nil {
			st.Assist.StableIDHint = *req.Assist.StableIDHint
		}
		if req.Assist.AutoRebindUID != nil {
			st.Assist.AutoRebindUID = *req.Assist.AutoRebindUID
		}
	}
	s.DB.SaveSettings(st)
	s.BR.ApplySettings(st.Browser)
	writeJSON(w, http.StatusOK, s.settingsView())
}

/* -------------------------------------------------------------- 浏览器 */

// browserStatus GET /api/browser/status
func (s *Server) browserStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.BR.Status())
}

// browserLaunch POST /api/browser/launch
func (s *Server) browserLaunch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	_ = readJSON(r, &req)
	if _, err := s.BR.Ensure(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := s.BR.Status()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "debug_url": st.DebugURL, "executable": st.Executable, "version": st.Version})
}

// browserClose POST /api/browser/close
func (s *Server) browserClose(w http.ResponseWriter, r *http.Request) {
	if err := s.BR.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// browserDiagnose GET /api/browser/diagnose?url=
// 「浏览器打得开但打不开网页」的一键诊断：真实开一个标签页取值，返回内核、参数、探测结果与日志尾部。
func (s *Server) browserDiagnose(w http.ResponseWriter, r *http.Request) {
	url := strings.TrimSpace(r.URL.Query().Get("url"))
	if url == "" {
		url = "about:blank"
	}
	st := s.BR.Status()
	probe := s.BR.ProbeRenderer(r.Context(), url)
	writeJSON(w, http.StatusOK, map[string]any{
		"running":      st.Running,
		"executable":   st.Executable,
		"version":      st.Version,
		"kernel_dir":   st.KernelDir,
		"launch_flags": st.LaunchFlags,
		"headless":     st.Headless,
		"debug_url":    st.DebugURL,
		"attached":     st.Attached,
		"probe":        probe,
		"log_file":     st.LogFile,
		"log_tail":     tailFile(st.LogFile, 40),
		"hint":         diagnoseHint(probe, st),
	})
}

func diagnoseHint(p browser.Probe, st browser.Status) string {
	if p.OK {
		return "页面渲染正常，可以直接用内置浏览器录制/回放。"
	}
	if !st.Running {
		return "浏览器还没启动，先点「启动浏览器」。"
	}
	return "浏览器进程起来了但渲染进程不可用（常见于受限环境或安全软件拦截）。" +
		"服务端已自动尝试多组启动参数；仍然失败时可以：① 在设置里填「自带内核目录」并放入 Chrome for Testing；" +
		"② 填 debug_url 连接手工启动的浏览器；③ 查看日志尾部定位原因。"
}

// tailFile 读文件最后 n 行（用于把日志尾部给到前端）。
func tailFile(path string, n int) string {
	if path == "" || n <= 0 {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
