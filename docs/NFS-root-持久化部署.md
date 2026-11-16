# NFS root 持久化部署（ro 基础 root + 每客户端 overlay）

本项目的 `public-nfs` 模板实现「live + 分层持久化」：OS 的 kernel/initrd 仍从绑定 ISO 的
解包内容（`/c/<mac>/<iso>/casper/...`）取得，而 **root 走只读 NFS 导出**，客户端的
可写层由 initramfs 按 MAC 挂一块「每客户端 NFS overlay」提供。重启后写入保留，且各客户端隔离。

> 对比：HTTP sanboot 是 RAM 盘，写入重启即丢；iSCSI（`public-iscsi`）是块设备持久盘。
> NFS 这条路的「分层」由客户端 initramfs 完成，本服务只负责下发正确的内核参数。

## 一、服务端（NFS 服务器，独立于本程序）

```bash
# 1) 只读基础 root（一份 live 系统解包后的 rootfs）
/srv/nfs/live   192.168.1.0/24(ro,no_root_squash,async)

# 2) 每客户端 overlay 基目录（可写），需为每个 MAC 预建子目录
/srv/nfs/overlay 192.168.1.0/24(rw,no_root_squash,async)
mkdir -p /srv/nfs/overlay/aa-bb-cc-dd-ee-ff
chown -R nobody:nogroup /srv/nfs/overlay
```

## 二、客户端镜像（准备一次，所有客户端复用）

1. 将 `examples/nfs-bottom-overlay` 复制到
   `/etc/initramfs-tools/scripts/nfs-bottom/overlay-mac` 并 `chmod +x`。
2. 确保 initramfs 含 NFS 客户端模块（Ubuntu 用 `boot=nfs` 通常已带；否则
   `echo nfs >> /etc/initramfs-tools/modules`）。
3. `update-initramfs -u`。

该脚本会：读内核参数 `ipxe_overlaybase` → 取本机 MAC → 挂
`<ipxe_overlaybase>/<mac>/` 为 overlay upper → 把 ro NFS root 叠成可写 overlay。

## 三、本项目配置

1. 导入一个 live ISO（提供 `casper/vmlinuz`、`casper/initrd`）。
2. 创建/编辑模板（kind=`nfs`），字段填：
   - `nfs_root`：`192.168.1.10:/srv/nfs/live`（ro 基础 root）
   - `nfs_upper`：`192.168.1.10:/srv/nfs/overlay`（overlay 基）
   - 绑定上面的 ISO（提供 kernel/initrd）
3. 客户端 PXE 启动即走 NFS root + 每客户端持久层。

## 四、排查

| 现象 | 原因 | 处理 |
|------|------|------|
| 启动卡在 `mount: /dev/nfs` | NFS 导出路径/权限错 | 在客户端 `ip=dhcp` 后手动 `mount -t nfs` 验证导出 |
| root 只读、无写入 | initramfs 脚本未生效 / `ipxe_overlaybase` 未传 | 确认模板含 `ipxe_overlaybase={{.NFSUpper}}`；客户端 `cat /proc/cmdline` |
| overlay 失败回退 tmpfs | `<nfs_upper>/<mac>/` 目录不存在或无写权限 | 服务端预建子目录并给写权限 |
| 多客户端互相看到对方文件 | 误用 rw 共享 root（`root=/dev/nfs ... ,rw`） | 本方案用 ro root + 每客户端 overlay，勿改成 rw 共享 |

## 五、已知限制

- overlay 的 upper 用 NFS 承载，在部分 NFS 实现下对 `O_DIRECT`/锁语义有边缘问题；
  对普通 live 持久化足够，极端负载建议 upper 落到本地盘或 iSCSI LUN（`public-iscsi` 方案）。
- 需要为每个新 MAC 在服务端预建 `<nfs_upper>/<mac>/` 目录（可用脚本批建）。
