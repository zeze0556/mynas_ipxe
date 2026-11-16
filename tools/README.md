# tools/ 运维与测试脚本

| 脚本 | 用途 | 执行位置 |
|---|---|---|
| `deploy.sh` | 把 build-host 上构建的产物部署到 deploy-target（rsync 白名单） | build-host |
| `qemu-pxe-test.sh` | 用 QEMU 测试 iPXE 启动链路（BIOS / UEFI，user / bridge 网络） | 任意 Linux 测试机 |
| `qemu-test.sh` | 早期简易 QEMU 测试脚本 | 任意 Linux 测试机 |
| `isoboot/isoboot.go` | ISO 引导辅助工具（Go 源码） | — |

项目整体状态与待办见 [`../docs/HANDOFF-2026-11-12.md`](../docs/HANDOFF-2026-11-12.md)。

## deploy.sh

设计红线（脚本头部有完整版，改动前先读）：

1. **绝不使用 `rsync --delete`**；只同步白名单文件，绝不触碰 `data/` 与 `config.yaml`。
2. 二进制 / UI 先落到 `.new` 并校验 sha256，再 `mv` 原子替换（避免覆盖正在执行的 inode 触发 ETXTBSY）。
3. 切换前必然生成回滚目录（含 `rollback.sh` / `SHA256SUMS` / `DEPLOY_INFO.txt`）。
4. 失败时只打印回滚指引，不自动回滚 —— 半部署状态下隐式动作可能二次破坏，由人决策。
5. 验证阶段只发只读请求与「必然失败」的请求，不写生产 DB。

```bash
bash tools/deploy.sh --dry-run                 # 只打印计划，不连接、不修改任何东西
bash tools/deploy.sh --check                  # 检查本机产物 + 远端环境 + SSH 通道
bash tools/deploy.sh                         # 交互确认后完整部署
bash tools/deploy.sh --yes                   # 跳过确认
bash tools/deploy.sh --frontend-only         # 只更新 index.html（运行时读盘，不重启）
bash tools/deploy.sh --build                 # 部署前用 Docker 重编二进制
bash tools/deploy.sh --rollback /srv/ipxe_rollback_YYYYmmddHHMMSS
```

SSH 认证由环境注入，脚本不保存任何密码：
```bash
ssh-keygen -t ed25519 -f ~/.ssh/ipxe_deploy -N ''
ssh-copy-id -i ~/.ssh/ipxe_deploy.pub root@<server-ip>
SSH_CMD='ssh -i ~/.ssh/ipxe_deploy -o BatchMode=yes' bash tools/deploy.sh --check
```

可覆盖环境变量：`SRC_DIR`、`DEPLOY_HOST`(默认 192.168.1.10)、`DEPLOY_USER`(root)、`DEPLOY_DIR`(/srv/ipxe)、`HTTP_PORT`(10180)、`SERVICE_CFG`、`SSH_CMD`。

### 部署顺序硬约束

**先换二进制 + 重启，再换 `index.html`。** UI 由 `http.ServeFile` 运行时读盘，单独换 UI 会立刻生效；若新 UI 调用了旧二进制不支持的 API（例如 `DELETE /api/isos`、`POST /api/isos/refresh`，旧版返回 405），按钮会「点了没反应」。白名单正好这两个文件，默认顺序正确。

## qemu-pxe-test.sh

固件由 `build_ipxe.sh` 编入 `chain http://${next-server}:10180/boot?mac=${mac}`，启动成败只取决于：① DHCP 分配地址；② siaddr + bootfile（option 67 按 Option 93 分流 BIOS/UEFI）；③ TFTP:69 与 HTTP:10180 可达。

- `user`（默认，零 root）：SLIRP 自带 DHCP+TFTP，宿主起纯用户态 TCP 反代 → 完全隔离，但不测生产 TFTP:69 与现网架构分流。
- `bridge`（`--bridge`，需 root）：TAP 挂宿主机网桥，VM 成为物理 LAN 一等公民，由现网 DHCP 完成 PXE。脚本只建/删自己的 TAP，**不起 dnsmasq、不改 iptables、不发 DHCP 响应**。硬约束：现网 DHCP 必须已配好 PXE。

```bash
sudo -n STATE_DIR=~/.local/state/ipxe-qemu BRIDGE=br0 ./qemu-pxe-test.sh probe --bridge
sudo -n STATE_DIR=~/.local/state/ipxe-qemu BRIDGE=br0 ./qemu-pxe-test.sh check
sudo -n STATE_DIR=~/.local/state/ipxe-qemu BRIDGE=br0 ./qemu-pxe-test.sh bios --bridge
sudo -n STATE_DIR=~/.local/state/ipxe-qemu BRIDGE=br0 ./qemu-pxe-test.sh uefi --bridge
```

桥接测试需按你的实际环境设置 `BRIDGE`/`STATE_DIR`（下方示例为占位值），并用你自己的测试 MAC。

### 踩坑清单

| # | 坑 | 正确做法 |
|---|---|---|
| 1 | `S=x sudo STATE_DIR=$S ...` 同行展开为空 | 写字面量 `STATE_DIR=~/.local/state/ipxe-qemu` |
| 2 | sudo 重置 `HOME=/root` | 必须显式传 `STATE_DIR=` |
| 3 | `timeout -k` 只杀 sudo 父进程，QEMU 成孤儿、TAP 不清理（RC=137） | 用脚本自带的 monitor / 手动清理 |
| 4 | `-display none -serial stdio` **抓不到** SeaBIOS/iPXE（走 VGA 0xB8000） | `QEMU_DISPLAY=curses QEMU_SERIAL=none ACCEL=tcg`（需真 TTY/tmux，加 `nomodeset`），或 VNC+SSH 转发 |
| 5 | monitor 已用 telnet，别叠 `-nographic` | telnet monitor 需先回 IAC 协商 + `\r\n` 才执行 `quit` |

脚本支持 `QEMU_DISPLAY` / `QEMU_SERIAL` / `ACCEL` 等环境变量覆盖。
