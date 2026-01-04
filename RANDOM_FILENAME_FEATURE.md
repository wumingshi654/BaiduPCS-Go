# 随机文件名同步功能说明

## 功能概述

为同步功能(`sync add`)添加了 `--random-filename` 选项，用于在加密上传文件时生成随机的UUID文件名，保护文件的原始名称，并在本地记录文件名映射关系。

## 使用方式

### 基本用法

```bash
# 使用随机文件名进行加密同步
BaiduPCS-Go sync add /local/path /remote/path \
  --interval 60 \
  --key your-encryption-key \
  --method aes-128-ctr \
  --random-filename
```

### 参数说明

- `--random-filename`: 启用随机文件名功能。当此选项启用时：
  - 每个加密后的文件将被生成一个随机的UUID作为文件名
  - 原始文件名和UUID的映射关系将被保存在配置文件中
  - 配置文件位于: `~/.config/BaiduPCS-Go/sync_config.json`

## 工作流程

1. **扫描本地文件**: 定期扫描指定的本地目录
2. **计算MD5**: 计算每个文件的MD5哈希值，判断文件是否有变化
3. **加密文件**: 如果文件有变化，先将文件加密
4. **生成UUID**: 如果启用了 `--random-filename`，生成一个随机UUID作为上传文件名
5. **记录映射**: 将原始相对路径 -> UUID 的映射关系保存到配置文件
6. **上传文件**: 使用UUID作为文件名上传加密文件到网盘
7. **保存状态**: 更新配置文件中的文件状态和映射关系

## 数据结构

配置文件中的映射关系示例：

```json
{
  "watches": {
    "watch_id_hash": {
      "local": "/local/path",
      "remote": "/remote/path",
      "interval": 60,
      "key": "your-encryption-key",
      "method": "aes-128-ctr",
      "random_filename": true,
      "file_name_map": {
        "550e8400-e29b-41d4-a716-446655440000": "documents/report.pdf",
        "6ba7b810-9dad-11d1-80b4-00c04fd430c8": "images/photo.jpg"
      },
      "files": {
        "documents/report.pdf": {
          "mod_time": 1234567890,
          "size": 1024000,
          "md5": "d41d8cd98f00b204e9800998ecf8427e"
        }
      }
    }
  }
}
```

## 安全性说明

- **文件名隐私**: 使用UUID作为文件名可以隐藏原始文件的名称，增强隐私性
- **加密保护**: 文件内容通过指定的加密算法进行保护
- **本地映射**: 文件名映射关系保存在本地配置文件中，仅在本地可见
- **重要提示**: 如果丢失本地配置文件，将无法直接从UUID识别原始文件

## 注意事项

1. `--random-filename` 选项只有在指定了 `--key`（加密密钥）时才会生效
2. 映射关系保存在配置文件中，因此需要妥善备份配置文件
3. 每次同步时，只有文件内容有变化(MD5不同)才会重新上传并重新生成UUID
4. 删除本地配置文件后，将无法通过配置恢复文件名映射关系

## 示例

```bash
# 创建一个同步任务，使用随机文件名加密上传
BaiduPCS-Go sync add ~/Documents /backup/documents \
  --interval 300 \
  --key mySecretKey123 \
  --method aes-256-ctr \
  --random-filename

# 查看已添加的同步任务
BaiduPCS-Go sync list

# 启动同步
BaiduPCS-Go sync start ~/Documents
```

## 代码修改总结

### 核心改动:

1. **syncmgr.go**:
   - 在 `WatchEntry` 结构中添加 `RandomFilename` 和 `FileNameMap` 字段
   - 创建 `AddWatchWithIgnoreAndRandom()` 方法支持随机文件名选项
   - 在 `scanAndUpload()` 中添加UUID生成和映射保存逻辑

2. **main.go**:
   - 在 `sync add` 命令中添加 `--random-filename` 标志选项
   - 更新命令处理逻辑以支持新参数

3. **dependencies**:
   - 添加 `github.com/google/uuid` 依赖用于生成UUID
