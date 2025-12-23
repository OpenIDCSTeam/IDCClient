# 数据库保存问题修复说明

## 问题描述

从日志中可以看到数据库保存过程中存在以下问题：
```
{"level":"info","msg":"更新主机 host0，接收到的数据: map[...]","time":"2025-12-23T20:26:42+08:00"}
{"level":"info","msg":"主机 host0 配置已保存到数据库","time":"2025-12-23T20:26:42+08:00"}
```

尽管日志显示保存成功，但实际数据可能没有正确保存到数据库中。

## 根本原因

### 1. JSON序列化错误被忽略
在 `host_config.go` 的 `SetHostConfig` 方法中：
```go
// 修复前
systemMapsJSON, _ := json.Marshal(config.SystemMaps)
imagesMapsJSON, _ := json.Marshal(config.ImagesMaps)
```

使用 `_` 忽略了JSON序列化错误，如果序列化失败，字段会是空的 `[]byte{}`。

### 2. 缺少详细的错误处理
- 没有检查数据库操作是否真的影响了行数
- 错误信息不够详细，难以定位问题
- SaveAll 方法的失败没有被正确处理和报告

### 3. 事务处理不完善
VM配置保存中的事务处理缺少详细的错误检查和回滚处理。

## 修复内容

### 1. 修复JSON序列化错误处理 (host_config.go)
```go
// 修复后
systemMapsJSON, err := json.Marshal(config.SystemMaps)
if err != nil {
    return fmt.Errorf("序列化system_maps失败: %w", err)
}
```

### 2. 改进数据库操作验证
```go
// 检查是否真的影响了行数
rowsAffected, err := result.RowsAffected()
if err != nil {
    // 无法获取影响的行数，但不认为是致命错误
} else if rowsAffected == 0 {
    return fmt.Errorf("保存主机配置失败: 没有行被影响")
}
```

### 3. 改进主机管理器错误处理 (host_manager.go)
- 在 `UpdateHost` 方法中添加回滚机制
- 改进 `SaveAll` 方法的错误计数和日志记录

### 4. 改进API错误处理 (host_api.go)
```go
// 成功后保存所有数据，与Python版本保持一致
if !r.hostManager.SaveAll() {
    r.logger.Error("保存数据失败")
    r.apiError(c, 500, "主机更新成功，但保存数据失败")
    return
}
```

### 5. 改进VM配置DAO错误处理 (vm_config.go)
- 添加详细的错误消息
- 改进事务处理
- 添加操作计数和调试日志

## 测试验证

创建了 `test_db_fix.go` 文件来验证修复：
1. 测试JSON序列化是否正常工作
2. 测试数据库保存和读取是否一致
3. 验证复杂数据结构（map、slice）的序列化

## 预期效果

修复后应该能够：
1. 正确捕获和报告JSON序列化错误
2. 确保数据库操作真正生效
3. 提供详细的错误信息便于调试
4. 在保存失败时正确回滚和报告
5. API响应能准确反映操作结果

## 使用建议

1. 运行测试文件验证修复：
   ```bash
   go run test_db_fix.go
   ```

2. 检查日志中的详细错误信息

3. 如果仍有问题，检查：
   - 数据库文件权限
   - 磁盘空间
   - SQLite数据库文件完整性

## 相关文件

- `internal/database/host_config.go` - 主机配置DAO
- `internal/database/vm_config.go` - VM配置DAO  
- `internal/host/host_manager.go` - 主机管理器
- `internal/api/host_api.go` - 主机API
- `test_db_fix.go` - 测试文件