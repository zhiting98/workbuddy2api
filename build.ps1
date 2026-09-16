# build.ps1 —— 本地构建 Windows exe（**不用于发布**）
#
# 为什么需要它：本项目部署文档只讲 Docker，但它是纯 Go、零外部依赖的静态二进制
# （3 个资源全 go:embed，无 CGO、无 python/bash 调用），本机单文件即可跑，
# 比常驻 Docker Desktop 轻得多。此脚本把 5 个命令一次编成 exe。
#
# 定位：**仅本地使用**。原项目有意保持「无预编译 release、产物 = 源码自构建」，
# 故本脚本不产出发布包、不接 CI。要对外分发请先考虑合规边界（见 README 安全与合规）。
#
# 用法：
#   .\build.ps1                  # 构建全部 5 个 exe 到 dist\
#   .\build.ps1 -Only server     # 只构建主服务
#   .\build.ps1 -Clean           # 先清空 dist\ 再构建
#   .\build.ps1 -Install Service # 额外给出任务计划程序自启的注册命令（不自动执行）
#
# 产物（dist\）：
#   wb2api.exe        主服务（网关 + 看板 + 安装向导，同端口）
#   signin_bin.exe    批量签到
#   login.exe         OAuth 登录 CLI（见下方「Windows 注意」）
#   credit.exe        积分查询
#   activity_bin.exe  活跃上报一次性触发器
param(
    [string]$Only = "",
    [switch]$Clean,
    [switch]$Install
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$dist = "dist"

# 交叉编译目标与产物名（与 Dockerfile 的 /out 命名保持一致，便于对照）。
# 顺序无关；-Only 可按名称过滤。
$targets = @(
    @{ Name = 'server';   Dir = 'server';   Out = 'wb2api.exe' },
    @{ Name = 'signin';   Dir = 'signin';   Out = 'signin_bin.exe' },
    @{ Name = 'login';    Dir = 'login';    Out = 'login.exe' },
    @{ Name = 'credit';   Dir = 'credit';   Out = 'credit.exe' },
    @{ Name = 'activity'; Dir = 'activity'; Out = 'activity_bin.exe' }
)

if ($Only) {
    $sel = $targets | Where-Object { $_.Name -eq $Only }
    if (-not $sel) {
        $names = ($targets | ForEach-Object { $_.Name }) -join ', '
        Write-Host "未知目标 '$Only'。可选: $names" -ForegroundColor Red
        exit 2
    }
    $targets = @($sel)
}

if ($Clean -and (Test-Path $dist)) {
    Write-Host "==> 清空 $dist\" -ForegroundColor Cyan
    Remove-Item $dist -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $dist | Out-Null

# 环境要求：Go 工具链。版本要求见 go.mod（go 1.22.5；Docker 用 1.23 构建）。
$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $go) {
    Write-Host "未找到 go 命令。请先安装 Go 工具链（见 go.mod 的版本要求）。" -ForegroundColor Red
    exit 1
}

# go version 里 "go1.24.4" 形如 go1.<minor>.<patch>；这里只提示，不硬拦
# （构建本身会因语言特性不兼容而报错，让 go 自己给出准确信息）。
$goVer = (go version) -replace '^go version go', '' -replace '\s.*$', ''
Write-Host "==> Go $goVer | 目标 Windows amd64" -ForegroundColor Cyan
Write-Host ""

# CGO_ENABLED=0 → 纯静态，无需 gcc；-trimpath 去本机路径；
# -s -w 去符号表与 DWARF（体积减半左右，本项目 8MB 级）。
$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'

$failed = @()
$sw = [System.Diagnostics.Stopwatch]::StartNew()

foreach ($t in $targets) {
    $out = Join-Path $dist $t.Out
    Write-Host "==> go build ./cmd/$($t.Dir)  ->  $($t.Out)" -ForegroundColor Yellow
    # 用 & 调用并捕获输出：构建失败时要能看到 go 的原始报错
    $log = go build -trimpath -ldflags="-s -w" -o $out "./cmd/$($t.Dir)" 2>&1
    if ($LASTEXITCODE -ne 0) {
        Write-Host "    构建失败:" -ForegroundColor Red
        $log | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkRed }
        $failed += $t.Name
        continue
    }
    $mb = [math]::Round((Get-Item $out).Length / 1MB, 2)
    Write-Host "    OK  $mb MB" -ForegroundColor Green
}
$sw.Stop()

Write-Host ""
if ($failed.Count -gt 0) {
    Write-Host "有 $($failed.Count) 个目标失败: $($failed -join ', ')" -ForegroundColor Red
    exit 1
}

Write-Host "==> 完成，用时 $($sw.Elapsed.TotalSeconds.ToString('0.0'))s，产物在 $dist\" -ForegroundColor Green
Get-ChildItem $dist -File | Sort-Object Name |
    Select-Object Name, @{n = 'Size(MB)'; e = { [math]::Round($_.Length / 1MB, 2) } } |
    Format-Table -AutoSize

# ---------------------------------------------------------------------------
# 运行说明（exe 模式与 Docker 的差异，务必知悉）
# ---------------------------------------------------------------------------
Write-Host "运行方式：" -ForegroundColor Cyan
Write-Host "  1) 准备配置与目录（可放任意位置，用相对路径最省事）："
Write-Host "       dist\config.json      # 从 config.example.json 复制后改"
Write-Host "       dist\auths\           # 凭证目录（可空，首次启动会走安装向导）"
Write-Host "       dist\data\            # 状态/统计（缺目录会自动建）"
Write-Host "  2) .\dist\wb2api.exe -config config.json"
Write-Host "  3) 浏览器打开 http://localhost:7863/（未配置凭据时是安装向导）"
Write-Host ""
Write-Host "exe 模式的三个注意点：" -ForegroundColor Yellow
Write-Host "  * 时区取自宿主机（Docker 里固定 TZ=Asia/Shanghai）。非 UTC+8 机器上，"
Write-Host "    签到 09/21、保活 22 点、硬冷却次日 04:00 都会按宿主时区偏移。"
Write-Host "  * login.exe 只有授权协议客户端部分；登录全流程由 login.sh 驱动，"
Write-Host "    而 login.sh 依赖 python3 + bash，Windows 上用不了。"
Write-Host "    → 加账号请用看板「+ 添加账号 → 网页授权登录」（同一份 internal/oauth，不需要 python3）。"
Write-Host "  * 改 config.json 别用 Set-Content / Out-File -Encoding UTF8：" -ForegroundColor Yellow
Write-Host "    PowerShell 5.1 会写入 UTF-8 BOM，Go 的 json 解析会直接报" -ForegroundColor Yellow
Write-Host "    invalid character 'i' looking for beginning of value 而启动失败。" -ForegroundColor Yellow
Write-Host "    安全做法：Copy-Item config.example.json config.json（字节级复制，不动编码）；" -ForegroundColor Yellow
Write-Host "    脚本改则用 [IO.File]::WriteAllText(p, s, [Text.UTF8Encoding]::new(`$false))。" -ForegroundColor Yellow

if ($Install) {
    Write-Host ""
    Write-Host "开机自启（任务计划程序，需管理员权限执行）：" -ForegroundColor Cyan
    $exe = (Resolve-Path (Join-Path $dist 'wb2api.exe')).Path
    $wd  = (Resolve-Path $dist).Path
    # 本项目未引用 Windows 服务库（golang.org/x/sys/windows/svc），
    # 不能 sc.exe create 成原生服务，故用任务计划程序。
    #
    # /tr 的参数含空格，必须整体加引号；路径本身也要引号 → 需转义为 \"
    # 用单引号拼接避免反引号转义出错（双引号在单引号串里是字面量）。
    # 保持单行输出：拆行后复制粘贴会被当成两条命令。
    $cmd = '  schtasks /create /tn WorkBuddy2API /sc onstart /ru SYSTEM /rl HIGHEST /tr "\"' +
           $exe + '\" -config config.json"'
    Write-Host $cmd -ForegroundColor Gray
    Write-Host ""
    Write-Host "  或图形界面「任务计划程序」→ 创建任务 → 触发器「启动时」→" -ForegroundColor DarkGray
    Write-Host "    操作「启动程序」= $exe" -ForegroundColor DarkGray
    Write-Host "    起始于          = $wd" -ForegroundColor DarkGray
    Write-Host ""
    Write-Host "  停止：schtasks /end /tn WorkBuddy2API" -ForegroundColor DarkGray
    Write-Host "  删除：schtasks /delete /tn WorkBuddy2API /f" -ForegroundColor DarkGray
    Write-Host "  注意：/ru SYSTEM 下进程以 SYSTEM 身份运行，auths\ 与 data\ 会被 SYSTEM 占用；" -ForegroundColor DarkYellow
    Write-Host "        改用当前用户 + 「不管用户是否登录都运行」可避免权限混杂。" -ForegroundColor DarkYellow
    Write-Host "  （本条仅打印命令，未自动执行——注册自启属于系统级改动，请自行确认后运行）" -ForegroundColor DarkGray
}
