# Chatlog 本地部署指南

从零开始编译并运行 chatlog server 的完整步骤。

## 1. 环境准备

| 依赖 | 版本要求 | 说明 |
|------|---------|------|
| Go | 1.24+ | 编译需要 |
| GCC (MinGW-w64) | - | CGO 编译 `go-sqlite3` 需要 C 编译器 |
| ffmpeg | - | 可选，dat 图片/视频转换需要，需加入系统 PATH |

## 2. 获取 wx_key.dll（可选）

项目编译不依赖此 DLL。程序内置了原生内存扫描方式获取密钥，DLL 只是一个可选的加速方案。

如需使用 DLL 方式，从上游项目 [wx_key](https://github.com/ycccccccy/wx_key) 的 Release 页下载对应微信版本的 DLL，放到 `lib/windows_x64/wx_key.dll`。

> DLL 导出函数与微信版本强绑定，版本不匹配时程序会自动降级为原生扫描，不影响使用。

## 3. 编译

```bash
# 安装依赖
go mod tidy

# 编译
CGO_ENABLED=1 go build -trimpath -o bin/chatlog.exe main.go

# 或使用 Makefile
make build
```

## 4. 获取微信密钥

微信 4.x 有两把独立的密钥，均通过扫描运行中的微信进程内存获取。

### 一键获取

```bash
./bin/chatlog.exe key
```

程序会自动检测微信进程并提取两把密钥。成功后输出：

```
Data Key: [xxxxxxxx...]
Image Key: [xxxxxxxx...]
```

密钥会自动保存到配置文件，后续启动 server 时无需再次手动指定。

### 4.1 数据库密钥 (Data Key)

- 用于解密 SQLCipher 加密的聊天记录数据库
- 微信登录期间**长驻内存**，提取成功率高
- 绑定账号 + 设备，**同一设备同一账号下保持不变**

获取条件：微信已登录即可。

### 4.2 图片密钥 (Image Key)

- 用于解密 V2 格式的 `.dat` 图片文件（AES-128-ECB）
- 密钥具有**易失性**，仅在查看图片时短暂加载到内存中
- 格式为 16 字符 ASCII 字母数字字符串（`[a-zA-Z0-9]{16}`）
- 同一设备同一账号下通常保持不变

获取步骤：

1. 确保微信已登录
2. 运行 `./bin/chatlog.exe key`
3. **在扫描的 60 秒内，切到微信点击打开几张图片（放大查看原图）**
4. 程序扫描所有 Weixin 子进程的内存，匹配并验证候选密钥

可选参数：

| 参数 | 说明 |
|------|------|
| `-f` / `--force` | 强制重新扫描（忽略已缓存的密钥） |
| `-x` / `--xor-key` | 同时显示 XOR Key |
| `-p <pid>` | 多开微信时指定进程 PID |

### 密钥是否会变？

两把密钥绑定**账号 + 设备**，正常使用中保持不变。以下情况可能导致密钥变化：

- 微信重新登录（退出后重新扫码）
- 微信大版本更新
- 重装微信或更换设备

如果发现解密失败，用 `chatlog key -f` 强制重新获取即可。

> 没有 Image Key 时，聊天记录、联系人等功能正常，仅图片解密不可用。

## 5. 确认微信数据目录

微信 4.x 的数据目录通常为：

```
<自定义路径>/xwechat_files/<wxid_xxx>/
```

可在微信「设置 → 文件管理」中查看。目录结构如下：

```
xwechat_files/
└── wxid_xxx/
    ├── db_storage/       # 加密数据库
    │   ├── message/
    │   ├── contact/
    │   ├── session/
    │   └── ...
    ├── msg/              # 媒体文件 (图片/视频)
    └── chatlog.json      # chatlog 自动生成的配置
```

## 6. 数据库解密

### 解密机制

Server 启动时会自动处理解密，无需手动操作：

- **work-dir 为空** → 自动全量解密所有数据库到 work-dir
- **数据库加载失败** → 自动重新解密

`--auto-decrypt` 参数启用持续监控模式：
- 使用 `fsnotify` 监控 data-dir 中 `.db` 文件的变化
- 检测到文件写入/创建后，等待 1 秒防抖，然后自动解密变更的文件
- 支持 WAL 模式的增量解密
- 解密失败时自动停止服务并报错（熔断机制）

### 单次手动解密

如果只需要解密数据库而不启动 server，可以使用独立的 `decrypt` 命令：

```bash
./bin/chatlog.exe decrypt \
  --platform windows \
  --version 4 \
  --data-dir "<微信数据目录>" \
  --data-key "<数据库密钥>" \
  --work-dir "<解密输出目录>"
```

### 图片批量解密

图片（`.dat` 文件）的解密是独立的流程，需要 img-key：

```bash
./bin/chatlog.exe batch-decrypt \
  --data-dir "<微信数据目录>" \
  --data-key "<图片密钥>" \
  ...
```

## 7. 启动 Server

### 命令行启动

```bash
./bin/chatlog.exe server \
  --platform windows \
  --version 4 \
  --data-dir "<微信数据目录>" \
  --data-key "<数据库密钥>" \
  --work-dir "<解密输出目录>" \
  --auto-decrypt
```

可选参数：
- `--img-key "<图片密钥>"` — 启用图片解密
- `--addr "0.0.0.0:5030"` — 自定义监听地址
- `--auto-decrypt` — 启用持续监控，自动解密新数据（推荐开启）

### 使用启动脚本

项目根目录提供了 `start_server.bat` 启动脚本，通过环境变量配置：

1. 创建 `.env.bat` 文件（已被 gitignore 排除）：
   ```bat
   set "CHATLOG_DATA_DIR=<微信数据目录>"
   set "CHATLOG_WORK_DIR=<解密输出目录>"
   set "CHATLOG_DATA_KEY=<数据库密钥>"
   set "CHATLOG_IMG_KEY=<图片密钥，可留空>"
   set "CHATLOG_ADDR=<监听地址，可留空>"
   ```
2. 双击 `start_server.bat` 即可启动

也可以通过系统环境变量设置上述变量，无需 `.env.bat`。

## 8. 使用

启动成功后访问：

| 地址 | 说明 |
|------|------|
| `http://localhost:5030` | 管理控制台 (Web UI) |
| `http://localhost:5030/api/v1/contact` | 联系人列表 |
| `http://localhost:5030/api/v1/chatlog` | 聊天记录查询 |
| `http://localhost:5030/api/v1/session` | 会话列表 |
| `http://localhost:5030/api/v1/chatroom` | 群聊列表 |
| `http://localhost:5030/api/v1/sns` | 朋友圈 |
| `http://localhost:5030/api/v1/db` | 数据库列表 |
| `http://localhost:5030/api/v1/db/query?sql=SELECT...` | SQL 查询 |

API 支持 `?format=json` 参数返回 JSON 格式。

## 9. MCP 集成

Server 同时提供 MCP (Model Context Protocol) 接口，可与 AI 助手集成使用。

## 常见问题

**Q: 解密报错 `unsupported platform: v0`**
A: 启动时必须指定 `--platform windows --version 4`。

**Q: API 返回 `db file not found`**
A: 需要指定 `--work-dir` 参数，程序需要一个目录存放解密后的数据库。

**Q: wx_key.dll 加载失败**
A: DLL 是可选的，程序会自动降级为原生内存扫描。如需使用 DLL，确认版本与微信匹配。

**Q: 图片无法显示**
A: 需要 Image Key。运行 `chatlog key`，同时在微信中点击查看几张图片触发密钥加载。

**Q: 图片密钥扫描超时**
A: 确保在 60 秒扫描期间**主动点击打开微信图片**（放大查看原图）。密钥仅在查看图片的瞬间存在于内存中。
