package iso

import (
	"os"
	"path/filepath"
	"testing"
)

// 模拟 ISO 解包产物：apks 仓库文件被 ISO9660 改写为下划线名（无 Rock Ridge），
// boot 文件保留 Rock Ridge 原名（小写带连字符）。
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveISO9660MangledNames(t *testing.T) {
	base := t.TempDir()
	cache := filepath.Join(base, "cache")
	isoName := "alpine-virt-3.20.10-x86_64"

	// 无 Rock Ridge：ISO9660 改写名
	apkMangled := filepath.Join(cache, isoName, "apks", "x86_64", "apkindex_tar.gz")
	pkgMangled := filepath.Join(cache, isoName, "apks", "x86_64", "alpine_base_3_20_10_r0.apk")
	// 有 Rock Ridge：原名
	vmlinuz := filepath.Join(cache, isoName, "boot", "vmlinuz-virt")
	// 诱饵：同名文件放在不同目录，验证目录前缀被尊重（修复前会误匹配）
	decoy := filepath.Join(cache, isoName, "decoy", "apkindex_tar.gz")

	writeFile(t, apkMangled)
	writeFile(t, pkgMangled)
	writeFile(t, vmlinuz)
	writeFile(t, decoy)

	cases := []struct {
		name    string
		req     string
		want    string
		wantOK  bool
	}{
		{"apk index mangled->orig", "apks/x86_64/APKINDEX.tar.gz", apkMangled, true},
		{"pkg mangled->orig", "apks/x86_64/alpine-base-3.20.10-r0.apk", pkgMangled, true},
		{"pkg orig case mixed", "apks/x86_64/ALPINE-BASE-3.20.10-R0.APK", pkgMangled, true},
		{"vmlinuz exact", "boot/vmlinuz-virt", vmlinuz, true},
		{"vmlinuz upper", "BOOT/VMLINUZ-VIRT", vmlinuz, true},
		{"missing", "apks/x86_64/nope.apk", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Resolve(cache, cache, isoName, nil, nil, c.req)
			if ok != c.wantOK {
				t.Fatalf("req=%q ok=%v wantOK=%v", c.req, ok, c.wantOK)
			}
			if got != c.want {
				t.Fatalf("req=%q got=%q want=%q", c.req, got, c.want)
			}
		})
	}
}

func TestCanonKey(t *testing.T) {
	cases := map[string]string{
		"apks/x86_64/APKINDEX.tar.gz":            "apks/x86_64/apkindex_tar_gz",
		"apks/x86_64/alpine-base-3.20.10-r0.apk": "apks/x86_64/alpine_base_3_20_10_r0_apk",
		"boot/vmlinuz-virt":                      "boot/vmlinuz_virt",
		"boot/System.map-6.6.134-0-virt":         "boot/system_map_6_6_134_0_virt",
		"some_file.TAR.GZ":                       "some_file_tar_gz",
	}
	for in, want := range cases {
		if got := canonKey(in); got != want {
			t.Errorf("canonKey(%q)=%q want %q", in, got, want)
		}
	}
}
