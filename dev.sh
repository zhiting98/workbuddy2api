#!/bin/bash
# dev.sh —— 本地调试（**不需要 Docker**）
#
# 为什么需要它：看板页面与安装向导是用 go:embed 打进二进制的
# （internal/server/dashboard/index.html、internal/server/setup_page.html）。
# 于是"改一行 CSS/JS"也必须重新编译 Go——很多人因此每改一次就跑
# `docker compose up -d --build`，几分钟一轮，调试体验极差。
#
# 实际耗时对比：本机 `go build ./cmd/server` 约 1 秒，Docker 多阶段重建要几分钟。
# 因此**日常调试就该在本机跑，Docker 只用于最终部署**。
#
# 用法：
#   ./dev.sh            # 构建并前台运行（Ctrl-C 停止）
#   ./dev.sh -w         # 监听文件变化，改完自动重启（推荐；需安装 fswatch 或 inotifywait）
#   ./dev.sh -b         # 只构建，不运行
#
# 首次使用前请复制一份配置：
#   cp config.example.json config.dev.json   # 或直接用仓库内已有的 config.dev.json
set -e
cd "$(dirname "$0")"

BIN=./wb2api_dev
CFG=${WB2_DEV_CONFIG:-config.dev.json}
WATCH=0

while [ $# -gt 0 ]; do
    case "$1" in
        -w|--watch) WATCH=1 ;;
        -b|--build) WATCH=2 ;;
        -c|--config) shift; CFG="$1" ;;
        -h|--help) sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "unknown arg: $1（-h 看用法）" >&2; exit 2 ;;
    esac
    shift
done

if [ ! -f "$CFG" ]; then
    echo "缺少配置文件 $CFG" >&2
    echo "先执行: cp config.example.json $CFG" >&2
    exit 1
fi

build() {
    echo "==> go build ./cmd/server"
    go build -o "$BIN" ./cmd/server
}

run() {
    echo "==> 启动 $BIN -config $CFG（Ctrl-C 停止）"
    "./$BIN" -config "$CFG"
}

build
[ "$WATCH" = "2" ] && { echo "仅构建完成：$BIN"; exit 0; }

if [ "$WATCH" = "1" ]; then
    # 选择可用的文件监听器；都没有就退回手动重启。
    if command -v fswatch >/dev/null 2>&1; then
        WATCHER="fswatch -o -e '\.git' -e "$BIN" -e '^data/' -e '^auths/' ."
    elif command -v inotifywait >/dev/null 2>&1; then
        WATCHER="inotifywait -q -r -e modify,create,delete,move --exclude '(\\.git/|$BIN|^data/|^auths/)' ."
    else
        echo "未找到 fswatch / inotifywait，无法自动重启。" >&2
        echo "  macOS: brew install fswatch    Linux: apt install inotify-tools" >&2
        echo "退回普通模式（手动 Ctrl-C 后重跑）。" >&2
        run
        exit 0
    fi

    echo "==> 监听文件变化（改完自动重建+重启）"
    # 前台跑服务，后台盯文件；任一退出就整体收尾，避免留孤儿进程。
    "./$BIN" -config "$CFG" &
    SERVER_PID=$!
    trap 'kill $SERVER_PID 2>/dev/null || true' EXIT INT TERM

    eval "$WATCHER" | while read -r _; do
        # 防抖：编辑器保存常触发多次事件。
        sleep 0.3
        echo "==> 检测到变化，重建…"
        if build; then
            kill "$SERVER_PID" 2>/dev/null || true
            wait "$SERVER_PID" 2>/dev/null || true
            "./$BIN" -config "$CFG" &
            SERVER_PID=$!
            echo "==> 已重启 (pid=$SERVER_PID)"
        else
            echo "!! 构建失败，保留当前进程（修好后再保存即自动重启）" >&2
        fi
    done
else
    run
fi
