# dev.ps1 —— 本地调试（Windows，**不需要 Docker**）
#
# 为什么需要它：看板页面与安装向导是用 go:embed 打进二进制的
# （internal\server\dashboard\index.html、internal\server\setup_page.html）。
# 于是"改一行 CSS/JS"也必须重新编译 Go——很多人因此每改一次就跑
# `docker compose up -d --build`，几分钟一轮，调试体验极差。
#
# 实测：本机 `go build .\cmd\server` 约 1 秒；Docker 多阶段重建要几分钟。
# 所以**日常调试在本机跑，Docker 只用于最终部署**。
#
# 用法：
#   .\dev.ps1              # 构建并前台运行（Ctrl-C 停止）
#   .\dev.ps1 -Watch       # 监听文件变化，改完自动重启（推荐）
#   .\dev.ps1 -BuildOnly   # 只构建，不运行
#   .\dev.ps1 -Config config.dev.json
param(
    [switch]$Watch,
    [switch]$BuildOnly,
    [string]$Config = "config.dev.json"
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$bin = ".\wb2api_dev.exe"

if (-not (Test-Path $Config)) {
    Write-Host "缺少配置文件 $Config" -ForegroundColor Red
    Write-Host "先执行: Copy-Item config.example.json $Config" -ForegroundColor Yellow
    exit 1
}

function Invoke-Build {
    Write-Host "==> go build .\cmd\server" -ForegroundColor Cyan
    go build -o $bin .\cmd\server
    if ($LASTEXITCODE -ne 0) { throw "构建失败" }
}

# 启动前明确告知当前处于哪种模式——避免"为什么没走向导 / 为什么弹登录框"的困惑。
# 判定规则与后端一致（internal/server/dashboard.go dashboardEnabled）：
# dashboard.user 与 dashboard.pass **两者都非空**才算已安装。
function Show-Mode {
    $user = ''; $pass = ''
    try {
        $j = Get-Content $Config -Raw -Encoding UTF8 | ConvertFrom-Json
        if ($j.dashboard) { $user = "$($j.dashboard.user)"; $pass = "$($j.dashboard.pass)" }
    } catch { }

    if ($user -and $pass) {
        Write-Host "==> 模式：已安装（看板已启用）" -ForegroundColor Yellow
        Write-Host "    账号：$user" -ForegroundColor Yellow
        Write-Host "    提示：http://127.0.0.1:7863/ 会弹 Basic 登录框，**不会**出现安装向导。" -ForegroundColor Yellow
        Write-Host "    想从头测安装流程：先跑 .\reset-dev.ps1 -PurgeData" -ForegroundColor DarkYellow
    } else {
        Write-Host "==> 模式：未安装（安装向导）" -ForegroundColor Green
        Write-Host "    http://127.0.0.1:7863/ 应显示【安装向导】，安装后凭据会写回 $Config" -ForegroundColor Green
    }
}

Invoke-Build
if ($BuildOnly) { Write-Host "仅构建完成：$bin" -ForegroundColor Green; exit 0 }

Show-Mode

if (-not $Watch) {
    Write-Host "==> 启动 $bin -config $Config（Ctrl-C 停止）" -ForegroundColor Green
    & $bin -config $Config
    exit $LASTEXITCODE
}

# -Watch：轮询源文件最后写入时间（Windows 无 fswatch，轮询最省事且无需依赖）。
Write-Host "==> 监听文件变化（改完自动重建+重启，Ctrl-C 停止）" -ForegroundColor Green

$roots = @('cmd', 'internal', 'scripts')
$exts = @('.go', '.html', '.css', '.js', '.md')

function Get-Stamp {
    $files = foreach ($r in $roots) {
        if (Test-Path $r) {
            Get-ChildItem -Path $r -Recurse -File -ErrorAction SilentlyContinue |
                Where-Object { $exts -contains $_.Extension }
        }
    }
    # 用"文件数 + 最新写入时间"做指纹：足以捕捉新增与修改。
    $count = ($files | Measure-Object).Count
    $max = ($files | Measure-Object -Property LastWriteTime -Maximum).Maximum
    return "$count|$max"
}

$proc = $null
function Start-Server {
    $script:proc = Start-Process -FilePath $bin -ArgumentList @('-config', $Config) -NoNewWindow -PassThru
    Write-Host "==> 已启动 (pid=$($script:proc.Id))" -ForegroundColor Green
}
function Stop-Server {
    if ($script:proc -and -not $script:proc.HasExited) {
        Stop-Process -Id $script:proc.Id -Force -ErrorAction SilentlyContinue
        $script:proc.WaitForExit(5000) | Out-Null
    }
}

Start-Server
$last = Get-Stamp

try {
    while ($true) {
        Start-Sleep -Milliseconds 700
        $now = Get-Stamp
        if ($now -eq $last) { continue }
        $last = $now

        # 防抖：编辑器保存常连触发多次。
        Start-Sleep -Milliseconds 300
        $last = Get-Stamp

        Write-Host "==> 检测到变化，重建…" -ForegroundColor Cyan
        try {
            Invoke-Build
            Stop-Server
            Start-Server
        } catch {
            # 构建失败时保留旧进程，避免"改错了连服务都没了"。
            Write-Host "!! 构建失败，保留当前进程（修好后再保存即自动重启）" -ForegroundColor Red
            Write-Host $_ -ForegroundColor DarkRed
        }
    }
} finally {
    Stop-Server
}
