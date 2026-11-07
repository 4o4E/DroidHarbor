# 上游 skill

核查日期：2026-10-23。以下来源已通过官方文档和源码确认。引用上游不扩大主 skill 的授权，也不自动安装依赖或配置额外服务。

## 加载方法

1. 检查当前助手的可用技能清单和目标项目的技能目录，按下面的名称与来源确认已安装 skill，避免把同名社区 skill 当作官方版本。
2. 已安装时加载其 `SKILL.md`，并沿入口只读取当前组件或功能需要的引用文件。
3. 未安装时，通过下面的官方源码入口读取说明，跟随其中相关的相对链接。直接读取单个入口不等于安装了完整 skill。
4. 需要安装时，使用官方支持的安装方式，并按照目标助手选择项目范围或用户范围；当前任务没有授权安装时先说明。安装主 skill 不代表用户同意修改全局配置。
5. 官方 skill 处理库的具体用法；主 skill 规定的复用要求、手写授权和本次用户需求继续有效。遇到不适用版本的内容时，查阅目标版本文档。

## shadcn/ui

- skill 名称：`shadcn`
- [官方 skill 介绍](https://ui.shadcn.com/docs/skills)
- [官方源码入口](https://github.com/shadcn-ui/ui/blob/main/skills/shadcn/SKILL.md)
- [完整 skill 目录](https://github.com/shadcn-ui/ui/tree/main/skills/shadcn)

涉及界面组件、预设、主题和组件组合时读取。重点按任务加载 CLI、组件组合、表单、样式、图标以及基础库差异的说明，实际引用路径以当时入口为准。

官方提供的安装命令：

```bash
npx skills add shadcn/ui --skill shadcn
```

官方 skill 可以支持多种设计选择；本项目的新项目默认值为 Nova 和 Lucide，已有项目按当前配置处理。默认组件来源已由主 skill 指定为官方 `@shadcn`；无需重复询问已确定的来源。涉及用户指定的其他来源时检查其组件与项目配置。

## Motion

- skill 名称：`motion`
- [官方 skill 介绍](https://motion.dev/docs/ai-kit-context)
- [官方安装说明](https://motion.dev/docs/ai-kit-install)
- [官方源码入口](https://github.com/motiondivision/ai-kit/blob/main/plugins/motion/skills/motion/SKILL.md)
- [完整 skill 目录](https://github.com/motiondivision/ai-kit/tree/main/plugins/motion/skills/motion)

涉及动画时读取其最佳实践和本次平台的说明。版本迁移、文档查询及额外服务按入口的条件读取，不能因为加载动画 skill 而自行执行迁移或全面性能审计。

官方安装器：

```bash
npx motion-ai
```

安装器会配置 skill 和 MCP，并提供作用范围及目标助手的选择。安装前确认这些变更属于用户已授权的范围。基础最佳实践文件可以直接读取；需要 MCP 的查询能力只有在服务实际可用时才能使用，不报告没有执行的工具结果。

## 其他指定库

本次核查了 Lucide、react-resizable-panels、dnd-kit 和 Zustand 的官方主仓库文件树，未发现可引用的官方 `SKILL.md`。这些库使用[库使用说明](libraries.md)中的官方文档。

维护时如果发现新增的官方 skill，先确认维护者、入口和完整引用目录，再补充名称、适用条件与官方安装方式。社区 skill 只有经过内容和版本检查后才能列入，并明确标注社区来源；不根据搜索摘要直接安装或调用。
