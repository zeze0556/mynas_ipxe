package iso

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kdomanski/iso9660"
)

// normName 把 ISO9660 文件名规整为小写并去掉 ;N 版本号。
// 这样引导脚本里用的小写路径（casper/vmlinuz）能与解包产物对应。
// 同时剔除 ISO9660 常见的 NUL 终止符与控制字符——否则 os.Create 会因路径含 NUL 报 "invalid argument"。
func normName(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if r == 0 || r < 0x20 { // 去掉 NUL 与控制字符
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// normPath 规整请求路径：去前导斜杠、转小写、去 ;N、反斜杠转正斜杠。
func normPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "/")
	p = normName(p)
	return p
}

// Import 把 ISO 解包到 destDir（只读 base）。返回解出的文件数。
func Import(isoPath, destDir string) (int, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}
	f, err := os.Open(isoPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	img, err := iso9660.OpenImage(f)
	if err != nil {
		return 0, fmt.Errorf("open iso: %w", err)
	}
	root, err := img.RootDir()
	if err != nil {
		return 0, fmt.Errorf("root dir: %w", err)
	}
	count := 0
	if err := walk(root, destDir, &count); err != nil {
		return count, err
	}
	return count, nil
}

func walk(dir *iso9660.File, outDir string, count *int) error {
	children, err := dir.GetChildren()
	if err != nil {
		return err
	}
	for _, c := range children {
		name := normName(c.Name())
		if name == "" {
			continue
		}
		target := filepath.Join(outDir, name)
		if c.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := walk(c, target, count); err != nil {
				return err
			}
		} else {
			if err := extractFile(c, target); err != nil {
				// 单个文件解包失败（如非法文件名）跳过，不中断整卷解包
				continue
			}
			*count++
		}
	}
	return nil
}

func extractFile(f *iso9660.File, target string) error {
	r := f.Reader()
	if r == nil {
		return nil
	}
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, r); err != nil {
		return err
	}
	return nil
}

// Resolve 按优先级合并路径，返回命中的本地文件与是否找到。
// 优先级：host 注入 > user 注入 > global 注入 > base(ISO 解包)。
//
// 匹配分两步：
//  1. 精确匹配（大小写由目标文件系统决定，Linux 下区分大小写）。
//  2. 容错匹配：ISO9660 对无 Rock Ridge 的文件只保留 ISO9660 基础名，
//     把 - . _ 统一改写（如 alpine-base-3.20.10-r0.apk → ALPINE_BASE_3_20_10_R0.APK），
//     而客户端（apk 等）请求的是原始大小写/连字符名。这里用规范化键做兜底，
//     让 apkindex_tar.gz 与 APKINDEX.tar.gz 互通，同时不影响 vmlinuz-virt 等精确名。
func Resolve(overlayDir, cacheDir, isoName string, hostID, userID *uint, reqPath string) (string, bool) {
	p := normPath(reqPath)
	layers := make([]string, 0, 4)
	if hostID != nil {
		layers = append(layers, filepath.Join(overlayDir, "host", fmt.Sprintf("%d", *hostID), isoName))
	}
	if userID != nil {
		layers = append(layers, filepath.Join(overlayDir, "user", fmt.Sprintf("%d", *userID), isoName))
	}
	layers = append(layers, filepath.Join(overlayDir, "global", isoName))
	layers = append(layers, filepath.Join(cacheDir, isoName))

	// 1) 精确匹配
	for _, dir := range layers {
		c := filepath.Join(dir, p)
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, true
		}
	}
	// 2) 容错匹配（ISO9660 文件名改写）
	for _, dir := range layers {
		if hit, ok := fuzzyFind(dir, p); ok {
			return hit, true
		}
	}
	return "", false
}

// fuzzyFind 在 root 下递归查找，返回首个规范化键与 target 相同的文件。
func fuzzyFind(root, target string) (string, bool) {
	tk := canonKey(target)
	hit := ""
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if canonKey(rel) == tk {
			hit = path
			return filepath.SkipDir
		}
		return nil
	})
	if hit == "" {
		return "", false
	}
	return hit, true
}

// canonKey 把路径规范化为大小写不敏感、连字符/点/下划线统一的键。
// 仅对 basename 做改写（目录层级原样保留，用 / 连接），避免跨目录误匹配。
// 因请求名与磁盘名都经过同一变换，对称即可，无需区分扩展名。
// 例：apks/x86_64/APKINDEX.tar.gz → apks/x86_64/apkindex_tar_gz
//
//	alpine-base-3.20.10-r0.apk   → apks/x86_64/alpine_base_3_20_10_r0_apk
//	vmlinuz-virt                 → boot/vmlinuz_virt
func canonKey(p string) string {
	p = strings.ToLower(filepath.ToSlash(p))
	dir := path.Dir(p)
	base := path.Base(p)
	normBase := sepNorm(base)
	if dir == "." || dir == "" {
		return normBase
	}
	return dir + "/" + normBase
}

// sepNorm 把 - . _ 统一为 _ 并折叠连续下划线（小写输入）。
func sepNorm(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '.' || r == '_' {
			if b.Len() == 0 || b.String()[b.Len()-1] != '_' {
				b.WriteRune('_')
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// 注入作用域（与 db.InjectionScope 的取值保持一致，避免引入 db 依赖）。
const (
	injGlobal = "global"
	injUser   = "user"
	injHost   = "host"
)

// OverlayStoredPath 计算一次注入文件的落盘相对/绝对路径。
func OverlayStoredPath(overlayDir, scope string, ownerID *uint, isoName, imagePath string) string {
	seg := injGlobal
	if scope == injUser && ownerID != nil {
		seg = fmt.Sprintf("user/%d", *ownerID)
	}
	if scope == injHost && ownerID != nil {
		seg = fmt.Sprintf("host/%d", *ownerID)
	}
	return filepath.Join(overlayDir, seg, isoName, normPath(imagePath))
}
