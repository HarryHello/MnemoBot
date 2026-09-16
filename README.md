# MnemoBot

mnemosync 的第一方 bot 运行时——对上连接 bot 平台（首发 OneBot v11 / NapCat），对下以 OpenAI 兼容 API 调用 mnemosync。

> **总原则：发言权在调用方（何时说、对谁说、在哪说），记忆权在服务端（记住什么、如何理解、如何衰减）。**

## 定位

```
QQ 群聊/私聊 ⇄ NapCat (OneBot v11) ⇄ MnemoBot ⇄ mnemosync (服务商)
                 协议翻译/连接维持        触发判定/投递      记忆/身份/推理
```

MnemoBot 持有"何时说话"的全部决策：触发判定（@我、回复我、名字提及、戳一戳）、主动消息调度、投递与重试。它**不持有任何服务端状态**——身份、记忆、关系、上下文全部在 mnemosync。

mnemosync 保持纯服务商契约（请求→响应，无出站主动性），对 MnemoBot 无感。bot 平台之于 MnemoBot，如同前端之于 mnemosync——同构分层。

## 现状

🚧 **设计阶段**——架构与协议契约已定稿，实现未开始。设计文档：[docs/design.md](docs/design.md)。

- **语言**：Go，单二进制，无框架依赖（薄客户端直写 OneBot v11 协议层）
- **目标平台**：OneBot v11（NapCat 等）；OneBot v12 与其他平台预留
- **形态**：独立进程，无数据库，配置文件 + 本地日志（断线补投数据源）

## 核心机制

- **群聊全量落库**：每条群消息经批量事件端点进入 mnemosync 对话流水（不触发推理），触发判定只对"值得回应的消息"发起回复——短期上下文由落库自动达成。
- **主动消息**：bot 侧调度 + `origin=persona_proactive` 合成请求，服务端只落 persona 的回复，不污染对话流水。
- **可靠性**：本地日志（journal）先于入队落盘，断线重连按事件 ID 幂等补投，零丢失零重复。
- **`mnemo-bot check`**：一条命令自检 mnemosync 版本、events 端点、鉴权与协议版本匹配。

## 路线图

1. **Phase 1（MVP）**：反向 WS、触发判定、群聊/私聊、记录管线、断线补投、主动消息（合成请求式）、`serve` / `check`
2. **Phase 2**：`/internal/bot` 只读查询平面（关系/记忆，供更聪明的触发）、水位线长期记忆清扫
3. **Phase 3**：OneBot v12 / 其他平台适配器、header 元数据通道

配套 mnemosync 侧加性改动（`mnemobot` 身份插件、批量事件端点、origin 语义、记录时视觉描述）见设计文档 §6。

## 相关仓库

- [mnemosync](https://github.com/HarryHello/mnemosync) — 服务商本体（LangGraph 人格记忆同步代理服务器）
- [mnemosync-plugins](https://github.com/HarryHello/mnemosync-plugins) — 身份插件分发渠道（`mnemobot` 插件发布于此）

## 协议

与 mnemosync 通过 **envelope v1** 通信：语言中立 JSON Schema + 双侧 golden 夹具契约测试，两项目独立拉取均可完整工作。详见设计文档 §4 与 §2.2 独立性约束。

## 许可证

AGPL-3.0（与 mnemosync 一致）
