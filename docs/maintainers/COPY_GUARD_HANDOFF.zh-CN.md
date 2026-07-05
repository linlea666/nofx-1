# Copy Guard 模块接手说明（v5 / v5.1）

**用途：** 供新会话 AI 或维护者快速了解 Copy Guard（CG）子系统背景、近期交付、设计约束与已知问题。  
**最后更新：** 2026-07-06（对应提交 `b92d30c`）

---

## 1. 项目是什么

**仓库：** `nofx-1`（NOFX 平台）  
**技术栈：** Go（Gin + SQLite）后端 + React/TypeScript（Vite）前端  
**业务：** AI 交易平台，支持多交易所跟单。Copy Guard 是 **OKX 专用**子系统，负责：

- 账户保护（两层硬止损：ATR / 保证金 / 账户权益）
- 确认式自动重入（v5）
- 自动重入用尽后的人工确认重入（v5.1）
- 周期统计（own-path baseline、attempt 账本、观察期采样）

**当前状态：** `main` 分支，已与 `origin/main` 同步。  
**最新相关提交：**

| 提交 | 说明 |
|------|------|
| `b92d30c` | v5.1 人工重入全栈 + 审计修复 |
| `91a0433` | v5 审计轮修复（重入默认、retry 语义、裸跑 accounting） |
| `40f4cd5` | Copy Guard v5 主体（两层硬止损、可保护性状态机、确认式重入） |

---

## 2. 演进脉络（前因后果）

### v5（`40f4cd5`）

- 两层硬止损 + 可保护性状态机（NORMAL / CLAMPED / UNPROTECTABLE）
- 确认式自动重入：连续 tick 确认、保守锚点、自适应距离/ATR、可保护性预检（自动路径下未通过则**不**重入）
- 周期统计 overhaul（own-path baseline）

### v5 审计修复（`91a0433`）

- 重入默认次数、保护 retry 计数语义、裸跑 accounting 等

### v5.1 人工重入（`b92d30c`）

**背景：** 自动重入次数用尽（`ATTEMPTS_EXHAUSTED`）后，用户仍希望系统在行情再次满足重入条件时**提醒人工**，确认后**代执行**（不自动下单）。

**核心决策（用户多次明确，修改时必须遵守）：**

| 约束 | 说明 |
|------|------|
| 人工确认最高优先 | 确认后直接执行，**不卡价格偏移** |
| 仅四项硬拦截 | 领航员已平仓、领航员已反手、本地已有同向仓、金额低于最小下单额 |
| 可保护性预检 | **仅提示，不拦截**；成交后由 v5 状态机按 `risk_unprotectable_action` 兜底 |
| 信号无时间过期 | 保持 `PENDING` 直至执行、忽略或根本性失效 |
| 根本性失效 | 领航员平仓/反向、周期终结（`CloseCopyGuardCycle` / `BeginCopyGuardAccounting` 事务内钩子） |
| 邮件防轰炸 | 1h 冷却 + 边缘触发；前端横幅 30s 轮询（不受冷却影响） |
| 默认开启 | `risk_manual_reentry_enabled` 存 **主表列 DEFAULT 1**（不用 JSON 缺省 false） |
| 邮件可读性 | 保护类 / 人工重入邮件须带**交易员显示名**（修复 `notifyProtection` 仅 TraderID 的问题） |

---

## 3. v5.1 数据流（端到端）

```
ATTEMPTS_EXHAUSTED + manualMode 开启
  │
  ▼
engine_risk.checkReentryConditions
  │  复用完整门控链（冷却 / 边界 / 连续确认 / 金额 / 可保护性预检仅提示）
  │  触发点：emitManualReentrySignal（非 emitReentryDecision）
  ▼
store.SaveManualReentrySignal → PENDING（同周期去重刷新快照）
  + RiskEventManualReentrySignal
  + GUARD_MANUAL_REENTRY_SIGNAL 事件
  ▼
integration.sendManualReentrySignalAlert → 邮件（1h 冷却）
  ▼
前端 CopyGuardPage ManualSignalsBanner（30s 轮询）
  │  【确认重入】 / 【忽略】
  ▼
ConfirmManualReentry（需跟单引擎运行中）
  │  Claim: PENDING → EXECUTING（幂等抢占）
  │  四项硬校验
  │  UpdateCopyGuardObservation → REENTRY_PENDING
  │  emitReentryDecision → executeFullDecision（标准执行链）
  ▼
成功: markManualReentryOutcome → EXECUTED
失败: markManualReentryOutcome → FAILED
忽略: Dismiss → DISMISSED
失效: Invalidate（领航员变化 / 周期闭合）→ INVALIDATED
```

**信号状态机：**

```
PENDING ──Claim──► EXECUTING ──MarkOutcome──► EXECUTED | FAILED
   │                    │
   ├──Dismiss──► DISMISSED
   └──Invalidate──► INVALIDATED
```

---

## 4. 关键文件索引

| 模块 | 文件 | 职责 |
|------|------|------|
| Store | `store/copyguard_manual.go` | 信号表 CRUD、Claim / Release / MarkOutcome / Invalidate |
| Store | `store/copyguard.go` | 表初始化；周期闭合事务内失效 PENDING 信号 |
| Store | `store/copytrade.go` | 列 `risk_manual_reentry_enabled DEFAULT 1` |
| Store | `store/copyguard_manual_test.go` | 信号生命周期测试（5 用例） |
| Engine | `copytrade/engine_risk.go` | `manualMode`、`emitManualReentrySignal`、门控链复用 |
| Integration | `copytrade/integration.go` | `ConfirmManualReentry`、`sendManualReentrySignalAlert`、`markManualReentryOutcome`、`notifyProtection` 补名称、`recoverV4PendingStates` 孤儿信号回写 |
| Types | `copytrade/types.go` | `RiskManualReentryEnabled`、`RiskEventManualReentrySignal` |
| API | `api/copytrade_handler.go` | manual-signals 列表 / confirm / dismiss |
| API | `api/server.go` | 配置字段透传 |
| 前端 | `web/src/pages/CopyGuardPage.tsx` | `ManualSignalsBanner`、事件 / gate 标签 |
| 前端 | `web/src/components/TraderConfigModal.tsx` | 开关「次数用尽后人工重入提醒」默认开 |
| 前端 | `web/src/types.ts`、`web/src/lib/api.ts` | 类型与 API 客户端 |

### API 端点

```
GET  /api/copytrade/risk/manual-signals?status=PENDING,EXECUTING
POST /api/copytrade/risk/manual-signals/:id/confirm   # 需跟单运行中
POST /api/copytrade/risk/manual-signals/:id/dismiss   # 仅需 store，不需引擎
```

### 配置字段

- **列名：** `risk_manual_reentry_enabled`（`copy_trade_configs` 表，INTEGER DEFAULT 1）
- **JSON：** `risk_manual_reentry_enabled`（读写透传）
- **生效条件：** `RiskReentryEnabled && RiskManualReentryEnabled && terminalWatchStatus == ATTEMPTS_EXHAUSTED && !noiseDisabled`

---

## 5. 与原功能的兼容性

- **自动重入路径零侵入：** `markManualReentryOutcome` 在无 EXECUTING 信号时为 no-op
- **Dismiss 不走引擎：** API 直接 `store.DismissManualReentrySignal`（比包级导出函数更解耦）
- **Confirm 复用自动重入执行链：** `emitReentryDecision` → `executeFullDecision`；Reasoning 含 `"reentry"` 触发 `updatePositionMapping` 成功钩子
- **manualMode 下周期状态保持 `ATTEMPTS_EXHAUSTED`**（UI 真实呈现自动次数已用尽，同时继续观察）

---

## 6. 审计结论（`b92d30c` 已合入，勿重复修）

### 已修复

| ID | 问题 | 修复 |
|----|------|------|
| A | 重启后 `recoverV4PendingStates` 只恢复周期、不回写 EXECUTING 信号 → 前端永久「执行中…」 | 三处恢复分支补 `markManualReentryOutcome` |
| H | dismiss 失败误用绿色 notice 样式 | 独立红色 `bannerError` |

### 已知低风险观察（记录即可）

- 确认后会额外收到 `RiskEventReentryInitiated` 的「二次进场触发」邮件（与人工邮件略重叠）
- Claim 前硬校验与执行间存在 ms 级 TOCTOU（与自动重入同级，可接受）
- `MarkAlerted` 在 RiskEvent 投递前落库，极端情况下可能丢邮件但冷却已起算
- Claim(EXECUTING) 与设 REENTRY_PENDING 之间极低概率并发生成第二条 PENDING（周期闭合时 INVALIDATED）

---

## 7. 回归检查清单

修改 Copy Guard / 人工重入相关代码后必须跑：

```bash
go vet ./copytrade/ ./store/ ./api/
go test ./copytrade/ ./store/ ./api/
cd web && npm run build
```

Store 专项：

```bash
go test ./store/ -run ManualReentry -v
```

---

## 8. 开发原则（用户要求，必须遵守）

1. **根因优先** — 先分析为什么会出问题，禁止盲目 patch  
2. **全局视角** — 评估影响范围，避免局部补丁破坏整体设计  
3. **复用决策** — 直接复用 / 扩展复用 / 提取复用 / 独立新写，并说明理由  
4. **一次到位** — 彻底解决问题，禁止反复试错式小改  
5. **范围纪律** — 只改当前问题相关代码；关键缺陷（根因明确、范围可控）可一并修  
6. **保守修改** — 不删改正常运行的逻辑，除非它是问题根因  
7. **先说后做** — 多文件 / 架构调整先说明方案  
8. **回归检查** — 原问题是否解决、是否引入新 bug、边界是否完整  
9. **最终确认** — 全部改完后整体审查；**未经用户明确要求不要 commit / push**

---

## 9. 可选后续（用户未明确要求）

- 人工确认后 suppress `RiskEventReentryInitiated` 邮件，避免双邮件  
- Confirm 路径集成 / E2E 测试（需运行中跟单引擎）  
- 生产验证：ATTEMPTS_EXHAUSTED 周期下 PENDING 信号 + 邮件 + 确认执行全链路  
- Copy Guard 用户文档（docs 尚无专章）

---

## 10. 新会话起手式

```text
你是 Copy Guard 模块维护者。仓库 nofx-1，main 已推送到 b92d30c（v5.1 人工重入已完成）。
请先读 docs/maintainers/COPY_GUARD_HANDOFF.zh-CN.md，再执行 git log -1 && git status。
我的任务是：[填具体需求]。
```

**历史对话 transcript（决策细节）：**  
`.cursor/projects/.../agent-transcripts/aa9e7315-fd0f-4a4c-adac-58143055ec1c/aa9e7315-fd0f-4a4c-adac-58143055ec1c.jsonl`

---

## 11. 联调 / 实盘验证前提

- OKX 跟单已启动（Confirm API 依赖运行中引擎）
- Copy Guard v4+ 配置（`risk_policy_version >= 4`，provider = okx）
- 某周期达到 `ATTEMPTS_EXHAUSTED` 且门控链通过
- `risk_manual_reentry_enabled = true`（默认即为 true）
