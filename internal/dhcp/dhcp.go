package dhcp

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	dhcp "github.com/krolaw/dhcp4"
)

// Config 轻量 DHCP 服务器配置。
type Config struct {
	Listen     string
	RangeStart string
	RangeEnd   string
	Netmask    string
	Router     string
	NextServer    string
	BootFile      string // 兜底 NBP（无架构匹配时）
	BootFileBIOS  string // BIOS 架构 NBP
	BootFileEFI   string // UEFI x86/x64 NBP
	BootFileARM64 string // ARM64 NBP
	LeaseTime     int
}

// PXE Client System Architecture（DHCP Option 93, RFC 4578）。
const (
	archBIOS     uint16 = 0x0000 // Intel x86PC（传统 BIOS PXE）
	archEFI32    uint16 = 0x0006 // EFI IA32
	archEFI64    uint16 = 0x0007 // EFI x86-64
	archEFI64BC uint16 = 0x0008 // EFI x86-64 + BC
	archARM64   uint16 = 0x000A // EFI ARM 64-bit
	archARM64BC uint16 = 0x000B // EFI BC ARM 64-bit
)

// orDefault 返回 v，空则用 d。
func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// nbpForArch 按 Option 93 架构值返回应下发的 NBP 文件名。
func nbpForArch(arch uint16, c Config) string {
	switch arch {
	case archEFI32, archEFI64, archEFI64BC:
		return orDefault(c.BootFileEFI, "ipxe.efi")
	case archARM64, archARM64BC:
		return orDefault(c.BootFileARM64, "ipxe-arm64.efi")
	case archBIOS:
		return orDefault(c.BootFileBIOS, "undionly.kpxe")
	default:
		// 未知架构：回退到 BIOS NBP（再回退兜底 BootFile）
		return orDefault(c.BootFileBIOS, orDefault(c.BootFile, "undionly.kpxe"))
	}
}

// pickBootFile 从 DHCP 请求的 Option 93 解析客户端架构，选择对应 NBP；
// 没有 Option 93（老式 PXE / 非 PXE 请求）时回退到 BIOS NBP。
func (h *Handler) pickBootFile(req dhcp.Packet) string {
	if opts := req.ParseOptions(); opts != nil {
		if b := opts[dhcp.OptionClientArchitecture]; len(b) >= 2 {
			arch := binary.BigEndian.Uint16(b)
			log.Printf("[dhcp] client arch=0x%04x mac=%s -> nbp=%s",
				arch, req.CHAddr().String(), nbpForArch(arch, h.cfg))
			return nbpForArch(arch, h.cfg)
		}
	}
	return orDefault(h.cfg.BootFileBIOS, orDefault(h.cfg.BootFile, "undionly.kpxe"))
}

// Handler 实现 dhcp4.Handler，按 MAC 分配 IP 并下发 next-server + bootfile。
type Handler struct {
	mu       sync.Mutex
	cfg      Config
	leases   map[string]Lease
	cursor   int
	pool     []net.IP
	serverID net.IP
}

// Lease 一条租约。
type Lease struct {
	IP     net.IP
	Expiry time.Time
}

// NewHandler 构建分配池。
func NewHandler(c Config) (*Handler, error) {
	start := net.ParseIP(c.RangeStart).To4()
	end := net.ParseIP(c.RangeEnd).To4()
	if start == nil || end == nil {
		return nil, fmt.Errorf("bad dhcp range: %s-%s", c.RangeStart, c.RangeEnd)
	}
	ns := net.ParseIP(c.NextServer)
	if ns == nil {
		ns = start
	}
	return &Handler{
		cfg:      c,
		leases:   map[string]Lease{},
		pool:     buildPool(start, end),
		serverID: ns,
	}, nil
}

func buildPool(start, end net.IP) []net.IP {
	var out []net.IP
	s, e := ip2long(start), ip2long(end)
	for x := s; x <= e; x++ {
		out = append(out, long2ip(x))
	}
	return out
}

func ip2long(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func long2ip(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).To4()
}

// ServeDHCP 按消息类型回包。
func (h *Handler) ServeDHCP(req dhcp.Packet, msgType dhcp.MessageType, _ dhcp.Options) dhcp.Packet {
	mac := req.CHAddr().String()
	switch msgType {
	case dhcp.Discover, dhcp.Request:
		ip := h.allocate(mac)
		mt := dhcp.Offer
		if msgType == dhcp.Request {
			mt = dhcp.ACK
		}
		reply := dhcp.ReplyPacket(req, mt, h.serverID, ip,
			time.Duration(h.cfg.LeaseTime)*time.Second, nil)
		h.fill(req, reply, ip)
		return reply
	case dhcp.Release, dhcp.Decline:
		h.mu.Lock()
		delete(h.leases, mac)
		h.mu.Unlock()
		return nil
	}
	return nil
}

func (h *Handler) fill(req, reply dhcp.Packet, ip net.IP) {
	reply.SetSIAddr(h.serverID)
	if mask := net.ParseIP(h.cfg.Netmask).To4(); mask != nil {
		reply.AddOption(dhcp.OptionSubnetMask, mask)
	}
	if r := net.ParseIP(h.cfg.Router).To4(); r != nil {
		reply.AddOption(dhcp.OptionRouter, r)
	}
	// 按客户端架构（Option 93）下发对应 NBP；BIOS 与 UEFI/ARM 各取所需
	boot := h.pickBootFile(req)
	reply.AddOption(dhcp.OptionBootFileName, []byte(boot))
	reply.SetFile([]byte(boot))
}

// allocate 复用有效租约，否则从池中找空闲 IP。
func (h *Handler) allocate(mac string) net.IP {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.leases[mac]; ok && time.Now().Before(l.Expiry) {
		return l.IP
	}
	n := len(h.pool)
	for i := 0; i < n; i++ {
		idx := (h.cursor + i) % n
		cand := h.pool[idx]
		used := false
		for _, l := range h.leases {
			if l.IP.Equal(cand) && time.Now().Before(l.Expiry) {
				used = true
				break
			}
		}
		if !used {
			h.cursor = (idx + 1) % n
			h.leases[mac] = Lease{IP: cand, Expiry: time.Now().Add(time.Duration(h.cfg.LeaseTime) * time.Second)}
			return cand
		}
	}
	return h.pool[0]
}

// Serve 启动 DHCP 服务。
func Serve(c Config) error {
	h, err := NewHandler(c)
	if err != nil {
		return err
	}
	l, err := net.ListenPacket("udp4", c.Listen)
	if err != nil {
		return fmt.Errorf("dhcp listen %s: %w", c.Listen, err)
	}
	defer l.Close()
	log.Printf("[dhcp] listening %s pool %s-%s next-server=%s (nbp by arch: bios=%s efi=%s arm64=%s)",
		c.Listen, c.RangeStart, c.RangeEnd, c.NextServer,
		orDefault(c.BootFileBIOS, "undionly.kpxe"),
		orDefault(c.BootFileEFI, "ipxe.efi"),
		orDefault(c.BootFileARM64, "ipxe-arm64.efi"))
	return dhcp.Serve(l, h)
}
