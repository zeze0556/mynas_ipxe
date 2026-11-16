# 示例：用 WIM / PE 启动 WinPE 并网络安装 Windows

本示例演示本服务器（类 iVentoy）的 WIM 启动能力如何拉起 **WinPE**，
进而完成 Windows 的网络安装。目标读者：已读 README 并跑通基本 ISO 启动。

---

## 1. 原理（wimboot 启动链）

WIM 是 Windows 映像归档，**不能**用 `sanboot`。正确做法是走 `wimboot`
（iPXE 提供的 Windows 引导加载器），把 PE 支撑文件 + 你的 WIM 作为
`boot.wim` 注入内存盘，由 Windows 引导管理器拉起 WinPE：

```
客户端 PXE → /boot?mac=..&kind=wim
  → 渲染 public-wim 脚本：
      kernel  /pe/wimboot
      initrd  /pe/bootmgr.exe      bootmgr.exe
      initrd  /pe/boot/bcd         bcd
      initrd  /pe/boot/boot.sdi    boot.sdi
      initrd  /r/<mac>/<wim名>     boot.wim     # 你上传的 WIM 作 boot.wim
      boot
  → wimboot 合成 RAMDisk → 引导 WinPE
```

`public-wim` 种子模板已随库自带（`db.go` 中 `SeedPublicTemplates`）。
其中 `/pe/...` 来自 `pe_support_dir`（默认 `data/pe/`），`/r/...` 是上传
的 WIM 原生流。

---

## 2. 前置准备

### 2.1 补齐 PE 支撑文件

把以下 4 个文件放入 `data/pe/`（详见 `data/pe/README.md`）：

- `wimboot`
- `bootmgr.exe`
- `boot/bcd`
- `boot/boot.sdi`

### 2.2 准备一个有效的 WIM（作 boot.wim）

| 用途 | 推荐 WIM | 来源 |
|------|----------|------|
| 仅启动 WinPE | 任意 WinPE 的 `boot.wim` | Windows ADK `copype` 生成，或 Windows ISO 内 `sources/boot.wim` |
| 自动安装 | 同上 + 内置 `startnet.cmd` | 用 `dism /mount-wim` 注入 `Windows/System32/startnet.cmd` |

> ⚠️ 必须是 **boot.wim**（PE 映像），不是 `install.wim`（完整的 OS 映像，
> 不能直接作为 boot.wim 启动）。

---

## 3. 操作步骤（curl 示例）

假设服务运行在 `localhost:8080`，客户端 MAC 为 `aa:bb:cc:dd:ee:ff`。

```bash
# 1) 导入 WIM（不解包，直接落盘，格式自动识别为 wim）
curl -X POST localhost:8080/api/isos/import -F "iso=@boot.wim"

# 2) 绑定 MAC → 主机（若尚未绑定）
curl -X POST localhost:8080/api/hosts \
  -d '{"mac":"aa:bb:cc:dd:ee:ff","name":"pc1","user_id":1}'

# 3) 用自带的 public-wim 模板即可（已 seed），无需新建；
#    若需自定义（例如换 WIM / 加 autounattend），建一个 wim 模板并绑 iso_id：
curl -X POST localhost:8080/api/templates -d '{
  "name":"my-pe","scope":"host","owner_id":1,
  "kind":"wim","iso_id":1,"body":"#!ipxe\necho Booting PE {{.ISOName}}\nkernel {{.PeURL}}wimboot\ninitrd {{.PeURL}}bootmgr.exe bootmgr.exe\ninitrd {{.PeURL}}boot/bcd bcd\ninitrd {{.PeURL}}boot/boot.sdi boot.sdi\ninitrd {{.RawURL}} boot.wim\nboot\n"
}'

# 4) 预览渲染结果，确认 /pe/ 与 /r/ 路径正确
curl "localhost:8080/api/boot-preview?mac=aa:bb:cc:dd:ee:ff&kind=wim"
```

客户端从该 MAC 启动时即拉起 WinPE。

---

## 4. 网络安装 Windows（两种路径）

### 路径 A：纯 PE + 手动安装（最简单）

1. PE 启动后，连上网络（DHCP 通常自动完成）。
2. 把 Windows 安装源挂到网络位置：
   - 方式一：把 Windows ISO 内容放到一台 SMB 共享，`net use Z: \\server\install`；
   - 方式二：用本服务的 `windows` 模板直接引导原版 ISO 的 `boot.wim`
     （免自建 WIM，但失去自定义 PE 的灵活性）。
3. 在 PE 里执行 `Z:\setup.exe` 走图形化安装。

参考 `examples/startnet.cmd` 做无人值守化（`wpeinit` + `net use` + `setup.exe`）。

### 路径 B：ISO 模板 + autounattend 自动安装（推荐量产）

利用 `windows` 模板：导入原版 Windows ISO（自动解包到 `cache/`），通过
注入 `autounattend.xml` 实现无人值守。该模板脚本会：

```
initrd ${base}/autounattend.xml autounattend.xml   # ${base}=/c/<mac>/<iso>
```

操作步骤：

```bash
# 导入 Windows ISO（kind=windows，自动解包）
curl -X POST localhost:8080/api/isos/import -F "iso=@Win11.iso"
# 把示例应答文件注入 ISO overlay 的 /autounattend.xml
curl -X POST localhost:8080/api/injections \
  -F "iso_id=2" -F "scope=host" -F "owner_id=1" \
  -F "image_path=/autounattend.xml" -F "file=@examples/autounattend.xml"
```

客户端从该 MAC 启动即自动分区、应用映像、完成安装。
示例应答文件见 `examples/autounattend.xml`（**请填入你的映像 index / 产品密钥**）。

---

## 5. 验证清单

- [ ] `GET /pe/wimboot` 能下载（200，非空）
- [ ] `GET /pe/bootmgr.exe`、`/pe/boot/bcd`、`/pe/boot/boot.sdi` 均 200
- [ ] `GET /r/<mac>/<wim名>` 能下载你的 WIM（200，大小匹配）
- [ ] `boot-preview?kind=wim` 输出的 `initrd ... boot.wim` 指向正确的 `/r/` URL
- [ ] 客户端启动后进入 WinPE（蓝底命令提示符 / 安装界面）

---

## 6. 常见问题排查

| 现象 | 原因 | 处理 |
|------|------|------|
| `Could not open \bootmgr.exe` | `data/pe/` 缺文件或路径错 | 按 [`docs/PE支撑文件清单.md`](PE支撑文件清单.md) 补齐 4 个文件 |
| 停在 `wimboot` 无后续 | 上传的不是有效 `boot.wim` | 换用 PE 的 boot.wim，勿用 install.wim |
| `/r/...` 404 | WIM 未导入或 iso_id 绑错 | 确认 `isos/import` 成功且模板 `iso_id` 正确 |
| PE 起来但无网络 | 缺网卡驱动 | 用 `dism /mount-wim` 把网卡驱动注入 WIM |
| autounattend 不生效 | 注入路径非 `/autounattend.xml` | 必须用 `image_path=/autounattend.xml` 注入到 ISO overlay |
