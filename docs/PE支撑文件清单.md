# PE 支撑文件清单（data/pe）

WIM 镜像经 `wim` 模板 / `wimboot` 启动时，需要以下支撑文件，由 iPXE 的
`wimboot` 加载。本服务器通过 `/pe/<path>` 对外提供这些文件
（目录路径由 `config.pe_support_dir` 配置，默认即 `data/pe/`）。

> 本说明位于 `docs/`（受版本控制）；部署时请在 `data/pe/` 下放置以下文件。
> `data/` 在 `.gitignore` 中被忽略（运行时数据），故这些二进制不随仓库提交。

## 必须放置的文件

| 文件 | 对外 URL | 说明 / 来源 |
|------|----------|------------|
| `wimboot` | `/pe/wimboot` | iPXE 官方的 `wimboot` 引导程序（非 Windows 文件） |
| `bootmgr.exe` | `/pe/bootmgr.exe` | 任意 Windows 安装 ISO 根目录的 `bootmgr.exe` |
| `boot/bcd` | `/pe/boot/bcd` | Windows ISO 的 `boot/bcd`（RAMDisk/PE 启动配置） |
| `boot/boot.sdi` | `/pe/boot/boot.sdi` | Windows ISO 的 `boot/boot.sdi`（RAMDisk 镜像） |

> `public-wim` 种子模板渲染出的脚本正是引用上述四个 `/pe/` 路径；
> 缺少任何一个，`wimboot` 都会失败（客户端停在 `Could not open ...`）。

## 获取方式

- **wimboot**：从 https://ipxe.org/wimboot 下载，或从本项目 `build_ipxe.sh`
  构建出的 iPXE 产物中取（部分发行版也直接提供 `wimboot` 二进制）。
- **bootmgr.exe / boot/bcd / boot.sdi**：从任意 Windows 安装 ISO（或
  Windows ADK 的 `copype`/`MakeWinPEMedia` 生成目录）拷贝 `boot/` 目录
  与根目录对应文件即可。

## 部署后目录结构示例

```
data/pe/
├── wimboot
├── bootmgr.exe
└── boot/
    ├── bcd
    └── boot.sdi
```

## 注意

- 若改用其它路径，请在 `config.yaml` 设置 `pe_support_dir: /your/path`，
  服务启动时会自动创建该目录。
- 完整 WIM / PE 启动与网络安装 Windows 的实操步骤见
  [`docs/网络安装Windows-WIM-PE.md`](网络安装Windows-WIM-PE.md)。
