package main

import (
	"flag"
	"log"

	"ipxe-server/config"
	"ipxe-server/internal/db"
	"ipxe-server/internal/dhcp"
	"ipxe-server/internal/httpd"
	"ipxe-server/internal/tftp"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	gdb, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("db: %v", err)
	}

	// TFTP：下发 iPXE 二进制（监听 :69，需要管理员/root 权限）
	go func() {
		if err := tftp.Serve(cfg.TFTPListen, cfg.IpxeDir()); err != nil {
			log.Printf("[tftp] start failed (need admin?): %v", err)
		}
	}()

	// DHCP：可选内置（监听 :67，需要管理员/root 权限）
	if cfg.DHCP.Enabled {
		go func() {
			c := dhcp.Config{
				Listen:     cfg.DHCP.Listen,
				RangeStart: cfg.DHCP.RangeStart,
				RangeEnd:   cfg.DHCP.RangeEnd,
				Netmask:    cfg.DHCP.Netmask,
				Router:     cfg.DHCP.Router,
				NextServer:    cfg.NextServer,
				BootFile:      cfg.DHCP.BootFile,
				BootFileBIOS:  cfg.DHCP.BootFileBIOS,
				BootFileEFI:   cfg.DHCP.BootFileEFI,
				BootFileARM64: cfg.DHCP.BootFileARM64,
				LeaseTime:     cfg.DHCP.LeaseTime,
			}
			if err := dhcp.Serve(c); err != nil {
				log.Printf("[dhcp] start failed (need admin?): %v", err)
			}
		}()
	}

	srv := httpd.New(cfg, gdb)
	log.Printf("[main] HTTP UI:        http://%s/", cfg.HTTPListen)
	log.Printf("[main] boot endpoint:  http://%s/boot?mac=<aa:bb:cc:dd:ee:ff>", cfg.HTTPListen)
	if err := srv.Start(); err != nil {
		log.Fatalf("http: %v", err)
	}
}
