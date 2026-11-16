# iPXE Server（类 iVentoy 的 Go 实现）

一个用 Go 编写的增强型 PXE/iPXE 网络启动服务器。内置 **TFTP + HTTP**，可选 **DHCP**，按客户端 **MAC 地址** 选择启动模板，支持 **ISO / IMG / VHD / WIM** 多格式启动与**文件动态注入替换**（类似 iVentoy），并提供一个 Web 管理控制台。

纯 Go 实现（SQLite 使用纯 Go 驱动，**无需 CGO**），单二进制即可运行。

## 功能特性

| 能力 | 说明 |
|------|------|
| TFTP + HTTP | 下发 iPXE 固件、启动脚本、镜像内容与注入文件 |
| 按 MAC 选模板 | `host → user → public` 三级优先级，`text/template` 渲染 iPXE 脚本 |
| 主机 ↔ MAC 绑定 | 一个用户可绑定多台主机，主机可带自定义 vars |
| 多格式镜像 | ISO（解包 overlay）、IMG 裸盘、VHD、WIM（wimboot） |
| 文件注入替换 | `host > user > global > base(ISO)` 多层 overlay，HTTP 逐路径合并，改 overlay 即时生效无需重解包 |
| ISO 幂等导入 | 按 checksum 去重、原子替换缓存，重新导入保持记录 ID 与模板绑定不变 |
| 交互启动菜单 | 两级分类菜单（系统 → 版本）、按主机灰度、菜单内显示 MAC/IP/GW、Reboot 项 |
| Web 控制台 | 用户 / 主机 / 模板 / 镜像 / 注入管理，启动脚本预览 |
| 可选 DHCP | 轻量 DHCP 池，按客户端 Option 93 架构分流 BIOS / UEFI / ARM64 固件 |

## 启动链路

```
客户端 PXE
  → DHCP(next-server=本服务, 按 Option 93 下发 undionly.kpxe / ipxe.efi / ipxe-arm64.efi)
  → TFTP 下载对应架构的 iPXE 二进制
      （内置脚本: chain http://${next-server}:<http_port>/boot?mac=${mac}）
  → HTTP /boot?mac=xx:xx  → 按 MAC 查 host → user → public 模板并渲染
  → 脚本从 /c/<mac>/<iso>/<path> 拉 kernel/initrd/boot.wim + 注入文件 → 启动
```

MAC 由 iPXE 在 URL 中上报（非网络嗅探），这是标准且最稳的做法。

## 快速开始

### 1. 构建

依赖 Go 1.23+；本项目使用纯 Go SQLite 驱动，无需 CGO/gcc。

```bash
export CGO_ENABLED=0
go build -o ipxe-server ./cmd/server
```

### 2. 构建 iPXE 固件（必做）

TFTP 需要真实的 iPXE 引导程序。`build_ipxe.sh` 会克隆 iPXE 源码并编译带内嵌脚本的二进制：

```bash
bash build_ipxe.sh 8080      # 参数为 HTTP 端口，需与 config.http_port 一致
```

生成的 `undionly.kpxe` / `ipxe.efi` / `ipxe.lkrn` 写入 `assets/ipxe/`。
（前置：git + gcc + make；Windows 可用 WSL / msys2。仓库已附带常用架构的固件，可直接使用。）

### 3. 运行

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml，至少设置 next_server 为客户端可访问的服务器 IP
./ipxe-server -config config.yaml
```

- Web 控制台：`http://<host>:<http_port>/`
- 启动端点：`http://<host>:<http_port>/boot?mac=<aa:bb:cc:dd:ee:ff>`

### 4. Docker Compose

```bash
cp config.example.yaml config.yaml   # 修改 next_server 为宿主机局域网 IP
docker compose up -d --build
```

> **必须 `network_mode: host`**：PXE 客户端通过局域网广播发现 TFTP/DHCP，iPXE 的 `next-server` 来自 DHCP 的 `siaddr`，客户端需与服务器在同一 L2 网段。Docker bridge 网络不通广播。`host` 网络仅 Linux Docker 引擎支持，Windows/macOS 的 Docker Desktop 不支持，请用裸二进制部署。
> 数据持久化：`./data` 需挂载卷，否则容器重建后 DB / ISO / 注入 / PE 文件全部丢失。

## 配置要点

完整示例见 [`config.example.yaml`](config.example.yaml)。

- `next_server`：客户端实际访问的服务器 IP（DHCP next-server 与 boot URL 共用）。
- `dhcp.enabled`：`true` 时需 root/管理员权限（监听 :67）；生产网络已有 DHCP 时应设 `false`，由现有 DHCP 下发 option 66/67。
- `dhcp.boot_file_bios` / `boot_file_efi` / `boot_file_arm64`：按 Option 93 分流的 NBP；`boot_file` 为兜底。
- `menu.enabled` / `menu.categories`：启动菜单开关与分类。未匹配主机在 `menu.enabled=true` 时进入菜单；已匹配主机按其 `boot_to_menu` 逐机开关决定。
- `deny_unknown_mac`：是否拒绝未登记的 MAC。
- `data_dir` / `db_path` / `assets_dir`：运行数据与资源目录。

## 使用流程（curl 示例）

```bash
# 1) 建用户
curl -X POST localhost:8080/api/users -d '{"name":"alice"}'
# 2) 绑定主机 MAC（一用户可绑多个）
curl -X POST localhost:8080/api/hosts -d '{"mac":"aa:bb:cc:dd:ee:ff","name":"pc1","user_id":1,"vars":"{\"hostname\":\"pc1\"}"}'
# 3) 导入 ISO（自动解包）
curl -X POST localhost:8080/api/isos/import -F "iso=@ubuntu.iso"
# 4) 绑定模板（公共/用户/主机级）
curl -X POST localhost:8080/api/templates -d '{"name":"t","scope":"host","owner_id":1,"kind":"linux","iso_id":1,"body":"#!ipxe\nkernel ${base}/casper/vmlinuz\ninitrd ${base}/casper/initrd\nboot\n"}'
# 5) 注入/替换文件（host 层覆盖 base）
curl -X POST localhost:8080/api/injections -F "iso_id=1" -F "scope=host" -F "owner_id=1" -F "image_path=/autounattend.xml" -F "file=@autounattend.xml"
# 6) 预览渲染后的启动脚本
curl "localhost:8080/api/boot-preview?mac=aa:bb:cc:dd:ee:ff&kind=linux"
```

## API 速查

| 方法 | 路径 | 说明 |
|------|------|------|
| GET/POST/DELETE | `/api/users` | 用户（token 可自动生成） |
| GET/POST/DELETE | `/api/hosts` | 主机↔MAC 绑定（`?id=`） |
| GET/POST/DELETE | `/api/templates` | 启动模板（`?id=`） |
| GET/DELETE | `/api/isos` | 镜像列表 / 删除（`?id=`；`&force=1` 解绑，`&purge=1` 连磁盘删） |
| POST | `/api/isos/import` | 上传或按路径导入；同名按 checksum 幂等更新（multipart `iso` 或 JSON `{"path":...}`） |
| POST | `/api/isos/refresh?id=` | 从 `data/isos/<filename>` 重新读取并解包，ID 与绑定不变 |
| GET/POST/DELETE | `/api/injections` | 注入文件（`?iso_id=` 过滤，`?id=` 删除） |
| GET | `/api/boot-preview` | 调试：渲染启动脚本 |
| GET | `/boot?mac=&kind=` | 客户端实际拉取的 iPXE 脚本 |
| GET | `/menu?mac=` | 客户端交互启动菜单 |
| GET | `/c/<mac>/<iso>/<path>` | 按 MAC 合并后的镜像文件 |
| GET | `/r/<mac>/<iso>` | 原生镜像流（IMG/VHD sanboot；WIM 经 wimboot） |
| GET | `/pe/<path>` | PE 支撑文件（wimboot/bootmgr.exe/bcd/boot.sdi） |

### 镜像导入语义

镜像记录身份是 `filename`（`data/isos/<filename>`），版本是 `checksum`：

- 同名再次导入 → 更新已有记录（ID 不变，模板绑定自动跟随），不新增行。
- 内容未变 → 跳过解包；内容变化 → 解包到临时目录后原子替换缓存，旧文件不残留、失败不破坏旧内容。
- 删除默认只删 DB 记录；被引用时返回 409，`force=1` 自动解绑，`purge=1` 才删磁盘与缓存。

## 镜像格式支持

| 格式 | 启动方式 | 说明 |
|------|----------|------|
| ISO | `linux`/`windows` 模板，解包后经 `/c/` overlay | 标准用法 |
| IMG（裸盘） | `raw` 模板 + `sanboot`（`/r/`） | 呈现为虚拟硬盘 |
| VHD | `raw` 模板 + `sanboot`；固定格式自动剥离 512B footer 转 raw | 动态 VHD/VHDX 原样落盘 |
| WIM | `wim` 模板经 `wimboot`（PE 支撑文件来自 `/pe/`） | 启动 WinPE / 网络安装 Windows |

> WIM 启动需在 `pe_support_dir`（默认 `data/pe/`）放置：`wimboot`、`bootmgr.exe`、`boot/bcd`、`boot/boot.sdi`。

## 目录结构

```
cmd/server/main.go         入口：装配 tftp / httpd / (可选)dhcp / db
config/config.go           YAML 配置 + 目录推导
internal/db                gorm 模型 + SQLite（纯 Go 驱动，无 CGO）+ 公共模板种子
internal/iso               ISO 解包 + overlay 解析器（host>user>global>base）
internal/tftp              TFTP 服务（RFC1350 + blocksize/timeout）
internal/dhcp              可选轻量 DHCP（IP 池 + next-server/bootfile 下发）
internal/httpd             boot 渲染 / 菜单 / overlay / 原生 ISO / REST API / Web UI
assets/ipxe                iPXE 固件（ipxe.efi / undionly.kpxe / ipxe.lkrn / ipxe.pxe）
assets/web                 Web 管理控制台
examples                   应答文件 / startnet.cmd / initramfs overlay 脚本示例
tools                      部署与 QEMU PXE 测试脚本（见 tools/README.md）
docs                       部署方案文档
```

## 文档与示例

- [`docs/网络安装Windows-WIM-PE.md`](docs/网络安装Windows-WIM-PE.md) —— wimboot 启动链、网络安装 Windows 流程与排查表。
- [`docs/PE支撑文件清单.md`](docs/PE支撑文件清单.md) —— `data/pe/` 下必须放置的文件及来源。
- [`docs/NFS-root-持久化部署.md`](docs/NFS-root-持久化部署.md) —— NFS ro 基础 root + 每客户端 overlay 的分层持久化部署。
- `examples/autounattend.xml` —— 无人值守安装应答文件示例。
- `examples/startnet.cmd` —— PE 启动后自动映射安装源并运行 setup 的示例。
- `examples/nfs-bottom-overlay` —— 按 MAC 挂每客户端 NFS overlay 作 upper 的 initramfs 脚本。
- [`tools/README.md`](tools/README.md) —— 部署脚本设计红线与 QEMU 测试脚本用法。

## 已知限制

- ISO9660 文件名按小写 + 去 `;N` 规整；Rock Ridge 长名未展开（多数 live ISO 引导路径不受影响）。
- 当前每个 (scope, owner) 单模板，按优先级取最高；多 ISO 菜单选择为后续扩展。
- 动态 VHD/VHDX 无法无损转 raw，原样落盘经 sanboot（必要时用 `qemu-img` 转 raw 后上传）。
- DHCP 当前为单池顺序分配，未做冲突探测/保留。
- QEMU 文本终端模式（`-nographic` / curses 从终端输入）下，方向键会被转成 ANSI 转义序列而在 iPXE 中显示为 A/B/C/D；菜单导航请在图形/VNC 前端操作，或使用 QEMU monitor 的 `sendkey up/down`。物理机不受影响。
