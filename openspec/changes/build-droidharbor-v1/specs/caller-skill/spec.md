# Spec Delta

## Purpose

为不同 harness 中运行的 AI 提供统一且可执行的设备池使用说明与辅助 CLI，使 Windows 和 Linux 调用方完成认领、连接、原生操作、续租和释放，并正确处理任务异常而不依赖特定运行器的内部生命周期协议。

## ADDED Requirements

### Requirement: 随产品交付完整调用流程
平台 SHALL 随版本交付一个包含产品 Skill 及 Windows/Linux amd64 CLI 的客户端 Skill ZIP，覆盖能力检查、按标签认领、建立连接、观察设备状态、原生 adb 操作、显式续租、查询和释放；产品 Skill MUST 与项目开发 skills 区分。

#### Scenario: 新调用方完整使用
- **WHEN** 调用方按发布的 Skill 配置平台地址和凭证
- **THEN** 可以使用同一套说明完成真实设备认领、连接、截图/操作、续租和释放，不依赖未交付的辅助脚本或占位接口

#### Scenario: 安装客户端 Skill 包
- **WHEN** 调用方解压并安装发布的 `droidharbor` Skill 目录
- **THEN** 按该目录选择 `scripts/windows-amd64/droidharbor-cli.exe` 或 `scripts/linux-amd64/droidharbor-cli` 的绝对路径，Linux 首次赋予可执行权限；无需另行下载 CLI 或写入全局 PATH，原生 ADB 仍由调用机提供

#### Scenario: 领用保留应用状态的手机
- **WHEN** 新任务成功认领上一任务释放的手机
- **THEN** Skill 指导先观察当前画面/设备状态，不假定应用已关闭、页面已复位或账号重新登录

### Requirement: Windows/Linux 且不限制 harness
CLI SHALL 在发布矩阵中的 Windows/Linux 提供相同语义的命令、退出码和机器可读结果；MUST NOT 要求特定 harness 插件、父进程名称、内部 session 协议或结束钩子。

#### Scenario: 不原生加载 Skill 的运行器
- **WHEN** harness 只能执行标准命令并读取输出
- **THEN** 仍可按产品说明完成任务流程，不需要另建控制协议或安装专用插件

#### Scenario: 跨系统使用
- **WHEN** Windows 与 Linux 调用方分别执行同一任务流程
- **THEN** 认领、续租、连接状态和错误语义一致，路径及进程管理差异不泄露为业务合同差异

### Requirement: 每任务独立本地 ADB 环境
CLI MUST 为每个任务使用独立本地 ADB Server、任务密钥/配置和 loopback 设备入口，显式选择本租约目标；MUST NOT 修改或终止调用机默认 ADB 实例，也不得自动连接无关本地设备。Windows 保留官方原生调用，允许官方 adb 读取默认用户密钥及缺失时生成，但 CLI MUST NOT 覆盖或删除已有默认密钥，也 MUST NOT 宣称环境变量已隔离 Windows 默认用户配置目录。

#### Scenario: 两个任务同时运行
- **WHEN** 同一调用机同时连接两个有效任务
- **THEN** 两者命令使用各自实例和设备身份，不能因默认设备选择而操作另一任务手机

#### Scenario: 调用机已有 ADB 工作
- **WHEN** CLI 启动或清理任务时调用机已有独立 ADB 实例和本地 USB 设备
- **THEN** 该实例和设备连接不被 CLI kill-server、清理全部转发或自动抢占 USB 所破坏

#### Scenario: Windows 默认密钥与任务密钥同时存在
- **WHEN** Windows 原生 adb 同时加载默认用户密钥和 CLI 生成的任务密钥
- **THEN** 平台仅接受登记任务密钥的认证，不将默认密钥授予任务权限，且用户无需手工管理任务密钥

### Requirement: 连接进程不自动续租
CLI SHALL 按任务运行连接进程，不建立全局常驻守护服务；续租必须由调用方显式命令触发。Skill SHALL 说明长操作期间也要在 15 分钟内续租，正常完成显式释放。

#### Scenario: 长命令期间显式续租
- **WHEN** 任务需要执行超过单次租期的长操作
- **THEN** 调用方可在连接持续运行时另行发出 renew，连接进程本身不会暗中延长租约

#### Scenario: 运行器未发送结束通知
- **WHEN** harness 异常退出且 CLI 没有收到结束钩子
- **THEN** 不再发生隐式续租，服务端仍在现有有效期结束时撤销访问

### Requirement: 明确结果与版本兼容
CLI SHALL 提供稳定机器可读管理结果，保留原生 adb 的真实输出和退出语义，并明确区分认证、设备不可用、租约终止、协议不支持及结果未知。发现必要版本不兼容 MUST 失败，不后台自动升级。

#### Scenario: 平台与 CLI 不兼容
- **WHEN** 建连发现双方没有兼容的必需协议版本
- **THEN** CLI 返回明确错误，不启动假连接、不静默升级或切换原始 ADB 端口

#### Scenario: 设备动作结果不确定
- **WHEN** 操作发送后连接中断且无法确认手机是否执行
- **THEN** CLI 报告结果未知，不自动重新认领、换手机或重放该操作

### Requirement: 定向本地清理与秘密保护
CLI SHALL 只清理本任务可核验归属的进程、端口和工作文件，并保护任务凭证，不将秘密写入普通日志或输出。产品 MUST 明确共享调用机可信、不提供本机任务沙箱；原生 ADB Server 的本地客户端没有任务认证，旧命令在端口复用后误用新 Server 不在隔离保证内。不互信任务需要独立网络环境，仅更换 OS 用户不足以隔离 loopback TCP 访问。

#### Scenario: 正常释放与连接退出
- **WHEN** 任务释放租约并停止连接进程
- **THEN** 服务端访问终止，本任务本地入口和专用 ADB 被清理，其他任务继续使用

#### Scenario: 崩溃后端口或 PID 被复用
- **WHEN** 后续 CLI 根据旧任务记录处理残留资源，但原 PID/端口已属于其他进程
- **THEN** CLI 不误杀或清理该进程，并明确报告不能确认归属的资源

### Requirement: 可核对的原生应用操作指引
产品 Skill SHALL 说明原生应用启动、界面观察、输入核对和登录门槛；使用当前任务作用域的官方 ADB，依据当前界面完成操作，不将命令返回成功等同于业务页面完成。

#### Scenario: 启动及搜索真实应用
- **WHEN** 调用方需要操作淘宝、拼多多或闲鱼等已安装应用
- **THEN** 根据包名解析当前 launcher，观察当前页面，通过截图或 Android 原生 UI 树选择控件；唤醒、提交查询及进入详情均核对实际结果，不依赖固定旧坐标或未验证 deep link

#### Scenario: 动态页面及输入限制
- **WHEN** 原生 UI 树无法取得静止状态，或原生 input/当前 IME 未产生预期文本
- **THEN** 以当前截图确认界面，并在提交前核对真实输入；通用中文输入使用明确批准的输入方式，不把 ASCII 演示当作 Unicode 能力，不自动修改全局键盘或动画设置

#### Scenario: 应用登录已失效
- **WHEN** 应用搜索或浏览要求重新登录
- **THEN** 请求用户手动处理并记录受阻路径，保留既有账号环境；设备标签不能代替应用当前登录有效性

#### Scenario: 减少无效观察与等待
- **WHEN** 调用方按 Skill 进行多步应用操作
- **THEN** 复用本任务已解析的启动入口，依据问题按需读取截图或 UI 树；页面未变化可复用已验证定位，导航/滚动/遮罩/旋转后重新观察，不逐步重复完整诊断或用固定等待代替结果确认

