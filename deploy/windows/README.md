# Windows 服务端

`DroidHarbor-server-windows-amd64.zip` 部署在连接手机的机器。网页和 scrcpy-server 已内嵌到 `droidharbor.exe`；无需 Node、Go、源码或单独静态目录。ADB 由外部提供，平台不会启动、替换或关闭系统 ADB。

## 首次使用

1. 解压到当前运行账户可读写的独立目录。
2. 确认外部 ADB Server 已运行并可见已授权手机；默认使用 `127.0.0.1:5037`。
3. 双击 `droidharbor.exe`。控制台直接启动，无需脚本或参数；缺少同目录 `.env` 时自动生成完整配置和随机 `DROIDHARBOR_ADMIN_TOKEN`，同时初始化数据库。路径相对 `.env`，不受启动工作目录影响。
4. 首次 `admin_credentials_initialized` 事件向控制台和日志交付初始 `adminToken`，不受普通日志级别过滤；重启不重复输出。令牌明文保存在 `.env`，正常运行日志不回显。保护整个运行目录，分享初始化日志前移除 token。
5. 本机打开 `http://localhost:8080`，局域网打开 `http://服务端IP:8080`，使用 `.env` 中的令牌登录。人工工作台无需安装任务客户端；媒体自动协商地址，无需填写服务器 IP。

网页“添加设备”支持“一键添加所有设备”：使用手机型号命名，已登记链路不再显示，同机 USB/Wi-Fi 经硬件核验后归到同一设备。未授权手机保留待核验，完成手机确认后点设备列表“同步状态”。也可单独选择发现设备或手动填写地址；核验失败保留已登记记录，错误统一出现在全局通知。维护仅暂停 AI 分配，不阻止已核验设备人工投屏。

应用使用 HTTP，不配置证书、私钥或固定 origin。令牌、会话和 CSRF 校验保持生效。桌面 Chrome/Edge 是本轮目标；聚焦手机画面后直接 Ctrl+V，使用浏览器粘贴事件，无需 HTTPS 系统剪贴板 API。剪贴板历史按设备保存在当前登录页面内存中，最多 20 条；刷新或退出清空，不自动写入电脑系统剪贴板。

## .env 配置

首次生成的 `.env` 包含全部常用项；登录令牌始终以该文件为准，其他配置可由同名进程环境变量覆盖，便于服务/容器部署。支持引号、注释、Windows CRLF 和 UTF-8 BOM。

| 项目 | 配置名 | 默认/说明 |
| --- | --- | --- |
| 网页/API | `DROIDHARBOR_LISTEN` | `0.0.0.0:8080` |
| 媒体 UDP | `DROIDHARBOR_RTC_LISTEN` | `0.0.0.0:8081` |
| 媒体地址覆盖 | `DROIDHARBOR_RTC_ADVERTISED_IP` | 可选，默认自动协商；特殊网络才指定 IPv4 |
| 外部 ADB | `DROIDHARBOR_ADB_SERVER` | `127.0.0.1:5037` |
| 数据库 | `DROIDHARBOR_DATABASE` | `data/droidharbor.db` |
| 登录令牌 | `DROIDHARBOR_ADMIN_TOKEN` | 首次自动随机生成，明文保存在 `.env` |
| 上传暂存 | `DROIDHARBOR_TRANSFER_TEMP_DIR` | `data/transfers` |
| 运行日志 | `DROIDHARBOR_LOG_FILE` | `logs/platform.jsonl` |
| 日志级别 | `DROIDHARBOR_LOG_LEVEL` | `info`，排障可改 `debug` |

高级可选项：`DROIDHARBOR_ADMIN_SESSION_TTL`（`12h`）、`DROIDHARBOR_ADB_TIMEOUT`（`10s`）、`DROIDHARBOR_TRANSFER_MAX_UPLOAD_BYTES`（268435456）、`DROIDHARBOR_TRANSFER_MAX_ACTIVE`（8）、`DROIDHARBOR_TRANSFER_MAX_COMMAND_OUTPUT`（65536）。明确覆盖内嵌资源时才设置 `DROIDHARBOR_WEB_DIR`、`DROIDHARBOR_SCRCPY_SERVER`。

## 停止、升级与排障

- 保持控制台窗口运行；Ctrl+C 停止。启动失败非零退出；从资源管理器双击启动时保留错误窗口，已有 CMD/PowerShell 或服务不等待输入。缺配置自动生成；已有配置格式、类型或必需令牌无效时明确失败，不覆盖修复。
- 升级前停止并备份 `.env` 与 `data`。发布包仅包含 EXE 和说明，不包含运行配置、凭证或数据库。旧版数据由部署者自行清理，本版不迁移独立凭证摘要文件。
- 日志同时输出到控制台和 `DROIDHARBOR_LOG_FILE`；每文件 20 MiB，最多保留 5 份压缩历史。文件创建失败直接退出，运行中写日志失败直接报告 stderr。
- 用发生时间、requestId、关联设备/会话查故障。首次管理员初始化事件包含 token；其余事件不记录凭证、配对码、完整 shell 命令、剪贴板或设备原始输出。日志目录按凭证资料保护。
- 采集异常日志的原因链包含启动/回收阶段和内部固定错误原因。设备在线但处于隔离时，在设备管理执行“重新核验并回收”，状态恢复后再连接视频；回收只处理平台自有资源，不清手机应用或停止外部 scrcpy。
- 已登记手机等待授权或核验时，在手机处理确认后直接从设备列表点“同步状态”。正常在线或未连接的设备不显示该快捷入口；同步不重新配对或解除维护。
- 开始投屏前先点亮手机。程序不自动唤醒或保持亮屏；若未取得关键帧，点亮后点击“重新连接视频”。
- 轮换登录令牌：停止平台，修改 `.env` 的 `DROIDHARBOR_ADMIN_TOKEN` 后重启。启动撤销其他凭证版本的旧登录会话，改回旧令牌也不会复活已撤销会话；不需要独立初始化命令或脚本。
- 本包不安装 Windows Service，不配置系统网络。服务部署可用 `serve --env-file 配置文件绝对路径`，运行参数仍由该文件读取。
- 新登记设备默认上限 60 fps、目标 12 Mbps；下拉支持 30/60/90/120 fps 与 4/8/12/20/40 Mbps。长边仍为后端策略，默认 1920；已有设备配置不自动改写。
- 工作台左侧导航和可拖动设备侧栏，预览放大；占用图标仅表示 AI 已分配/未分配，人工观看不占用租约。离线设备置底置灰，维护/隔离/回收为独立状态。视频状态仅表示当前页面会话。
- 画质和剪贴板常驻右侧；操作栏贴着矩形手机画面，背景随浅色/深色/系统主题变化。独立小窗按可用高度和手机比例计算初始宽度，保留三档尺寸，手动调整后不持续重置。

## 重建与客户端

项目根目录执行 `npm run package:windows`，需要开发工具链和锁定 scrcpy-server。输出 `release/windows-amd64-时间戳` 下独立的服务端和客户端 ZIP；已有输出根目录拒绝覆盖。任务调用机使用 `DroidHarbor-client-windows-amd64.zip`，其 CLI、Skill 和说明与服务端分开分发。
