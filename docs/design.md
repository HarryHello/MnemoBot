# MnemoBot 设计文档

状态：草案 v0.1（2026-09-16）· 架构与契约已定稿，Go 实施细节在开工时补全
本文是 mnemo-bot 仓库的设计权威文档；mnemosync 侧的新增 API 面（events 端点、origin 语义、身份插件）在实施时补入父项目自己的文档，本文只留引用。

---

## 0. 一句话定位

MnemoBot 是 mnemosync 的**第一方 bot 运行时**：对上连接 bot 平台（首发 OneBot v11 / NapCat），对下以标准 OpenAI 兼容 API 调用 mnemosync。它持有"何时说话"的全部决策，不持有任何服务端状态。

**总原则：发言权在调用方（何时说、对谁说、在哪说），记忆权在服务端（记住什么、如何理解、如何衰减）。**

## 1. 定位与原则

### 1.1 同构分层

bot 平台之于 MnemoBot，如同前端之于 mnemosync。mnemosync 保持纯服务商契约：**请求→响应，无出站主动性，无对应接口**。主动消息能力全部位于 bot 侧——把该能力给到服务商即改变基本设计，此为不可动摇的边界。

### 1.2 决策权分工表

| 决策 | 归属 |
|---|---|
| 何时回复 / 是否回复（triage） | MnemoBot |
| 何时主动说话、主动说什么话题的触发 | MnemoBot |
| persona 主动消息的内容生成 | mnemosync（正常 graph 调用） |
| 记住什么（长期记忆抽取）、如何理解（视觉/情绪） | mnemosync |
| 身份归属（Actor）、空间分区、上下文装填 | mnemosync |
| 投递、重试、限速、平台风控应对 | MnemoBot |

### 1.3 MnemoBot 与 mnemosync-bot 谱系

MnemoBot 不持有数据库、不做身份推理、不做上下文裁剪、不做历史重放。适配器（adapter）的职责白名单：协议翻译（OneBot segment ↔ envelope content parts）、连接维持、投递与回执、平台限速。一切状态与决策在 mnemosync。

## 2. 两项目边界与独立性约束

### 2.1 职责表

| | mnemosync（服务商） | mnemo-bot（运行时） |
|---|---|---|
| 职责 | graph 推理、记忆/关系/身份、落库、视觉描述、上下文装填 | OneBot 连接维持、协议翻译、triage、投递、本地暂存与补投 |
| 状态 | 全部（SQLite + Chroma） | 无数据库，仅配置文件 + 本地日志（JSONL） |
| 对方感知 | 对 bot 无感（bot 走标准 API + 插件策略，与任意前端同构） | 通过版本探测获知 mnemosync 能力 |

### 2.2 独立性硬规则（双方分开拉取均可独立构建、测试、运行）

1. **零跨仓库引用**：任何 pyproject / go.mod 不出现指向对方目录的路径依赖；venv、CI 完全自持。
2. **规格先行、双侧实现**：envelope 的唯一权威是语言中立的 JSON Schema + 本契约文档；mnemo-bot（Go 结构体）与 mnemosync（pydantic 插件）各自实现。
3. **golden 夹具双份 vendored**：同一组样例 JSON 在两仓库各放一份，双侧 CI 校验 schema + 跨侧解析。将来 schema 变更频率高到双份维护成为负担时，再升级为共享 `mnemo-protocol` 包——由增长逼出，非默认。
4. **文档按归属拆分**：各自仓库文档化自己的 API 面；只引用不复制。
5. **本地联调栈放 bot 仓库**：docker-compose（mnemosync 镜像 + napcat + bot）不硬依赖同机父仓库源码。
6. **兼容性用版本探测**：bot 探测 mnemosync 版本（复用面板已有版本接口），mnemosync 主体对 bot 无感；插件版本匹配责任在插件安装侧。

### 2.3 仓库布局

```
mnemosync/          # 父项目（不变）
└── mnemo-bot/      # gitignored 独立 git 仓库（开发期物理摆放，不构成构建依赖）
```

- 父项目 `.gitignore` 增加 `mnemo-bot/`；父项目 CI / wheel / install.sh / Docker **永不包含** mnemo-bot。
- mnemo-bot 独立 remote、独立发版（git tag）、独立 CI。
- 跨仓库变更天然是两个提交，兼容性由 envelope `version` 声明。

## 3. 语言决策记录：Go

**结论：Go 1.22+，单二进制，无框架。**

决策依据（按权重）：
1. 部署现实：维护机内存紧张、多项目共存，常驻内存 10–25MB（对比 Python ~40–70MB、Bun ~35–55MB）。
2. 负载为 I/O 中继，各语言性能均绰绰有余；语言对端到端延迟贡献 1–5ms（瓶颈是 LLM 与服务端落库）。
3. 单二进制对 napcat 生态用户分发最友好；goroutine 处理多群并发是舒适区。
4. **保险丝**：薄客户端 + spec-first + golden 夹具使协议核心（~300–500 行）语言无关，未来跨语言迁移协议层是受控小工程。

明确不用：NoneBot2 / koishi 等 bot 框架（会重新制造 AstrBot 问题：框架抽象替我们做决定）。OneBot v11 协议面很小（WS 事件 + `send_group_msg` / `send_private_msg` / `get_msg`），直接手写薄客户端。

Go 工程形态：
- 模块：`github.com/HarryHello/mnemo-bot`
- 依赖：WS 库（gorilla/websocket 或 nhooyr/websocket，开工时定）+ `net/http` + `encoding/json`，无其他框架级依赖
- 结构：`cmd/mnemo-bot` + `internal/{envelope,onebot,triage,journal,forwarder,config}`
- CI：go test + golangci-lint；契约测试 job 从发布渠道（PyPI / pinned git tag）安装 mnemosync 跑全链路，**不从路径引入**

### 3.1 版本与兼容模型

- **bot → mnemosync**：`mnemo-bot check` 探测版本 ≥ 最低要求（复用面板已有版本接口，如 `GET /health`；若该接口的可达面需调整，实施时做最小加性改动）、events 端点存在性（探测请求）、auth 有效。
- **mnemosync → bot**：不做任何探测，保持相对无感。服务端所有新增都是加性的、惰性的：无人调用即无行为差异。
- **插件**：`mnemosync_bot` 身份插件按 astrbot 模式走 `mnemosync-plugins` 仓库分发（**不随 wheel**——主项目 wheel 只打包 `src`），版本与 envelope 版本相符由安装侧保证；插件缺失/版本不符的被动症状是非归属模式，`check` 的诊断能力边界见 §6.5。

## 4. 协议契约：Envelope v1

### 4.1 通用事件对象

```jsonc
{
  "id": "onebot-msg-987654",       // 外部事件 ID，幂等键，必填
  "ts": 1737091282000,             // 平台时间：epoch 毫秒 或 ISO 8601 带显式 offset；naive 拒收
  "kind": "message",               // message | notice（notice 预留：戳一戳/入退群等）
  "actor": {                       // notice 事件可为 null
    "external_key": "10001",       // 平台内唯一 ID（QQ 号）
    "display_name": "小明"
  },
  "content": [                     // OpenAI content-part 兼容
    {"type": "text", "text": "……"},
    {"type": "image_url", "image_url": {"url": "https://…"}}
  ],
  "reply_to": "onebot-msg-987653", // 可选：引用的消息 ID（触发请求中由 adapter 还原为引用文本）
  "meta": {                        // 可选，服务端不做行为分叉
    "is_bot": false,               // 仅提示位，供未来装填策略/分析
    "correlation_id": "bot-…"      // 跨进程追踪：mnemosync 侧接 debug_context contextvars
  }
}
```

### 4.2 记录路径：批量事件端点（mnemosync 新增）

```
POST /v1/conversation/events
Authorization: Bearer <api_key>
{
  "version": 1,
  "protocol": "onebot11",
  "platform": "qq",
  "space": {"type": "group", "id": "123456"},
  "events": [ <事件对象>, … ]
}
→ 200 { "results": [ {"index": 0, "status": "accepted"},
                      {"index": 1, "status": "rejected", "reason": "…"} ] }
```

- 语义：**只落库、不推理、不触发记忆分析、无 LLM 成本**。内部复用现有 `_conversation_events()` + `append_events()` 指纹去重链路。
- 失败语义：**按事件粒度**返回结果数组，整体不原子；bot 据此重试被拒事件。
- 限流：按 API Key 限流 + 批量大小上限（具体数值实施时定）；batch 按 space 分组提交。
- 服务端对该空间沿用 space lock 串行，与 `/v1` 写入保持一致的交错语义。

### 4.3 触发路径：走标准 `/v1/chat/completions`

- 只发**当前这一条消息**（含 media parts），**不带历史快照**——流水里已有全部 ambient 消息，短期记忆双窗按 space 自动装填。
- envelope 以结构化块嵌入最后一条 user 消息（一期进消息体；header 通道为三期预留）：

  ```
  <mnemo-envelope>{"version":1, "protocol":"onebot11", "platform":"qq",
                   "space":{"type":"group","id":"123456"},
                   "event": <事件对象>, "origin":"user"}</mnemo-envelope>
  实际消息文本……
  ```

- `origin` 字段：`user` | `persona_proactive`。后者为主动消息合成请求（见 §5.4）。

### 4.4 时区规则

1. envelope 时间戳必须带时区（epoch 或 ISO+offset），naive 拒收（宁可 400 不猜测）。
2. 服务端统一 UTC 存储（现有 `_utc_iso` 已保证，`ORDER BY ts` 字典序正确）。
3. 渲染层才转 persona 本地时区（现有默认 Asia/Shanghai，保持为 persona 级配置）；"今天/昨天"等日期边界判定一律用 persona 时区。

### 4.5 幂等与去重

- 记录路径：`id` → `external_event_id` → 事件指纹 → `conversation_turns` UNIQUE 索引去重（现有机制）。重发/补投零成本。
- 触发路径：复用现有 idempotency.db 幂等重放。
- **自回声过滤（唯一硬过滤，且是去重不是排除）**：adapter 过滤 `user_id == self_id` 的事件——persona 回复在 `/v1` 时已落库一次，OneBot 回吐的 echo 不得二次落库。

### 4.6 命名空间：平台与协议分离（已定）

身份与空间命名把两个维度严格分开：

| 维度 | 含义 | 取值示例 |
|---|---|---|
| `platform` | 底层社交平台 | `qq` / `telegram` / `discord`…（OneBot v12 的 `self.platform` 原生提供；v11 无此字段，由 adapter 配置声明，默认 `qq`） |
| `protocol` | 传输实现 | `onebot11` / `onebot12` / 未来非 OneBot 的原生协议 |

**Actor**：单一 frontend `mnemobot` 命名空间，external_key 复合平台前缀 = `{platform}:{平台原生ID}`（如 `qq:10001`、`telegram:5432`）。**协议不参与身份**：OneBot v11↔v12 切换、未来非 OneBot 协议接入同一平台，命中同一 Actor；同平台多协议实例并存也不分裂。

**空间**：envelope 只报原始坐标 `space: {type, id}`（group/private + 平台原生频道/会话 ID）；mnemosync 服务端统一组合 `space_id = "{platform}.{type}.{id}"`（如 `qq.group.123456`）。组合规则单一出处（服务端 envelope 归一模块，身份插件与 events 端点共用），未来调整命名或迁移只动一处。

**与 astrbot 路径共存**：同一 QQ 号 → astrbot Actor（frontend="astrbot"）与 mnemobot Actor 并存，用既有 UserGroup 绑定机制归并；空间短期分立（astrbot 用群名、MnemoBot 用数字 ID），长期记忆跟受众走，短期流水在新空间重新积累。

## 5. MnemoBot 运行时设计

### 5.1 进程与组件

单实例 asyncio（goroutine）进程，无数据库。组件：

```
OneBot WS ⇄ [adapter: 协议翻译/自回声过滤] ⇄ [记录管线] → 批量 → mnemosync events 端点
                                        ⇄ [triage] → [触发管线] → /v1 (流外) → [投递] → OneBot API
                              [journal(JSONL)] ← 全部出站请求与回执落本地日志
```

### 5.2 记录管线

- 收到事件 → 自回声过滤 → journal 落盘 → 按空间批缓冲。
- flush 条件：~1s 定时或 N 条（实施时定阈值）；**每批按 space 分组**。
- 与触发的竞态纪律（**硬规则，"只发触发消息"设计成立的前提**）：**发出触发请求前，必须先 flush 未发送批次并等到 append 确认，再调 `/v1`**。否则 persona 上下文会缺失触发前的 ambient 消息。

### 5.3 触发管线

- triage 输入：@me（`user_id == self_id` 的 at segment）、reply-to-me（resolve `reply_to`）、名字提及（persona 昵称列表，一期 bot 本地配置）、戳一戳 notice（预留）、私聊。
- triage 输出触发后：**先 flush 批次（§5.2）→ 引用还原（本地 journal 或 `get_msg` 取被引消息文本，并入触发请求）→ 调 `/v1`（非流式，一期）→ 投递**。
- persona 昵称一期在 bot 配置维护，二期改为从服务端人格定义读取（`/internal/bot`）。

### 5.4 主动消息（合成请求式）

- 触发：bot 侧调度（静默阈值/冷却——bot 自己看得见群消息，判定不需要服务端数据），一期基于时间型启发式。
- 执行：向 `/v1` 发送带 `origin: "persona_proactive"` 的合成请求；mnemosync 正常走 graph 生成；**服务端不落 user turn（防合成指令污染流水），回复按 assistant 轮落库（防人格失忆自己主动说过的话）**。
- bot 拿到回复后投递。bot 掉线期间不存在"服务端想说而未说的消息"（触发器不会 fire），投递可靠性由 bot 本地队列自负。

### 5.5 记录规则（服务端，全部通用，无发送者类型特例）

| 规则 | 内容 |
|---|---|
| 全收 | 不按发送者类型排除：同位 bot（OneBot 类普通账号 bot）与平台官方 bot 消息照常落库，归属各自 Actor。多 bot 群聊（人格与人格对话）自然成立 |
| 归属 | 每条消息归属其 Actor（`external_key` = QQ 号）；上下文中以昵称出现，与普通群成员无差异 |
| 占位降级 | 展不开的特殊消息（合并转发、卡片等）→ 占位符 + 可得摘要；与媒体占位符同一条规则 |
| 幂等去重 | 见 §4.5 |
| notice 事件 | 入群/退群/戳一戳落库为带占位文本的事件（如"戳了戳 persona"）；是否触发回复由 triage 决定 |

"记录面"与"触发面"严格分离：记录忠实于现实（人格像普通群友一样看得见群里一切），回应与否由 triage 决定。

### 5.6 媒体解析决策表（adapter 翻译职责）

| OneBot image `file` 形态 | 处理 |
|---|---|
| http(s) URL | 直接入 envelope（记录时视觉描述只需 URL 在"到达→描述完成"短窗口内有效，规避 QQ 图片 URL 限时问题） |
| `file://` 本地路径（bot 与 napcat 同机） | 可配置 base64 兜底；超限或失败 → 占位符 |
| 不可解析 | 占位符 |

失败一律占位符降级，不阻塞批量提交。

### 5.7 可靠性

- **断线补投**：OneBot 反向 WS 掉线期间的消息 NapCat 不保证补发，丢失即永久丢失。对策：journal 落盘先于入队，重连后按 `id` 幂等重推（指纹去重使补投零成本）。同时覆盖进程崩溃恢复。
- **背压**：热点群 burst → 队列限深，溢出丢旧保新（幂等补投兜底）。
- **投递失败**（风控/禁言/长度超限）：bot 侧重试与拆分（QQ 文本长度上限）责任；流水里已存在的 assistant 轮**不因投递失败回滚**——这个不对称要明示。

### 5.8 CLI 与配置

- `mnemo-bot serve`：运行。
- `mnemo-bot check`：自检——mnemosync 版本 ≥ 最低要求、events 端点存在、auth 有效、envelope 版本匹配；插件缺失/不符的诊断指引。
- 配置 TOML（对齐父项目风格，不做热重载）：

  ```toml
  [mnemosync]
  base_url = "http://127.0.0.1:16125"
  api_key  = "sk-…"
  min_version = "0.4.…"        # envelope v1 + events 端点的最低版本

  [onebot]
  mode       = "reverse"       # napcat 连入
  listen     = ":16530"
  access_token = "…"
  self_id    = "10000"         # 自回声过滤依据

  [persona]
  nicknames = ["…"]            # 一期本地配置，二期改从服务端读

  [triage]
  private_always   = true      # 私聊总是回复
  group_cooldown_seconds = 30
  proactive_silence_minutes = 120   # 0 = 禁用主动消息

  [spaces]
  group_prefix   = "qq.group."
  private_prefix = "qq.private."
  ```

## 6. mnemosync 侧加性改动（实施清单，全部对现有前端无行为差异）

1. **`mnemobot` 身份插件**：解析 `<mnemo-envelope>` 块（pydantic 结构化解析，不做正则猜名），负责 §4.6 的身份与空间组合（平台前缀复合键、`space_id` 组合），结构体与 bot 侧对同一 JSON Schema 实现；走 `mnemosync-plugins` 分发仓库。
2. **`POST /v1/conversation/events`**：§4.2 契约；复用 `_conversation_events()` + `append_events()`；space lock 沿用。
3. **origin 语义**：forward 管线对 `persona_proactive` 请求不落 user turn、回复按 assistant 落库、不触发 user 向的记忆/关系分析。
4. **记录时异步视觉描述**：events 端点收到 media part 后调度现有 vision agent（role binding `vision`，回退 `assist`），描述文本物化进流水；失败落 `[图片]`；**per-space 成本开关（默认开）**；模式与 memory_graph 异步记忆图同构。
5. **（可选）events 端点响应回显 envelope 版本**——服务端自述版本，便于 `check` 精确诊断插件匹配；是否值得做实施时定。

父项目对应 API 文档条目在实施时补入 mnemosync 仓库。

## 7. 记忆与理解（服务端域，MnemoBot 零参与）

- **短期上下文免费达成**：`list_for_space` 无 origin 过滤、按 space + 7 天窗装填——落库即进上下文。
- **长期记忆缺口（一期接受）**：ambient 消息不触发 memory_analysis。二期**水位线清扫**：每空间记录"上次分析到哪条"，触发回复时把 memory_analysis 输入从"当前轮"扩展为"自上次分析以来的窗口"，persona 回复是天然批处理检查点。
- **记录时视觉描述**：见 §6.4；语音等未来模态同框架（envelope 预留 kind/模态标注，服务端按模态路由辅助 agent，处理不了就占位符）。
- 时区渲染细节见 §4.4。

## 8. 阶段划分与验收边界

### Phase 1 — MVP（OneBot v11）
**范围**：bot——反向 WS、triage（@/回复/名字）、群+私聊、记录管线（flush-before-trigger 纪律）、journal 补投、自回声过滤、主动消息合成式、`serve`/`check`；mnemosync——插件、events 端点、origin 语义、记录时视觉描述。
**验收**：
- 真实群挂两周，落库条数与群消息数对账（扣除自回声与展不开消息）零缺漏；
- 触发回复的上下文包含此前 ambient 消息（引用还原生效，无答非所问）；
- 断线重连补投零重复零丢失（幂等指纹验证）；
- 主动消息合成请求不污染流水（user turn 零新增、assistant 轮正确）；
- `check` 在版本不符/端点缺失/插件缺失三种场景给出准确诊断。

### Phase 2 — 查询平面与记忆清扫
- `/internal/bot` 只读查询平面（关系/记忆摘要，供更聪明触发；可选 assist 打分端点）——只读 + 独立 scope 的 token；
- 水位线记忆清扫；
- persona 昵称改从服务端读取。

### Phase 3 — 扩展
- OneBot v12 / 其他平台 adapter（协议核心语言无关，新增 adapter 只做翻译层）；
- header 元数据通道（信封从消息体迁出，向后兼容的插件接口扩展）；
- 有夏令时平台的时区实测。

## 9. 测试策略

| 层 | 内容 |
|---|---|
| 单测 | envelope 编解码（Go 结构体 ↔ JSON schema 校验）、triage 规则、journal 回放、flush-before-trigger 纪律 |
| golden 夹具 | 双仓库各放同一组样例 JSON；私聊 / 戳一戳 / 合并转发 / 图片 / 引用回复 / 乱序到达 为必覆盖项 |
| 集成 | fake napcat（最小 WS server 按脚本注入事件）；CI 从发布渠道装真 mnemosync 跑全链路 |
| 契约 | 双侧夹具同步靠契约测试；envelope 版本变更须双侧夹具同步更新 |

## 10. 开放问题（留待实施或二期）

1. **astrbot 路径共存与空间分立**：Actor 归并靠既有 UserGroup 绑定（§4.6）；空间短期分立（astrbot 群名 vs MnemoBot 数字 ID），是否做空间别名/合并机制留观察。
2. notice 事件（戳一戳等）的 envelope 表达细节（占位文本模板）。
3. 流式回复（一期非流式，留位观察）。
4. `is_bot` meta 的消费方（未来装填策略/分析）。
5. events 端点响应是否回显 envelope 版本（§6.5）。
6. `mnemo-bot check` 对"插件缺失"的主动诊断方法（当前只能给出被动症状指引）。
