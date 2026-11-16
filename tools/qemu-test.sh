#!/usr/bin/env bash
# 用多个 QEMU 实例验证 iPXE 服务器"按 MAC 下发不同配置"的核心链路（方案 A：轻量）。
#
# 前置条件:
#   1) 安装 qemu:
#        Windows: https://www.qemu.org/download/ (下载 qemu-system-x86_64)
#        WSL:     apt install qemu-system-x86
#   2) 构建 ipxe.lkrn (可被 -kernel 直接加载的 iPXE, 内嵌 chain 脚本):
#        cd <ipxe-src>/src && make bin/ipxe.lkrn EMBED=embed.ipxe
#      其中 embed.ipxe 内容:
#        #!ipxe
#        chain http://<SERVER_IP>:<SERVER_PORT>/boot?mac=${mac}
#      SLIRP 模式下 SERVER_IP 用 10.0.2.2 (QEMU user 模式 host 网关)；
#      若服务端跑在 WSL 实体 IP 上，则 SERVER_IP 用该 IP。
#   3) 服务端 config 必须 dhcp.enabled=false (避免与 SLIRP 自带 DHCP 冲突)。
#   4) 服务端已启动 (默认 http_port 8080)。
#
# 本脚本启动 N 个 BIOS 客户端，各自不同 MAC，验证服务端按 MAC 返回不同脚本与 overlay 内容。
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
LKRN="${IPXE_LKRN:-$HERE/assets/ipxe/ipxe.lkrn}"
SERVER_IP="${SERVER_IP:-10.0.2.2}"
SERVER_PORT="${SERVER_PORT:-8080}"
N="${N:-3}"
QEMU="${QEMU_BIN:-qemu-system-x86_64}"

if ! command -v "$QEMU" >/dev/null 2>&1; then
  echo "ERROR: 找不到 $QEMU，请先安装 qemu (见脚本顶部说明)" >&2
  exit 1
fi
if [ ! -f "$LKRN" ]; then
  echo "ERROR: 缺少 $LKRN (请用 'make bin/ipxe.lkrn EMBED=embed.ipxe' 构建)" >&2
  exit 1
fi

PIDS=()
for i in $(seq 0 $((N-1))); do
  MAC="52:54:00:00:00:$(printf '%02x' "$i")"
  MON_PORT=$((4440 + i))
  echo "启动 client-$i  mac=$MAC  monitor=telnet:127.0.0.1:$MON_PORT"
  "$QEMU" \
    -machine pc -m 512 -accel tcg \
    -kernel "$LKRN" \
    -netdev user,id=n0 -device e1000,netdev=n0,mac="$MAC" \
    -serial mon:stdio -display none \
    -monitor "telnet:127.0.0.1:$MON_PORT,server,nowait" \
    -name "client-$i" &
  PIDS+=($!)
done

echo "已启动 $N 个客户端。"
echo "  观察: 用 'telnet 127.0.0.1 444X' 连 monitor，或看服务端日志/数据库确认各 MAC 命中不同模板。"
echo "  终止: Ctrl-C 会杀掉全部客户端。"
trap 'echo; echo "terminating..."; kill "${PIDS[@]}" 2>/dev/null || true' INT TERM
wait

# ---------------------------------------------------------------------------
# 方案 B（完整 PXE：测自研 DHCP + Option93 分流）切换说明（需手动改）:
#   1) WSL2 内建内部 bridge:
#        ip link add name br0 type bridge; ip link set br0 up
#        ip addr add 192.168.100.1/24 dev br0
#      服务端 config: dhcp.enabled=true, next_server=192.168.100.1, dhcp.range_start/end 同网段
#   2) 每个 QEMU 改用 tap 挂 br0 (需 root + tun/tap):
#        -netdev tap,id=n0,ifname=tap$i,script=no,downscript=no
#        -device e1000,netdev=n0,mac=$MAC
#      并去掉 -kernel (改用真实 PXE ROM): BIOS 用默认, UEFI 加 -bios OVMF.fd,
#      ARM64 用 qemu-system-aarch64 + -bios OVMF_AA64.fd（需 EDK2 OVMF 固件）。
#   3) 不同 machine 类型验证 Option93: BIOS(pc) / UEFI(pc + OVMF) / ARM64(virt + OVMF_AA64)。
# ---------------------------------------------------------------------------
