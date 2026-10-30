#!/bin/bash
# 小智服务端：源码编译 → 部署到阿里云新加坡服务器
# 用法：bash build-and-deploy.sh [--build-only|--deploy-only]
#   默认两段都做
# 前提：~/.ssh/config 里有 aliyun 别名（见 AGENTS.md 3.1），本机 Docker 守护进程在跑
set -uo pipefail

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"      # 仓库根（本脚本就放在仓库根）
OUT_DIR="$SRC_DIR/out"
PKG_DIR="$OUT_DIR/linux-amd64/xiaozhi_server-linux-amd64"
REMOTE="aliyun"
REMOTE_DIR="/opt/xiaozhi-server-golang"

MODE="${1:-all}"

info() { echo ""; echo "==== $* ===="; }
fail() { echo "!! $*" >&2; exit 1; }

# ---------------- 构建 ----------------
if [ "$MODE" = "all" ] || [ "$MODE" = "--build-only" ]; then
    info "0. 检查环境"
    docker info >/dev/null 2>&1 || fail "Docker 守护进程没起来，请先启动 Docker Desktop"
    echo "  Docker: $(docker --version)"
    test -f "$SRC_DIR/asr_server/server/setup.go" || fail "asr_server 子模块缺失，先跑 git submodule update --init --recursive"

    info "1. 构建镜像（首次约 15-25 分钟，之后走缓存快很多）"
    cd "$SRC_DIR" || fail "进不去源码目录"
    docker build --target builder \
        --build-arg UBUNTU_VERSION=18.04 \
        --build-arg NODE_VERSION=20 \
        -f docker/Dockerfile.linux \
        -t xiaozhi-build:local . || fail "docker build 失败，看上面报错"

    info "2. 从镜像里提取产物"
    rm -rf "$OUT_DIR"
    docker rm -f xz-extract >/dev/null 2>&1 || true
    docker create --name xz-extract xiaozhi-build:local >/dev/null || fail "docker create 失败"
    docker cp xz-extract:/out "$OUT_DIR" >/dev/null || fail "docker cp 失败"
    docker rm xz-extract >/dev/null 2>&1 || true

    test -x "$PKG_DIR/xiaozhi_server" || fail "产物里没有 xiaozhi_server"
    echo "  产物目录: $PKG_DIR"
    ls -lh "$PKG_DIR/xiaozhi_server" | awk '{print "  二进制大小: "$5}'
    file "$PKG_DIR/xiaozhi_server"
fi

if [ "$MODE" = "--build-only" ]; then
    echo ""
    echo "构建完成。产物在: $PKG_DIR"
    exit 0
fi

# ---------------- 部署 ----------------
info "3. 推送到服务器"
test -f "$PKG_DIR/xiaozhi_server" || fail "找不到本地产物，先跑构建"
scp "$PKG_DIR/xiaozhi_server" "$REMOTE:/tmp/xiaozhi_server.new" >/dev/null || fail "scp 失败"
echo "  已上传"

info "4. 替换并重启"
ssh "$REMOTE" "set -e
cd $REMOTE_DIR
# 版本留档，出问题能一键回滚
STAMP=\$(date +%Y%m%d-%H%M%S)
cp xiaozhi_server xiaozhi_server.bak.\$STAMP
chmod +x /tmp/xiaozhi_server.new
mv /tmp/xiaozhi_server.new xiaozhi_server
# 只保留最近 3 个备份
ls -1t xiaozhi_server.bak.* 2>/dev/null | tail -n +4 | xargs -r rm -f
systemctl restart xiaozhi-server-golang
sleep 8
systemctl is-active xiaozhi-server-golang
"

info "5. 验收"
ssh "$REMOTE" '
code_root=$(curl -s -o /dev/null -w "%{http_code}" --resolve wx.originagent.cn:443:127.0.0.1 https://wx.originagent.cn/)
code_ota=$(curl -s -o /dev/null -w "%{http_code}" -X POST --resolve wx.originagent.cn:443:127.0.0.1 \
    https://wx.originagent.cn/xiaozhi/ota/ -H "Content-Type: application/json" \
    -H "Device-Id: ba:8f:17:aa:bb:cc" -H "Client-Id: 1a2b3c4d-0000-1111-2222-333344445555" \
    -d "{\"application\":{\"version\":\"1.0.0\",\"board\":\"test\"},\"flash_size\":33554432}")
# 必须带设备头：不带 Device-Id / Client-Id / Protocol-Version 时服务端返回 400 而不是 101
ws=$(curl -s -o /dev/null -w "%{http_code}" --max-time 6 --resolve wx.originagent.cn:443:127.0.0.1 \
    -H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Version: 13" \
    -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
    -H "Device-Id: ba:8f:17:aa:bb:cc" -H "Client-Id: 1a2b3c4d-0000-1111-2222-333344445555" \
    -H "Protocol-Version: 1" -H "Authorization: Bearer x" \
    https://wx.originagent.cn/xiaozhi/v1/)
echo "  首页      -> $code_root"
echo "  OTA       -> $code_ota"
echo "  WebSocket -> $ws   (101 表示正常)"
systemctl show xiaozhi-server-golang -p MemoryCurrent --value | awk "{printf \"  服务内存  -> %.1f MB\n\", \$1/1048576}"
'

echo ""
echo "部署完成。回滚命令："
echo "  ssh $REMOTE 'cd $REMOTE_DIR && cp xiaozhi_server.bak.<时间戳> xiaozhi_server && systemctl restart xiaozhi-server-golang'"
