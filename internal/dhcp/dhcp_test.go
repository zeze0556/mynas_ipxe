package dhcp

import (
	"net"
	"testing"

	dhcp "github.com/krolaw/dhcp4"
)

// mkReq 构造一个带（或不带）Option 93 的 DHCP Discover 请求包。
func mkReq(arch uint16, hasArch bool) dhcp.Packet {
	opts := []dhcp.Option{}
	if hasArch {
		opts = append(opts, dhcp.Option{
			Code:  dhcp.OptionClientArchitecture,
			Value: []byte{byte(arch >> 8), byte(arch)},
		})
	}
	return dhcp.RequestPacket(dhcp.Discover,
		net.HardwareAddr{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		nil, []byte{1, 2, 3, 4}, true, opts)
}

func TestPickBootFileByArch(t *testing.T) {
	h := &Handler{cfg: Config{}} // 各 BootFile 空 -> 走常量兜底
	cases := []struct {
		name    string
		arch    uint16
		hasArch bool
		want    string
	}{
		{"bios", archBIOS, true, "undionly.kpxe"},
		{"efi32", archEFI32, true, "ipxe.efi"},
		{"efi64", archEFI64, true, "ipxe.efi"},
		{"efi64bc", archEFI64BC, true, "ipxe.efi"},
		{"arm64", archARM64, true, "ipxe-arm64.efi"},
		{"arm64bc", archARM64BC, true, "ipxe-arm64.efi"},
		{"no-option93", 0, false, "undionly.kpxe"},
		{"unknown-arch", 0x0020, true, "undionly.kpxe"},
	}
	for _, c := range cases {
		got := h.pickBootFile(mkReq(c.arch, c.hasArch))
		if got != c.want {
			t.Errorf("%s: want %q got %q", c.name, c.want, got)
		}
	}
}

func TestPickBootFileOverride(t *testing.T) {
	h := &Handler{cfg: Config{BootFileEFI: "custom.efi", BootFileBIOS: "bios.kpxe"}}
	if got := h.pickBootFile(mkReq(archEFI64, true)); got != "custom.efi" {
		t.Errorf("efi override: want custom.efi got %q", got)
	}
	if got := h.pickBootFile(mkReq(archBIOS, true)); got != "bios.kpxe" {
		t.Errorf("bios override: want bios.kpxe got %q", got)
	}
}
