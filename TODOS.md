# TODOS

## P2

### [autodecrypt] 首次全量完成后自动预热 md5PathCache

**What**: 首次全量解密结束后，自动扫 message 表抽 md5，冷启 `md5PathCache`。
**Why**: README 2026-04-18 条目明确指出"正确用法：先用完整 `?time=&talker=&limit=1000` 预热，再拉 `/image`"。首启后用户立刻拉 `/image` 会 404 + `X-Backup-Hint: warm-cache-first`。自动化后这个文档依赖消失，`/image` 开箱即用。
**Pros**:
- 消除 `X-Backup-Hint: warm-cache-first` 这条运维提示的存在必要
- MCP 客户端首启 → 立即能拉图，体验统一
**Cons**:
- 首启总耗时 +50~100%（扫 message 表抽 md5 IO 重）
- 和"首次全量 = 后台异步、秒级返回"的设计相冲，需要明确是"首次全量完成后"再跑，不是"启动时"
**Context**: 来自 2026-04-20 `/plan-ceo-review` 的 cherry-pick E6 决策 —— 当时 defer 掉是因为不想让 PR#1/PR#2 首启变重。PR#2 落地后再做独立 PR。实现位置参考 `internal/chatlog/http/backup_index.go` 的索引构建思路。
**Effort**: M (CC ~30min, human ~2h)
**Priority**: P2
**Depends on**: PR#2（异步全量 + progress channel）已 landed
