# 库使用说明

只读取本次任务涉及的部分。API 以目标项目版本和相应官方文档为准，不从本文件推断固定版本。资料核查日期：2026-10-23。

## shadcn/ui、Nova 与 Tailwind CSS

- [官方组件与说明](https://ui.shadcn.com/docs)
- [官方 CLI](https://ui.shadcn.com/docs/cli)
- [项目配置](https://ui.shadcn.com/docs/components-json)
- [Nova 等视觉样式](https://ui.shadcn.com/docs/changelog/2025-12-shadcn-create)
- [主题配置](https://ui.shadcn.com/docs/theming)
- [Tailwind 配套安装](https://ui.shadcn.com/docs/installation/manual)
- 官方 skill 的来源和加载方式见[上游 skill 说明](upstream-skills.md#shadcnui)。

使用项目包管理器运行官方 CLI；通过 `info` 读取配置、`docs` 获取组件文档、`search` 和 `view` 检查现成组件，再执行所需的 `add`。默认来源为官方 `@shadcn`，优先检查项目已经安装的组件。

Nova 减少组件内外间距，适合紧凑布局。新项目采用 Nova；既有项目的风格变更依照主 skill 的授权规则处理。字体、颜色和页面信息密度根据用户需求与已有设计确定。

读取 `components.json` 和实际组件，区分项目的 Base UI、Radix 或其他已支持基础库，不混用其属性和组合方式。通过官方 CLI 引入匹配配置的组件代码，不手工拼接预设或凭记忆复制其他版本的实现。

Tailwind 用于页面布局和语义变量，组件自身的颜色、尺寸与状态优先使用已有属性和变体。通用交互控件使用相应组件；业务页面可以用普通语义元素和 Grid / Flexbox 组织内容。

## Lucide

- [官方 React 文档](https://lucide.dev/guide/react)
- [官方图标目录](https://lucide.dev/icons/)
- [官方源码](https://github.com/lucide-icons/lucide)

新项目统一使用 `lucide-react`，按需导入图标。已有项目遵循当前图标来源，变更来源需要确认。优先复用组件内置的图标尺寸、颜色和位置规则；仅有图标的按钮需要可理解的操作名称。

需要的图标先查找目录、别名与同义项，不自行绘制 SVG 替代库图标。库没有合适图标时，按主 skill 的能力缺口流程处理。

## Motion

- [官方 React 文档](https://motion.dev/docs/react)
- [减少加载体积](https://motion.dev/docs/react-reduce-bundle-size)
- [无障碍说明](https://motion.dev/docs/react-accessibility)
- 官方 skill 的来源和加载方式见[上游 skill 说明](upstream-skills.md#motion)。

优先使用组件已有动画。进入退出、布局变化或复杂动画按需使用 Motion；简单颜色、透明度或悬停过渡可以使用 CSS。检查当前项目使用 `motion` 还是历史版本的 `framer-motion`，不因调用 skill 自动迁移或重复安装。

使用组件属性、内置过渡和官方组合方式，保留减少动态效果的用户偏好。不自行编写逐帧动画循环或手势系统替代 Motion。

普通动画使用免费库的能力。付费组件、服务或额外工具遵循用户选择与项目现有授权；不要把付费 API 当作免费包的导出。

## react-resizable-panels

- [官方源码与文档](https://github.com/bvaughn/react-resizable-panels)
- [shadcn/ui Resizable](https://ui.shadcn.com/docs/components/resizable)

仅用于用户能够调整尺寸的面板，不作为普通页面布局的默认工具。项目采用 shadcn/ui 时优先使用其 Resizable 组件。

检查底层库版本与项目封装是否匹配。不同版本可能使用不同的组件名称、方向属性和尺寸单位，不能将旧示例与新依赖混合。使用库提供的分隔线、尺寸限制、键盘操作和布局保存能力；业务需要额外保存时连接项目现有持久化机制，不重复实现拖动监听。

根据实际页面检查最小可用尺寸和窄屏行为。窄屏切换页面结构时保留用户需要的操作与内容，不单纯压缩全部面板。

## dnd-kit

- [官方当前文档](https://dndkit.com/)
- [官方源码](https://github.com/clauderic/dnd-kit)
- [官方旧版文档](https://docs.dndkit.com/)

先检查目标项目安装的包。当前 `@dnd-kit/react` 系列与旧 `@dnd-kit/core`、`@dnd-kit/sortable` 系列使用不同 API，分别读取对应文档；已有项目不自动升级，新项目根据当前官方指南选择匹配的包。

使用库提供的排序、传感器、碰撞检测、取消与键盘能力；优先采用官方方案，不自行实现指针监听与目标命中系统。业务代码负责把库输出的结果连接到真实数据与保存操作。

修改前明确可移动对象、有效目标、排序保存规则和取消行为。服务端拒绝操作或保存失败时，按项目既有错误处理方式报告失败并保持业务状态正确，不隐藏错误或展示虚假的保存成功。

与 Motion 同时使用时，由 dnd-kit 管理拖拽位置；视觉反馈采用兼容的官方方式，避免两个系统同时写入同一位置属性。

## Zustand

- [官方源码与用法](https://github.com/pmndrs/zustand)
- [官方文档入口](https://zustand.docs.pmnd.rs/)

适用于跨组件共享的客户端状态，如选中对象、跨区域草稿和界面偏好。单组件临时状态保留在组件内；服务器数据、请求缓存和实时同步遵循项目现有机制，不额外复制成另一份可独立修改的数据。

复用项目已有 store 和 selectors，只读取组件需要的状态；按当前版本文档处理对象选择、持久化和服务端渲染。不要在没有需要时增加 slices、中间件或自动生成 selectors 的抽象。

服务端渲染与持久化存储可能影响请求隔离和初始渲染；仅在涉及这些场景时查阅相应官方指南，再选择实现方式。
