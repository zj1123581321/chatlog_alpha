#Requires -RunAsAdministrator
<#
.SYNOPSIS
    在 Windows 10 LTSC 上一键配置 Windows Terminal 作为默认终端,
    绕开出厂 conhost.exe 19041.1 的 /GS 栈溢出误报 bug。

.DESCRIPTION
    LTSC 2021 (Build 19044) 出厂自带 conhost.exe 10.0.19041.1,
    在 chatlog TUI 启动后约 1-2 分钟会弹 "基于堆栈的缓冲区溢出" 对话框。
    主进程不死但 TUI 丢 console。本脚本做四件事:
      1. 下载并 Provision Windows Terminal 1.24+ 给所有用户
      2. 为当前登录的 Administrator 注册 WT (通过 Interactive ScheduledTask)
      3. 写注册表 HKU\<adminSID>\Console\%%Startup 和 HKLM 对应路径,
         设置 DelegationConsole/DelegationTerminal CLSID 让系统自动用 WT 托管
      4. 提示用户去 Windows Settings UI 最后点一下"默认终端"确认

    幂等: 已装合适版本会跳过; 注册表已正确会跳过。

.PARAMETER WtVersion
    要装的 Windows Terminal 版本 tag (默认 v1.24.10921.0)。

.PARAMETER BundleUrl
    msixbundle 下载 URL (默认从 GitHub release 拉)。若网络访问 github 慢,
    可以先手动下到本地再通过 -LocalBundle 传入。

.PARAMETER LocalBundle
    本地 msixbundle 路径 (跳过下载)。

.EXAMPLE
    .\setup_wt_for_ltsc.ps1

.EXAMPLE
    .\setup_wt_for_ltsc.ps1 -LocalBundle C:\Temp\WindowsTerminal.msixbundle
#>

param(
    [string]$WtVersion   = "v1.24.10921.0",
    [string]$BundleUrl   = "",
    [string]$LocalBundle = ""
)

$ErrorActionPreference = "Stop"

# ---------- 0. 先看要不要做 ----------
function Write-Step($msg) { Write-Host "`n==> $msg" -ForegroundColor Cyan }

Write-Step "检查 Windows 版本"
$os = Get-CimInstance Win32_OperatingSystem
Write-Host ("  {0} Build {1}" -f $os.Caption, $os.BuildNumber)
if ([int]$os.BuildNumber -lt 19041) {
    throw "Windows Build < 19041, 不支持 Default Terminal Delegation。需要 19041+。"
}

Write-Step "检查 conhost.exe 版本"
$conhostVer = (Get-Item "C:\Windows\System32\conhost.exe").VersionInfo.FileVersion
Write-Host "  conhost.exe: $conhostVer"
if ($conhostVer -notmatch "^10\.0\.1904[0-4]") {
    Write-Warning "  conhost 已不是出厂老版本, 可能不需要走本脚本。但继续无害。"
}

Write-Step "检查 Windows Terminal 是否已装"
$wtPkg = Get-AppxPackage -AllUsers Microsoft.WindowsTerminal -ErrorAction SilentlyContinue | Select-Object -First 1
if ($wtPkg) {
    Write-Host "  已装: $($wtPkg.Version)"
    $skipInstall = $true
} else {
    Write-Host "  未装, 继续安装流程"
    $skipInstall = $false
}

# ---------- 1. 下载 msixbundle ----------
$bundlePath = "C:\Temp\WindowsTerminal.msixbundle"

if (-not $skipInstall) {
    Write-Step "准备 Windows Terminal 安装包"
    New-Item -Path C:\Temp -ItemType Directory -Force | Out-Null

    if ($LocalBundle -and (Test-Path $LocalBundle)) {
        Copy-Item $LocalBundle $bundlePath -Force
        Write-Host "  使用本地文件: $LocalBundle"
    } else {
        if (-not $BundleUrl) {
            $BundleUrl = "https://github.com/microsoft/terminal/releases/download/$WtVersion/Microsoft.WindowsTerminal_$($WtVersion.TrimStart('v'))_8wekyb3d8bbwe.msixbundle"
        }
        Write-Host "  下载: $BundleUrl"
        [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
        $wc = New-Object System.Net.WebClient
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $wc.DownloadFile($BundleUrl, $bundlePath)
        $sw.Stop()
        $sizeMB = [math]::Round((Get-Item $bundlePath).Length / 1MB, 1)
        Write-Host ("  下载完成 {0}MB / {1:N1}s" -f $sizeMB, $sw.Elapsed.TotalSeconds)
    }

    # ---------- 2. Provision (DISM, 支持 SYSTEM/无当前用户 session) ----------
    Write-Step "Provision Windows Terminal 给所有用户"
    Add-AppxProvisionedPackage -Online -PackagePath $bundlePath -SkipLicense | Out-Null
    $provisioned = Get-AppxProvisionedPackage -Online | Where-Object { $_.DisplayName -eq "Microsoft.WindowsTerminal" } | Select-Object -First 1
    Write-Host "  已 Provision: $($provisioned.PackageName)"
    $wtPkg = Get-AppxPackage -AllUsers Microsoft.WindowsTerminal | Select-Object -First 1
}

# ---------- 3. 为当前 Administrator 注册 (如果当前身份不是 Administrator, 用 ScheduledTask) ----------
Write-Step "为 Administrator 注册 Windows Terminal"
$curUser = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
Write-Host "  当前身份: $curUser"

$adminInstalled = (Get-AppxPackage -AllUsers Microsoft.WindowsTerminal).PackageUserInformation |
    Where-Object { $_.UserSecurityId.Sid -match "-500$" -and $_.InstallState -eq "Installed" }

if ($adminInstalled) {
    Write-Host "  Administrator 已注册 (Installed 状态), 跳过"
} else {
    if ($curUser -match "\\Administrator$") {
        $manifest = Join-Path $wtPkg.InstallLocation "AppxManifest.xml"
        Add-AppxPackage -DisableDevelopmentMode -Register $manifest
        Write-Host "  已通过直接 Register 注册"
    } else {
        # SYSTEM 或其他身份: 通过 ScheduledTask 让 Administrator 的 Interactive session 跑
        $innerScript = @'
$pkg = Get-AppxPackage -AllUsers -Name Microsoft.WindowsTerminal | Select-Object -First 1
if ($pkg) {
    Add-AppxPackage -DisableDevelopmentMode -Register (Join-Path $pkg.InstallLocation AppxManifest.xml)
    "OK $(Get-Date)" | Out-File C:\Temp\wt_reg.log
}
'@
        $innerScript | Out-File -FilePath C:\Temp\register_wt_inner.ps1 -Encoding UTF8 -Force
        $action    = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -ExecutionPolicy Bypass -File C:\Temp\register_wt_inner.ps1"
        $principal = New-ScheduledTaskPrincipal -UserId "Administrator" -LogonType Interactive -RunLevel Highest
        $task      = New-ScheduledTask -Action $action -Principal $principal
        Unregister-ScheduledTask -TaskName "RegisterWTForAdmin" -Confirm:$false -ErrorAction SilentlyContinue
        Register-ScheduledTask -TaskName "RegisterWTForAdmin" -InputObject $task -Force | Out-Null
        Start-ScheduledTask   -TaskName "RegisterWTForAdmin"
        Start-Sleep -Seconds 10
        $info = Get-ScheduledTaskInfo -TaskName "RegisterWTForAdmin"
        Write-Host ("  Task LastTaskResult=0x{0:X8}" -f $info.LastTaskResult)
        Unregister-ScheduledTask -TaskName "RegisterWTForAdmin" -Confirm:$false

        if (-not (Test-Path C:\Temp\wt_reg.log)) {
            Write-Warning "  ScheduledTask 可能没拉起 Register (Administrator 未登录?) — 需要 Administrator 手动登录后再跑此脚本, 或跑完本脚本后注销再登录。"
        }
    }
}

# ---------- 4. 写 DelegationConsole / DelegationTerminal 注册表 ----------
Write-Step "设置 Default Terminal = Windows Terminal"

# Windows Terminal 1.11+ 官方 CLSID (所有版本不变)
$delegationConsole  = "{2EACA947-7F5F-4CFA-BA87-8F7FBEEFBE69}"
$delegationTerminal = "{E12CFF52-A866-4C77-9A90-FBB7D0FE6CB9}"

# 4a. HKLM 机器级 fallback
$hklmStartup = "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Console\%%Startup"
if (-not (Test-Path $hklmStartup)) { New-Item -Path $hklmStartup -Force | Out-Null }
Set-ItemProperty -Path $hklmStartup -Name "DelegationConsole"  -Value $delegationConsole  -Type String
Set-ItemProperty -Path $hklmStartup -Name "DelegationTerminal" -Value $delegationTerminal -Type String
Write-Host "  HKLM 已设"

# 4b. 每个已加载的本地 Administrator hive 都设一遍
if (-not (Get-PSDrive -Name HKU -ErrorAction SilentlyContinue)) {
    New-PSDrive -Name HKU -PSProvider Registry -Root HKEY_USERS | Out-Null
}
$adminHives = Get-ChildItem HKU:\ | Where-Object { $_.PSChildName -match "^S-1-5-21-.*-500$" }
foreach ($hive in $adminHives) {
    $startupKey = "HKU:\$($hive.PSChildName)\Console\%%Startup"
    if (-not (Test-Path $startupKey)) { New-Item -Path $startupKey -Force | Out-Null }
    Set-ItemProperty -Path $startupKey -Name "DelegationConsole"  -Value $delegationConsole  -Type String
    Set-ItemProperty -Path $startupKey -Name "DelegationTerminal" -Value $delegationTerminal -Type String
    Write-Host "  HKU:\$($hive.PSChildName) 已设"
}

if (-not $adminHives) {
    Write-Warning "  没有发现已加载的 Administrator hive。HKLM 已作为 fallback, 但建议 Administrator 登录后再跑一次本脚本, 或在 Windows Settings UI 里手动确认默认终端。"
}

# ---------- 5. 验证 + 下一步提示 ----------
Write-Step "完成"
$openCon = Join-Path $wtPkg.InstallLocation "OpenConsole.exe"
Write-Host "  Windows Terminal: $($wtPkg.Version)"
Write-Host "  OpenConsole.exe:  $((Get-Item $openCon -ErrorAction SilentlyContinue).VersionInfo.FileVersion)"

Write-Host "`n下一步:" -ForegroundColor Yellow
Write-Host "  1. 注销 Administrator 后重新登录 (让注册表缓存刷新)"
Write-Host "  2. 打开 Windows Settings -> Privacy & Security -> For Developers -> Terminal"
Write-Host "     确认 '默认终端应用' = Windows Terminal"
Write-Host "     (注册表改完通常需要这一步才对 Explorer 里双击启动的 console app 生效)"
Write-Host "  3. 双击 chatlog.exe 验证: 应该弹 Windows Terminal 窗口, 不再有老版对话框"
