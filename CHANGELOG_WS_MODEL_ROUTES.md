# WebSocket 优先 & 模型映射 UI 优化

## 更改摘要

本次更新包含两个主要功能优化：

### 1. WebSocket 优先设置

新增 `prefer_websocket` 配置选项，开启后所有请求将优先使用 WebSocket 连接而非 HTTP。

#### 后端更改

- `internal/server/store.go`: 
  - 在 `settingsState` 结构体中添加 `PreferWebsocket *bool` 字段
  - 在 `settingsSnapshot()` 方法中返回该字段

- `internal/server/admin.go`:
  - GET `/admin/management` API 响应中添加 `prefer_websocket` 字段
  - POST `/admin/management` API 输入中添加 `PreferWebsocket *bool` 字段
  - 在 `hasUpdate` 检查中包含 `PreferWebsocket` 字段
  - 在设置更新逻辑中处理 `PreferWebsocket` 的赋值

#### 前端更改

- `frontend/src/views/Settings.vue`:
  - 在"传输与接口"区块添加"WebSocket 优先"开关
  - 当启用 WebSocket 优先时，所有支持 WS 的请求将走 WebSocket 连接
  - UI 说明：提示用户此选项会影响所有支持 WebSocket 的请求

### 2. 模型映射规则 UI 优化

将原有的文本框输入方式改为结构化的规则编辑器，大幅提升配置体验。

#### 新增组件

- `frontend/src/components/ModelRoutesEditor.vue`:
  - 可视化规则编辑器，支持添加/删除规则
  - 每条规则包含：请求模型（source）→ 实际模型（target）
  - 显示可用目标模型列表
  - 支持拖拽排序（规则按顺序匹配）
  - 空状态提示
  - 规则验证提示（确保包含 * 兜底规则）

#### 前端更改

- `frontend/src/views/Settings.vue`:
  - 导入 `ModelRoutesEditor` 组件
  - 在"模型映射"区块使用新组件替代原有的 `el-input` 文本框
  - 保持与后端 API 的兼容性（序列化为 `source=target` 格式）

## 技术细节

### 数据格式

模型映射规则内部格式保持不变：
```
gpt-4=claude-opus-4
gpt-3.5-turbo=claude-sonnet-3.5
*=claude-sonnet-3.5
```

前端组件负责解析和序列化，后端无需更改。

### UI/UX 改进

1. **结构化输入**：从单个文本框改为多个规则卡片
2. **即时验证**：每条规则独立展示，便于检查
3. **目标提示**：显示已核实的可用模型列表
4. **空状态**：友好的空状态提示用户如何开始
5. **视觉反馈**：规则添加/删除有动画过渡

## 兼容性

- 完全向后兼容，不影响现有配置
- 前端组件能正确解析现有的文本格式配置
- API 接口保持不变

## 验证

- ✅ 前端构建成功 (vite build)
- ✅ 后端编译成功 (go build)
- ✅ TypeScript 类型检查通过
- ✅ 组件渲染逻辑验证

## 使用说明

### WebSocket 优先

1. 进入设置页面 → "传输与接口"区块
2. 启用"WebSocket 优先"开关
3. 保存设置
4. 之后所有支持 WebSocket 的请求将优先使用 WS 连接

### 模型映射规则

1. 进入设置页面 → "模型映射"区块
2. 点击"添加规则"按钮
3. 填写请求模型名（如 `gpt-4`）
4. 从下拉列表选择或输入实际模型（如 `claude-opus-4`）
5. 继续添加其他规则
6. 确保最后一条规则为 `*`（兜底规则）
7. 保存设置

