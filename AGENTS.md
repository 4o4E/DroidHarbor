# DroidHarbor 开发规则

## 入口与文档

- 存在 `AGENTS.local.md` 时同时读取；本机路径、端口、进程启动和受保护资料约束仅放本地文件，由 Git 忽略。
- [架构设计](docs/架构设计.md)说明系统边界，[架构决策](docs/架构决策.md)记录已确认选择；API 以 `api/openapi.yaml`、功能场景和待办以相关 OpenSpec 为准，不复制任务计数。
- `docs/` 只放架构与决策。开发日志只写 `.workspace/WORKLOG.md`，使用 UTC+8 时间；不把提交回执、测试流水或环境快照塞进共享规则/设计文档。部署用法在 `deploy/`，客户端操作在 `skills/droidharbor/SKILL.md`。

## 实施边界

- 延续 Go/Pion/SQLite 和现有 React/TypeScript、shadcn Base UI 组件；直接修改正式界面，另做原型须明确要求。架构、schema 和新增业务取舍先确认，不重新选择已定路线。
- 权限在实际派发处校验，保持任务/链路/采集代次隔离；资源按拥有者定向回收。状态未知和清理失败明确报错，不以清库、全局杀进程或静默降档恢复。
- 错误进入统一日志/通知；已有兼容回退遵循合同并说明实际结果。设备动作不自动重放，不用命令成功、HTTP 存活、旧封面或 Peer connected 冒充操作完成/视频呈现。

## 构建与验证

从项目根执行；前端或锁定采集资源变更后，先准备嵌入资源再构建/测试 Go：

```text
npm ci
npm --prefix web ci
npm run web:build
npm run assets:prepare
```

- 按改动选择最窄检查，例如媒体修改运行 `go test ./internal/media ./internal/rtc`；网页操作需在正式界面验证，不每次重复完整矩阵。
- OpenSpec 修改运行 `npm run spec -- validate <变更名> --strict`；打包修改运行 `deploy/windows/package.Tests.ps1`。协议夹具、故障注入、真机和部署现场证据分开报告，不扩大覆盖声明。
- 设备接入依赖外部 ADB；先确认实例和 USB 归属，避免 CLI 自动启动默认服务或多个实例争用手机。运行配置、凭据、数据库和初始化日志使用源码外受限私有目录；无敏感脚本和构建中间文件放 `.workspace/tmp/`。

## 发布与收尾

- 运行 `npm run package:windows`，只交付根目录 `release/droidharbor.exe`（服务端）和 `release/DroidHarbor-skill.zip`（含 Windows/Linux CLI 的客户端）；只保留最新版本，不建日期/版本子目录或常规备份。
- 通过构建和版本检查后成对发布并清旧产物；失败成对恢复，恢复本身失败明确报出并保留临时恢复资料，恢复后清理。运行 `.env`、数据库和日志保留；旧版本目录含非产物文件时先报错处理。
- 经授权提交推送后，从相同源码重建；EXE `version` 的 revision 对应交付提交且 `vcs.modified=false`。dirty 试用包明确标注；同一实现只重建/检查版本，不重复验收。仅文档修改不重打包。
- 优雅停止自有平台，按本轮资源 ID 核对回收终态，不只看退出码；只结束本轮页面、映射和自有 ADB，恢复必要手机状态，随后按清单清理私有资料并同步 TMP/REF 索引。
- 工作日志仅交接当前进度、证据入口、阻塞和剩余资源；有价值结论归入合同，已完成阶段记录及时归拢，不维持第二套现行规则。提交推送仅按用户要求执行。
