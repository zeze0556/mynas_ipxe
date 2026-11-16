#!/usr/bin/env bash
# qemu-pxe-test.sh -- 在任意 Linux 主机用 QEMU 测试 iPXE 启动链路 (BIOS / UEFI)
#
# ============ 第一性原理 ============
# 全部固件 (undionly.kpxe / ipxe.pxe / ipxe.efi / ipxe.lkrn) 都由
# build_ipxe.sh 用 EMBED=embed.ipxe 编入同一段脚本:
#     #!ipxe
#     chain http://${next-server}:10180/boot?mac=${mac}
# ${next-server} 来自 DHCP 的 siaddr (option 66 亦可覆盖)，${mac} 是 iPXE 变量。
# 因此「网络启动」的成败只取决于三件事:
#   1) DHCP 是否分配地址 (yiaddr)
#   2) DHCP 是否给出 siaddr + bootfile (option 67)，并按 Option 93 分流 BIOS/UEFI
#   3) siaddr 那台机器的 TFTP:69 与 HTTP:10180 是否可达
#
# ============ 两种网络模式 ============
# user (默认, 零 root): QEMU SLIRP 自带 DHCP+TFTP，siaddr=10.0.2.2。
#      宿主 0.0.0.0:<PORT> 起纯用户态 TCP 反代 -> SERVER_HOST。完全隔离。
#      边界: 不测生产 TFTP:69，不测现网 DHCP 的 Option 93 架构分流。
#
# bridge (--bridge, 需 root): TAP 挂到宿主机的物理网桥 BRIDGE(默认 br0)，
#      VM 成为物理 LAN 的一等公民，由现网 DHCP 分配真实 IP 并完成 PXE 启动。
#      链路: VM --DHCP广播--> 现网 DHCP(yiaddr + siaddr + opt67 按 arch 分流)
#              --TFTP:69-->  siaddr 取 NBP
#              --HTTP:10180--> siaddr /boot
#      脚本只建/删自己创建的 TAP；不起 dnsmasq、不起反代、不改 iptables、
#      不占本机端口、不向现网发任何 DHCP 服务响应 —— 零污染。
#      !! 硬约束: 现网 DHCP 必须已配置 PXE (siaddr 指向 SERVER_HOST，
#         option 67 按 Option 93 分流 undionly.kpxe / ipxe.efi)。
#         用 `probe` 动作实测；不满足时请让网络管理员配置 option 66/67，
#         或改用「隔离网桥 + dnsmasq」(本脚本不提供，避免在物理 LAN 上
#         误起第二个 DHCP 服务器)。
#
# ============ 用法 ============
#   ./qemu-pxe-test.sh prepare                    # 首次: 生成并持久化 MAC
#   ./qemu-pxe-test.sh mac                        # 打印当前 MAC
#   ./qemu-pxe-test.sh probe   --bridge           # 实测现网 DHCP 的 PXE 能力 (需 root)
#   ./qemu-pxe-test.sh check   --bridge           # 前置检查，不启动 VM
#   ./qemu-pxe-test.sh bios    --bridge           # BIOS: 物理网桥 + SeaBIOS + undionly.kpxe
#   ./qemu-pxe-test.sh uefi    --bridge           # UEFI: 物理网桥 + OVMF   + ipxe.efi
#   ./qemu-pxe-test.sh bios                       # 默认 user(SLIRP) 模式
#   ./qemu-pxe-test.sh bios    --bridge --dry-run # 只打印命令，不建 TAP / 不启动 VM
#   ./qemu-pxe-test.sh --new-mac uefi --bridge    # 换一个 MAC 再跑
#
# 环境变量:
#   SERVER_HOST=192.168.1.10   SERVER_PORT=10180
#   NET_MODE=user|bridge         (--bridge / --user 可覆盖)
#   BRIDGE=br0                   TAP_NAME=<自动按 MAC 派生>
#   DHCP_IFACE=<probe 用网卡, 默认 $BRIDGE>
#   STATE_DIR=$HOME/.local/state/ipxe-qemu
#   FIRMWARE_DIR=$STATE_DIR/tftp   (仅 user 模式需要，内含 undionly.kpxe / ipxe.efi)
#   QEMU_BIN=qemu-system-x86_64  MEM=1024  MON_PORT=4550  ACCEL=kvm|tcg
#
# 前置安装 (Debian/Ubuntu):
#   sudo apt install -y qemu-system-x86 ipxe-qemu seabios ovmf
#   # user 模式: 把固件拷进 FIRMWARE_DIR (见 prepare 打印的命令)
#   # bridge 模式: NBP 由生产 TFTP 提供，无需本地固件；UEFI 仍需 ovmf
set -euo pipefail

SERVER_HOST="${SERVER_HOST:-192.168.1.10}"
SERVER_PORT="${SERVER_PORT:-10180}"
STATE_DIR="${STATE_DIR:-$HOME/.local/state/ipxe-qemu}"
FIRMWARE_DIR="${FIRMWARE_DIR:-$STATE_DIR/tftp}"
QEMU="${QEMU_BIN:-qemu-system-x86_64}"
MEM="${MEM:-4096}"
MON_PORT="${MON_PORT:-4550}"
NET_MODE="${NET_MODE:-user}"
BRIDGE="${BRIDGE:-br0}"
TAP_NAME="${TAP_NAME:-}"
DHCP_IFACE="${DHCP_IFACE:-$BRIDGE}"
MAC_FILE="$STATE_DIR/mac"
VARS_FILE="$STATE_DIR/OVMF_VARS.fd"
PROXY_SCRIPT="$STATE_DIR/forward.py"
PROBE_SCRIPT="$STATE_DIR/dhcp_probe.py"

ACTION=""
MODE=""
DRY_RUN=0
NEW_MAC=0
PROXY_PID=""
TAP_CREATED=0

die(){  echo "ERROR: $*" >&2; exit 1; }
warn(){ echo "WARN : $*" >&2; }
info(){ echo "  $*"; }
usage(){ awk 'NR==1{next} /^set -euo pipefail/{exit} {sub(/^# ?/,""); print}' "$0"; }

for a in "$@"; do
  case "$a" in
    prepare|mac|probe) ACTION="$a" ;;
    check)             ACTION=check; MODE=both ;;
    bios|uefi)         ACTION=run;   MODE="$a" ;;
    --bridge)          NET_MODE=bridge ;;
    --user)            NET_MODE=user ;;
    --dry-run)         DRY_RUN=1 ;;
    --new-mac)         NEW_MAC=1 ;;
    -h|--help)         usage; exit 0 ;;
    *) die "未知参数: $a (用 --help 看用法)" ;;
  esac
done
[[ -n $ACTION ]] || { usage; exit 1; }
[[ $NET_MODE == user || $NET_MODE == bridge ]] || die "NET_MODE 只能是 user 或 bridge: $NET_MODE"

# ---------------- 1. MAC: 首次随机生成，之后一直复用 ----------------
# 52:54:00 是 QEMU 的 OUI；首字节 0x52 的 U/L 位=1(本地管理)、I/G 位=0(单播)，
# 不会和真实网卡冲突，也不会被交换机当成组播。
gen_mac(){
  python3 -c 'import secrets; b=secrets.token_bytes(3); print("52:54:00:%02x:%02x:%02x" % tuple(b))'
}
mkdir -p "$STATE_DIR" "$FIRMWARE_DIR"
if (( NEW_MAC )) || [[ ! -s "$MAC_FILE" ]]; then
  MAC="$(gen_mac)"
  printf '%s\n' "$MAC" > "$MAC_FILE"
  info "已生成新 MAC: $MAC (持久化于 $MAC_FILE)"
else
  MAC="$(tr -d '[:space:]' < "$MAC_FILE")"
fi
[[ "$MAC" =~ ^52:54:00(:[0-9a-f]{2}){3}$ ]] || die "MAC 格式非法: $MAC (删掉 $MAC_FILE 或加 --new-mac 重建)"

# TAP 名按 MAC 派生且 < 16 字节 (IFNAMSIZ)，避免与已有 tap0 等撞车
if [[ -z $TAP_NAME ]]; then
  TAP_NAME="ipxe-$(printf '%s' "$MAC" | tr -d ':' | tail -c 7)"
fi
[[ ${#TAP_NAME} -le 15 ]] || die "TAP_NAME 太长(>15): $TAP_NAME"

if [[ $ACTION == mac ]]; then echo "$MAC"; exit 0; fi

if [[ $ACTION == prepare ]]; then
  echo "MAC       : $MAC"
  echo "STATE_DIR : $STATE_DIR"
  echo "网络模式  : $NET_MODE"
  if [[ $NET_MODE == bridge ]]; then
    echo "物理网桥  : $BRIDGE    TAP: $TAP_NAME"
    echo
    echo "bridge 模式无需本地固件：NBP 由现网 DHCP 指定的 TFTP 服务器提供。"
    echo "但必须先用 probe 实测现网 DHCP 是否已配置 PXE："
    echo "  sudo $0 probe --bridge"
    echo "  sudo $0 check --bridge"
    echo "UEFI 另需 OVMF: sudo apt install -y ovmf"
  else
    echo "固件目录  : $FIRMWARE_DIR"
    echo
    echo "把 iPXE 固件拷进固件目录（任选一条，路径按实际改）："
    echo "  rsync -av user@<build-host>:/path/to/ipxe/assets/ipxe/ $FIRMWARE_DIR/"
    echo "  rsync -av root@<server-ip>:/srv/ipxe/assets/ipxe/     $FIRMWARE_DIR/"
    echo
    echo "所需: undionly.kpxe (BIOS) / ipxe.efi (UEFI)；UEFI 另需 OVMF"
  fi
  echo "完成后: $0 check --bridge && $0 bios --bridge   /   $0 uefi --bridge"
  exit 0
fi

if [[ $MODE == bios ]]; then
  NBP="undionly.kpxe"; MACHINE="pc";  FW_NOTE="SeaBIOS"
elif [[ $MODE == uefi ]]; then
  NBP="ipxe.efi";      MACHINE="q35"; FW_NOTE="OVMF"
else
  NBP=""; MACHINE="-"; FW_NOTE="BIOS+UEFI"
fi

# ---------------- 2. 固件/ROM 探测 ----------------
OVMF_CODE=""; OVMF_VARS_SRC=""; PXE_ROM=""
find_ovmf(){
  local c v
  while IFS= read -r c; do
    v="${c/CODE/VARS}"
    if [[ -f "$v" ]]; then OVMF_CODE="$c"; OVMF_VARS_SRC="$v"; return 0; fi
  done < <(find /usr/share /usr/lib -maxdepth 5 -type f -iname 'OVMF_CODE*.fd' 2>/dev/null | sort)
  return 1
}
find_pxe_rom(){
  PXE_ROM="$(find /usr/share/qemu /usr/lib/ipxe /usr/share/ipxe -type f -name 'pxe-e1000.rom' -print -quit 2>/dev/null || true)"
}
# 无条件探测，保证 --dry-run 打印的命令与实跑逐字一致
find_pxe_rom
find_ovmf || true

# ---------------- 3. DHCP PXE 能力探测 (纯只读，只发 DISCOVER) ----------------
write_probe(){
  cat > "$PROBE_SCRIPT" <<'PY'
import socket, struct, secrets, sys, time
IFACE, ARCH = sys.argv[1], int(sys.argv[2])
MAC = bytes.fromhex("525400a1b2c3")

def opt(c, p): return bytes([c, len(p)]) + p

def discover(arch):
    xid = secrets.token_bytes(4)
    vc = ("PXEClient:Arch:%05d:UNDI:002001" % arch).encode()
    pkt = struct.pack("!BBBBIHH4s4s4s4s16s64s128s",
                      1, 1, 6, 0, int.from_bytes(xid, "big"), 0, 0x8000,
                      b"\0"*4, b"\0"*4, b"\0"*4, b"\0"*4,
                      MAC + b"\0"*10, b"\0"*64, b"\0"*128)
    o = opt(53, b"\x01")
    o += opt(55, bytes([1, 3, 6, 43, 60, 66, 67, 97]))
    o += opt(60, vc)
    o += opt(93, struct.pack("!H", arch))
    o += opt(97, b"\0"*17)
    o += b"\xff"
    return xid, pkt + b"\x63\x82\x53\x63" + o + b"\0"*8

def parse(d, xid):
    if len(d) < 240 or d[4:8] != xid: return None
    r = {"yiaddr": socket.inet_ntoa(d[16:20]),
         "siaddr": socket.inet_ntoa(d[20:24]),   # BOOTP siaddr @ offset 20
         "file": d[108:236].split(b"\0", 1)[0].decode(errors="replace"),
         "bootfile": "", "server": "", "codes": []}
    p = 240
    if d[236:240] != b"\x63\x82\x53\x63": return r
    while p < len(d):
        c = d[p]
        if c == 0xff: break
        if c == 0: p += 1; continue
        if p + 1 >= len(d): break
        ln, v = d[p+1], d[p+2:p+2+d[p+1]]
        r["codes"].append(c)
        if c == 54 and len(v) == 4: r["server"] = socket.inet_ntoa(v)
        if c == 66: r["file"] = v.decode(errors="replace").rstrip("\x00")
        if c == 67: r["bootfile"] = v.decode(errors="replace").rstrip("\x00")
        p += 2 + ln
    return r

s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
try:
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, IFACE.encode())
except OSError as e:
    print("WARN SO_BINDTODEVICE:", e, file=sys.stderr)
s.bind(("0.0.0.0", 68))
s.settimeout(1.0)
xid, pkt = discover(ARCH)
s.sendto(pkt, ("255.255.255.255", 67))
seen = []
end = time.time() + 4
while time.time() < end:
    try: d, a = s.recvfrom(2048)
    except socket.timeout: continue
    r = parse(d, xid)
    if r: seen.append((a[0], r))
if not seen:
    print("[SUMMARY] arch=%d NO_REPLY" % ARCH)
else:
    for src, r in seen:
        print("[SUMMARY] arch=%d src=%s yiaddr=%s siaddr=%s bootfile=%r file=%r server=%s codes=%s" %
              (ARCH, src, r["yiaddr"], r["siaddr"], r["bootfile"], r["file"],
               r["server"], r["codes"]))
s.close()
PY
}

do_probe(){
  (( EUID == 0 )) || die "probe 需要 root (要绑定 UDP:68 并 SO_BINDTODEVICE): sudo $0 probe --bridge"
  command -v python3 >/dev/null 2>&1 || die "缺 python3"
  ip link show "$DHCP_IFACE" >/dev/null 2>&1 || die "网卡不存在: $DHCP_IFACE (用 DHCP_IFACE= 指定)"
  write_probe
  echo "=== 现网 DHCP PXE 能力探测 (iface=$DHCP_IFACE, 只发 DISCOVER/不申请租约) ==="
  local out a0 a7
  out="$(python3 "$PROBE_SCRIPT" "$DHCP_IFACE" 0; python3 "$PROBE_SCRIPT" "$DHCP_IFACE" 7)" \
    || die "DHCP 探测执行失败 (检查 $DHCP_IFACE 与 UDP:68 是否被占用)"
  printf '%s\n' "$out"
  if printf '%s\n' "$out" | grep -q 'NO_REPLY'; then
    die "现网 DHCP 无响应 —— 纯桥接无法网络启动，需网络管理员配置 PXE 或改用隔离网桥 + dnsmasq"
  fi
  a0="$(printf '%s\n' "$out" | grep -m1 '^\[SUMMARY\] arch=0 ' || true)"
  a7="$(printf '%s\n' "$out" | grep -m1 '^\[SUMMARY\] arch=7 ' || true)"
  [[ -n $a0 && -n $a7 ]] || die "未收齐 BIOS/UEFI 两种架构的 DHCP 响应"
  local si; si="$(printf '%s' "$a0" | sed -n 's/.*siaddr=\([0-9.]*\).*/\1/p')"
  local b0 b7
  b0="$(printf '%s' "$a0" | sed -n "s/.*bootfile='\([^']*\)'.*/\1/p")"
  b7="$(printf '%s' "$a7" | sed -n "s/.*bootfile='\([^']*\)'.*/\1/p")"
  echo "--- 判定 ---"
  if [[ "$si" == "$SERVER_HOST" ]]; then
    info "OK  siaddr=$si 与 SERVER_HOST 一致，iPXE chain 会打到 $SERVER_HOST:$SERVER_PORT"
  else
    warn "siaddr=$si ≠ SERVER_HOST=$SERVER_HOST —— chain 会打到 $si:$SERVER_PORT；请改 SERVER_HOST 或修 DHCP"
  fi
  [[ "$b0" == "undionly.kpxe" ]] || warn "BIOS bootfile=$b0 (期望 undionly.kpxe)"
  [[ "$b7" == "ipxe.efi" ]]      || warn "UEFI bootfile=$b7 (期望 ipxe.efi)"
  info "BIOS bootfile=$b0  UEFI bootfile=$b7 (Option 93 分流)"
}

if [[ $ACTION == probe ]]; then do_probe; exit 0; fi

# ---------------- 4. 前置检查 ----------------
check_tftp(){
  python3 - "$SERVER_HOST" <<'PY' || die "生产 TFTP :69 不可达/无响应: $SERVER_HOST (检查服务与防火墙)"
import socket, sys
h = sys.argv[1]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(3)
s.sendto(b"\x00\x01undionly.kpxe\x00octet\x00", (h, 69))
try:
    d, a = s.recvfrom(2048)
    print("TFTP OK  from %s:%d opcode=%s len=%d" % (a[0], a[1], d[:2].hex(), len(d)))
    sys.exit(0 if d[:2] in (b"\x00\x03", b"\x00\x06") else 1)
except Exception as e:
    print("TFTP FAIL:", e); sys.exit(1)
PY
}

do_check(){
  local strict="${1:-1}"
  command -v "$QEMU" >/dev/null 2>&1 || die "缺 QEMU ($QEMU): apt install -y qemu-system-x86"
  command -v python3  >/dev/null 2>&1 || die "缺 python3"
  if [[ ( $MODE == bios || $MODE == both ) && -z $PXE_ROM ]]; then
    warn "未找到 pxe-e1000.rom，BIOS 可能无法网络启动: apt install -y ipxe-qemu"
  fi
  if [[ $MODE == uefi || $MODE == both ]]; then
    if [[ -z $OVMF_CODE ]]; then
      if (( strict )); then
        die "缺 OVMF 固件: apt install -y ovmf (Debian 路径 /usr/share/OVMF/OVMF_CODE_4M.fd)"
      else
        warn "缺 OVMF 固件 (仅影响 uefi 模式): apt install -y ovmf"
      fi
    fi
  fi

  if [[ $NET_MODE == bridge ]]; then
    (( EUID == 0 )) || die "bridge 模式需要 root (建 TAP 需 CAP_NET_ADMIN): sudo $0 $MODE --bridge"
    if [[ -n ${SUDO_USER:-} ]]; then
      warn "sudo 下 HOME 可能变化：当前 MAC 文件为 $MAC_FILE；若与 prepare 时不同，请用 sudo -E 或显式 STATE_DIR="
    fi
    command -v ip >/dev/null 2>&1 || die "缺 iproute2 (ip 命令)"
    ip link show "$BRIDGE" >/dev/null 2>&1 || die "网桥不存在: $BRIDGE (用 BRIDGE= 指定)"
    [[ "$(cat "/sys/class/net/$BRIDGE/operstate" 2>/dev/null)" == "up" ]] || warn "网桥 $BRIDGE 非 UP 状态"
    if ip link show "$TAP_NAME" >/dev/null 2>&1; then
      die "TAP $TAP_NAME 已存在(残留或他人占用)，换 TAP_NAME= 或先手工清理"
    fi
    info "物理网桥: $BRIDGE   TAP: $TAP_NAME"
    check_tftp
  else
    [[ -s "$FIRMWARE_DIR/undionly.kpxe" ]] || die "缺 $FIRMWARE_DIR/undionly.kpxe —— 先跑: $0 prepare"
    [[ -s "$FIRMWARE_DIR/ipxe.efi" ]]      || die "缺 $FIRMWARE_DIR/ipxe.efi —— 先跑: $0 prepare"
    python3 - "$SERVER_PORT" <<'PY' || die "本机 $SERVER_PORT 端口已被占用；若本机也在跑 ipxe-server，请让测试脚本与服务分机部署，或改用 --bridge"
import socket, sys
s = socket.socket()
try: s.bind(("0.0.0.0", int(sys.argv[1])))
finally: s.close()
PY
  fi
  code="$(curl -s -m 4 -o /dev/null -w '%{http_code}' "http://$SERVER_HOST:$SERVER_PORT/" || true)"
  [[ "$code" != "000" && -n "$code" ]] || die "连不上 http://$SERVER_HOST:$SERVER_PORT/ (检查防火墙/服务是否启动)"
  info "服务可达: http://$SERVER_HOST:$SERVER_PORT/ -> HTTP $code"
}

if (( ! DRY_RUN )); then
  if [[ $ACTION == run ]]; then do_check 1; else do_check 0; fi
fi

if [[ $ACTION == check ]]; then
  echo "CHECK OK  (未启动 VM / 未建 TAP / 未起反代)"
  info "网络模式 : $NET_MODE"
  info "MAC      : $MAC"
  info "QEMU     : $(command -v "$QEMU")"
  if [[ $NET_MODE == bridge ]]; then
    info "建议先跑: sudo $0 probe --bridge  (实测现网 DHCP 的 siaddr / Option 93 分流)"
  else
    info "固件目录 : $FIRMWARE_DIR"
  fi
  if [[ -n $OVMF_CODE ]]; then
    info "OVMF CODE: $OVMF_CODE"
    info "OVMF VARS: $OVMF_VARS_SRC"
  fi
  exit 0
fi

# ---------------- 5. user 模式的 TCP 反代 (纯标准库) ----------------
write_proxy(){
  cat > "$PROXY_SCRIPT" <<'PY'
import socket, sys, threading
LPORT, RHOST, RPORT = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("0.0.0.0", LPORT)); srv.listen(64)
print("proxy 0.0.0.0:%d -> %s:%d" % (LPORT, RHOST, RPORT), flush=True)
def pipe(a, b):
    try:
        while True:
            d = a.recv(65536)
            if not d: break
            b.sendall(d)
    except OSError:
        pass
    finally:
        try: b.shutdown(socket.SHUT_WR)
        except OSError: pass
def handle(c):
    try: r = socket.create_connection((RHOST, RPORT), 5)
    except OSError as e:
        print("upstream failed:", e, flush=True); c.close(); return
    threading.Thread(target=pipe, args=(c, r), daemon=True).start()
    pipe(r, c); c.close(); r.close()
while True:
    c, _ = srv.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
PY
}

# ---------------- 6. bridge 模式的 TAP 生命周期 ----------------
create_tap(){
  ip tuntap add dev "$TAP_NAME" mode tap || die "创建 TAP 失败 (需 CAP_NET_ADMIN)"
  TAP_CREATED=1
  ip link set "$TAP_NAME" master "$BRIDGE" || die "把 $TAP_NAME 挂到 $BRIDGE 失败"
  ip link set "$TAP_NAME" up
  info "TAP $TAP_NAME 已挂到 $BRIDGE"
}

cleanup(){
  if [[ -n "$PROXY_PID" ]] && kill -0 "$PROXY_PID" 2>/dev/null; then
    kill "$PROXY_PID" 2>/dev/null || true
    wait "$PROXY_PID" 2>/dev/null || true
  fi
  # 只删自己创建的 TAP，绝不误删别人的 tap0 等
  if (( TAP_CREATED )); then
    ip link del "$TAP_NAME" 2>/dev/null || warn "删除 TAP $TAP_NAME 失败，请手工: ip link del $TAP_NAME"
    info "已清理 TAP $TAP_NAME"
  fi
}
trap cleanup EXIT INT TERM

# ---------------- 7. 组装 QEMU 命令 ----------------
ACCEL="${ACCEL:-}"
if [[ -z $ACCEL ]]; then [[ -w /dev/kvm ]] && ACCEL=kvm || ACCEL=tcg; fi

DEV="e1000,netdev=n0,mac=$MAC"
if [[ $MODE == bios && -n $PXE_ROM ]]; then
  DEV+=",romfile=$PXE_ROM"
fi

if [[ $NET_MODE == bridge ]]; then
  NETDEV=( -netdev "tap,id=n0,ifname=$TAP_NAME,script=no,downscript=no" )
else
  NETDEV=( -netdev "user,id=n0,tftp=$FIRMWARE_DIR,bootfile=$NBP" )
fi

QEMU_ARGS=(
  -machine "$MACHINE" -m "$MEM" -accel "$ACCEL"
  -boot order=n
  -display "${QEMU_DISPLAY:-curses}" -serial "${QEMU_SERIAL:-stdio}"
  -name "ipxe-$NET_MODE-$MODE-$MAC"
  -device "$DEV" "${NETDEV[@]}"
  -monitor "telnet:127.0.0.1:$MON_PORT,server,nowait"
)
if [[ $MODE == uefi ]]; then
  OVMF_CODE_ARG="${OVMF_CODE:-<OVMF_CODE>}"
  if (( DRY_RUN )); then
    QEMU_ARGS+=( -drive "if=pflash,format=raw,readonly=on,file=$OVMF_CODE_ARG"
                 -drive "if=pflash,format=raw,file=$VARS_FILE" )
  else
    [[ -f "$VARS_FILE" ]] || cp "$OVMF_VARS_SRC" "$VARS_FILE"
    QEMU_ARGS+=( -drive "if=pflash,format=raw,readonly=on,file=$OVMF_CODE"
                 -drive "if=pflash,format=raw,file=$VARS_FILE" )
  fi
fi

# ---------------- 8. 打印链路信息 ----------------
echo "=============================================================="
echo " 模式     : $MODE ($FW_NOTE / $MACHINE / accel=$ACCEL / net=$NET_MODE)"
echo " 固定 MAC : $MAC   [$MAC_FILE]"
echo " 目标服务 : $SERVER_HOST:$SERVER_PORT"
if [[ $NET_MODE == bridge ]]; then
  echo " 物理网桥 : $BRIDGE   TAP: $TAP_NAME"
  echo " 链路     : VM(TAP) --DHCP广播--> 现网 DHCP 分配真实 IP"
  echo "            siaddr = $SERVER_HOST (需 probe 实测)"
  echo "            option 67 按 Option 93 分流: BIOS=undionly.kpxe / UEFI=ipxe.efi"
  echo "            --TFTP :69--> $SERVER_HOST 取 $NBP (生产服务)"
  echo "            iPXE chain http://\${next-server}:$SERVER_PORT/boot?mac=\${mac}"
  echo "            --HTTP :$SERVER_PORT--> $SERVER_HOST /boot (生产服务)"
  echo " 零污染   : 不起 DHCP / 不起 TFTP / 不起反代 / 不改 iptables / 不占本机端口"
  echo "            NBP 由现网 DHCP 决定，脚本指定的 $NBP 仅作预期对照"
else
  echo " 链路     : PXE ROM --TFTP(QEMU内置)--> $NBP"
  echo "            iPXE chain http://\${next-server}:$SERVER_PORT/boot?mac=\${mac}"
  echo "            \${next-server} 应 = 10.0.2.2 (SLIRP siaddr)"
  echo "            宿主 0.0.0.0:$SERVER_PORT --TCP反代--> $SERVER_HOST:$SERVER_PORT"
fi
echo " monitor  : telnet 127.0.0.1 $MON_PORT   (quit 可终止)"
echo "--------------------------------------------------------------"
echo " 注意: 生产库 public-* 模板 iso_id 均为 NULL，/boot 会返回"
echo "       'ERROR: template ... has no ISO bound' —— 看到它即证明"
echo "       DHCP / TFTP / iPXE / HTTP / 模板匹配 全链路已打通。"
if [[ $NET_MODE == bridge ]]; then
  echo " 注意: 若停在 iPXE shell 或 chain 到非 $SERVER_HOST 的地址，"
  echo "       先跑 'sudo $0 probe --bridge' 核对 siaddr 与 option 67。"
else
  echo " 注意: 若 chain 到非 10.0.2.2 的地址，说明 SLIRP siaddr 与预期不符，"
  echo "       建议改用 --bridge 走物理网络真实链路。"
fi
echo "=============================================================="

if (( DRY_RUN )); then
  if [[ $NET_MODE == bridge ]]; then
    echo "[DRY-RUN] ip tuntap add dev $TAP_NAME mode tap"
    echo "[DRY-RUN] ip link set $TAP_NAME master $BRIDGE"
    echo "[DRY-RUN] ip link set $TAP_NAME up"
  else
    echo "[DRY-RUN] python3 $PROXY_SCRIPT $SERVER_PORT $SERVER_HOST $SERVER_PORT &"
  fi
  printf '[DRY-RUN] %q ' "$QEMU"; printf '%q ' "${QEMU_ARGS[@]}"; echo
  if [[ $NET_MODE == bridge ]]; then echo "[DRY-RUN] ip link del $TAP_NAME  (trap cleanup)"; fi
  exit 0
fi

if [[ $NET_MODE == bridge ]]; then
  create_tap
else
  write_proxy
  python3 "$PROXY_SCRIPT" "$SERVER_PORT" "$SERVER_HOST" "$SERVER_PORT" > "$STATE_DIR/proxy.log" 2>&1 &
  PROXY_PID=$!
  sleep 0.5
  kill -0 "$PROXY_PID" 2>/dev/null || die "反代启动失败: $(cat "$STATE_DIR/proxy.log")"
  info "反代已启动 (pid=$PROXY_PID, 日志 $STATE_DIR/proxy.log)"
fi

echo "[启动 QEMU] Ctrl-C 退出并自动清理 $( [[ $NET_MODE == bridge ]] && echo TAP || echo 反代 )"
"$QEMU" "${QEMU_ARGS[@]}"
