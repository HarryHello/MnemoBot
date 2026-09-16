// Package config 加载 mnemo-bot 的 TOML 配置.
//
// 约定: config.example.toml 是提交模板, 本地实际配置为 config.local.toml (gitignored).
package config

import (
	"errors"
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// Config 顶层配置.
type Config struct {
	Mnemosync Mnemosync `toml:"mnemosync"`
	Onebot    Onebot    `toml:"onebot"`
	Persona   Persona   `toml:"persona"`
	Triage    Triage    `toml:"triage"`
	Deliver   Deliver   `toml:"deliver"`
}

// Mnemosync 上游服务商连接.
type Mnemosync struct {
	BaseURL    string `toml:"base_url"`
	APIKey     string `toml:"api_key"`
	MinVersion string `toml:"min_version"`
	Model      string `toml:"model"`
	TimeoutSec int    `toml:"timeout_seconds"`
	// Mock 仅由 serve --mock 置位 (不入 TOML): 启动内置 mock 上游,
	// 不连接真实 mnemosync, 用于 bot 接口端到端自测.
	Mock bool `toml:"-"`
}

// Onebot OneBot v11 反向 WS 接入配置.
type Onebot struct {
	Mode        string `toml:"mode"`
	Listen      string `toml:"listen"`
	AccessToken string `toml:"access_token"`
	SelfID      string `toml:"self_id"`
	Platform    string `toml:"platform"`
	Base64Media bool   `toml:"base64_media_fallback"`
}

// Persona 触发判定所需的人格侧信息 (一期本地配置, 二期改从服务端读取).
type Persona struct {
	Nicknames []string `toml:"nicknames"`
}

// Triage 触发判定参数.
type Triage struct {
	// 指针类型用于区分"未配置"与"显式 0": 未配置走默认值, 显式 0 是合法语义 (如禁用).
	PrivateAlways           *bool  `toml:"private_always"`
	GroupCooldownSeconds    int    `toml:"group_cooldown_seconds"`
	ProactiveSilenceMinutes *int   `toml:"proactive_silence_minutes"`
	ProactivePrompt         string `toml:"proactive_prompt"`
}

// PrivateAlwaysEnabled 私聊是否总是回复 (默认 true).
func (t Triage) PrivateAlwaysEnabled() bool {
	return t.PrivateAlways == nil || *t.PrivateAlways
}

// SilenceMinutes 主动消息静默阈值 (分钟); 0 = 禁用.
func (t Triage) SilenceMinutes() int {
	if t.ProactiveSilenceMinutes == nil {
		return 120
	}
	return *t.ProactiveSilenceMinutes
}

// Deliver 投递参数 (QQ 侧限长与节奏).
type Deliver struct {
	MaxTextChars int `toml:"max_text_chars"`
	SendDelayMs  int `toml:"send_delay_ms"`
}

// Load 读取并解析配置, 应用默认值后校验必填项.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ResolvePath 在未显式指定 --config 时, 依次查找 config.local.toml / config.toml.
func ResolvePath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	for _, name := range []string{"config.local.toml", "config.toml"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	return "config.local.toml"
}

func (c *Config) applyDefaults() {
	if c.Mnemosync.TimeoutSec <= 0 {
		c.Mnemosync.TimeoutSec = 120
	}
	if c.Mnemosync.Model == "" {
		c.Mnemosync.Model = "mnemosync"
	}
	if c.Onebot.Mode == "" {
		c.Onebot.Mode = "reverse"
	}
	if c.Onebot.Listen == "" {
		c.Onebot.Listen = ":16530"
	}
	if c.Onebot.Platform == "" {
		c.Onebot.Platform = "qq"
	}
	if c.Triage.GroupCooldownSeconds <= 0 {
		c.Triage.GroupCooldownSeconds = 30
	}
	if c.Triage.ProactivePrompt == "" {
		c.Triage.ProactivePrompt = "（内部触发，非用户消息）已经安静了一段时间，请以 persona 的口吻自然地主动开启一个话题。"
	}
	if c.Deliver.MaxTextChars <= 0 {
		c.Deliver.MaxTextChars = 2000
	}
	if c.Deliver.SendDelayMs <= 0 {
		c.Deliver.SendDelayMs = 200
	}
}

func (c *Config) validate() error {
	// mock 自测模式不连真实 mnemosync, 上游连接参数豁免校验
	if !c.Mnemosync.Mock {
		if c.Mnemosync.BaseURL == "" {
			return errors.New("[mnemosync] base_url 未配置")
		}
		if c.Mnemosync.APIKey == "" {
			return errors.New("[mnemosync] api_key 未配置")
		}
	}
	if c.Onebot.Mode != "reverse" {
		return fmt.Errorf("[onebot] mode=%q: 一期仅支持 reverse", c.Onebot.Mode)
	}
	if c.Onebot.SelfID == "" {
		return errors.New("[onebot] self_id 未配置 (自回声过滤依赖)")
	}
	return nil
}
