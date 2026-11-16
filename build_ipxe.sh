#!/usr/bin/env bash
# 构建带嵌入脚本的 iPXE 二进制（undionly.kpxe / ipxe.efi / ipxe-arm64.efi / ipxe.lkrn）
# 前置：git + gcc + make + mtools + syslinux（Linux/WSL 执行）
#
# ipxe.lkrn 说明：用于 QEMU `-kernel` 直接加载做方案 A 轻量测试（跳过真实 PXE ROM）。
#   因 -kernel 加载时没有前置 PXE ROM 做 DHCP，其嵌入脚本需显式 `dhcp` 后再 chain。
#   其余目标（undionly.kpxe / ipxe.efi 等）由 TFTP 加载，PXE ROM 已完成 DHCP，嵌入脚本不写 dhcp。
# 用法：bash build_ipxe.sh [HTTP_PORT] [WORKDIR]
#   HTTP_PORT：嵌入脚本里拼接的 boot URL 端口，必须与服务端 http_port 一致（默认 8080）
#   WORKDIR ：iPXE 源码与构建产物临时目录（默认 /tmp/ipxebuild，避免污染项目仓库）
#   IPXE_REPO：可覆盖克隆源，国内网络慢可设镜像，例如
#     IPXE_REPO=https://gitclone.com/github.com/ipxe/ipxe.git
set -e
PORT="${1:-8080}"
WORKDIR="${2:-/tmp/ipxebuild}"
IPXE_REPO="${IPXE_REPO:-https://github.com/ipxe/ipxe.git}"
mkdir -p "$WORKDIR"
OUT="$WORKDIR/assets/ipxe"   # 绝对路径，避免 cd 后相对路径失效
mkdir -p "$OUT"

cd "$WORKDIR"
if [ ! -d ipxe ]; then
  git clone --depth 1 "$IPXE_REPO" ipxe
fi
cd ipxe/src

# 显式确保 SAN boot / iSCSI 命令编入镜像（默认构建已含 sanhook/sanboot/iscsi）。
# 用 #ifndef 守卫，避免与 config/general.h 默认值冲突；此处仅为显式声明意图。
mkdir -p config/local
cat > config/local/general.h <<'EOF'
#ifndef SANBOOT_CMD
#define SANBOOT_CMD
#endif
#ifndef ISCSI_PROTOCOL
#define ISCSI_PROTOCOL
#endif
EOF

# 嵌入脚本：启动后向本服务请求按 MAC 渲染的启动脚本
# ${next-server} 由 DHCP siaddr 注入；${mac} 是 iPXE 固件变量，自动填充
cat > embed.ipxe <<EOF
#!ipxe
chain http://\${next-server}:${PORT}/boot?mac=\${mac}
EOF

make bin/undionly.kpxe        EMBED=embed.ipxe
make bin/ipxe.pxe             EMBED=embed.ipxe   # BIOS 完整 ROM 备选（与 undionly 同款嵌入）
make bin-x86_64-efi/ipxe.efi  EMBED=embed.ipxe

# ARM64 需要 aarch64 交叉编译器（可选；缺失时跳过）
if command -v aarch64-linux-gnu-gcc >/dev/null 2>&1; then
  make bin-arm64-efi/ipxe.efi EMBED=embed.ipxe CROSS_COMPILE=aarch64-linux-gnu- || true
fi

# ipxe.lkrn：用于 QEMU -kernel 方案 A 测试。-kernel 加载无前置 DHCP，需显式 dhcp 后再 chain。
cat > embed-lkrn.ipxe <<EOF
#!ipxe
dhcp
chain http://\${next-server}:${PORT}/boot?mac=\${mac}
EOF
make bin/ipxe.lkrn EMBED=embed-lkrn.ipxe

cp bin/undionly.kpxe                       "$OUT"/
cp bin/ipxe.pxe                           "$OUT"/
cp bin-x86_64-efi/ipxe.efi                 "$OUT"/
cp bin/ipxe.lkrn                           "$OUT"/
[ -f bin-arm64-efi/ipxe.efi ] && cp bin-arm64-efi/ipxe.efi "$OUT"/ || true
echo "iPXE 二进制已写入 $OUT"
ls -la "$OUT"
