# ChatLog MCP 扩展方案 TODO

## 1. 媒体内容感知服务 (Media Perception)
- [x] **get_media_content (Tool)**: 
  - **功能**: 根据消息 ID 获取解码后的媒体文件（图片自动解密、语音转 MP3）。
  - **用法**: `get_media_content(talker="ID", message_id=123456)`
- [x] **ocr_image_message (Tool)**: 
  - **功能**: 对图片消息进行 OCR 解析（由模型视觉能力驱动）。
  - **用法**: `ocr_image_message(talker="ID", message_id=123456)`

## 2. 实时消息通知与 Webhook 推送 (Real-time Interaction)
- [x] **subscribe_new_messages (Tool)**: 
  - **功能**: 订阅实时消息流。订阅后，当有新消息时，系统会自动推送到指定的 Webhook 地址。
  - **要求**: 必须提供订阅目标 (talker) 和推送地址 (webhook_url)。
  - **用法**: `subscribe_new_messages(talker="ID", webhook_url="http://...")`
- [x] **unsubscribe_new_messages (Tool)**:
  - **功能**: 取消订阅实时消息流。
  - **用法**: `unsubscribe_new_messages(talker="ID")`
- [x] **get_active_subscriptions (Tool)**:
  - **功能**: 获取当前活跃的订阅列表及推送地址。
  - **用法**: `get_active_subscriptions()`
- [x] **订阅持久化**: 订阅信息自动保存至本地配置文件，重启后自动恢复。
- [x] **推送状态监控**: TUI 界面可实时查看每个订阅的推送成功/失败状态及错误原因。
- [x] **send_webhook_notification (Tool)**: 
  - **功能**: 当模型分析完记录后，触发外部分析报告 Hook。

## 3. 数据分析与社交画像 (Social Insights)
- [x] **analyze_chat_activity (Tool)**: 
  - **功能**: 统计发言频率、活跃时段（带柱状图可视化模拟）。
  - **用法**: `analyze_chat_activity(talker="ID", time="2023-04-01~2023-04-30")`
- [x] **get_user_profile (Tool)**: 
  - **功能**: 获取备注、微信号、群成员及群主等背景信息。
  - **用法**: `get_user_profile(key="ID或名称")`

## 4. 增强型提示词模板 (Prompts)
- [x] **chat_summary_daily (Prompt)**: 每日聊天摘要。参数: `date`, `talker`。
- [x] **conflict_detector (Prompt)**: 情绪与冲突检测。参数: `talker`。
- [x] **relationship_milestones (Prompt)**: 关系里程碑回顾。参数: `talker`。

## 5. 跨应用检索 (Cross-app Retrieval)
- [x] **search_shared_files (Tool)**: 
  - **功能**: 专门搜索聊天记录中发送的文件元数据。
  - **用法**: `search_shared_files(talker="ID", keyword="报告")`

## 6. 系统优化 (Infrastructure)
- [x] **唯一消息 ID 系统**: 引入 `(timestamp * 1000000 + local_id)` 算法，解决多媒体消息 ID 冲突问题。
- [x] **多格式输出适配**: 文本、CSV、JSON 均已支持显示唯一 `MessageID`。

## 7. 长期后台运行优化 (Background Operation)

上下文：chatlog server 作为后台常驻服务运行时，核心原则是"永远不阻塞微信"。
默认改为 60s 安静期 + 10min maxWait + 进程低优先级后，以下项是后续可选增强，
均不阻塞核心功能。

- [ ] **自动解密模式切换（TUI）**: 在设置中提供四种模式一键切换
  - **选项**: 激进（1s debounce）/ 温和（60s，当前默认）/ 按需（仅 API 触发）/ 定时（cron 式）
  - **优先级**: P2
  - **预估**: S（human team） / S（CC+gstack）
  - **依赖**: 本轮 SCOPE REDUCTION 先落地
  - **价值**: 覆盖所有典型使用场景，让不同需求用户都能找到合适模式
  - **风险**: 配置面变大，需要好的默认值引导

- [ ] **Work Usage 标签增强：最近解密状态可视化**: 在 TUI "Work Usage" 标签显示
  - **内容**: 最近一次自动解密时间 / 最近一次跳过的原因 / 过去 24h 解密次数
  - **优先级**: P3
  - **预估**: S / S
  - **依赖**: 无
  - **价值**: 诊断"数据为何落后"这类问题不用看日志
  - **风险**: 低

- [ ] **定时模式（cron 式）**: 每天固定时间（如凌晨 3 点）触发全量/增量解密
  - **接口**: 配置中加 `AutoDecryptSchedule: "0 3 * * *"`（cron 表达式）
  - **优先级**: P3
  - **预估**: M / S
  - **依赖**: cron 库（项目当前未引入）
  - **价值**: 精准匹配"每天凌晨查一次"的使用场景
  - **风险**: 新增依赖；用户时区处理需要谨慎