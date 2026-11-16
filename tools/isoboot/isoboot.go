package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/template"
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

var (
	listenAddr = envOr("ISOBOOT_ADDR", ":18080")
	serveDir   = envOr("ISOBOOT_WWW", "./www")
	alpineDev  = envOr("ISOBOOT_ALPINE_DEV", "")
	modloop    = envOr("ISOBOOT_MODLOOP", "")
	nextServer = envOr("ISOBOOT_NEXT", "10.0.2.2")
)

// Store 仅用于演示 MAC->profile 差异化（与生产 ipxe-server 同思路）。
type Store struct {
	mu    sync.Mutex
	hosts map[string]string // mac(lower) -> profile
}

func NewStore() *Store {
	s := &Store{hosts: map[string]string{}}
	s.hosts["52:54:00:00:00:01"] = "alpine"
	s.hosts["52:54:00:00:00:02"] = "alpine"
	return s
}

func (s *Store) profile(mac string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.hosts[strings.ToLower(mac)]; ok {
		return p
	}
	return "public"
}

type bootData struct {
	MAC       string
	Profile   string
	Next      string
	AlpineDev string
	Modloop   string
}

// bootTmpl：iPXE 脚本。真实内核/内存盘由本服务 /www/ 提供，apks/modloop 由 ISOBOOT_ALPINE_DEV/MODLOOP 决定（镜像或本地提取）。
var bootTmpl = template.Must(template.New("boot").Parse(`#!ipxe
echo PXE boot for mac {{.MAC}} profile={{.Profile}}
kernel http://{{.Next}}:18080/www/vmlinuz-virt
initrd http://{{.Next}}:18080/www/initramfs-virt
imgargs vmlinuz-virt initrd=initramfs-virt alpine_dev={{.AlpineDev}} modloop={{.Modloop}} console=ttyS0 ip=dhcp
boot
`))

var store = NewStore()

func bootHandler(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	if mac == "" {
		mac = "unknown"
	}
	prof := store.profile(strings.ToLower(mac))
	log.Printf("BOOT mac=%s known=%v profile=%s", mac, prof != "public", prof)
	w.Header().Set("Content-Type", "text/plain")
	_ = bootTmpl.Execute(w, bootData{
		MAC:       mac,
		Profile:   prof,
		Next:      nextServer,
		AlpineDev: alpineDev,
		Modloop:   modloop,
	})
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/boot", bootHandler)
	mux.Handle("/www/", http.StripPrefix("/www/", http.FileServer(http.Dir(serveDir))))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	log.Printf("isoboot listening on %s, static from %s", listenAddr, serveDir)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
