#!/bin/bash
# 从 GitHub Actions 下载最近一次构建产物并部署到服务器
# 用法：bash deploy-from-actions.sh [run_id]
#   不传 run_id 就用最近一次成功的运行
# 前提：~/.ssh/config 里有 aliyun 别名（见 AGENTS.md 3.1），gh 已登录
set -uo pipefail

REPO="aishangwuji/xiaozhi-esp32-server-golang"
ARTIFACT="xiaozhi_server-linux-amd64"
REMOTE="aliyun"
REMOTE_DIR="/opt/xiaozhi-server-golang"
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$HERE/ci-out"

info() { echo ""; echo "==== $* ===="; }
fail() { echo "!! $*" >&2; exit 1; }

RUN_ID="${1:-}"
if [ -z "$RUN_ID" ]; then
    RUN_ID=$(gh run list --repo "$REPO" --workflow build-linux.yml --status success --limit 1 \
        --json databaseId --jq '.[0].databaseId')
    [ -n "$RUN_ID" ] || fail "找不到成功的构建，先去 Actions 页面看一眼"
    echo "使用最近一次成功的构建: $RUN_ID"
fi

info "1. 下载产物"
# 本机到 GitHub 产物域名常常只有几十 KB/s（40MB 要半小时），而新加坡服务器 1~2 MB/s。
# 因此优先让服务器自己拉（token 走 stdin，不落 argv/history），失败再回退本机下载。
ART_ID=$(gh api "repos/$REPO/actions/runs/$RUN_ID/artifacts" \
    --jq ".artifacts[] | select(.name==\"$ARTIFACT\") | .id" 2>/dev/null | head -1)
ART_URL=""

FETCH_REMOTE_OK=0
if [ -n "$ART_ID" ]; then
    ART_URL="https://api.github.com/repos/$REPO/actions/artifacts/$ART_ID/zip"
    rm -rf "$WORK"; mkdir -p "$WORK"
    cat > "$WORK/.remote_fetch.sh" <<'EOS'
set -e
read -r TOKEN
rm -rf /tmp/ws_art_raw /tmp/ws_art.zip
mkdir -p /tmp/ws_art_raw
curl -sS -L --max-time 300 -H "Authorization: Bearer $TOKEN" -o /tmp/ws_art.zip "$ART_URL"
cd /tmp/ws_art_raw && unzip -oq /tmp/ws_art.zip
test -f xiaozhi_server
echo "  sha256: $(sha256sum xiaozhi_server | cut -d' ' -f1)"
EOS
    if scp -q "$WORK/.remote_fetch.sh" "$REMOTE:/tmp/ws_remote_fetch.sh" 2>/dev/null \
        && gh auth token 2>/dev/null | ssh "$REMOTE" "ART_URL='$ART_URL' bash /tmp/ws_remote_fetch.sh"; then
        FETCH_REMOTE_OK=1
        echo "  （产物已在服务器上拉取并解包：/tmp/ws_art_raw）"
    else
        echo "  服务器侧拉取失败，回退本机下载"
    fi
fi

BIN=""
if [ "$FETCH_REMOTE_OK" -eq 1 ]; then
    # 服务器侧已就位，直接复用，不再经过本机链路
    :
else
    rm -rf "$WORK"
    gh run download "$RUN_ID" --repo "$REPO" -n "$ARTIFACT" -D "$WORK" || fail "下载失败（产物保留期 14 天，可能已过期；也可手动在服务器上 curl 产物 zip）"
    find "$WORK" -maxdepth 2 -type f -printf '  %-45p %8s bytes\n' | head -12
    BIN="$WORK/xiaozhi_server"
    [ -f "$BIN" ] || fail "产物里没有 xiaozhi_server"
    echo "  sha256: $(sha256sum "$BIN" | cut -d' ' -f1)"
    info "2. 上传到服务器"
    scp "$BIN" "$REMOTE:/tmp/xiaozhi_server.new" >/dev/null || fail "scp 失败"
fi

info "3. 替换并重启（旧版自动备份）"
ssh "$REMOTE" "set -e
cd $REMOTE_DIR
if [ -f /tmp/ws_art_raw/xiaozhi_server ]; then
    cp /tmp/ws_art_raw/xiaozhi_server /tmp/xiaozhi_server.new
fi
STAMP=\$(date +%Y%m%d-%H%M%S)
cp xiaozhi_server xiaozhi_server.bak.\$STAMP
chmod +x /tmp/xiaozhi_server.new
mv /tmp/xiaozhi_server.new xiaozhi_server
ls -1t xiaozhi_server.bak.* 2>/dev/null | tail -n +4 | xargs -r rm -f
systemctl restart xiaozhi-server-golang
sleep 8
systemctl is-active xiaozhi-server-golang
"

info "4. 验收"
ssh "$REMOTE" '
code_root=$(curl -s -o /dev/null -w "%{http_code}" --resolve wx.originagent.cn:443:127.0.0.1 https://wx.originagent.cn/)
code_ota=$(curl -s -o /dev/null -w "%{http_code}" -X POST --resolve wx.originagent.cn:443:127.0.0.1 \
    https://wx.originagent.cn/xiaozhi/ota/ -H "Content-Type: application/json" \
    -H "Device-Id: ba:8f:17:aa:bb:cc" -H "Client-Id: 1a2b3c4d-0000-1111-2222-333344445555" \
    -d "{\"application\":{\"version\":\"1.0.0\",\"board\":\"test\"},\"flash_size\":33554432}")
ws=$(curl -s -o /dev/null -w "%{http_code}" --max-time 6 --resolve wx.originagent.cn:443:127.0.0.1 \
    -H "Connection: Upgrade" -H "Upgrade: websocket" -H "Sec-WebSocket-Version: 13" \
    -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
    -H "Device-Id: ba:8f:17:aa:bb:cc" -H "Client-Id: 1a2b3c4d-0000-1111-2222-333344445555" \
    -H "Protocol-Version: 1" -H "Authorization: Bearer x" \
    https://wx.originagent.cn/xiaozhi/v1/)
echo "  首页      -> $code_root"
echo "  OTA       -> $code_ota"
echo "  WebSocket -> $ws   (101 正常)"
systemctl show xiaozhi-server-golang -p MemoryCurrent --value | awk "{printf \"  内存      -> %.1f MB\n\", \$1/1048576}"
'

echo ""
echo "完成。回滚：ssh $REMOTE 'cd $REMOTE_DIR && cp xiaozhi_server.bak.<时间戳> xiaozhi_server && systemctl restart xiaozhi-server-golang'"
