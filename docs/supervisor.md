# chatlog Supervisor 部署指南

> 配套 `architecture-rework-2026-05-06.md`，Step 8 文档交付。
>
> 适用：Step 7 进程拆分落地后的 watcher / server / tui 三进程长跑部署。
> 当前 main 分支 watcher 与 server 仍合一，本指南先行规划部署形态，待 Step 7 ship 后立即可用。

---

## 1. 部署拓扑

```
┌─────────────────── Windows 主机 ───────────────────┐
│                                                    │
│   任务计划程序 (Task Scheduler) 或 Windows Service │
│           ↓ 进程退出 → 30s 后自动重启             │
│   ┌──────────────────┬──────────────────┐          │
│   │ chatlog watcher  │ chatlog server   │          │
│   │ (后台 daemon)    │ (HTTP/MCP)       │          │
│   │ - WAL-aware copy │ - :5030          │          │
│   │ - decrypt cycle  │ - /healthz       │          │
│   │ - schema gate    │ - GenerationPoll │          │
│   │ - atomic swap    │ - dbm pool       │          │
│   └──────────────────┴──────────────────┘          │
│           ↓ writes              ↑ reads            │
│   ┌────────────────────────────────────┐           │
│   │ work_dir/                          │           │
│   │   status.json                      │           │
│   │   generations/{id}/db_storage/     │           │
│   │   corrupt/                         │           │
│   └────────────────────────────────────┘           │
│                                                    │
│   chatlog tui (用户主动启动) — 不受 supervisor 管 │
└────────────────────────────────────────────────────┘
```

设计原则：watcher 与 server 是**对等的两个 daemon**，由外部 supervisor 各自重启，互相不依赖；TUI 是用户工具不进 supervisor。

---

## 2. Windows 任务计划程序模板

最轻量的部署方式。无需安装额外软件。

### 2.1 chatlog-watcher 任务

将下面 XML 保存为 `chatlog-watcher.xml`（注意修改 `<UserId>`、可执行路径、工作目录）：

```xml
<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>chatlog watcher daemon: decrypt cycle + schema gate + atomic swap</Description>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
      <Delay>PT30S</Delay>
    </BootTrigger>
    <LogonTrigger>
      <Enabled>true</Enabled>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-21-XXXX-XXXX-XXXX-1001</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT30S</Interval>
      <Count>9999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>D:\path\to\chatlog.exe</Command>
      <Arguments>watcher --interval 5m --schema-check-mode quick</Arguments>
      <WorkingDirectory>D:\path\to\chatlog</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
```

注册：

```powershell
schtasks /Create /XML "chatlog-watcher.xml" /TN "chatlog-watcher"
schtasks /Run /TN "chatlog-watcher"
```

关键字段：

- `<RestartOnFailure><Interval>PT30S</Interval>` —— 与 spec §1.1 的 "进程退出 → 30s 后自动重启" 对齐
- `<Count>9999</Count>` —— 不主动放弃；密钥失效（exit 2）那种"该停就停"由 watcher 自身识别
- `<Priority>7</Priority>` —— 普通用户进程优先级，不与微信抢 IO
- `<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>` —— 多次触发时旧实例还在跑就不起新实例（再加 status.json 内 watcher_pid 校验做双保险）

### 2.2 chatlog-server 任务

复制 watcher 的 XML，把 `<Description>` 改成 server，`<Arguments>` 改成 `server --addr :5030`，`<RegistrationInfo><URI>` 改成 `\chatlog-server`。注册：

```powershell
schtasks /Create /XML "chatlog-server.xml" /TN "chatlog-server"
schtasks /Run /TN "chatlog-server"
```

### 2.3 验证

```powershell
schtasks /Query /TN "chatlog-watcher" /V /FO LIST
schtasks /Query /TN "chatlog-server"  /V /FO LIST

# 查看运行历史
Get-WinEvent -LogName "Microsoft-Windows-TaskScheduler/Operational" `
  -MaxEvents 50 | Where-Object { $_.Message -like '*chatlog*' }
```

---

## 3. Windows Service（NSSM 模板，可选）

需要更精细的日志重定向 + 服务依赖时，用 [NSSM](https://nssm.cc/)。

```powershell
# watcher
nssm install chatlog-watcher D:\path\to\chatlog.exe
nssm set    chatlog-watcher AppParameters "watcher --interval 5m --schema-check-mode quick"
nssm set    chatlog-watcher AppDirectory  D:\path\to\chatlog
nssm set    chatlog-watcher AppStdout     D:\path\to\chatlog\logs\watcher.stdout.log
nssm set    chatlog-watcher AppStderr     D:\path\to\chatlog\logs\watcher.stderr.log
nssm set    chatlog-watcher AppRotateFiles 1
nssm set    chatlog-watcher AppRotateBytes 10485760
nssm set    chatlog-watcher AppExit Default Restart
nssm set    chatlog-watcher AppRestartDelay 30000
nssm start  chatlog-watcher

# server
nssm install chatlog-server D:\path\to\chatlog.exe
nssm set    chatlog-server AppParameters "server --addr :5030"
nssm set    chatlog-server AppDirectory  D:\path\to\chatlog
nssm set    chatlog-server AppStdout     D:\path\to\chatlog\logs\server.stdout.log
nssm set    chatlog-server AppStderr     D:\path\to\chatlog\logs\server.stderr.log
nssm set    chatlog-server AppExit Default Restart
nssm set    chatlog-server AppRestartDelay 30000
nssm start  chatlog-server
```

NSSM 优点：
- 日志按大小自动 rotate（任务计划程序原生不支持）
- `nssm status chatlog-watcher` 即时查看
- 服务依赖关系（如 server 依赖 watcher 起好）可以配

NSSM 缺点：要装第三方工具，受限环境（公司 IT 锁了 service 安装权限）走任务计划程序。

---

## 4. 健康检查

`server` 提供 `GET /healthz` 真实健康状态（spec §8.3）。

```powershell
# 期待返回 200 ok
curl.exe -fsS http://127.0.0.1:5030/healthz
```

返回 503 的可能原因：

| 响应文本 | 含义 |
|---|---|
| `watcher heartbeat stale` | `now - status.watcher_heartbeat_ts > 10min`，watcher 进程死了或 supervisor 在重启循环 |
| `watcher reports unhealthy` | `status.healthy=false`，watcher 自己探测到磁盘满 / schema 大版本变化 / 连续 corrupt |
| `sqlite ping fail` | server 自己 db 池断了，可能 `current_generation` 路径不存在或文件被锁 |
| `last decrypt > 30min ago` | watcher 在跑但已 30min 没成功 swap，常见于 `corrupt/` 累积 |

### 监控脚本（每 1min 跑一次的 cron 思路）

```powershell
$resp = try { Invoke-WebRequest -Uri http://127.0.0.1:5030/healthz -TimeoutSec 5 -UseBasicParsing } catch { $null }
if (-not $resp -or $resp.StatusCode -ne 200) {
    Write-EventLog -LogName Application -Source chatlog -EntryType Warning `
        -EventId 5030 -Message "chatlog /healthz unhealthy: $($resp.StatusCode) $($resp.Content)"
}
```

---

## 5. 故障 Runbook

整理自 spec §8.5。每个症状对应**最少诊断步骤**。

### 5.1 `/healthz` 503 — `watcher heartbeat stale`

```powershell
# 1. watcher 进程在不在
Get-Process chatlog -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowTitle -eq '' }

# 2. 任务计划是不是在重启循环
Get-WinEvent -LogName Microsoft-Windows-TaskScheduler/Operational -MaxEvents 30 |
  Where-Object Message -like '*chatlog-watcher*'

# 3. 看 watcher 最近的 stderr
Get-Content D:\path\to\chatlog\logs\watcher.stderr.log -Tail 100
```

常见根因：watchdog 触发了 self-kill（spec §A4），主循环卡住。看日志最末尾的 `[watchdog] main loop hang detected` 后面那一行的 `phase` 字段定位卡在哪。

### 5.2 `/healthz` 503 — `last decrypt > 30min ago`

```powershell
# 1. 看 corrupt/ 累积情况
Get-ChildItem D:\path\to\chatlog_workdir\corrupt | Sort-Object LastWriteTime -Descending | Select-Object -First 10

# 2. watcher 日志里 schema-fail 的具体原因
Select-String -Path D:\path\to\chatlog\logs\watcher.stderr.log -Pattern 'schema gate FAIL|moved to corrupt'
```

常见根因：微信版本升级，schema 变了。corrupt/ 目录名 `<id>-schema*` 后面是 schema gate 报错，对照 chatlog 业务级 schema 定义看缺哪张表 → 升级 chatlog 二进制。

### 5.3 `corrupt/` 越来越多

```powershell
# 24h 内的 corrupt count
Get-ChildItem D:\path\to\chatlog_workdir\corrupt |
  Where-Object { $_.LastWriteTime -gt (Get-Date).AddHours(-24) } |
  Measure-Object | Select-Object -ExpandProperty Count

# 按失败 reason 分类
Get-ChildItem D:\path\to\chatlog_workdir\corrupt |
  ForEach-Object { ($_.Name -split '-', 4)[-1] } | Group-Object | Sort-Object Count -Descending
```

如果 reason 集中在 `wal-incoherent` → 微信写入太频繁，考虑加大 `--io-yield-threshold`；
集中在 `schema` → 见 5.2；
零散随机 → 检查 D 盘 SMART 状态。

### 5.4 watcher 频繁 self-kill

```powershell
# 看 watchdog 触发频率
Select-String -Path D:\path\to\chatlog\logs\watcher.stderr.log -Pattern '\[watchdog\] main loop hang' -Context 0,3
```

一般卡在 SQLite checkpoint。短期解法：增大 `--watchdog-timeout`（默认 5min for non-FirstFull）；长期解法：升级 SQLite 或减少单 db 大小。

### 5.5 `status.json.watcher_pid` 不匹配运行中进程

旧 watcher 没正常退出 status.json 没更新。手动恢复：

```powershell
# 停掉所有 chatlog 进程
schtasks /End /TN "chatlog-watcher"
schtasks /End /TN "chatlog-server"
Get-Process chatlog -ErrorAction SilentlyContinue | Stop-Process -Force

# 删 status.json，让 watcher 重新初始化
Remove-Item D:\path\to\chatlog_workdir\status.json -Force

# 重启
schtasks /Run /TN "chatlog-watcher"
schtasks /Run /TN "chatlog-server"
```

### 5.6 紧急 rollback 到旧 in-place decrypt 路径

Step 5 generation 管道异常时（spec §1.7 escape hatch）：

```powershell
# 在任务计划程序的 chatlog-watcher 任务上加环境变量 CHATLOG_LEGACY_DECRYPT=1
schtasks /End /TN "chatlog-watcher"
[Environment]::SetEnvironmentVariable("CHATLOG_LEGACY_DECRYPT", "1", "User")
schtasks /Run /TN "chatlog-watcher"
```

恢复后清掉环境变量重启即可。

---

## 6. 日志位置

| 来源 | 路径 |
|---|---|
| watcher stdout/stderr | NSSM 配置的 `AppStdout` / `AppStderr`；任务计划走系统事件日志 |
| 任务计划程序事件 | `Microsoft-Windows-TaskScheduler/Operational` Windows 事件日志 |
| chatlog 内部结构化日志 | watcher 进程 stderr，zerolog JSON 格式 |
| watcher 状态快照 | `work_dir/status.json` |
| 失败的 generation | `work_dir/corrupt/{id}-{reason}/` |

---

## 7. 升级流程

```powershell
# 1. 停服务
schtasks /End /TN "chatlog-server"
schtasks /End /TN "chatlog-watcher"

# 2. 等 30s 让 supervisor 不会立即重启上一个版本
Start-Sleep -Seconds 30

# 3. 替换二进制
Copy-Item .\bin\chatlog.exe D:\path\to\chatlog\chatlog.exe -Force

# 4. 重启
schtasks /Run /TN "chatlog-watcher"
Start-Sleep -Seconds 5
schtasks /Run /TN "chatlog-server"

# 5. 健康检查
Start-Sleep -Seconds 10
curl.exe -fsS http://127.0.0.1:5030/healthz
```

升级期间 server 503 一段时间是正常的，spec §9.4 灰度策略要求这种短暂中断在用户感知的"5-15min 数据延迟"接受范围内。

---

## 8. 多账号 / 多机器

当前架构假设单账号单实例（spec NOT in Scope）。如果同一台机器跑多账号：

- 每个账号独立 `work_dir/`
- 每个账号独立的 `chatlog-watcher-{wxid}` 任务
- server 也按 wxid 起多个实例不同端口
- 端口分配：`5030 + N`，N 是账号序号

跨机器同步 generation 目录（用 robocopy / rsync 同步 `work_dir/generations/`）属 P2 future work，不在本指南覆盖。
