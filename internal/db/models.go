package db

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// User 一个用户，可绑定多台主机。
type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:128;not null;uniqueIndex" json:"name"`
	Token     string    `gorm:"size:64;not null;uniqueIndex" json:"token"`
	CreatedAt time.Time `json:"created_at"`
	Hosts     []Host    `gorm:"foreignKey:UserID" json:"-"`
}

// Host 一台主机，以 MAC 作为唯一标识，归属某个 User。
// 一个 User 可拥有多台 Host（需求 3、4）。
type Host struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	MAC        string    `gorm:"size:32;not null;uniqueIndex" json:"mac"` // 规范格式 aa:bb:cc:dd:ee:ff
	Name       string    `gorm:"size:128" json:"name"`
	UserID     uint      `gorm:"index" json:"user_id"`
	Vars       string    `gorm:"type:text" json:"vars"`             // JSON map，用于模板变量（hostname/ip 等）
	BootToMenu bool      `gorm:"default:false" json:"boot_to_menu"` // 按主机开关：true 时 /boot 重定向到 /menu（逐机灰度）
	CreatedAt  time.Time `json:"created_at"`
}

// VarsMap 解析 Host.Vars 为 map。
func (h Host) VarsMap() map[string]string {
	if h.Vars == "" {
		return map[string]string{}
	}
	m := map[string]string{}
	_ = json.Unmarshal([]byte(h.Vars), &m)
	return m
}

// TemplateScope 模板作用域：公共/用户级/主机级。
type TemplateScope string

const (
	ScopePublic TemplateScope = "public"
	ScopeUser   TemplateScope = "user"
	ScopeHost   TemplateScope = "host"
)

// TemplateKind 启动类型：Linux(解包+overlay 链克) / Windows(wimboot) / Raw(原生 ISO)。
type TemplateKind string

const (
	KindLinux   TemplateKind = "linux"
	KindWindows TemplateKind = "windows"
	KindRaw     TemplateKind = "raw"
	KindWIM     TemplateKind = "wim"   // 经由 wimboot 启动 WIM（PE / 网络安装 Windows）
	KindISCSI   TemplateKind = "iscsi" // OS 仍从 ISO 取，sanhook 外部 iSCSI LUN 作持久块设备
	KindNFS     TemplateKind = "nfs"   // ro NFS 基础 root + 每客户端 overlay（LTSP 式）
)

// ISOFormat 镜像格式：iso(可解包) / img(裸盘) / vhd(虚拟磁盘) / wim(Windows 映像)。
type ISOFormat string

const (
	ISOFormatISO ISOFormat = "iso"
	ISOFormatIMG ISOFormat = "img"
	ISOFormatVHD ISOFormat = "vhd"
	ISOFormatWIM ISOFormat = "wim"
)

// Template 启动模板（iPXE 脚本）。按 host > user > public 优先级选取。
type Template struct {
	ID           uint          `gorm:"primaryKey" json:"id"`
	Name         string        `gorm:"size:128;not null" json:"name"`
	Scope        TemplateScope `gorm:"size:16;not null;index" json:"scope"`
	OwnerID      *uint         `gorm:"index" json:"owner_id"` // scope=user/host 时指向 User/Host ID
	Kind         TemplateKind  `gorm:"size:16;not null" json:"kind"`
	ISOID        *uint         `gorm:"index" json:"iso_id"`           // 绑定的 ISO（提供 kernel/initrd 或内容）
	Body         string        `gorm:"type:text" json:"body"`         // iPXE 脚本模板（Go text/template）
	ISCSITarget  string        `gorm:"size:255" json:"iscsi_target"`  // 服务端 Target IQN 所在的 iSCSI root-path，如 iscsi:192.168.1.10::::iqn.2024-01.com.example:nas:disk01（IQN 后不要再拼 :lunN）
	InitiatorIQN string        `gorm:"size:255" json:"initiator_iqn"` // 客户端(iPXE)名字，须与 targetcli acls/ 条目一致；模板用 {{.InitiatorIQN}} 渲染 set initiator-iqn
	NFSRoot      string        `gorm:"size:255" json:"nfs_root"`      // ro 基础 root 导出，如 192.168.1.10:/srv/nfs/live
	NFSUpper     string        `gorm:"size:255" json:"nfs_upper"`     // 每客户端 overlay 基础导出，如 192.168.1.10:/srv/nfs/overlay
	Priority     int           `json:"priority"`
	CreatedAt    time.Time     `json:"created_at"`
}

// ISO 导入的镜像及其解包缓存目录。
type ISO struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	Name          string    `gorm:"size:255;not null" json:"name"`
	Filename      string    `gorm:"size:255;not null;index" json:"filename"` // 记录身份；刻意不加 UNIQUE，存量库可能有重复行
	ExtractedPath string    `gorm:"size:512" json:"extracted_path"`          // cache/<isoname>，仅 ISO 解包后非空
	Format        string    `gorm:"size:16" json:"format"`                   // iso/img/vhd/wim
	Size          int64     `json:"size"`
	Checksum      string    `gorm:"size:64" json:"checksum"`
	CreatedAt     time.Time `json:"created_at"`
}

// InjectionScope 注入作用域：全局/用户级/主机级。
type InjectionScope string

const (
	InjGlobal InjectionScope = "global"
	InjUser   InjectionScope = "user"
	InjHost   InjectionScope = "host"
)

// Injection 注入到镜像中的文件。按 host > user > global > base(ISO) 优先级合并。
type Injection struct {
	ID         uint           `gorm:"primaryKey" json:"id"`
	ISOID      uint           `gorm:"index" json:"iso_id"`
	Scope      InjectionScope `gorm:"size:16;index" json:"scope"`
	OwnerID    *uint          `gorm:"index" json:"owner_id"`
	ImagePath  string         `gorm:"size:512;not null" json:"image_path"`  // 镜像内路径，如 /autounattend.xml
	StoredPath string         `gorm:"size:512;not null" json:"stored_path"` // overlay 目录下的相对路径
	CreatedAt  time.Time      `json:"created_at"`
}

// AutoMigrate 建表。
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(&User{}, &Host{}, &Template{}, &ISO{}, &Injection{})
}
