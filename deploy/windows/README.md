# Windows 服务端

`DroidHarbor-server-windows-amd64.zip` 部署在连接手机的机器。网页和 scrcpy-server 已内嵌到 `droidharbor.exe`；无需 Node、Go、源码或单独静态目录。ADB 由外部提供，平台不会启动、替换或关闭系统 ADB。

## 首次使用

1. 解压到当前运行账户可读写的独立目录。
2. 将 `.env.example` 复制为 `.env`，默认配置可直接使用，不需要填写服务器 IP；媒体通过 WebRTC 自动协商本机/局域网地址。
3. 确认外部 ADB Server 已运行且能看到已授权手机；默认连接 `127.0.0.1:5037`，不同地址修改 `.env` 中的 `DROIDHARBOR_ADB_SERVER`。
4. 双击 `start.cmd`，无需任何参数。也可直接双击 `droidharbor.exe`。程序直接读取 EXE 同目录的 `.env`；路径均相对 `.env` 所在目录，不受启动工作目录影响。
5. 启动时自动初始化数据库和管理员凭证。首次 `admin_credentials_initialized` 事件在控制台和 `logs/platform.jsonl` 中包含 `adminToken`，即使运行日志级别设为 error 也会输出；请保存该 token。已有数据直接复用，不重置、不重复输出 token。凭证文件只存摘要，初始化日志含登录凭证，分享排障日志前去掉该字段。
6. 本机打开 `http://localhost:8080`，局域网打开 `http://服务端IP:8080`。使用管理员令牌登录。人工工作台用浏览器即可，无需安装任务客户端。

在网页“添加设备”中读取发现列表，选择设备、填写名称并点击“登记并核验”；核验成功后从卡片进入工作台。登记后核验失败会保留记录并显示“重新核验”，不重复登记。旧版本已登记但待核验的设备，可在卡片“管理”中点击“核验身份”；ADB 连接在线不等于平台身份已经绑定。维护状态仅暂停 AI 分配，不阻止已核验设备的人工投屏。

应用使用 HTTP，不配置证书、私钥或 origin。管理员登录、调用凭证和 CSRF 校验仍然生效；浏览器请求来源自动与当前请求 Host 核对。HTTP 不加密网络内容，仅在可信内网/隔离网络使用。已实测通过局域网 IP 的 HTTP 页面投屏两台真机；该页面不能调用浏览器系统剪贴板 API，可手动选择文本复制，不影响投屏连接。

## .env 配置

模板包含常用项；未声明的可选项使用默认值，保留模板默认值或注释该行即可。进程环境中的同名变量优先于文件，便于服务/容器部署。支持引号、注释、Windows CRLF 和 UTF-8 BOM。

| 项目 | 配置名 | 默认/说明 |
| --- | --- | --- |
| 网页/API | `DROIDHARBOR_LISTEN` | `0.0.0.0:8080` |
| 媒体 UDP | `DROIDHARBOR_RTC_LISTEN` | `0.0.0.0:8081` |
| 媒体地址覆盖 | `DROIDHARBOR_RTC_ADVERTISED_IP` | 可选，默认自动协商；特殊网络才指定 IPv4 |
| 外部 ADB | `DROIDHARBOR_ADB_SERVER` | `127.0.0.1:5037` |
| 数据库 | `DROIDHARBOR_DATABASE` | `data/droidharbor.db` |
| 管理员凭证摘要 | `DROIDHARBOR_ADMIN_FILE` | `data/admin.json` |
| 上传暂存 | `DROIDHARBOR_TRANSFER_TEMP_DIR` | `data/transfers` |
| 运行日志 | `DROIDHARBOR_LOG_FILE` | `logs/platform.jsonl` |
| 日志级别 | `DROIDHARBOR_LOG_LEVEL` | `info`，排障可改 `debug` |

高级可选项：`DROIDHARBOR_ADMIN_SESSION_TTL`（`12h`）、`DROIDHARBOR_ADB_TIMEOUT`（`10s`）、`DROIDHARBOR_TRANSFER_MAX_UPLOAD_BYTES`（268435456）、`DROIDHARBOR_TRANSFER_MAX_ACTIVE`（8）、`DROIDHARBOR_TRANSFER_MAX_COMMAND_OUTPUT`（65536）。明确覆盖内嵌资源时才设置 `DROIDHARBOR_WEB_DIR`、`DROIDHARBOR_SCRCPY_SERVER`。

## 停止、升级与排障

- 保持启动窗口运行；Ctrl+C 停止。启动失败以非零退出码退出，`start.cmd` 暂停窗口以便查看错误。缺 `.env`、格式或值无效时明确指出文件/配置项，不要求补命令行参数。
- 升级前停止程序并备份，保留 `.env` 和 `data`。已有 `.env` 中如填写过 `DROIDHARBOR_RTC_ADVERTISED_IP`，删除或注释该行即可使用自动协商。发布包不包含真实 `.env`、凭证或数据库，不覆盖现有运行数据。
- 日志同时输出到控制台和 `DROIDHARBOR_LOG_FILE`；每文件 20 MiB，最多保留 5 份压缩历史。文件创建失败直接退出，运行中写日志失败直接报告 stderr。
- 用发生时间、requestId、关联设备/会话查故障。首次管理员初始化事件包含 token；其余事件不记录凭证、配对码、完整 shell 命令、剪贴板或设备原始输出。日志目录按凭证资料保护。
- 采集异常日志的原因链包含启动/回收阶段和内部固定错误原因。设备在线但处于隔离时，在设备管理执行“重新核验并回收”，状态恢复后再连接视频；回收只处理平台自有资源，不清手机应用或停止外部 scrcpy。
- 已登记手机等待授权或核验时，在手机处理确认后直接从设备列表点“同步状态”。正常在线或未连接的设备不显示该快捷入口；同步不重新配对或解除维护。
- 开始投屏前先点亮手机。程序不自动唤醒或保持亮屏；若未取得关键帧，点亮后点击“重新连接视频”。
- 首次初始化随启动自动完成，无需单独脚本。需要轮换凭证时使用 `droidharbor.exe admin-init --admin-file 新文件路径`，再修改 `.env` 的管理员文件路径。已有摘要文件损坏会明确失败，不覆盖重置。
- 本包不安装 Windows Service，不配置系统网络。服务部署可用 `serve --env-file 配置文件绝对路径`，运行参数仍由该文件读取。

## 重建与客户端

项目根目录执行 `npm run package:windows`，需要开发工具链和锁定 scrcpy-server。输出 `release/windows-amd64-时间戳` 下独立的服务端和客户端 ZIP；已有输出根目录拒绝覆盖。任务调用机使用 `DroidHarbor-client-windows-amd64.zip`，其 CLI、Skill 和说明与服务端分开分发。
