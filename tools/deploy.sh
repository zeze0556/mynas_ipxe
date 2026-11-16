#!/usr/bin/env bash
# deploy.sh - 用 rsync 把 build-host 上构建的 ipxe 产物部署到 deploy-target
#
# 设计红线（改动前请先读）:
#   1. 绝不使用 rsync --delete；只同步白名单文件，绝不触碰 data/ 与 config.yaml。
#   2. 二进制/UI 先落到 .new 并校验 sha256，再 mv 原子替换（避免覆盖正在执行的 inode 触发 ETXTBSY）。
#   3. 切换前必然生成回滚目录（含 rollback.sh / SHA256SUMS / DEPLOY_INFO.txt）。
#   4. 失败时只打印回滚指引，不自动回滚 —— 半部署状态下隐式动作可能二次破坏，由人决策。
#   5. 验证阶段只发只读请求与「必然失败」的请求，不写生产 DB。
#
# 用法:
#   bash tools/deploy.sh --dry-run                 # 只打印计划，不连接、不修改任何东西
#   bash tools/deploy.sh --check                  # 检查本机产物 + 远端环境 + SSH 通道
#   bash tools/deploy.sh                         # 交互确认后完整部署
#   bash tools/deploy.sh --yes                   # 跳过确认（CI / 二次执行）
#   bash tools/deploy.sh --frontend-only         # 只更新 index.html（运行时读盘，不重启）
#   bash tools/deploy.sh --build                 # 部署前用 Docker 重编二进制
#   bash tools/deploy.sh --rollback /srv/ipxe_rollback_YYYYmmddHHMMSS
#
# 前置条件（SSH 认证，脚本不保存任何密码）:
#   ssh-keygen -t ed25519 -f ~/.ssh/ipxe_deploy -N ''
#   ssh-copy-id -i ~/.ssh/ipxe_deploy.pub root@<server-ip>
#   SSH_CMD='ssh -i ~/.ssh/ipxe_deploy -o BatchMode=yes' bash tools/deploy.sh --check
#   若只能用密码: SSH_CMD="sshpass -p '\$PW' ssh -o StrictHostKeyChecking=accept-new" bash tools/deploy.sh
#
# 可覆盖的环境变量:
#   SRC_DIR      源码/产物根目录，默认脚本所在仓库根
#   DEPLOY_HOST  默认 192.168.1.10      DEPLOY_USER 默认 root
#   DEPLOY_DIR   默认 /srv/ipxe   HTTP_PORT  默认 10180
#   SERVICE_CFG  默认 config.yaml        SSH_CMD    默认 BatchMode 的 ssh
#   BUILD_IMAGE  默认 ipxe-buildenv:local
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
SRC_DIR=${SRC_DIR:-$ROOT}
DEPLOY_HOST=${DEPLOY_HOST:-192.168.1.10}
DEPLOY_USER=${DEPLOY_USER:-root}
DEPLOY_DIR=${DEPLOY_DIR:-/srv/ipxe}
HTTP_PORT=${HTTP_PORT:-10180}
SERVICE_CFG=${SERVICE_CFG:-config.yaml}
BUILD_IMAGE=${BUILD_IMAGE:-ipxe-buildenv:local}
SSH_CMD=${SSH_CMD:-"ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=5"}

BIN_LOCAL="$SRC_DIR/ipxe-server.linux"
UI_LOCAL="$SRC_DIR/assets/web/index.html"
TS=$(date +%Y%m%d%H%M%S)
BK_DIR="${DEPLOY_DIR}_rollback_$TS"
GIT_REV=$(git -C "$SRC_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)

DRY_RUN=0; DO_CHECK=0; FRONTEND_ONLY=0; DO_BUILD=0; ASSUME_YES=0; ROLLBACK_DIR=""
BIN_SHA=""; UI_SHA=""; ROLLBACK_B64=""   # 由 local_preflight / build_rollback_script 填充；回滚模式无需产物
SWITCH_BIN=1                              # frontend-only 时置 0，远端据此跳过二进制切换与后端探活

die()  { echo "ERROR: $*" >&2; exit 1; }
warn() { echo "WARN:  $*" >&2; }
log()  { echo "==> $*"; }
usage(){ sed -n '2,/^set -Eeuo pipefail/p' "${BASH_SOURCE[0]}" | grep '^#' | sed 's/^# \{0,1\}//'; }

while (( $# )); do
  case "$1" in
    --dry-run)       DRY_RUN=1 ;;
    --check)         DO_CHECK=1 ;;
    --frontend-only) FRONTEND_ONLY=1 ;;
    --build)         DO_BUILD=1 ;;
    --yes|-y)        ASSUME_YES=1 ;;
    --rollback)      shift; ROLLBACK_DIR=${1:-}; [[ -n $ROLLBACK_DIR ]] || die "--rollback 需要目录参数" ;;
    -h|--help)       usage; exit 0 ;;
    *) die "未知参数: $1（用 --help 查看用法）" ;;
  esac
  shift
done

(( FRONTEND_ONLY )) && SWITCH_BIN=0

read -r -a SSH_ARR <<< "$SSH_CMD"
remote() { "${SSH_ARR[@]}" "$DEPLOY_USER@$DEPLOY_HOST" "$@"; }

# 注入到远端脚本的变量。所有值都校验为安全字符集，防止命令注入。
remote_vars() {
  local k v
  for kv in "DEPLOY_DIR=$DEPLOY_DIR" "HTTP_PORT=$HTTP_PORT" "SERVICE_CFG=$SERVICE_CFG" \
            "BIN_SHA=$BIN_SHA" "UI_SHA=$UI_SHA" "BK_DIR=$BK_DIR" "GIT_REV=$GIT_REV" \
            "TS=$TS" "ROLLBACK_DIR=$ROLLBACK_DIR" "ROLLBACK_B64=$ROLLBACK_B64" \
            "DEPLOY_HOST=$DEPLOY_HOST" "SWITCH_BIN=$SWITCH_BIN"; do
    k=${kv%%=*}; v=${kv#*=}
    [[ $v =~ ^[A-Za-z0-9_./:=@+-]*$ ]] || die "变量 $k 含不安全字符，拒绝注入远端: $v"
    printf "%s='%s'\n" "$k" "$v"
  done
}

# rx: 从 stdin 读一段远端脚本，拼上变量声明后通过 ssh bash -s 执行。
# dry-run 时只打印，不建立任何连接。
rx() {
  local body full
  body="$(cat)"
  full="$(remote_vars)"$'\n'"$body"
  if (( DRY_RUN )); then
    printf '\n----- REMOTE SCRIPT -----\n%s\n' "$full"
    return 0
  fi
  remote 'bash -s' <<< "$full"
}

rsync_one() {
  local src=$1 dst=$2
  local cmd=(rsync -a --partial --checksum --info=progress2 -e "$SSH_CMD" \
             "$src" "$DEPLOY_USER@$DEPLOY_HOST:$dst")
  if (( DRY_RUN )); then
    printf '\n----- RSYNC -----\n'; printf '%q ' "${cmd[@]}"; printf '\n'
    return 0
  fi
  "${cmd[@]}"
}

local_preflight() {
  log "本机前置检查"
  command -v rsync >/dev/null || die "本机没有 rsync"
  [[ -f $BIN_LOCAL && -x $BIN_LOCAL ]] || die "缺少可执行产物: $BIN_LOCAL（先跑 build_all.sh 或加 --build）"
  [[ -f $UI_LOCAL ]] || die "缺少前端文件: $UI_LOCAL"
  local newest bin_mt
  newest=$(find "$SRC_DIR" -type f \( -name '*.go' -o -name 'index.html' \) \
           -printf '%T@\n' 2>/dev/null | sort -n | tail -1)
  bin_mt=$(stat -c %Y "$BIN_LOCAL")
  if [[ -n $newest ]] && (( bin_mt + 0 < ${newest%.*} + 0 )); then
    warn "二进制比源码旧，建议先构建（--build 或 build_all.sh）"
  fi
  local dirty
  dirty=$(git -C "$SRC_DIR" status --porcelain 2>/dev/null || true)
  if [[ -n $dirty ]]; then
    warn "工作区有未提交/未跟踪文件（部署产物以本地文件为准）: $(printf '%s' "$dirty" | tr '\n' ' ')"
  fi
  BIN_SHA=$(sha256sum "$BIN_LOCAL" | cut -d' ' -f1)
  UI_SHA=$(sha256sum "$UI_LOCAL" | cut -d' ' -f1)
  echo "  commit     : $GIT_REV"
  echo "  binary     : $(stat -c '%s bytes  %y' "$BIN_LOCAL")"
  echo "  binary sha : $BIN_SHA"
  echo "  ui sha     : $UI_SHA"
}

ssh_preflight() {
  log "检查 SSH 通道: $DEPLOY_USER@$DEPLOY_HOST"
  if ! remote 'echo SSH_OK' >/tmp/ipxe_ssh_probe.$$ 2>/tmp/ipxe_ssh_err.$$; then
    echo "----- ssh 报错 -----" >&2; cat /tmp/ipxe_ssh_err.$$ >&2
    rm -f /tmp/ipxe_ssh_probe.$$ /tmp/ipxe_ssh_err.$$
    die "SSH 不通。请先配置免密（见 --help 的「前置条件」），SSH_CMD 当前为: $SSH_CMD"
  fi
  grep -q SSH_OK /tmp/ipxe_ssh_probe.$$ || die "SSH 返回异常"
  rm -f /tmp/ipxe_ssh_probe.$$ /tmp/ipxe_ssh_err.$$
  echo "  SSH_OK"
}

# 回滚脚本内容：在目标机可独立执行，不依赖本脚本
build_rollback_script() {
  local t
  t=$(cat <<'EOS'
#!/usr/bin/env bash
# 由 deploy.sh 生成，独立可执行
set -Eeuo pipefail
BK="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIR="__DEPLOY_DIR__"
PORT="__HTTP_PORT__"
CFG="__SERVICE_CFG__"
echo "回滚自 $BK"
cp -a "$BK/ipxe-server" "$DIR/ipxe-server"
cp -a "$BK/index.html"  "$DIR/assets/web/index.html"
pids=$(pgrep -f '^\./ipxe-server ' || true)
[ -n "$pids" ] && kill $pids || true
for i in $(seq 1 20); do
  [ -z "$(pgrep -f '^\./ipxe-server ' || true)" ] && break
  sleep 0.5
done
cd "$DIR"
setsid bash -c "exec ./ipxe-server -config $CFG >>/tmp/ipxe_srv.log 2>&1" </dev/null >/dev/null 2>&1 &
sleep 3
curl -s -o /dev/null -w "GET / -> HTTP %{http_code}\n" "http://127.0.0.1:$PORT/"
echo "回滚完成，请执行 SHA256SUMS 校验: (cd $BK && sha256sum -c SHA256SUMS)"
EOS
)
  t=${t//__DEPLOY_DIR__/$DEPLOY_DIR}
  t=${t//__HTTP_PORT__/$HTTP_PORT}
  t=${t//__SERVICE_CFG__/$SERVICE_CFG}
  ROLLBACK_B64=$(printf '%s' "$t" | base64 -w0)
}

print_plan() {
  cat <<EOF

  源   : $SRC_DIR  (commit $GIT_REV)
  目标 : $DEPLOY_USER@$DEPLOY_HOST:$DEPLOY_DIR
  回滚 : $BK_DIR
  模式 : $( (( DRY_RUN )) && echo DRY-RUN || echo REAL )$( (( FRONTEND_ONLY )) && echo ' / frontend-only' || true )
  文件 : assets/web/index.html -> assets/web/index.html.new
         $( (( FRONTEND_ONLY )) && echo '(二进制本次不动)' || echo 'ipxe-server.linux -> ipxe-server.new' )
  SSH  : $SSH_CMD

EOF
}

preflight_remote() {
  log "远端前置检查"
  rx <<'EOS'
set -euo pipefail
command -v rsync >/dev/null || { echo "ERROR: 目标机没有 rsync"; exit 1; }
[ -d "$DEPLOY_DIR" ]                       || { echo "ERROR: $DEPLOY_DIR 不存在"; exit 1; }
[ -f "$DEPLOY_DIR/ipxe-server" ]            || { echo "ERROR: 缺少现网 ipxe-server"; exit 1; }
[ -f "$DEPLOY_DIR/assets/web/index.html" ]  || { echo "ERROR: 缺少现网 index.html"; exit 1; }
[ -f "$DEPLOY_DIR/$SERVICE_CFG" ]           || { echo "ERROR: 缺少 $SERVICE_CFG"; exit 1; }
echo "  rsync      : $(rsync --version | head -1)"
echo "  现网 bin   : $(sha256sum "$DEPLOY_DIR/ipxe-server" | cut -d' ' -f1)"
echo "  现网 ui    : $(sha256sum "$DEPLOY_DIR/assets/web/index.html" | cut -d' ' -f1)"
echo "  运行进程   : $(pgrep -af '^\./ipxe-server ' || echo '(未运行)')"
echo "  磁盘       : $(df -h "$DEPLOY_DIR" | tail -1 | awk '{print $4" 可用"}')"
EOS
}

make_rollback() {
  log "生成回滚点 $BK_DIR"
  rx <<'EOS'
set -euo pipefail
[ ! -e "$BK_DIR" ] || { echo "ERROR: 回滚目录已存在 $BK_DIR"; exit 1; }
mkdir -p "$BK_DIR"
cp -a "$DEPLOY_DIR/ipxe-server"           "$BK_DIR/ipxe-server"
cp -a "$DEPLOY_DIR/assets/web/index.html" "$BK_DIR/index.html"
cp -a "$DEPLOY_DIR/$SERVICE_CFG"          "$BK_DIR/$SERVICE_CFG"
( cd "$BK_DIR" && sha256sum ipxe-server index.html "$SERVICE_CFG" > SHA256SUMS )
{
  echo "deployed_at   : $(date -Is)"
  echo "git_rev       : $GIT_REV"
  echo "binary_sha256 : $BIN_SHA"
  echo "ui_sha256     : $UI_SHA"
  echo "source_host   : $(hostname)"
  echo "target        : $DEPLOY_HOST:$DEPLOY_DIR"
} > "$BK_DIR/DEPLOY_INFO.txt"
printf '%s' "$ROLLBACK_B64" | base64 -d > "$BK_DIR/rollback.sh"
chmod +x "$BK_DIR/rollback.sh"
echo "  rollback.sh / SHA256SUMS / DEPLOY_INFO.txt 已生成"
cat "$BK_DIR/DEPLOY_INFO.txt" | sed 's/^/  /'
EOS
}

switch_files() {
  log "校验并原子替换"
  rx <<'EOS'
set -euo pipefail
cd "$DEPLOY_DIR"
echo "$UI_SHA  assets/web/index.html.new" | sha256sum -c - || { echo "ERROR: UI 校验和不符"; exit 1; }
if [ "$SWITCH_BIN" = 1 ]; then
  [ -f ipxe-server.new ] || { echo "ERROR: 缺少 ipxe-server.new（rsync 未成功?）"; exit 1; }
  echo "$BIN_SHA  ipxe-server.new" | sha256sum -c - || { echo "ERROR: 二进制校验和不符"; exit 1; }
  chmod 755 ipxe-server.new
fi
chmod 644 assets/web/index.html.new
mv -f assets/web/index.html.new assets/web/index.html
if [ "$SWITCH_BIN" = 1 ]; then mv -f ipxe-server.new ipxe-server; fi
echo "  ui    : $(sha256sum assets/web/index.html | cut -d' ' -f1)"
echo "  binary: $(sha256sum ipxe-server | cut -d' ' -f1)"
EOS
}

restart_service() {
  log "重启 ipxe-server"
  rx <<'EOS'
set -euo pipefail
cd "$DEPLOY_DIR"
pids=$(pgrep -f '^\./ipxe-server ' || true)
if [ -n "$pids" ]; then
  echo "  停止旧进程: $pids"
  kill $pids || true
  for i in $(seq 1 20); do
    [ -z "$(pgrep -f '^\./ipxe-server ' || true)" ] && break
    sleep 0.5
  done
  left=$(pgrep -f '^\./ipxe-server ' || true)
  [ -z "$left" ] || { echo "  强制结束: $left"; kill -9 $left || true; sleep 1; }
fi
setsid bash -c "exec ./ipxe-server -config $SERVICE_CFG >>/tmp/ipxe_srv.log 2>&1" </dev/null >/dev/null 2>&1 &
ok=0
for i in $(seq 1 30); do
  if curl -s -o /dev/null "http://127.0.0.1:$HTTP_PORT/"; then ok=1; break; fi
  sleep 0.5
done
[ "$ok" = 1 ] || { echo "ERROR: 启动后 $HTTP_PORT 未就绪，日志尾部:"; tail -20 /tmp/ipxe_srv.log; exit 1; }
echo "  新进程: $(pgrep -af '^\./ipxe-server ' || echo '(未找到)')"
curl -s -o /dev/null -w "  GET / -> HTTP %{http_code}\n" "http://127.0.0.1:$HTTP_PORT/"
EOS
}

verify() {
  log "只读回归验证（不写生产 DB）"
  rx <<'EOS'
set -uo pipefail
base="http://127.0.0.1:$HTTP_PORT"
fail=0
code=$(curl -s -o /dev/null -w '%{http_code}' "$base/" || true)
echo "  GET /                     -> $code (期望 200)"; [ "$code" = 200 ] || fail=1
code=$(curl -s -o /dev/null -w '%{http_code}' "$base/api/users" || true)
echo "  GET /api/users            -> $code (期望 200)"; [ "$code" = 200 ] || fail=1
if curl -s "$base/" | grep -q 'importISOPath'; then
  echo "  新 UI 标记 importISOPath  -> OK"
else
  echo "  新 UI 标记 importISOPath  -> MISSING"; fail=1
fi
if [ "$SWITCH_BIN" = 1 ]; then
  # id=0 不存在：PUT 返回 404 且零行更新，用于证明新路由已生效且无副作用
  code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: application/json' \
         -d '{"name":"probe","scope":"public","kind":"linux"}' "$base/api/templates?id=0" || true)
  echo "  PUT /api/templates?id=0   -> $code (期望 404)"; [ "$code" = 404 ] || fail=1
  # 相对路径必被拒：证明路径导入已生效且不落盘
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
         -d '{"path":"relative/bad.iso"}' "$base/api/isos/import" || true)
  echo "  POST /api/isos/import bad -> $code (期望 400)"; [ "$code" = 400 ] || fail=1
else
  echo "  (frontend-only: 后端未换，跳过 PUT / 路径导入探活)"
fi
if ss -lunp 2>/dev/null | grep -q ':69 '; then
  echo "  TFTP :69                  -> LISTEN"
else
  echo "  TFTP :69                  -> MISSING"; fail=1
fi
echo "  ---- /boot 抽样（只读）----"
curl -s "$base/boot?mac=00:11:22:33:44:55" | head -5 | sed 's/^/    /'
echo "  RESULT: $([ "$fail" -eq 0 ] && echo PASS || echo FAIL)"
exit "$fail"
EOS
}

do_rollback() {
  log "执行回滚: $ROLLBACK_DIR"
  [[ $ROLLBACK_DIR =~ ^[A-Za-z0-9_./+-]+$ ]] || die "回滚目录路径含非法字符"
  rx <<'EOS'
set -euo pipefail
[ -x "$ROLLBACK_DIR/rollback.sh" ] || { echo "ERROR: $ROLLBACK_DIR/rollback.sh 不存在或不可执行"; exit 1; }
"$ROLLBACK_DIR/rollback.sh"
EOS
}

on_err() {
  echo >&2
  echo "部署失败（line $1）。回滚点: $BK_DIR" >&2
  echo "  目标机直接回滚 : $BK_DIR/rollback.sh" >&2
  echo "  或本机执行     : bash tools/deploy.sh --rollback $BK_DIR" >&2
  exit 1
}
trap 'on_err $LINENO' ERR

# ---------------- main ----------------
# 回滚不需要本地构建产物，提前处理，避免被产物前置检查挡住
if [[ -n $ROLLBACK_DIR ]] && (( ! DRY_RUN )); then
  ssh_preflight
  do_rollback
  exit 0
fi

if (( DO_BUILD )) && (( ! DRY_RUN )); then
  log "Docker 重编二进制"
  docker run --rm -v "$SRC_DIR":/work -w /work -e CGO_ENABLED=0 -e GOOS=linux \
    "$BUILD_IMAGE" go build -trimpath -buildvcs=false -ldflags="-s -w" \
    -o /work/ipxe-server.linux ./cmd/server
fi

local_preflight
build_rollback_script
print_plan

if (( DRY_RUN )); then
  log "DRY-RUN：以下为将执行的远端脚本与 rsync 命令（不连接、不修改）"
  preflight_remote
  make_rollback
  rsync_one "$UI_LOCAL" "$DEPLOY_DIR/assets/web/index.html.new"
  (( FRONTEND_ONLY )) || rsync_one "$BIN_LOCAL" "$DEPLOY_DIR/ipxe-server.new"
  switch_files
  (( FRONTEND_ONLY )) || restart_service
  verify
  log "DRY-RUN 结束，未做任何修改"
  exit 0
fi

if (( DO_CHECK )); then
  ssh_preflight
  preflight_remote
  log "CHECK 完成，未做任何修改"
  exit 0
fi

if (( ! ASSUME_YES )); then
  read -r -p "确认部署到 $DEPLOY_USER@$DEPLOY_HOST:$DEPLOY_DIR ? [y/N] " ans
  [[ $ans == y || $ans == Y ]] || die "已取消"
fi

ssh_preflight
preflight_remote
make_rollback
rsync_one "$UI_LOCAL" "$DEPLOY_DIR/assets/web/index.html.new"
(( FRONTEND_ONLY )) || rsync_one "$BIN_LOCAL" "$DEPLOY_DIR/ipxe-server.new"
switch_files
(( FRONTEND_ONLY )) || restart_service
verify
log "部署完成。回滚点: $BK_DIR"
