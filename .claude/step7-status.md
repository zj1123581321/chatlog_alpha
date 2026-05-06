# Step 7 进程拆分 — 落地状态（2026-05-06）

Branch: `step7-process-split`（独立分支，未合 main）

## ✅ 已完成

| Sub | Commit | 内容 |
|---|---|---|
| 7a | `7ca02ce` | `WatcherDaemon` 编排器 + 5 个 TDD（pure 模块，注入 DecryptFunc） |
| 7b | `7cd1ddc` | `Service.DecryptDBFileExplicit` + `NewServiceDecryptFunc` adapter + `BuildDBJobs` |
| 7c | `e32d7b5` | `Manager.CommandWatcher` + `chatlog watcher` cobra 子命令 |

总：~840 LOC，13 个新 unit test，433 全仓 tests 绿。`bin/chatlog.exe` 编译通过，`chatlog watcher --help` 可用。

## 现状能做什么

```powershell
chatlog watcher \
  --data-dir D:\path\to\xwechat_files\wxid_xxx \
  --data-key <hex> \
  --work-dir D:\path\to\workdir \
  --interval 5m \
  --schema-check-mode quick
```

- 启动期跑 `DetectAndMigrate` 把 legacy `db_storage/` 搬进 `generations/{id}/`
- 起 watchdog goroutine + LockOSThread，phase-aware timeout
- 主循环每 interval 跑 `WatcherDaemon.RunOnce`：copy（WAL-aware）→ decrypt → schema gate → atomic swap → prune
- `CHATLOG_LEGACY_DECRYPT=1` 紧急回退到旧 `DecryptDBFiles`（一次性，外层 supervisor 30s 重启）

## ⛔ 未完成（推迟）

### 7d — server 端 current_generation 路由

`chatlog server` 当前仍按旧 `work_dir/db_storage` 直读。让它读
`work_dir/generations/{current_generation}/db_storage` 需要：

1. `CommandHTTPServer` 启动时 `ReadStatus(workDir).CurrentGeneration` → 拼物理路径
   传给 `database.Service` / `wechatdb.New`
2. `datasource.DataSource` 接口加 `Invalidate()` 或 `Rebuild(newPath)` 方法，
   v4 实现走 `dbm.InvalidateAll()` + 替换 `dbm.path`
3. 起 `GenerationPoller`，OnChange 调上面 (2) 的方法

第 2 步是真正的 invasive 改动 —— DataSource 是 chatlog 所有读路径的根，改它的接口
会牵动所有 query handler。建议作为独立 PR 立项，配合 §6.3 T3 集成测试
（"watcher 崩溃 server 仍能 serve"）一起落地。

### 7e — 删 pkg/filemonitor/

spec §5.1 计划 Step 7 时删 ~400 LOC 的 filemonitor。但 `dbm.DBManager`
仍 import 它（Callback 用 `fsnotify.Event` 类型），dbm 还没独立反向适配新
generation 模型。和 7d 一起处理更合理。

### 7f — TUI supervisor 改造

spec §11 把 TUI 改成 minimal supervisor（不内嵌 decrypt/HTTP，spawn watcher
+ server 子进程）。本 branch 不动 TUI；用户继续按今天的方式启动 TUI 即可。

## 推荐合并策略

1. 推送本 branch 到 origin（备查 + 团队 review）
2. **不直接合 main** —— 等 7d 落地、配 §9.4 灰度 1 周观察 `status.json.healthy`
   持续天数 / `corrupt/` 累积速度 / 微信打开图片主观感受。
3. 灰度通过后 → 7d/7e/7f 单独 PR → 全部 OK 再 fast-forward 到 main

## 回归路径

- 出问题：`git revert e32d7b5 7cd1ddc 7ca02ce` 三个 commit 即可，0 数据损坏风险（都是新增文件）
- 紧急 rollback：`CHATLOG_LEGACY_DECRYPT=1` env var → `chatlog watcher` 直接走旧路径
