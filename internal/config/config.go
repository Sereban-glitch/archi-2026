// Package config загружает, валидирует и нормализует конфигурацию Archi (config.yaml).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration — обёртка над time.Duration с YAML-парсингом ("30s", "5m", "1h30m" или голые секунды).
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	s := strings.TrimSpace(value.Value)
	if s == "" {
		d.Duration = 0
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		d.Duration = time.Duration(n) * time.Second
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("неверная длительность %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) { return d.String(), nil }

// Config — корневая структура конфигурации.
type Config struct {
	NodeName string `yaml:"node_name"`
	Timezone string `yaml:"timezone"`
	DataDir  string `yaml:"data_dir"`
	LogLevel string `yaml:"log_level"`

	LLM     LLMConfig     `yaml:"llm"`
	Agent   AgentConfig   `yaml:"agent"`
	Bridge  BridgeConfig  `yaml:"bridge"`
	Alerts  AlertsConfig  `yaml:"alerts"`
	Tools   ToolsConfig   `yaml:"tools"`
	Telegram TelegramConfig `yaml:"telegram"`
	IPC     IPCConfig     `yaml:"ipc"`
	Memory  MemoryConfig  `yaml:"memory"`
}

type LLMConfig struct {
	BaseURL    string   `yaml:"base_url"`     // антгравити-прокси, напр. http://127.0.0.1:18080
	APIKey     string   `yaml:"api_key"`     // напр. local-proxy
	Model      string   `yaml:"model"`
	MaxTokens  int      `yaml:"max_tokens"`
	Timeout    Duration `yaml:"timeout"`
	MaxRetries int      `yaml:"max_retries"`
}

type AgentConfig struct {
	MaxSteps        int      `yaml:"max_steps"`
	HistoryLimit    int      `yaml:"history_limit"`
	CompactAfter    int      `yaml:"compact_after_chars"`
	IdleSessionTTL  Duration `yaml:"idle_session_ttl"`
	BackoffBase     Duration `yaml:"backoff_base"`
	BackoffMax      Duration `yaml:"backoff_max"`
	MaxToolParallel int      `yaml:"max_tool_parallel"`
}

type BridgeConfig struct {
	Interval        Duration    `yaml:"interval"`
	RemoteTimeout   Duration    `yaml:"remote_timeout"`
	CommandTimeout  Duration    `yaml:"command_timeout"`
	MaxOutputBytes  int         `yaml:"max_output_bytes"`
	Remote          SSHConfig   `yaml:"remote"`
	Watchdog        WatchdogConfig `yaml:"watchdog"`
}

type SSHConfig struct {
	Host              string   `yaml:"host"`        // ~/.ssh/config Host, напр. agrobot-prod
	User              string   `yaml:"user"`
	Port              int      `yaml:"port"`
	KeyPath           string   `yaml:"key_path"`
	KnownHosts        string   `yaml:"known_hosts"`
	StrictHostKey     bool     `yaml:"strict_host_key"`
	PoolSize          int      `yaml:"pool_size"`
	ConnectTimeout    Duration `yaml:"connect_timeout"`
	KeepaliveInterval Duration `yaml:"keepalive_interval"`
	MaxRetries        int      `yaml:"max_retries"`
}

type WatchdogConfig struct {
	AutoRestart     []string `yaml:"auto_restart"` // только на удалённой ноде
	RestartCooldown Duration `yaml:"restart_cooldown"`
}

type AlertsConfig struct {
	RAMPercent   float64  `yaml:"ram_percent"`
	CPUPercent   float64  `yaml:"cpu_percent"`
	DiskPercent  float64  `yaml:"disk_percent"`
	Cooldown     Duration `yaml:"cooldown"`
	Services     []string `yaml:"services"` // мониторятся на обеих нодах
}

type ToolsConfig struct {
	MaxReadBytes         int      `yaml:"max_read_bytes"`
	AllowedWriteDirs     []string `yaml:"allowed_write_dirs"`
	AllowedRestart       []string `yaml:"allowed_restart_services"`
	BashDenyPatterns     []string `yaml:"bash_deny_patterns"`
	ForbiddenReadPatterns []string `yaml:"forbidden_read_patterns"`
}

type TelegramConfig struct {
	Enabled    bool         `yaml:"enabled"`
	APIBase    string       `yaml:"api_base"`
	Token      string       `yaml:"token"`
	Allowed    []int64      `yaml:"allowed"`
	Admins     []int64      `yaml:"admins"`
	PollTimeout Duration    `yaml:"poll_timeout"`
	Digest     DigestConfig `yaml:"digest"`
}

type DigestConfig struct {
	Enabled bool     `yaml:"enabled"`
	Times   []string `yaml:"times"` // ["09:00","21:00"]
	Chats   []int64  `yaml:"chats"` // по умолчанию — admins
}

type IPCConfig struct {
	Enabled    bool   `yaml:"enabled"`
	SocketPath string `yaml:"socket_path"`
}

type MemoryConfig struct {
	Dir         string  `yaml:"dir"`
	RecallLimit int     `yaml:"recall_limit"`
	MaxEntries  int     `yaml:"max_entries"`
}

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv подставляет ${VAR} и ${VAR:-default} в сыром YAML до парсинга.
func expandEnv(raw []byte) []byte {
	return envRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		groups := envRe.FindSubmatch(m)
		name, def := string(groups[1]), string(groups[2])
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return []byte(v)
		}
		return []byte(def)
	})
}

// Load читает YAML-файл, раскрывает переменные окружения, применяет дефолты и валидирует.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("читаю конфиг %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(expandEnv(raw), &c); err != nil {
		return nil, fmt.Errorf("парсю YAML %s: %w", path, err)
	}
	c.applyDefaults()
	if errs := c.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("конфиг невалиден:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	def := func(s *string, v string) {
		if strings.TrimSpace(*s) == "" {
			*s = v
		}
	}
	def(&c.NodeName, "laptop")
	def(&c.Timezone, "Europe/Kiev")
	def(&c.LogLevel, "info")
	def(&c.DataDir, "/var/lib/archi")

	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = "http://127.0.0.1:18080"
	}
	c.LLM.BaseURL = strings.TrimRight(c.LLM.BaseURL, "/")
	if c.LLM.APIKey == "" {
		c.LLM.APIKey = "local-proxy"
	}
	if c.LLM.Model == "" {
		c.LLM.Model = "claude-sonnet-4-5"
	}
	if c.LLM.MaxTokens == 0 {
		c.LLM.MaxTokens = 4096
	}
	if c.LLM.Timeout.Duration == 0 {
		c.LLM.Timeout.Duration = 120 * time.Second
	}
	if c.LLM.MaxRetries == 0 {
		c.LLM.MaxRetries = 4
	}

	if c.Agent.MaxSteps == 0 {
		c.Agent.MaxSteps = 24
	}
	if c.Agent.HistoryLimit == 0 {
		c.Agent.HistoryLimit = 40
	}
	if c.Agent.CompactAfter == 0 {
		c.Agent.CompactAfter = 150_000
	}
	if c.Agent.IdleSessionTTL.Duration == 0 {
		c.Agent.IdleSessionTTL.Duration = 12 * time.Hour
	}
	if c.Agent.BackoffBase.Duration == 0 {
		c.Agent.BackoffBase.Duration = 1 * time.Second
	}
	if c.Agent.BackoffMax.Duration == 0 {
		c.Agent.BackoffMax.Duration = 30 * time.Second
	}
	if c.Agent.MaxToolParallel == 0 {
		c.Agent.MaxToolParallel = 4
	}

	if c.Bridge.Interval.Duration == 0 {
		c.Bridge.Interval.Duration = 30 * time.Second
	}
	if c.Bridge.RemoteTimeout.Duration == 0 {
		c.Bridge.RemoteTimeout.Duration = 20 * time.Second
	}
	if c.Bridge.CommandTimeout.Duration == 0 {
		c.Bridge.CommandTimeout.Duration = 120 * time.Second
	}
	if c.Bridge.MaxOutputBytes == 0 {
		c.Bridge.MaxOutputBytes = 262_144
	}
	if c.Bridge.Remote.Port == 0 {
		c.Bridge.Remote.Port = 22
	}
	if c.Bridge.Remote.PoolSize == 0 {
		c.Bridge.Remote.PoolSize = 3
	}
	if c.Bridge.Remote.ConnectTimeout.Duration == 0 {
		c.Bridge.Remote.ConnectTimeout.Duration = 10 * time.Second
	}
	if c.Bridge.Remote.KeepaliveInterval.Duration == 0 {
		c.Bridge.Remote.KeepaliveInterval.Duration = 15 * time.Second
	}
	if c.Bridge.Remote.MaxRetries == 0 {
		c.Bridge.Remote.MaxRetries = 3
	}
	if c.Bridge.Watchdog.RestartCooldown.Duration == 0 {
		c.Bridge.Watchdog.RestartCooldown.Duration = 5 * time.Minute
	}

	if c.Alerts.RAMPercent == 0 {
		c.Alerts.RAMPercent = 85
	}
	if c.Alerts.CPUPercent == 0 {
		c.Alerts.CPUPercent = 80
	}
	if c.Alerts.DiskPercent == 0 {
		c.Alerts.DiskPercent = 90
	}
	if c.Alerts.Cooldown.Duration == 0 {
		c.Alerts.Cooldown.Duration = 10 * time.Minute
	}

	if c.Tools.MaxReadBytes == 0 {
		c.Tools.MaxReadBytes = 262_144
	}
	if len(c.Tools.AllowedWriteDirs) == 0 {
		c.Tools.AllowedWriteDirs = []string{"/home", "/tmp", "/var/lib/archi", "/srv"}
	}
	if len(c.Tools.BashDenyPatterns) == 0 {
		c.Tools.BashDenyPatterns = []string{
			`rm\s+-rf\s+/(?:\s|$)`, `mkfs`, `dd\s+.*of=/dev/`, `:\(\)\{.*\};:`,
			`shutdown|reboot|halt|poweroff`, `>\s*/dev/sd`, `chmod\s+-R\s+777\s+/`,
		}
	}
	if len(c.Tools.ForbiddenReadPatterns) == 0 {
		c.Tools.ForbiddenReadPatterns = []string{
			`/etc/shadow`, `/etc/gshadow`, `\.ssh/`, `id_rsa`, `id_ed25519`, `\.pem$`, `\.key$`, `credentials\.json$`,
		}
	}

	if c.Telegram.APIBase == "" {
		c.Telegram.APIBase = "https://api.telegram.org"
	}
	c.Telegram.APIBase = strings.TrimRight(c.Telegram.APIBase, "/")
	if c.Telegram.PollTimeout.Duration == 0 {
		c.Telegram.PollTimeout.Duration = 50 * time.Second
	}

	if c.IPC.SocketPath == "" {
		c.IPC.SocketPath = "/run/archi/archi.sock"
	}

	if c.Memory.Dir == "" {
		c.Memory.Dir = filepath.Join(c.DataDir, "memory")
	}
	if c.Memory.RecallLimit == 0 {
		c.Memory.RecallLimit = 8
	}
	if c.Memory.MaxEntries == 0 {
		c.Memory.MaxEntries = 5000
	}

	// Разворачиваем ~ и делаем пути абсолютными.
	c.Bridge.Remote.KeyPath = expandPath(c.Bridge.Remote.KeyPath)
	c.Bridge.Remote.KnownHosts = expandPath(c.Bridge.Remote.KnownHosts)
	c.DataDir = expandPath(c.DataDir)
	c.Memory.Dir = expandPath(c.Memory.Dir)
}

func expandPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Validate возвращает список ошибок конфигурации (пустой список = всё в порядке).
func (c *Config) Validate() []string {
	var errs []string
	if c.LLM.BaseURL == "" {
		errs = append(errs, "llm.base_url обязателен")
	}
	if !strings.HasPrefix(c.LLM.BaseURL, "http://") && !strings.HasPrefix(c.LLM.BaseURL, "https://") {
		errs = append(errs, "llm.base_url должен начинаться с http:// или https://")
	}
	if c.Bridge.Remote.Host == "" {
		errs = append(errs, "bridge.remote.host обязателен (SSH-хост, напр. agrobot-prod)")
	}
	if c.Bridge.Remote.User == "" {
		errs = append(errs, "bridge.remote.user обязателен")
	}
	if c.Telegram.Enabled && c.Telegram.Token == "" {
		errs = append(errs, "telegram.enabled=true, но telegram.token пуст")
	}
	if c.Telegram.Enabled && len(c.Telegram.Allowed) == 0 {
		errs = append(errs, "telegram.allowed пуст — укажи хотя бы один Telegram user ID")
	}
	if len(c.Telegram.Admins) == 0 {
		c.Telegram.Admins = c.Telegram.Allowed
	}
	if c.Bridge.Remote.StrictHostKey && c.Bridge.Remote.KnownHosts == "" {
		errs = append(errs, "bridge.remote.strict_host_key=true требует known_hosts путь")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Sprintf("timezone %q: %v", c.Timezone, err))
	}
	return errs
}
