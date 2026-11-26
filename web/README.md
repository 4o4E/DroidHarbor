# DroidHarbor 网页

React/TypeScript、Vite、shadcn Base UI Nova；通过 Go 同源入口使用真实 API、SSE、WebRTC 与 scrcpy 控制。

- 开发命令、验证与交付：[项目规则](../AGENTS.md)。
- 系统边界与媒体生命周期：[架构设计](../docs/架构设计.md)。
- 逐场景 UI/权限合同：[网页规格](../openspec/changes/build-droidharbor-v1/specs/web-workbench/spec.md)及[媒体与偏好增量](../openspec/changes/device-preview-audio-settings/specs/)。

设备卡随设备状态自动更新。发现未授权、身份待核验或输入被拒绝等问题时，状态旁会出现警告标记；悬浮或用键盘聚焦查看原因和处理建议，点击复制同一份详情到电脑剪贴板。诊断只读，不注入按键、不重连、不配对，也不改变任务分配。未检测到触控能力不算故障；输入拒绝只取自当前链路上仍在运行的采集，超过一分钟即不再作为当前证据。

连接按整台设备判断：USB 或 Wi‑Fi 任一路已核验可用，设备卡就不提示处理备用链路；当前使用中的链路、画面和输入故障仍正常提示。
