# Windows 服务端试用包

`DroidHarbor-server-windows-amd64.zip` 部署在连接手机的服务端机器。包内 `droidharbor.exe` 内含网页和锁定的 scrcpy-server v5.0.1，另附 `start.cmd`、`admin-init.cmd` 和本说明；不包含任务 CLI 或 Skill。部署机器不需要 Node、Go、源码、web/dist 或独立 scrcpy 文件；ADB 由外部提供。

人工管理和工作台直接用浏览器访问服务端，无需安装客户端。任务调用机使用独立的 `DroidHarbor-client-windows-amd64.zip`，客户端包含 CLI、Skill 和客户端操作说明。

## CMD 启动

1. 解压到独立、可写目录；在 CMD 中进入该目录。保留 `data` 目录用于后续升级。
2. 首次执行 `admin-init.cmd`。终端只显示一次管理员 token，请自行安全保存。重复初始化会报错，不会替换已有凭证。
3. 外部 ADB Server 必须已经启动，且其 `adb devices -l` 能列出已授权设备。平台只连接 `--adb-server` 指定的管理端点；不会启动、替换或关闭外部 ADB。
4. 执行 `start.cmd`，在后面追加部署环境已有的启动参数。`start.cmd` 固定在解压目录运行，自动设置日志和上传暂存目录，其余参数与 `droidharbor.exe serve` 相同：

   ```cmd
   start.cmd --adb-server 127.0.0.1:5037 --listen 0.0.0.0:8443 --origin "%DH_ORIGIN%" --tls-cert "%DH_TLS_CERT%" --tls-key "%DH_TLS_KEY%" --rtc-listen 0.0.0.0:8444 --rtc-advertised-ip "%DH_RTC_IP%"
   ```

   示例变量由现有部署环境提供。本交付不配置域名、证书或网络基础设施，沿用服务已有访问协议。也可直接执行 `droidharbor.exe serve` 并自行传入所有参数。
5. 当前控制台保持运行；使用 Ctrl+C 停止。程序启动失败以非零退出码退出。`droidharbor.exe version` 查看版本和构建标识。

## 排障

- `start.cmd` 的运行日志同时显示在控制台并写入 `logs/platform.jsonl`；单文件 20 MiB，最多保留 5 份压缩历史文件。
- 需要详细请求时加 `--log-level debug`；正常试用默认 info。对照网页错误中的 `requestId` 搜索日志。
- 启动失败、ADB 清单断开、采集退出、资源回收、数据库故障均有对应阶段/错误码和错误原因类型。已知 OS 错误包含系统原因，SQLite 包含结果码；设备原始输出、SQL 绑定参数、token、配对码、剪贴板和完整 shell 命令不进日志。
- 日志文件创建失败直接退出；运行中日志写入失败直接输出到 stderr，不能以“文件没有日志”判断服务正常。
- 提交问题时附上发生时间、操作、requestId 和相关日志片段。管理员 token、ADB 私钥和证书私钥单独保管。
- 本包不安装 Windows Service、不修改系统 ADB，不进行系统级证书、域名或防火墙配置。
- 开始投屏前先点亮手机屏幕。程序不自动唤醒或保持亮屏；若未取得关键帧，点亮后点击“重新连接视频”。试用包不把占位画面当作视频成功。

## 从源码重建

在项目根目录执行 `npm run package:windows`。需要 Node/npm、项目要求的 Go 和已下载的锁定 scrcpy-server（见项目开发文档）。脚本运行前端构建、校验并准备内嵌资源，分别生成服务端和客户端目录及独立 ZIP。默认发布根目录为 `release/windows-amd64-时间戳`；根目录内 `DroidHarbor-server-windows-amd64.zip` 和 `DroidHarbor-client-windows-amd64.zip` 分别分发，已有输出根目录拒绝覆盖。

可直接调用 `powershell -NoProfile -ExecutionPolicy Bypass -File deploy/windows/package.ps1 -GoExecutable C:\path\to\go.exe -OutputDirectory C:\output\DroidHarbor` 指定工具链和新发布根目录；该根目录同时包含两类包，不是任一角色的运行目录。
