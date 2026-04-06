# WeChat V2 图片密钥提取 — 调试记录

## 问题描述

微信 4.x (V2 格式) 的 `chatlog key` 命令无法提取图片解密密钥 (Image Key)，扫描 60 秒超时后失败。

## 根因分析

经过多轮调试，最终定位到 **两个根因**：

### 根因 1: 密钥验证器只检查了 JPEG 和 WXGF 格式

原始代码在验证候选密钥时，仅检查解密结果是否以 JPEG (`FF D8 FF`) 或 WXGF (`77 78 67 66`) 头部开始。但实际微信图片可能是 PNG、GIF、WEBP 等格式，导致正确的密钥被验证器误判为无效。

**修复**: 扩展验证逻辑，检查所有已知图片格式头 (JPG, PNG, GIF, TIFF, BMP, WXGF, WEBP/RIFF)。

### 根因 2: DLL 函数查找导致 panic

`wx_key.dll` 使用 `LazyProc` 加载导出函数，`NewProc()` 不会返回 nil（它是懒加载的），实际查找在 `Call()` 时触发 `mustFind` 并 panic。当 DLL 版本不匹配时（缺少 `InitializeHook` 等函数），程序直接崩溃。

**修复**: 在 `init()` 中使用 `Find()` 方法提前检查每个函数是否存在，不存在则标记 `dllAvailable = false`。

## 调试过程中的其他改进

### 多进程扫描

微信 4.x 运行时会有多个 `Weixin.exe` 进程（主进程 + 子进程）。密钥可能存在于任意一个子进程的内存中。

**改进**: `findAllWeixinPIDs()` 收集所有 Weixin 进程 PID，`findMemory()` 为每个进程启动独立的 producer goroutine 并行扫描。

### 内存区域类型扩展

原始代码仅扫描 `MEM_PRIVATE` 类型的内存区域。

**改进**: 扩展为 `MEM_PRIVATE | MEM_MAPPED | MEM_IMAGE`，与上游 wx_key (Dart) 一致。

### 字符集扩展

原始代码仅搜索小写字母+数字 (`[a-z0-9]`)。

**改进**: 扩展为大小写字母+数字 (`[a-zA-Z0-9]`)，与上游 wx_key 和 wechat-decrypt 一致。

### 密钥长度匹配

同时搜索 32 字符和 16 字符的候选：
- 32 字符候选: 取前 16 字节作为 AES-128 key（主要命中路径）
- 16 字符候选: 直接作为 AES-128 key

这与 wechat-decrypt 项目的 `RE_KEY32` / `RE_KEY16` 正则逻辑一致。

## V2 格式 DAT 文件结构

```
偏移    长度    内容
0x00    6B      签名: 07 08 56 32 08 07 (即 07 08 'V' '2' 08 07)
0x06    4B      AES 密文块大小 (小端序 uint32)
0x0A    4B      XOR 密文块大小 (小端序 uint32)
0x0E    1B      对齐填充字节
0x0F    动态    AES-128-ECB 加密区块 (图片头部)
...     动态    未加密明文区块 (图片主体)
...     动态    单字节 XOR 加密区块 (图片尾部)
```

## V2 密钥特征

- **格式**: 16 字节 ASCII 字母数字字符串 `[a-zA-Z0-9]{16}`
- **加密算法**: AES-128-ECB
- **易失性**: 密钥仅在用户查看图片时短暂存在于进程内存中
- **存储形态**: 在内存中通常作为 32 字符字符串的一部分出现，取前 16 字节即为有效 AES key
- **XOR Key**: 通过 JPEG 尾部标记 `FF D9` 的已知明文攻击自动推算

## 参考项目

- [wx_key](https://github.com/ycccccccy/wx_key) — Flutter + C++ 实现，Dart 层内存扫描 + C++ DLL Hook
- [wechat-decrypt](https://github.com/ylytdeng/wechat-decrypt) — Python 实现，`find_image_key.py` 使用正则匹配
