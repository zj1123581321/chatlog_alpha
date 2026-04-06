# Chatlog 本地部署指南

从零开始编译并运行 chatlog server 的完整步骤。

## 1. 环境准备

| 依赖 | 版本要求 | 说明 |
|------|---------|------|
| Go | 1.24+ | 编译需要 |
| GCC (MinGW-w64) | - | CGO 编译 `go-sqlite3` 需要 C 编译器 |
| ffmpeg | - | 可选，dat 图片/视频转换需要，需加入系统 PATH |

## 2. 获取 wx_key.dll

项目编译不依赖此 DLL，但运行时需要它来获取微信密钥。

从上游项目 [wx_key](https://github.com/ycccccccy/wx_key) 的 Release 页下载：

- **通用版**: `dlls` Release 中的 `wx_key.dll`
- **版本专用**: 同一 Release 下有 `wx_key-4.1.x.xx.dll` 等针对特定微信版本的 DLL

下载后放到项目的 `lib/windows_x64/` 目录下：

```
lib/
└── windows_x64/
    └── wx_key.dll
```

验证加载是否成功：

```bash
./bin/chatlog.exe version
# 应看到: wx_key.dll 加载成功，将使用DLL方式获取密钥
```

如果加载失败，程序会降级为原生内存扫描方式（功能受限）。

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

### 4.1 数据库密钥 (Data Key)

**方式一：通过 chatlog TUI 获取**

```bash
./bin/chatlog.exe
```

启动 TUI 后：
1. 启动微信（先不登录）
2. 等待 chatlog 识别到微信 PID
3. 登录微信
4. 程序通过 DLL 自动捕获密钥

**方式二：通过 wx_key 工具获取**

从 [wx_key Releases](https://github.com/ycccccccy/wx_key/releases) 下载 `wx_key-windows-vX.X.X.zip`，解压后运行 `wx_key.exe`（Flutter GUI 应用）。

### 4.2 图片密钥 (Image Key)

微信 4.x 的图片使用独立密钥加密，获取方式：

1. 确保微信已登录
2. 在 chatlog TUI 选择「获取图片密钥」，或在 wx_key 工具中操作
3. 在微信中打开朋友圈或聊天图片（点击大图），重复 2-3 次
4. 工具会通过内存扫描自动获取密钥

> 没有 img-key 时，聊天记录、联系人等功能正常，仅图片解密不可用。

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

## 6. 启动 Server

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

### 使用启动脚本

编辑项目根目录的 `start_server.bat`，填入你的密钥和路径，然后双击运行。

### 首次启动

首次启动时，程序会自动将加密数据库解密到 work-dir。后续启动如果 work-dir 已有数据，会跳过已解密的文件。开启 `--auto-decrypt` 后会监控数据目录，自动解密新增数据。

## 7. 使用

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

## 8. MCP 集成

Server 同时提供 MCP (Model Context Protocol) 接口，可与 AI 助手集成使用。

## 常见问题

**Q: 解密报错 `unsupported platform: v0`**
A: 启动时必须指定 `--platform windows --version 4`。

**Q: API 返回 `db file not found`**
A: 需要指定 `--work-dir` 参数，程序需要一个目录存放解密后的数据库。

**Q: wx_key.dll 加载失败**
A: 确认 DLL 放在 `lib/windows_x64/wx_key.dll`，且路径中不含中文。

**Q: 图片无法显示**
A: 需要 img-key。通过 TUI 或 wx_key 工具获取后加 `--img-key` 参数。
