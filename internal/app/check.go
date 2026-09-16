// mnemosync 连通性与能力自检 (§5.8 mnemo-bot check).
package app

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HarryHello/mnemo-bot/internal/config"
)

// Check 探测 mnemosync: 版本 / 鉴权 / events 端点; 返回退出码.
func Check(cfgPath string, log *slog.Logger) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Error("配置加载失败", "err", err)
		return 2
	}
	base := strings.TrimRight(cfg.Mnemosync.BaseURL, "/")
	hc := &http.Client{Timeout: 10 * time.Second}
	pass := true

	// 1. mnemosync 版本
	version, verOK := probeVersion(hc, base)
	if cfg.Mnemosync.MinVersion == "" {
		log.Warn("未配置 min_version, 跳过版本比对", "mnemosync_version", version)
	} else if verOK && versionAtLeast(version, cfg.Mnemosync.MinVersion) {
		log.Info("✅ mnemosync 版本满足最低要求", "current", version, "required", cfg.Mnemosync.MinVersion)
	} else {
		pass = false
		log.Error("❌ mnemosync 版本不满足最低要求 (升级 mnemosync)", "current", version, "required", cfg.Mnemosync.MinVersion)
	}

	// 2. 鉴权 (/v1/models)
	status, body := probeGet(hc, base+"/v1/models", cfg.Mnemosync.APIKey)
	switch {
	case status == http.StatusOK:
		log.Info("✅ API Key 鉴权有效 (/v1/models)")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		pass = false
		log.Error("❌ API Key 无效或被禁用", "status", status)
	case status == 0:
		pass = false
		log.Error("❌ 无法连接 mnemosync", "base_url", base)
	default:
		pass = false
		log.Error("❌ /v1/models 探测异常", "status", status, "body", truncateStr(body, 200))
	}

	// 3. events 端点存在性
	probeBody := map[string]any{
		"version": envelopeVersion, "protocol": "probe", "platform": "probe",
		"space": map[string]any{"type": "group", "id": "probe"}, "events": []any{},
	}
	status, body = probePost(hc, base+"/v1/conversation/events", cfg.Mnemosync.APIKey, probeBody)
	switch {
	case status == http.StatusOK:
		log.Info("✅ 批量事件端点存在 (/v1/conversation/events)")
	case status == http.StatusNotFound:
		pass = false
		log.Error("❌ 批量事件端点不存在 — 需要部署含 envelope v1 的 mnemosync (设计文档 §6.2)")
	default:
		pass = false
		log.Error("❌ 批量事件端点探测异常", "status", status, "body", truncateStr(body, 200))
	}

	// 4. 插件缺失的被动症状指引 (§10.6 主动诊断留位)
	log.Info("ℹ️ 身份插件 (mnemobot) 缺失或版本不符时的症状: 身份进入非归属模式, 短期记忆/关系/记忆分析被跳过; 可在 mnemosync 面板检查插件安装与版本")

	if pass {
		log.Info("自检通过")
		return 0
	}
	log.Error("自检未通过 (见上方 ❌ 项)")
	return 1
}

const envelopeVersion = 1

func probeVersion(hc *http.Client, base string) (string, bool) {
	status, body := probeGet(hc, base+"/health", "")
	if status != http.StatusOK {
		return "未知", false
	}
	var d struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &d); err != nil || d.Version == "" {
		return "未知", false
	}
	return d.Version, true
}

func probeGet(hc *http.Client, url, apiKey string) (int, []byte) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, body
}

func probePost(hc *http.Client, url, apiKey string, payload any) (int, []byte) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, respBody
}

// versionAtLeast 简易版本比较: 忽略 v 前缀与额外后缀, 三段数字比较.
func versionAtLeast(current, required string) bool {
	cur := semver3(current)
	min := semver3(required)
	for i := 0; i < 3; i++ {
		if cur[i] != min[i] {
			return cur[i] > min[i]
		}
	}
	return true
}

func semver3(s string) [3]int {
	s = strings.TrimPrefix(s, "v")
	var out [3]int
	for i, part := range strings.SplitN(s, ".", 3) {
		digits := ""
		for _, ch := range part {
			if ch >= '0' && ch <= '9' {
				digits += string(ch)
			} else {
				break
			}
		}
		out[i], _ = strconv.Atoi(digits)
	}
	return out
}

func truncateStr(s []byte, max int) string {
	if len(s) <= max {
		return string(s)
	}
	return string(s[:max]) + "…"
}
