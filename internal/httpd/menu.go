package httpd

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"ipxe-server/internal/db"
)

// handleMenu 提供「系统 -> 版本」两级 iPXE 启动菜单。
//   - menu.enabled=false 或没有分类时，退化为单模板 /boot，保证向后兼容、零破坏。
//   - 专属模板匹配（host/user scope）：菜单首项为“匹配主机模板”，倒计时默认指向它（菜单先显示，可中断）；
//     其余项为配置的默认分类菜单。
//   - 无专属匹配（含未知 MAC 或仅 public 回退）：仅展示分类菜单，不预置、不默认、不强制 public-linux。
//   - 启动失败回转人工菜单，避免循环。
func (s *Server) handleMenu(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Menu.Enabled || len(s.cfg.Menu.Categories) == 0 {
		s.handleBoot(w, r)
		return
	}

	mac := normMac(r.URL.Query().Get("mac"))

	var host *db.Host
	if mac != "" {
		var h db.Host
		if s.db.Where("mac = ?", mac).First(&h).Error == nil {
			host = &h
		}
	}

	// 匹配模板：host>user>public；用于自动启动与预选高亮。
	var matchedTmplName, matchedLabel string
	var specificMatch bool
	if host != nil {
		if tmpl, ok := s.pickTemplate(host, "", ""); ok {
			matchedTmplName = tmpl.Name
			specificMatch = tmpl.Scope != db.ScopePublic
			for _, cat := range s.cfg.Menu.Categories {
				for _, it := range cat.Items {
					if it.Template == matchedTmplName {
						matchedLabel = it.Name
						break
					}
				}
				if matchedLabel != "" {
					break
				}
			}
			if matchedLabel == "" {
				matchedLabel = matchedTmplName
			}
		}
	}

	countdownMs := s.cfg.Menu.CountdownSeconds * 1000
	if countdownMs < 0 {
		countdownMs = 0
	}
	hasTimeout := countdownMs > 0

	var b strings.Builder
	b.WriteString("#!ipxe\n")

	// 系统信息：一次性组装为变量，所有菜单用 item --gap 引用。
	// echo 会被 choose 的全屏菜单绘制覆盖，只有把信息放进菜单项（--gap）才会稳定显示在界面内。
	// MAC 用服务端解析到的引导 MAC（权威）；IP/GW 用 iPXE 官方 ${net0/*} 变量，
	// 不依赖未定义的 active_net，也不用兼容性差的嵌套展开 ${${...}/...}。
	b.WriteString(fmt.Sprintf("set sys_info IP: ${net0/ip} | MAC: %s | GW: ${net0/gateway}\n\n", mac))

	// 顶部信息：每次进入菜单都打印（含 MAC / IP / 匹配状态），返回菜单时信息不丢失
	writeHeader := func() {
		b.WriteString("echo ============================================\n")
		b.WriteString("echo  iPXE Boot Menu\n")
		if specificMatch {
			if hasTimeout {
				b.WriteString(fmt.Sprintf("echo  [matched] host template = %s (countdown will auto-boot; you may pick another)\n", matchedLabel))
			} else {
				b.WriteString(fmt.Sprintf("echo  [matched] host template = %s (no countdown; pick it manually)\n", matchedLabel))
			}
		} else {
			b.WriteString("echo  [no match] no host-specific template; menu only, will NOT auto-boot public-linux\n")
		}
		b.WriteString("echo ============================================\n")
		b.WriteString(fmt.Sprintf("echo  Client MAC : %s\n", mac))
		b.WriteString("echo  Client IP  : ${net0/ip}\n\n")
	}

	// 菜单项：匹配主机时首项为“匹配主机模板”，其后为分类菜单
	writeItems := func() {
		if specificMatch {
			b.WriteString(fmt.Sprintf("item --key a boot_matched Matched host template: %s\n", matchedLabel))
		}
		for _, cat := range s.cfg.Menu.Categories {
			b.WriteString(fmt.Sprintf("item %s %s\n", cat.ID, cat.Name))
		}
	}

	// 每个菜单底部的公共项：分隔线 + 系统信息（--gap 不可选中，不会成为 choose 的 target）+ Reboot。
	// 文本以 '-' 开头必须用 '--' 结束选项解析，避免被 iPXE 当成命令选项（官方要求）。
	writeExtras := func() {
		b.WriteString("item --gap --  -----------------------------------------\n")
		b.WriteString("item --gap --  ${sys_info}\n")
		b.WriteString("item --key r reboot  [R] Reboot\n")
	}

	// 主菜单：匹配主机且有倒计时时预选 boot_matched 并倒计时自动启动（菜单先显示、可中断）；
	// 无匹配或无可倒计时时仅展示分类菜单，不预置、不默认 public-linux。
	b.WriteString(":menu\n")
	writeHeader()
	b.WriteString("menu \"iPXE Boot Menu\"\n")
	writeItems()
	writeExtras()
	if specificMatch {
		if hasTimeout {
			b.WriteString(fmt.Sprintf("choose --default boot_matched --timeout %d target || goto shell\n", countdownMs))
		} else {
			b.WriteString("choose --default boot_matched target || goto shell\n")
		}
	} else {
		b.WriteString("choose target || goto shell\n")
	}
	b.WriteString("goto ${target}\n\n")

	// 手动菜单（防循环）：启动失败回转时进入，无默认、无超时，但保留匹配项与 MAC/IP 信息
	b.WriteString(":menu_manual\n")
	writeHeader()
	b.WriteString("menu \"iPXE Boot Menu (manual)\"\n")
	writeItems()
	writeExtras()
	b.WriteString("choose target || goto shell\ngoto ${target}\n\n")

	// 启动失败回转：进入手动菜单（破除“自动倒计时→失败→再倒计时”死循环），仍保留匹配项与 MAC/IP
	b.WriteString(":boot_failed\necho Boot failed, returning to manual menu ...\ngoto menu_manual\n\n")

	// 匹配模板启动标签（label 与 item 的 boot_matched 对齐）
	if specificMatch {
		b.WriteString(":boot_matched\n")
		b.WriteString(s.renderNamedTemplate(mac, host, matchedTmplName))
		b.WriteString("goto boot_failed\n\n")
	}

	// 一级分类子菜单 + 二级项启动标签。
	// 关键：item 的 label 直接用完整启动标签名（boot_<cat>_<item>），使所有菜单统一用 goto ${target}，
	// 这样 [R] Reboot（label=reboot）能落到全局 :reboot，而不会拼成 boot_<cat>_reboot 悬空标签。
	for _, cat := range s.cfg.Menu.Categories {
		b.WriteString(fmt.Sprintf(":%s\nmenu %q\n", cat.ID, cat.Name))
		for _, it := range cat.Items {
			b.WriteString(fmt.Sprintf("item boot_%s_%s %s\n", cat.ID, it.ID, it.Name))
		}
		writeExtras()
		b.WriteString("choose target || goto menu\ngoto ${target}\n\n")
		for _, it := range cat.Items {
			b.WriteString(fmt.Sprintf(":boot_%s_%s\n", cat.ID, it.ID))
			b.WriteString(s.renderNamedTemplate(mac, host, it.Template))
			b.WriteString("goto boot_failed\n\n")
		}
	}

	// Reboot：所有菜单的 [R] Reboot 汇聚于此。reboot 失败（REBOOT_CMD 未编译）则回主菜单兜底。
	b.WriteString(":reboot\necho Rebooting ...\nreboot || goto menu\n\n")

	// 兜底 shell。
	b.WriteString(":shell\necho Dropped to iPXE shell. Type 'exit' to return to menu.\nshell\ngoto menu\n")

	writeScript(w, b.String())
}

// renderNamedTemplate 按名称解析模板并渲染为 iPXE 脚本片段。
// 与 handleBoot 共享 bootData；模板缺失时返回错误片段（goto boot_failed），
// 缺失 ISO 时仍渲染（ContentURL/RawURL 为空），避免单个项拖垮整个菜单。
func (s *Server) renderNamedTemplate(mac string, host *db.Host, tmplName string) string {
	tmpl, ok := s.pickTemplate(host, "", tmplName)
	if !ok {
		return fmt.Sprintf("echo ERROR: menu item template '%s' not found\ngoto boot_failed\n", tmplName)
	}
	hostName, userName := "unknown", "public"
	vars := map[string]string{}
	isoName, contentURL, rawURL := "", "", ""
	if tmpl.ISOID != nil {
		var isoRec db.ISO
		if s.db.First(&isoRec, *tmpl.ISOID).Error == nil {
			isoName = isoRec.Name
			contentURL = fmt.Sprintf("http://%s:%d/c/%s/%s", s.cfg.NextServer, s.cfg.HTTPPort, mac, isoRec.Name)
			rawURL = fmt.Sprintf("http://%s:%d/r/%s/%s", s.cfg.NextServer, s.cfg.HTTPPort, mac, isoRec.Name)
		}
	}
	if host != nil {
		hostName = host.Name
		var u db.User
		if s.db.First(&u, host.UserID).Error == nil {
			userName = u.Name
		}
		vars = host.VarsMap()
	}
	data := bootData{
		MAC:          mac,
		HostName:     hostName,
		UserName:     userName,
		ISOName:      isoName,
		ContentURL:   contentURL,
		RawURL:       rawURL,
		PeURL:        fmt.Sprintf("http://%s:%d/pe/", s.cfg.NextServer, s.cfg.HTTPPort),
		ISCSITarget:  tmpl.ISCSITarget,
		InitiatorIQN: tmpl.InitiatorIQN,
		NFSRoot:      tmpl.NFSRoot,
		NFSUpper:     tmpl.NFSUpper,
		Vars:         vars,
	}
	t, err := template.New("m").Parse(tmpl.Body)
	if err != nil {
		return fmt.Sprintf("echo ERROR: bad template '%s': %s\ngoto boot_failed\n", tmpl.Name, err.Error())
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return fmt.Sprintf("echo ERROR: render '%s': %s\ngoto boot_failed\n", tmpl.Name, err.Error())
	}
	return buf.String() + "\n"
}
