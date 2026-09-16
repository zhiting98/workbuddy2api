# reset-dev.ps1 —— 把本地调试环境恢复到"全新未安装"状态
#
# 用途：反复从头测试安装向导 / 首次部署流程。
# 安装向导会把凭据写回 config.dev.json，所以跑过一次之后就不再是"未安装"了。
# 本脚本把它重置回去（并可选清掉 auths/ 与 data/）。
#
# 用法：
#   .\reset-dev.ps1              # 只重置配置（保留账号与统计）
#   .\reset-dev.ps1 -PurgeData   # 连 auths/ 与 data/ 一起清空（真·全新）
#   .\reset-dev.ps1 -WhatIf      # 只看会做什么，不实际改动
param(
    [switch]$PurgeData,
    [switch]$WhatIf
)

$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$cfg = 'config.dev.json'
$template = @'
{
  "listen": "127.0.0.1:7863",
  "api_key": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "server": {
    "max_body_mb": 32
  },
  "schedule": {
    "checkin_enabled": false,
    "travel_enabled": false,
    "activity_enabled": false,
    "keepalive_enabled": false
  },
  "prompt": {
    "mode": "custom",
    "file": ""
  },
  "dashboard": {
    "user": "",
    "pass": ""
  }
}
'@

Write-Host "==> 重置 $cfg 为未安装状态（dashboard 凭据留空、api_key 留空）" -ForegroundColor Cyan
if (-not $WhatIf) {
    # 保留一份已安装的配置以便回退（万一你想回到刚才装好的状态）。
    if (Test-Path $cfg) {
        Copy-Item $cfg "$cfg.installed" -Force
        Write-Host "    已备份原配置 -> $cfg.installed" -ForegroundColor DarkGray
    }
    [System.IO.File]::WriteAllText(
        (Join-Path $PWD $cfg),
        $template,
        (New-Object System.Text.UTF8Encoding($false))
    )
    Write-Host "    done" -ForegroundColor Green
}

if ($PurgeData) {
    foreach ($d in @('auths', 'data')) {
        Write-Host "==> 清空 $d/" -ForegroundColor Cyan
        if (-not $WhatIf) {
            if (Test-Path $d) {
                Get-ChildItem $d -Force | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
            } else {
                New-Item -ItemType Directory -Force -Path $d | Out-Null
            }
            Write-Host "    done" -ForegroundColor Green
        }
    }
}

Write-Host ""
Write-Host "现在启动：.\dev.ps1" -ForegroundColor Yellow
Write-Host "浏览器打开：http://127.0.0.1:7863/  应看到【安装向导】" -ForegroundColor Yellow
