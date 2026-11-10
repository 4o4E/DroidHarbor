# Windows 客户端

`DroidHarbor-client-windows-amd64.zip` 用于任务调用机，包含 `droidharbor-cli.exe`、产品操作 `SKILL.md` 和本说明。客户端连接已部署的 DroidHarbor 服务端；不包含平台程序、网页资源、管理员初始化或服务端启动脚本。

人工设备池和工作台直接在浏览器访问服务端，无需安装此 CLI。服务端使用独立的 `DroidHarbor-server-windows-amd64.zip`。

## 使用

1. 解压到独立目录，在 CMD 中进入该目录。运行 `droidharbor-cli.exe version` 检查程序和 API major。
2. 向服务端管理员取得调用凭证，保存到仅任务运行账户可读的私有 JSON 文件。客户端使用调用凭证，不使用管理员登录 token。
3. 调用机自行提供官方 platform-tools 37.0.1 ADB；连接任务时通过 `--adb-bin` 指定其绝对路径，客户端不会下载或替换系统 ADB。
4. 按 `SKILL.md` 执行 capabilities、claim、connect、env、renew、disconnect、release。每个任务使用独立私有目录；长任务显式续租，操作结束显式释放。`connect` 在前台持续运行，不要退出后继续使用其任务入口。

示例中的变量是现有环境提供的服务地址和凭证文件路径，不是 CLI 自动读取的配置项：

```cmd
droidharbor-cli.exe version
droidharbor-cli.exe capabilities --server "%DH_SERVER%" --credential-file "%DH_CREDENTIAL_FILE%"
```

完整参数、隔离边界、原生 ADB 使用、结果未知时的处理和退出码均以同包 `SKILL.md` 为准。成功结果输出到 stdout，错误输出到 stderr；排障保留服务端 requestId，不公开凭证文件或任务秘密。
