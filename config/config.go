package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config 是整个服务的运行配置。
type Config struct {
	TFTPListen     string     `yaml:"tftp_listen"` // TFTP 监听地址，默认 :69
	HTTPListen     string     `yaml:"http_listen"` // HTTP 监听地址，默认 :8080
	HTTPPort       int        `yaml:"http_port"`   // HTTP 端口，用于拼接 boot/content URL
	DHCP           DHCPConfig `yaml:"dhcp"`
	DataDir        string     `yaml:"data_dir"`         // 数据根目录
	DBPath         string     `yaml:"db_path"`          // SQLite 文件路径（空则用 DataDir/server.db）
	NextServer     string     `yaml:"next_server"`      // 通告给客户端的服务器 IP（DHCP next-server / boot URL 主机）
	AssetsDir      string     `yaml:"assets_dir"`       // iPXE 二进制与 Web 静态资源目录
	PeSupportDir   string     `yaml:"pe_support_dir"`   // PE 支撑文件目录（wimboot/bootmgr.exe/bcd/boot.sdi），供 wimboot 启动 WIM
	DenyUnknownMAC bool       `yaml:"deny_unknown_mac"` // 未绑定 MAC 是否拒绝启动（默认 false，给公共模板）

	// Menu 两级 iPXE 启动菜单配置（覆盖单模板 /boot）。
	Menu MenuConfig `yaml:"menu"`
}

type DHCPConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Listen        string `yaml:"listen"`      // 监听 :67
	RangeStart    string `yaml:"range_start"` // 分配池起始 IP
	RangeEnd      string `yaml:"range_end"`   // 分配池结束 IP
	Netmask       string `yaml:"netmask"`
	Router        string `yaml:"router"`
	BootFile      string `yaml:"boot_file"`       // 兜底 NBP（无架构匹配时），默认 undionly.kpxe
	BootFileBIOS  string `yaml:"boot_file_bios"`  // BIOS 架构 NBP，默认 undionly.kpxe
	BootFileEFI   string `yaml:"boot_file_efi"`   // UEFI x86/x64 NBP，默认 ipxe.efi
	BootFileARM64 string `yaml:"boot_file_arm64"` // ARM64 NBP，默认 ipxe-arm64.efi
	LeaseTime     int    `yaml:"lease_time"`      // 秒
}

// MenuConfig 两级 iPXE 启动菜单配置。
type MenuConfig struct {
	// Enabled 为 true 时 /menu 提供两级菜单；为 false 时 /menu 退化为单模板 /boot（向后兼容）。
	Enabled bool `yaml:"enabled"`
	// CountdownSeconds MAC 匹配时自动进入匹配模板的倒计时秒数（0=仅预选等待，不自动启动）。
	CountdownSeconds int `yaml:"countdown_seconds"`
	// Categories 一级分类（按系统）：linux / windows / windows pe / 工具类。
	Categories []MenuCategory `yaml:"categories"`
}

// MenuCategory 一级菜单分类。
type MenuCategory struct {
	ID    string     `yaml:"id"`   // 分类标识，如 linux / windows / winpe / utils
	Name  string     `yaml:"name"` // 展示名，如 Linux / Windows / Windows PE / 工具类
	Items []MenuItem `yaml:"items"`
}

// MenuItem 二级菜单项，引用一个已存在的 Template 名称。
type MenuItem struct {
	ID       string `yaml:"id"`       // 项标识，作为 iPXE 跳转标签后缀
	Name     string `yaml:"name"`     // 展示名，如 Ubuntu 26.04 / WinPE 1 (ISO)
	Template string `yaml:"template"` // 引用的 Template 名称（必须存在于 DB）
}

// Default 返回一份可用默认值。
func Default() *Config {
	return &Config{
		TFTPListen: ":69",
		HTTPListen: ":8080",
		HTTPPort:   8080,
		DataDir:    "data",
		NextServer: "0.0.0.0",
		AssetsDir:  "assets",
		DHCP: DHCPConfig{
			Enabled:       false,
			Listen:        ":67",
			BootFile:      "undionly.kpxe",
			BootFileBIOS:  "undionly.kpxe",
			BootFileEFI:   "ipxe.efi",
			BootFileARM64: "ipxe-arm64.efi",
			Netmask:       "255.255.255.0",
			LeaseTime:     3600,
		},
		Menu: MenuConfig{
			Enabled:          false,
			CountdownSeconds: 10,
		},
	}
}

// Load 从 YAML 文件读取并合并默认值。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// 配置文件不存在则用默认并写回，方便后续修改
				_ = cfg.Save(path)
				return cfg, nil
			}
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.AssetsDir == "" {
		cfg.AssetsDir = "assets"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "server.db")
	}
	for _, d := range []string{cfg.DataDir, cfg.IsosDir(), cfg.CacheDir(), cfg.OverlayDir(), cfg.PeDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	_ = cfg.Save(path) // 把合并后的配置落盘，便于查看
	return cfg, nil
}

// Save 写回 YAML（best-effort）。
func (c *Config) Save(path string) error {
	if path == "" {
		return nil
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func (c *Config) IsosDir() string    { return filepath.Join(c.DataDir, "isos") }
func (c *Config) CacheDir() string   { return filepath.Join(c.DataDir, "cache") }
func (c *Config) OverlayDir() string { return filepath.Join(c.DataDir, "overlay") }
func (c *Config) IpxeDir() string    { return filepath.Join(c.AssetsDir, "ipxe") }
func (c *Config) WebDir() string     { return filepath.Join(c.AssetsDir, "web") }
func (c *Config) PeDir() string {
	if c.PeSupportDir != "" {
		return c.PeSupportDir
	}
	return filepath.Join(c.DataDir, "pe")
}
