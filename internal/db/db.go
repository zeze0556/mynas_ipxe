package db

import (
	"log"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open 打开 SQLite（纯 Go 驱动，无 CGO），并自动建表 + 播种公共模板。
func Open(path string) (*gorm.DB, error) {
	gdb, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}
	if err := AutoMigrate(gdb); err != nil {
		return nil, err
	}
	if err := SeedPublicTemplates(gdb); err != nil {
		return nil, err
	}
	return gdb, nil
}

// publicLinuxBody / publicWindowsBody / publicRawBody 是开箱即用的默认模板。
// 占位符由 boot 渲染器注入：.ContentURL .RawURL .PeURL .ISOName .HostName .MAC .UserName .Vars
// .ISCSITarget .InitiatorIQN .NFSRoot .NFSUpper
const (
	publicLinuxBody = `#!ipxe
echo Booting {{.ISOName}} for {{.HostName}} ({{.MAC}})
set base {{.ContentURL}}
kernel ${base}/casper/vmlinuz boot=casper ip=dhcp ---
initrd ${base}/casper/initrd
boot
`
	publicWindowsBody = `#!ipxe
echo Booting Windows {{.ISOName}} for {{.HostName}}
set base {{.ContentURL}}
kernel ${base}/wimboot
initrd ${base}/bootmgr.exe      bootmgr.exe
initrd ${base}/boot/bcd         bcd
initrd ${base}/boot/boot.sdi    boot.sdi
initrd ${base}/sources/boot.wim boot.wim
initrd ${base}/autounattend.xml autounattend.xml
boot
`
	publicRawBody = `#!ipxe
echo Booting raw ISO {{.ISOName}}
sanboot {{.RawURL}}
`
	// publicWimBody 经 wimboot 启动 WIM（WinPE / 网络安装 Windows）。
	// bootmgr.exe/bcd/boot.sdi/wimboot 来自 PE 支撑目录（/pe/，由 config.pe_support_dir 配置），
	// 用户上传的 WIM 文件作为 boot.wim 经 /r/ 原生流提供。
	publicWimBody = `#!ipxe
echo Booting WIM {{.ISOName}} for {{.HostName}}
kernel {{.PeURL}}wimboot
initrd {{.PeURL}}bootmgr.exe      bootmgr.exe
initrd {{.PeURL}}boot/bcd         bcd
initrd {{.PeURL}}boot/boot.sdi    boot.sdi
initrd {{.RawURL}}                boot.wim
boot
`
	// publicISCSIBody：OS（kernel/initrd）仍从绑定的 ISO 解包内容（/c/）取，
	// 启动前 sanhook 一个外部 iSCSI LUN 作为持久块设备；casper 以 persistence-label=PERSIST 找到它。
	// ISCSITarget 形如 iscsi:192.168.1.10::::iqn.2024-01.com.example:nas:disk01（URI 末段是服务端 Target IQN，
	// LUN 是独立字段，不要在 IQN 后拼 :lunN，更不要把 ACL 条目名当成 target 名）。
	// InitiatorIQN 是客户端 iPXE 名字，须与 targetcli acls/ 条目一致，body 中用 {{.InitiatorIQN}} 渲染。
	publicISCSIBody = `#!ipxe
echo Booting {{.ISOName}} with iSCSI persistence ({{.MAC}})
set base {{.ContentURL}}
kernel ${base}/casper/vmlinuz boot=casper ip=dhcp persistence persistence-label=PERSIST ---
initrd ${base}/casper/initrd
sanhook {{.ISCSITarget}}
boot
`
	// publicNFSBody：OS 的 kernel/initrd 从 ISO 解包内容（/c/）取，root 走 ro NFS 基础导出；
	// 客户端 initramfs（见 examples/nfs-bottom-overlay）按 MAC 挂每客户端 NFS overlay 作 upper，实现分层持久化。
	publicNFSBody = `#!ipxe
echo Booting NFS live root for {{.HostName}} ({{.MAC}})
set base {{.ContentURL}}
kernel ${base}/casper/vmlinuz root=/dev/nfs nfsroot={{.NFSRoot}},ro ip=dhcp ipxe_overlaybase={{.NFSUpper}} ---
initrd ${base}/casper/initrd
boot
`
)

// SeedPublicTemplates 若公共模板不存在则写入默认三个。
func SeedPublicTemplates(gdb *gorm.DB) error {
	seeds := []Template{
		{Name: "public-linux", Scope: ScopePublic, Kind: KindLinux, Body: publicLinuxBody, Priority: 0},
		{Name: "public-windows", Scope: ScopePublic, Kind: KindWindows, Body: publicWindowsBody, Priority: 0},
		{Name: "public-raw", Scope: ScopePublic, Kind: KindRaw, Body: publicRawBody, Priority: 0},
		{Name: "public-wim", Scope: ScopePublic, Kind: KindWIM, Body: publicWimBody, Priority: 0},
		{Name: "public-iscsi", Scope: ScopePublic, Kind: KindISCSI, Body: publicISCSIBody, Priority: 0},
		{Name: "public-nfs", Scope: ScopePublic, Kind: KindNFS, Body: publicNFSBody, Priority: 0},
	}
	for _, s := range seeds {
		var cnt int64
		if err := gdb.Model(&Template{}).Where("scope = ? AND name = ?", ScopePublic, s.Name).Count(&cnt).Error; err != nil {
			return err
		}
		if cnt == 0 {
			if err := gdb.Create(&s).Error; err != nil {
				return err
			}
			log.Printf("[db] seeded public template: %s", s.Name)
		}
	}
	return nil
}
