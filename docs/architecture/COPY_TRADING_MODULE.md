# NOFX 真人领航员跟单系统设计文档

> **版本**: v2.1  
> **状态**: 已实现  
> **作者**: Core Engineering Team  
> **创建日期**: 2025-12-16  
> **更新日期**: 2025-12-21

---

## 目录

1. [概述](#1-概述)
2. [跟单规则与交易动作](#2-跟单规则与交易动作)
3. [系统架构](#3-系统架构)
4. [核心模块设计](#4-核心模块设计)
5. [数据模型](#5-数据模型)
6. [API 设计](#6-api-设计)
7. [跟单比例算法](#7-跟单比例算法)
8. [前端集成](#8-前端集成)
9. [风险预警机制](#9-风险预警机制)
10. [日志规范](#10-日志规范)
11. [实现路线图](#11-实现路线图)
12. [附录](#12-附录)

---

## 1. 概述

### 1.1 项目背景

NOFX 当前的交易决策完全由 AI 模型驱动。为扩展信号来源，我们需要引入「真人领航员跟单」功能，允许用户跟随 Hyperliquid 或 OKX 上优秀交易员的操作进行同步交易。

### 1.2 设计目标

| 目标 | 描述 |
|------|------|
| **插件化** | 跟单模块作为独立插件，可插拔，不影响核心系统 |
| **最小侵入** | 复用现有执行层、仪表盘、日志系统，避免重复造轮子 |
| **多账户隔离** | 基于 `trader_id` 完全隔离，每个 Trader 绑定独立的 Provider 实例 |
| **风险隔离** | 插件异常时自动降级/熔断，不影响主系统稳定性 |
| **向后兼容** | 决策接口（Decision API）保持向后兼容，便于未来升级 |

### 1.3 核心理念

将「跟单」视为一种 **Decision Provider（决策提供者）**，与现有 AI Decision Provider 平行。

**关键原则：领航员的所有交易动作都要无条件跟随，系统不做任何限制，只做预警日志。**

```
┌─────────────────────────────────────────────────────────────────┐
│                     Decision Layer (决策层)                      │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  ┌─────────────────┐         ┌─────────────────────────────┐   │
│  │  AI Provider    │         │  Copy Trading Provider       │   │
│  │  (现有)         │         │  (新增 - 插件化)              │   │
│  │                 │         │                              │   │
│  │ - DeepSeek      │         │ - Hyperliquid Tracker        │   │
│  │ - Qwen          │         │ - OKX Tracker                │   │
│  │ - GPT/Claude    │         │                              │   │
│  └────────┬────────┘         └──────────────┬───────────────┘   │
│           │                                  │                   │
│           └──────────────┬───────────────────┘                   │
│                          │                                       │
│                          ▼                                       │
│              ┌───────────────────────┐                          │
│              │  Unified Decision API │                          │
│              │  (统一决策接口)        │                          │
│              └───────────┬───────────┘                          │
└──────────────────────────┼──────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│                   Execution Layer (执行层)                       │
│                   (复用现有 trader/* 模块)                       │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. 跟单规则与交易动作

### 2.1 交易动作定义

#### 2.1.1 Hyperliquid 领航员动作

| 动作 | 说明 | API 字段标识 |
|------|------|-------------|
| **开仓 (Open)** | 无持仓 → 建立新仓位 | `dir: "Open Long"` / `"Open Short"` |
| **加仓 (Add)** | 已有持仓 → 增加仓位 | `dir: "Open Long/Short"` + `startPosition != 0` |
| **减仓 (Reduce)** | 已有持仓 → 部分平仓 | `dir: "Close Long/Short"` + 仓位未清零 |
| **平仓 (Close)** | 已有持仓 → 完全平仓 | `dir: "Close Long/Short"` + 仓位清零 |
| **反向开仓 (Reverse)** | 平掉原仓位 → 开反向仓位 | 一次操作同时出现平仓和反向开仓 |

#### 2.1.2 OKX 领航员动作

| 动作 | 说明 | API 字段标识 |
|------|------|-------------|
| **开仓 (Open)** | 无持仓 → 建立新仓位 | `side: "buy"` + `posSide: "long"` 或 `side: "sell"` + `posSide: "short"` |
| **加仓 (Add)** | 已有持仓 → 增加仓位 | 同开仓方向，仓位增加 |
| **减仓 (Reduce)** | 已有持仓 → 部分平仓 | `side: "sell"` + `posSide: "long"` 或 `side: "buy"` + `posSide: "short"` |
| **平仓 (Close)** | 已有持仓 → 完全平仓 | 同减仓，仓位清零 |

> ⚠️ **注意**：OKX 没有反向开仓动作，只有上述四种基本动作。

### 2.2 核心跟单规则：统一 posId + lastKnownSize 方案（v2.1）

> **🎯 核心设计**：使用 OKX 的 `posId` 作为仓位唯一标识，通过 `lastKnownSize` 变化精确判断操作目标，统一处理所有跟单场景（开仓、加仓、减仓、平仓、同币种多仓位）。

#### 2.2.0 核心架构：通知 + 持仓对比

> **重要理解**：OKX 的 `trade-records` API 只作为"交易通知"，不依赖其返回的具体数据（因为缺少关键信息）。

```
┌─────────────────────────────────────────────────────────────────────────┐
│                      信号处理核心架构                                    │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  trade-records API (通知)              position-current API (真实数据)   │
│  ┌─────────────────────┐               ┌─────────────────────────────┐  │
│  │ 提供：               │               │ 提供：                       │  │
│  │ ✓ symbol            │               │ ✓ posId (仓位唯一标识)       │  │
│  │ ✓ side/posSide      │      →        │ ✓ mgnMode (cross/isolated)  │  │
│  │ ✓ size/value        │   触发获取    │ ✓ lever (杠杆)              │  │
│  │ ✗ posId (无!)       │               │ ✓ size (当前持仓)           │  │
│  │ ✗ mgnMode (无!)     │               │ ✓ 完整准确的仓位信息        │  │
│  └─────────────────────┘               └─────────────────────────────┘  │
│              ↓                                      ↓                    │
│         交易发生通知                        精确的仓位信息               │
│              ↓                                      ↓                    │
│              └────────────→ 对比 lastKnownSize ←────┘                    │
│                                      ↓                                   │
│                           精确判断哪个仓位被操作                         │
│                                                                          │
└─────────────────────────────────────────────────────────────────────────┘
```

**数据来源对比：**

| 字段 | trade-records (通知) | position-current (持仓) | 实际使用来源 |
|------|---------------------|------------------------|-------------|
| posId | ❌ 没有 | ✅ 有 | **持仓 API** |
| mgnMode | ❌ 没有 | ✅ 有 | **持仓 API** |
| lever | ⚠️ 可能不准 | ✅ 准确 | **持仓 API** |
| size | ✅ 有（交易量） | ✅ 有（当前持仓） | 通知用于触发，持仓用于判断 |
| symbol | ✅ 有 | ✅ 有 | 通知 |
| side/posSide | ✅ 有 | ✅ 有 | 通知 |

#### 2.2.1 posId 是什么？

`posId` 是 OKX 为每个仓位分配的唯一标识符。即使同一币种、同一方向，不同保证金模式的仓位也有不同的 `posId`：

| 仓位 | posId | symbol | side | marginMode |
|------|-------|--------|------|------------|
| SOL 全仓做空 | `123456` | SOLUSDT | short | cross |
| SOL 逐仓做空 | `234567` | SOLUSDT | short | isolated |
| ETH 全仓做多 | `345678` | ETHUSDT | long | cross |

**关键特性：**
- ✅ 全局唯一，不会重复
- ✅ 精确区分同币种不同保证金模式的仓位
- ✅ 仓位生命周期内保持不变
- ✅ 平仓后 posId 失效，新开仓会分配新 posId

#### 2.2.2 lastKnownSize 是什么？

`lastKnownSize` 是本地数据库记录的领航员某仓位的"上次已知大小"。用于精确判断哪个仓位发生了变化：

```go
type CopyTradePositionMapping struct {
    TraderID      string    // 跟随者 ID
    LeaderPosID   string    // 领航员仓位 ID (posId)
    Symbol        string    // 交易对
    Side          string    // long/short
    MarginMode    string    // cross/isolated
    Status        string    // active/ignored/closed
    LastKnownSize float64   // ← 上次已知大小（关键字段）
    AddCount      int       // 加仓次数
    ReduceCount   int       // 减仓次数
}
```

**更新时机：**
- 开仓成功 → 保存初始 `lastKnownSize`
- 加仓成功 → 更新为当前大小
- 减仓成功 → 更新为当前大小
- 平仓成功 → 标记 status=closed

**为什么需要 lastKnownSize？**

当领航员有两个同币种同方向仓位时：
```
posId=A: BNBUSDT long isolated 10x, lastKnownSize=2.0
posId=B: BNBUSDT long cross 3x, lastKnownSize=4.0
```

收到加仓信号时，trade-records 只告诉我们 "BNBUSDT long buy"，不知道是加到哪个仓位。

通过对比 `currentSize`（持仓 API）和 `lastKnownSize`（数据库）：
- posId=A: currentSize=2.0, lastKnownSize=2.0 → 无变化
- posId=B: currentSize=6.0, lastKnownSize=4.0 → **增加了！这个是加仓目标**

#### 2.2.3 统一信号匹配逻辑（核心！）

**核心规则：开仓/加仓和减仓/平仓使用不同的匹配策略**

```
┌──────────────────────────────────────────────────────────────────────────┐
│                    统一 posId + lastKnownSize 跟单方案                   │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  数据库映射表：copy_trade_position_mappings                              │
│  ┌────────────┬────────────┬────────────┬────────────┬───────────────┐  │
│  │ trader_id  │ leader_pos_id │ symbol  │ status     │ lastKnownSize │  │
│  ├────────────┼────────────┼────────────┼────────────┼───────────────┤  │
│  │ trader_001 │ 123456     │ BNBUSDT   │ active     │ 2.0           │  │
│  │ trader_001 │ 234567     │ BNBUSDT   │ active     │ 4.0           │  │
│  │ trader_001 │ 345678     │ SOLUSDT   │ ignored    │ 10.0          │  │
│  │ trader_001 │ 456789     │ ETHUSDT   │ closed     │ 0.0           │  │
│  └────────────┴────────────┴────────────┴────────────┴───────────────┘  │
│                                                                          │
│  🔵 开仓/加仓信号匹配（三轮匹配）：                                      │
│  ┌─────────────────────────────────────────────────────────────────────┐│
│  │ 第1轮：查找新开仓（无映射或 closed 的 posId）                       ││
│  │        → 找到则返回 ActionOpen                                      ││
│  │                                                                     ││
│  │ 第2轮：通过 lastKnownSize 变化判断加仓                              ││
│  │        → currentSize > lastKnownSize 的仓位 = 加仓目标              ││
│  │        → 找到则返回 ActionAdd                                       ││
│  │                                                                     ││
│  │ 第3轮：兜底 - 只有一个 active 仓位时直接匹配                        ││
│  │        → 多个 active 但无法判断时跳过并警告                         ││
│  └─────────────────────────────────────────────────────────────────────┘│
│                                                                          │
│  🔴 减仓/平仓信号匹配（反向查找法）：                                    │
│  ┌─────────────────────────────────────────────────────────────────────┐│
│  │ 1. 查找所有 symbol+side 匹配的 active 映射                          ││
│  │ 2. 对每个映射的 posId，检查领航员持仓：                             ││
│  │    - posId 不在领航员持仓中 → 全部平仓 (ActionClose)                ││
│  │    - posId 仍在但 currentSize < lastKnownSize → 部分减仓 (Reduce)   ││
│  │ 3. 更新 lastKnownSize 或标记 closed                                 ││
│  └─────────────────────────────────────────────────────────────────────┘│
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

**完整判断流程图：**

```
                 收到 trade-records 交易通知
                          │
                          ▼
               ┌─────────────────────┐
               │ 拉取领航员最新持仓   │
               │ (position-current)  │
               └──────────┬──────────┘
                          │
          ┌───────────────┴───────────────┐
          │                               │
          ▼                               ▼
    开仓/加仓信号                   减仓/平仓信号
    (buy+long/sell+short)          (sell+long/buy+short)
          │                               │
          ▼                               ▼
   ┌──────────────────┐           ┌──────────────────┐
   │  matchOpenAdd    │           │ matchCloseReduce │
   │  (三轮匹配)       │           │ (反向查找法)      │
   └────────┬─────────┘           └────────┬─────────┘
            │                               │
    ┌───────┴───────┐               ┌───────┴───────┐
    │               │               │               │
    ▼               ▼               ▼               ▼
 第1轮:新posId   第2轮:size↑     posId不存在    size减少
    │               │               │               │
    ▼               ▼               ▼               ▼
 ✅ ActionOpen  ✅ ActionAdd   ✅ ActionClose  ✅ ActionReduce
 创建映射       更新size         关闭映射       更新size
```

**代码实现（matchOpenAddSignal 核心逻辑）：**

```go
func (e *Engine) matchOpenAddSignal(signal *TradeSignal, leaderPosMap map[string]*Position) *SignalMatchResult {
    fill := signal.Fill
    
    // 第1轮：查找新开仓（无映射或 closed 的 posId）
    for _, pos := range matchedPositions {
        mapping, _ := e.store.CopyTrade().GetMapping(e.traderID, pos.PosID)
        if mapping == nil || mapping.Status == "closed" {
            return &SignalMatchResult{
                Action: ActionOpen,
                PosID:  pos.PosID,
                // ... 
            }
        }
    }
    
    // 第2轮：通过 lastKnownSize 变化判断加仓
    for _, pos := range matchedPositions {
        mapping, _ := e.store.CopyTrade().GetMapping(e.traderID, pos.PosID)
        if mapping != nil && mapping.Status == "active" {
            if pos.Size > mapping.LastKnownSize {  // ← 关键判断
                return &SignalMatchResult{
                    Action: ActionAdd,
                    PosID:  pos.PosID,
                    // ...
                }
            }
        }
    }
    
    // 第3轮：兜底
    if activeCount == 1 {
        return &SignalMatchResult{Action: ActionAdd, ...}
    }
    
    return &SignalMatchResult{ShouldFollow: false, Reason: "无法判断加仓目标"}
}
```

#### 2.2.3 各操作类型详解

##### 1️⃣ 新开仓

| 条件 | 处理 |
|------|------|
| posId 无映射 | ✅ 跟随开仓 |
| posId 状态=closed | ✅ 跟随开仓（同一 posId 不会复用，但逻辑兼容） |

```go
// 新开仓处理
if mapping == nil {
    // 执行开仓
    executeOpen(signal)
    // 保存映射
    store.SavePositionMapping(&CopyTradePositionMapping{
        TraderID:    traderID,
        LeaderPosID: signal.LeaderPosID,
        Symbol:      signal.Fill.Symbol,
        Side:        signal.Fill.PositionSide,
        MarginMode:  signal.MarginMode,
        Status:      "active",
    })
}
```

##### 2️⃣ 加仓

| 条件 | 处理 |
|------|------|
| posId 状态=active + 开仓信号 | ✅ 跟随加仓 |

```go
// 加仓处理
if mapping != nil && mapping.Status == "active" && signal.Fill.Action == "open" {
    // 执行加仓
    executeAdd(signal)
    // 更新加仓次数
    store.IncrementAddCount(traderID, signal.LeaderPosID)
}
```

##### 3️⃣ 减仓

| 条件 | 处理 |
|------|------|
| posId 状态=active + 平仓信号 + 领航员仍有持仓 | ✅ 按比例减仓 |

```go
// 减仓处理
if mapping != nil && mapping.Status == "active" && 
   signal.Fill.Action == "close" && signal.LeaderPosition.Size > 0 {
    // 计算减仓比例
    ratio := calculateReduceRatio(signal)
    // 执行减仓
    executeReduce(signal, ratio)
    // 更新减仓次数
    store.IncrementReduceCount(traderID, signal.LeaderPosID)
}
```

##### 4️⃣ 平仓

| 条件 | 处理 |
|------|------|
| posId 状态=active + 平仓信号 + 领航员持仓=0 | ✅ 全部平仓 |

```go
// 平仓处理
if mapping != nil && mapping.Status == "active" && 
   signal.Fill.Action == "close" && signal.LeaderPosition.Size == 0 {
    // 执行平仓
    executeClose(signal)
    // 关闭映射
    store.CloseMapping(traderID, signal.LeaderPosID, closePrice)
}
```

##### 5️⃣ 历史仓位（不跟随）

| 条件 | 处理 |
|------|------|
| posId 状态=ignored | ❌ 不跟随任何操作 |

```go
// 历史仓位处理
if mapping != nil && mapping.Status == "ignored" {
    logger.Infof("📊 历史仓位 | posId=%s status=ignored → 不跟随", signal.LeaderPosID)
    return false, "历史仓位，不跟随"
}
```

#### 2.2.4 同币种多仓位处理（重点！）

**场景：领航员对同一币种同时持有全仓和逐仓仓位**

```
领航员持仓状态：
┌─────────────────────────────────────────────────────────────────┐
│  posId=123456 │ SOLUSDT │ short │ cross    │ 100 SOL │ active  │
│  posId=234567 │ SOLUSDT │ short │ isolated │ 50 SOL  │ active  │
└─────────────────────────────────────────────────────────────────┘

旧方案问题：
- 用 symbol+side 作为 key → 两个仓位会覆盖，只能追踪一个
- 减仓时无法区分是哪个仓位

posId 方案优势：
- 每个仓位独立追踪
- 减仓/平仓时精确匹配
```

**处理流程：**

```
领航员操作：减仓 posId=234567 (逐仓 SOL short)
                    │
                    ▼
          查询映射 GetMapping("234567")
                    │
                    ▼
          找到 active 映射，marginMode=isolated
                    │
                    ▼
          设置 OKX 交易模式为 isolated
                    │
                    ▼
          执行减仓（精确操作逐仓仓位）
```

**代码实现：**

```go
// 执行减仓/平仓时，使用映射中的 marginMode
func executeReduceOrClose(signal *TradeSignal, mapping *CopyTradePositionMapping) {
    // 设置正确的保证金模式
    trader.SetMarginMode(signal.Symbol, mapping.MarginMode == "cross")
    
    // 执行操作（会操作对应模式的仓位）
    if signal.LeaderPosition.Size == 0 {
        trader.CloseLong(signal.Symbol, quantity)
    } else {
        trader.ReduceLong(signal.Symbol, ratio)
    }
}
```

#### 2.2.5 启动时初始化历史仓位

**时机：启动跟单服务时，在开始监听信号之前**

```go
// InitIgnoredPositions 初始化领航员历史仓位
func (e *Engine) InitIgnoredPositions() error {
    // 获取领航员当前所有持仓
    state, err := e.provider.GetAccountState(e.config.LeaderID)
    if err != nil {
        return err
    }
    
    // 将所有持仓标记为 ignored
    for _, pos := range state.Positions {
        if pos.PosID != "" {
            e.store.CopyTrade().SaveIgnoredPosition(
                e.traderID,
                e.config.LeaderID,
                pos.PosID,
                pos.Symbol,
                string(pos.Side),
                pos.MarginMode,
            )
            logger.Infof("📊 标记历史仓位 | posId=%s %s %s %s",
                pos.PosID, pos.Symbol, pos.Side, pos.MarginMode)
        }
    }
    
    return nil
}
```

**为什么在启动时标记？**

1. **100% 准确**：启动时领航员的所有持仓都是"历史仓位"
2. **一次性操作**：不需要每次收到信号都判断
3. **无误判风险**：去掉了阈值检测（1.2x）的不确定性

#### 2.2.6 映射生命周期

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        映射状态机                                        │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│   启动跟单                     新开仓成功                               │
│      │                            │                                     │
│      ▼                            ▼                                     │
│  ┌───────┐                   ┌───────┐                                 │
│  │ignored│                   │active │ ←─────────────────────┐         │
│  └───────┘                   └───┬───┘                       │         │
│      │                           │                           │         │
│      │ 任何信号                  │ 加仓/减仓成功              │         │
│      ▼                           ▼                           │         │
│  ❌ 不跟随                   ┌───────┐                       │         │
│                              │active │ (更新 add/reduce count)│         │
│                              └───┬───┘                       │         │
│                                  │                           │         │
│                                  │ 平仓成功                  │         │
│                                  ▼                           │         │
│                              ┌───────┐     同一 posId 重新   │         │
│                              │closed │ ──── 开仓（理论上不会）─┘         │
│                              └───────┘                                  │
│                                                                          │
│   注：closed 状态永久保留，作为历史记录                                 │
└─────────────────────────────────────────────────────────────────────────┘
```

#### 2.2.7 方案优势对比

| 对比项 | 旧方案（symbol+side+阈值） | 新方案（posId 统一） |
|--------|--------------------------|---------------------|
| 新开仓判断 | 依赖 1.2x 阈值，有误判 | 查数据库，100% 准确 |
| 加仓判断 | 依赖本地仓位对比 | 查数据库，100% 准确 |
| 减仓/平仓 | 无法区分同币种多仓位 | posId 精确匹配 |
| 同币种多仓位 | ❌ 不支持（会覆盖） | ✅ 独立追踪 |
| 历史仓位 | 阈值判断，有漏判 | ignored 状态，100% 准确 |
| 逻辑复杂度 | 多种判断逻辑 | 统一查数据库 |
| 可维护性 | 分散在多处 | 集中管理 |

#### 2.2.8 Hyperliquid 反向开仓处理

**处理策略：将反向开仓拆分为"先平后开"两个独立事件**

```
领航员反向开仓动作
       │
       ▼
┌──────────────────────────────────────────────────┐
│  Hyperliquid API 返回的反向开仓数据：              │
│  例：从 Long 反向到 Short                         │
│  • 先出现 "Close Long" 事件                       │
│  • 再出现 "Open Short" 事件                       │
└──────────────────────────────────────────────────┘
       │
       ▼
┌──────────────────────────────────────────────────┐
│               跟单系统处理逻辑                     │
├──────────────────────────────────────────────────┤
│                                                   │
│  事件1: Close Long                                │
│  ┌─────────────────────────────────────────────┐ │
│  │ 本地有 Long 仓位？                           │ │
│  │   ├── 是 → ✅ 执行平多                       │ │
│  │   └── 否 → ❌ 忽略（我们没跟过这个仓位）     │ │
│  └─────────────────────────────────────────────┘ │
│                                                   │
│  事件2: Open Short                                │
│  ┌─────────────────────────────────────────────┐ │
│  │ 本地有 Short 仓位？                          │ │
│  │   ├── 否 → ✅ 执行开空（新开仓）             │ │
│  │   └── 是 → ✅ 执行加空（加仓）               │ │
│  └─────────────────────────────────────────────┘ │
│                                                   │
└──────────────────────────────────────────────────┘
```

**为什么这样处理最简单稳定？**

1. **不需要本地推算反向量**：直接按 API 返回的事件顺序处理
2. **逻辑统一**：平仓就是平仓，开仓就是开仓，不需要特殊分支
3. **状态清晰**：每个事件独立处理，不依赖复杂的状态机

#### 2.2.9 边界情况处理

##### 边界 1：系统重启后仓位状态恢复

**问题场景**：系统重启导致内存状态丢失，领航员后续操作被错误忽略。

**解决方案**：`getLocalPosition()` 必须从**持久化存储**获取，不依赖内存缓存。

```go
// ✅ 正确做法：从交易所/数据库获取真实仓位
func (e *CopyTradeEngine) getLocalPosition(symbol, side string) *Position {
    // 复用现有系统的仓位查询（已经是持久化的）
    positions := e.followerTrader.GetPositions()  // 从交易所同步
    
    for _, pos := range positions {
        if pos.Symbol == symbol && pos.Side == side {
            return pos
        }
    }
    return nil
}

// ❌ 错误做法：依赖内存缓存
// var memoryCache map[string]*Position  // 重启后丢失！
```

> 💡 现有系统已有 `PositionSyncManager` 定期同步仓位，直接复用即可。

##### 边界 2：用户手动操作仓位

**问题场景**：用户手动平仓/加仓，导致本地仓位和跟单状态不一致。

**解决方案**：这是**用户主动行为**，系统应该尊重。

```go
// 用户手动平仓后：
// - 本地仓位 = 0
// - 领航员后续 add/reduce/close 会被忽略
// - 这是合理行为！因为用户已经主动退出该仓位

// 如果用户想重新跟随：
// - 等待领航员下一次新开仓即可
// - 系统会自动开始跟随
```

| 用户操作 | 系统行为 | 是否合理 |
|---------|---------|---------|
| 手动平仓 | 后续操作忽略 | ✅ 用户主动退出 |
| 手动加仓 | 后续按本地仓位处理 | ✅ 用户主动介入 |
| 手动减仓 | 后续按本地仓位处理 | ✅ 用户主动介入 |

##### 边界 3：反向开仓事件乱序（防御性处理）

**问题场景**：极端网络抖动导致 Open Short 先于 Close Long 到达。

**解决方案**：加一个"反向窗口"保护。

```go
// 反向窗口保护
type ReverseWindowProtection struct {
    recentCloses map[string]time.Time  // symbol -> close 时间
    windowSize   time.Duration         // 窗口大小，如 5 秒
}

func (e *CopyTradeEngine) shouldFollowSignal(signal *TradeSignal) (bool, string) {
    fill := signal.Fill
    localPosition := e.getLocalPosition(fill.Symbol, fill.PositionSide)
    
    // 检查是否是反向开仓（可能乱序）
    if fill.Action == "open" && localPosition != nil {
        oppositeSide := oppositeOf(fill.PositionSide)
        oppositePosition := e.getLocalPosition(fill.Symbol, oppositeSide)
        
        // 本地有反向仓位 + 新开仓 = 可能是反向开仓的乱序事件
        if oppositePosition != nil && oppositePosition.Size > 0 {
            // 检查是否在反向窗口内（最近 5 秒内有该 symbol 的 close）
            if e.isInReverseWindow(fill.Symbol, 5*time.Second) {
                logger.Warnf("⚠️ [%s] 检测到可能的反向开仓乱序 | %s | 等待 close 先执行",
                    e.traderID, fill.Symbol)
                // 暂存这个 open 事件，等 close 执行后再处理
                e.pendingReverseOpens[fill.Symbol] = signal
                return false, "反向窗口保护：等待 close 先执行"
            }
        }
    }
    
    // ... 正常判断逻辑
}
```

> 💡 这是**防御性编程**，实际发生概率很低（API 数据已按时间排序），但加上更安全。

### 2.3 无条件跟随原则

#### 2.3.1 核心原则

> **🎯 领航员的所有交易动作都要无条件跟随，系统不做任何限制，只做预警日志。**

**为什么？**

1. **真人交易员有独特风格**：每个领航员都有自己的交易策略和风格
2. **系统不应干预决策**：跟单的本质是信任领航员的判断
3. **保持一致性**：限制某些操作会导致仓位和领航员不同步
4. **用户自主选择**：用户选择跟单某个领航员，就是接受其全部操作

#### 2.3.2 风控改为预警模式

| 原设计 (限制模式) | 新设计 (预警模式) |
|------------------|------------------|
| 最小成交额：低于则**忽略**信号 | 最小成交额：低于则**记录预警日志**，仍执行 |
| 最大成交额：超过则**截断** | 最大成交额：超过则**记录预警日志**，按原金额执行 |
| 可用余额：超过95%则**限制** | 可用余额：不足则**记录预警日志**，尝试最大可能执行 |
| 频率限制：30秒内**拒绝** | 频率限制：**移除**，领航员所有操作都执行 |
| 持仓数量：超限**拒绝** | 持仓数量：**移除**，跟随领航员的全部仓位 |

```go
// 预警日志结构
type CopyTradeWarning struct {
    Timestamp   time.Time
    TraderID    string
    LeaderID    string
    Symbol      string
    WarningType string    // "low_value" | "high_value" | "insufficient_balance" | etc.
    Message     string
    Signal      *TradeSignal
    
    // 仍然执行
    Executed    bool
    ExecutedAt  time.Time
}
```

#### 2.3.3 预警类型定义

| 预警类型 | 触发条件 | 日志消息示例 |
|---------|---------|-------------|
| `low_value` | 计算跟单金额 < 10 USDT | "⚠️ 跟单金额较小 (5.2 USDT)，仍执行" |
| `high_value` | 计算跟单金额 > 设定上限 | "⚠️ 跟单金额较大 (5000 USDT)，仍执行" |
| `insufficient_balance` | 可用余额不足 | "⚠️ 余额不足，以最大可用 (980 USDT) 执行" |
| `high_frequency` | 同币种短时间多次操作 | "⚠️ 30秒内重复操作 BTCUSDT，仍执行" |
| `high_leverage` | 杠杆超过推荐值 | "⚠️ 同步杠杆 50x 较高，仍执行" |
| `large_position_ratio` | 单仓位占比超过账户 50% | "⚠️ 仓位占比 68%，仍执行" |

---

## 3. 系统架构

### 3.1 整体架构图（更新版）

```
                                    ┌─────────────────────────────────────┐
                                    │          External Data Sources      │
                                    ├─────────────────────────────────────┤
                                    │  Hyperliquid API    │   OKX API     │
                                    │  - userFills        │   - trade-records│
                                    │  - clearinghouseState│  - asset      │
                                    │                     │   - position   │
                                    └──────────┬──────────┴───────┬───────┘
                                               │                  │
                         ┌─────────────────────┴──────────────────┴─────────────────────┐
                         │                                                               │
                         ▼                                                               ▼
┌─────────────────────────────────────────┐     ┌─────────────────────────────────────────┐
│        Hyperliquid Provider             │     │            OKX Provider                 │
│  ┌───────────────────────────────────┐  │     │  ┌───────────────────────────────────┐  │
│  │   LeaderTracker (领航员追踪器)    │  │     │  │   LeaderTracker (领航员追踪器)    │  │
│  │   - 轮询/WebSocket监听成交记录    │  │     │  │   - 轮询监听成交记录             │  │
│  │   - 解析 dir (Open/Close/Add)    │  │     │  │   - 解析 posSide + side          │  │
│  │   - 去重 (tid/ordId)              │  │     │  │   - 去重 (ordId)                 │  │
│  └───────────────────────────────────┘  │     │  └───────────────────────────────────┘  │
│  ┌───────────────────────────────────┐  │     │  ┌───────────────────────────────────┐  │
│  │   PositionSync (持仓同步器)       │  │     │  │   PositionSync (持仓同步器)       │  │
│  │   - 定期同步领航员持仓快照        │  │     │  │   - 定期同步领航员持仓快照        │  │
│  │   - 用于比例计算和状态校验        │  │     │  │   - 用于比例计算和状态校验        │  │
│  └───────────────────────────────────┘  │     │  └───────────────────────────────────┘  │
└──────────────────────┬──────────────────┘     └───────────────────────┬─────────────────┘
                       │                                                │
                       └───────────────────────┬────────────────────────┘
                                               │
                                               ▼
                       ┌───────────────────────────────────────────────────┐
                       │              Copy Trading Engine                  │
                       │  ┌─────────────────────────────────────────────┐  │
                       │  │        Signal Processor (信号处理器)        │  │
                       │  │  - 标准化交易信号 (Open/Close/Add/Reduce)  │  │
                       │  │  - 计算跟单比例                            │  │
                       │  │  - 生成统一 Decision 结构                  │  │
                       │  └─────────────────────────────────────────────┘  │
                       │  ┌─────────────────────────────────────────────┐  │
                       │  │        Copy Ratio Calculator                │  │
                       │  │  - 按资金比例计算开仓金额                   │  │
                       │  │  - 应用跟单系数 (100%/200%/50%)            │  │
                       │  │  - 最小/最大限额校验                       │  │
                       │  └─────────────────────────────────────────────┘  │
                       └───────────────────────┬───────────────────────────┘
                                               │
                                               ▼
                       ┌───────────────────────────────────────────────────┐
                       │         Decision Adapter (决策适配器)             │
                       │  - 将跟单信号转换为 decision.Decision 结构        │
                       │  - 填充 reasoning (跟单来源说明)                  │
                       │  - 兼容现有 decision.FullDecision 格式            │
                       └───────────────────────┬───────────────────────────┘
                                               │
                                               ▼
          ┌────────────────────────────────────────────────────────────────────────────────┐
          │                        NOFX Core System (现有系统)                              │
          ├────────────────────────────────────────────────────────────────────────────────┤
          │                                                                                │
          │  ┌─────────────┐   ┌─────────────┐   ┌─────────────┐   ┌─────────────────────┐ │
          │  │ AutoTrader  │   │ DecisionStore│  │ Execution   │   │ Dashboard/Frontend │ │
          │  │ (执行调度)  │   │ (日志存储)   │  │ (交易执行)  │   │ (仪表盘/前端)      │ │
          │  └─────────────┘   └─────────────┘   └─────────────┘   └─────────────────────┘ │
          │                                                                                │
          └────────────────────────────────────────────────────────────────────────────────┘
```

### 3.2 模块目录结构（精简版）

> 采用「仓位对比法」后，逻辑大幅简化，不需要复杂的状态管理。

```
nofx/
├── copytrade/                      # 🆕 跟单插件模块 (新增)
│   │
│   ├── provider.go                 # 领航员数据获取（HL + OKX 合并）
│   │                               # - GetFills() 获取成交记录
│   │                               # - GetAccountState() 获取持仓/余额
│   │
│   ├── engine.go                   # 跟单核心引擎（所有逻辑合并）
│   │                               # - 信号接收与去重
│   │                               # - 仓位对比判断（跟/不跟）
│   │                               # - 比例计算
│   │                               # - Decision 生成
│   │
│   ├── types.go                    # 类型定义
│   │                               # - Fill, Position, Signal 等
│   │
│   └── manager.go                  # 跟单管理器（入口）
│                                   # - 启动/停止
│                                   # - 多账户隔离
│
├── store/
│   └── copytrade.go                # 🆕 跟单配置存储
│
├── api/
│   └── copytrade_handler.go        # 🆕 跟单 API 处理器
│
└── decision/
    └── engine.go                   # ⚠️ 轻微修改：增加 Provider 接口
```

### 3.3 为什么可以精简？

| 原设计 | 精简后 | 原因 |
|-------|--------|------|
| `provider/` 目录 (4文件) | `provider.go` (1文件) | HL 和 OKX 的 API 调用逻辑简单，合并即可 |
| `tracker/` 目录 (5文件) | 合并到 `engine.go` | 仓位对比法不需要复杂的状态追踪 |
| `tracker/position_sync.go` | ❌ 删除 | 直接调用 API 获取实时持仓，不需要本地同步 |
| `tracker/dedup.go` | 合并到 `engine.go` | 去重逻辑很简单，几十行代码 |
| `engine/` 目录 (4文件) | `engine.go` (1文件) | 信号处理、比例计算、适配器都很简单，合并 |
| `config/` 目录 | ❌ 删除 | 配置结构放在 `store/copytrade.go` |

### 3.4 精简后的代码量估算

| 文件 | 预估行数 | 核心功能 |
|-----|---------|---------|
| `provider.go` | ~200 行 | API 调用（HL + OKX） |
| `engine.go` | ~400 行 | 核心逻辑（判断+计算+生成） |
| `types.go` | ~100 行 | 类型定义 |
| `manager.go` | ~150 行 | 生命周期管理 |
| **总计** | **~850 行** | 整个跟单模块 |

> 💡 **设计原则**：仓位对比法让逻辑变简单，代码也应该简单。不要为了"看起来专业"而过度拆分。

### 3.5 关键设计原则

#### 3.5.1 复用现有系统，不重复造轮子

> ⚠️ **核心原则**：跟单模块直接接入现有系统，复用已有的设计，不单独设计。

| 功能 | 做法 | 复用什么 |
|-----|------|---------|
| **多账户隔离** | 直接复用 | 现有的 `trader_id` 隔离机制 |
| **Trader 管理** | 直接复用 | 现有的 `TraderManager` |
| **交易执行** | 直接复用 | 现有的 `trader/*` 执行器 |
| **仓位查询** | 直接复用 | 现有的 `GetPositions()` |
| **余额查询** | 直接复用 | 现有的 `GetBalance()` |
| **日志系统** | 直接复用 | 现有的 `logger` 包 |
| **数据存储** | 直接复用 | 现有的 `store` 层 |
| **API 路由** | 直接复用 | 现有的 `api/server.go` 路由结构 |

#### 3.5.2 跟单 = Trader 的一个"决策源"选项

```go
// 现有 Trader 结构（不需要大改）
type Trader struct {
    ID              string
    Name            string
    Exchange        string
    // ... 现有字段
    
    // 🆕 新增：决策模式
    DecisionMode    string        // "ai" | "copy_trade"
    CopyTradeConfig *CopyConfig   // 跟单配置（如果 DecisionMode == "copy_trade"）
}

// 跟单配置（作为 Trader 的属性，不是独立实体）
type CopyConfig struct {
    ProviderType    string        // "hyperliquid" | "okx"
    LeaderID        string        // 领航员地址/uniqueName
    CopyRatio       float64       // 跟单系数 (1.0 = 100%)
    SyncLeverage    bool          // 同步杠杆
    // ... 其他配置
}
```

#### 3.5.3 多账户隔离 = 现有机制

```go
// 现有的 TraderManager 已经实现了多账户隔离
// 跟单模块直接接入，不需要单独设计

// 启动 Trader 时，根据 DecisionMode 选择决策源
func (m *TraderManager) StartTrader(traderID string) error {
    trader := m.traders[traderID]
    
    switch trader.DecisionMode {
    case "ai":
        // 现有的 AI 决策流程
        go m.runAIDecisionLoop(trader)
        
    case "copy_trade":
        // 新增的跟单决策流程（复用同一个 trader 实例）
        go m.runCopyTradeLoop(trader)
    }
    
    return nil
}

// 跟单循环复用现有的 trader 实例
func (m *TraderManager) runCopyTradeLoop(trader *Trader) {
    engine := copytrade.NewEngine(
        trader.ID,
        trader.CopyTradeConfig,
        trader.Exchange,           // 复用现有的交易所连接
        func() float64 {           // 复用现有的余额查询
            return trader.GetBalance()
        },
        func() []*Position {       // 复用现有的仓位查询
            return trader.GetPositions()
        },
    )
    
    engine.Run()
}
```

#### 3.5.4 这样设计的好处

| 好处 | 说明 |
|-----|------|
| **代码更少** | 不需要重写隔离、管理、执行逻辑 |
| **完全兼容** | 跟单 Trader 和 AI Trader 共用同一套管理界面 |
| **维护简单** | 只维护一套系统，不用担心两套逻辑不同步 |
| **切换方便** | 同一个 Trader 可以在 AI 和跟单模式之间切换 |
| **风控统一** | 复用现有的仓位同步、资金管理等 |

---

## 4. 核心模块设计

### 4.1 Provider 模块 (领航员数据提供者)

#### 4.1.1 接口定义

```go
// copytrade/provider/interface.go

package provider

import "time"

// LeaderProvider 领航员数据提供者接口
type LeaderProvider interface {
    // GetFills 获取最近成交记录
    GetFills(leaderID string, since time.Time) ([]Fill, error)
    
    // GetAccountState 获取账户状态 (资产 + 持仓)
    GetAccountState(leaderID string) (*AccountState, error)
    
    // Type 返回提供者类型
    Type() string
}

// Fill 成交记录 (标准化结构)
type Fill struct {
    ID            string    // 唯一标识 (HL: tid, OKX: ordId)
    Symbol        string    // 交易对 (BTCUSDT 格式)
    Side          string    // "buy" | "sell"
    PositionSide  string    // "long" | "short"
    Action        string    // "open" | "close" | "add" | "reduce"
    Price         float64   // 成交价格
    Size          float64   // 成交数量
    Value         float64   // 成交价值 (USDT)
    Timestamp     time.Time // 成交时间
    ClosedPnL     float64   // 平仓盈亏 (如有)
    
    // 原始数据
    Raw           interface{}
}

// AccountState 账户状态
type AccountState struct {
    TotalEquity     float64              // 总权益
    AvailableBalance float64             // 可用余额
    Positions       map[string]*Position // 当前持仓 (symbol -> position)
    Timestamp       time.Time
}

// Position 持仓信息
type Position struct {
    Symbol        string
    Side          string   // "long" | "short"
    Size          float64  // 持仓数量
    EntryPrice    float64  // 开仓均价
    MarkPrice     float64  // 标记价格
    Leverage      int      // 杠杆
    MarginMode    string   // "cross" | "isolated"
    UnrealizedPnL float64
    PositionValue float64  // 仓位价值
}
```

#### 4.1.2 Hyperliquid 实现

```go
// copytrade/provider/hyperliquid.go

package provider

import (
    "encoding/json"
    "fmt"
    "net/http"
    "time"
)

const (
    HLInfoAPI = "https://api.hyperliquid.xyz/info"
)

type HyperliquidProvider struct {
    client *http.Client
}

func NewHyperliquidProvider() *HyperliquidProvider {
    return &HyperliquidProvider{
        client: &http.Client{Timeout: 10 * time.Second},
    }
}

func (p *HyperliquidProvider) Type() string {
    return "hyperliquid"
}

// GetFills 获取成交记录
func (p *HyperliquidProvider) GetFills(leaderID string, since time.Time) ([]Fill, error) {
    req := map[string]string{
        "type": "userFills",
        "user": leaderID,
    }
    
    // 调用 API
    var rawFills []HLFillRaw
    if err := p.post(req, &rawFills); err != nil {
        return nil, err
    }
    
    // 转换为标准格式
    var fills []Fill
    for _, raw := range rawFills {
        ts := time.UnixMilli(raw.Time)
        if ts.Before(since) {
            continue
        }
        
        fill := Fill{
            ID:           fmt.Sprintf("%d", raw.TID),
            Symbol:       normalizeSymbol(raw.Coin),
            Price:        parseFloat(raw.Px),
            Size:         parseFloat(raw.Sz),
            Timestamp:    ts,
            ClosedPnL:    parseFloat(raw.ClosedPnl),
            Raw:          raw,
        }
        
        // 解析方向
        fill.Side, fill.PositionSide, fill.Action = parseHLDirection(raw.Side, raw.Dir)
        
        // 计算成交价值
        fill.Value = fill.Price * fill.Size
        
        fills = append(fills, fill)
    }
    
    return fills, nil
}

// GetAccountState 获取账户状态
func (p *HyperliquidProvider) GetAccountState(leaderID string) (*AccountState, error) {
    req := map[string]string{
        "type": "clearinghouseState",
        "user": leaderID,
    }
    
    var raw HLClearinghouseState
    if err := p.post(req, &raw); err != nil {
        return nil, err
    }
    
    state := &AccountState{
        TotalEquity:      parseFloat(raw.MarginSummary.AccountValue),
        AvailableBalance: parseFloat(raw.Withdrawable),
        Positions:        make(map[string]*Position),
        Timestamp:        time.UnixMilli(raw.Time),
    }
    
    // 解析持仓
    for _, ap := range raw.AssetPositions {
        pos := ap.Position
        symbol := normalizeSymbol(pos.Coin)
        
        size := parseFloat(pos.Szi)
        side := "long"
        if size < 0 {
            side = "short"
            size = -size
        }
        
        state.Positions[symbol] = &Position{
            Symbol:        symbol,
            Side:          side,
            Size:          size,
            EntryPrice:    parseFloat(pos.EntryPx),
            Leverage:      pos.Leverage.Value,
            MarginMode:    pos.Leverage.Type,
            UnrealizedPnL: parseFloat(pos.UnrealizedPnl),
            PositionValue: parseFloat(pos.PositionValue),
        }
    }
    
    return state, nil
}

// parseHLDirection 解析 Hyperliquid 的交易方向
func parseHLDirection(side, dir string) (tradeSide, posSide, action string) {
    // side: "B" = Buy, "A" = Ask/Sell
    // dir: "Open Long", "Close Long", "Open Short", "Close Short"
    
    switch dir {
    case "Open Long":
        return "buy", "long", "open"
    case "Close Long":
        return "sell", "long", "close"
    case "Open Short":
        return "sell", "short", "open"
    case "Close Short":
        return "buy", "short", "close"
    default:
        // 兜底：根据 side 和 startPosition 判断
        if side == "B" {
            return "buy", "long", "add"
        }
        return "sell", "short", "add"
    }
}
```

#### 4.1.3 OKX 实现

```go
// copytrade/provider/okx.go

package provider

import (
    "encoding/json"
    "fmt"
    "net/http"
    "time"
)

const (
    OKXTradeRecordsAPI = "https://www.okx.com/priapi/v5/ecotrade/public/community/user/trade-records"
    OKXAssetAPI        = "https://www.okx.com/priapi/v5/ecotrade/public/community/user/asset"
    OKXPositionAPI     = "https://www.okx.com/priapi/v5/ecotrade/public/community/user/position-current"
)

type OKXProvider struct {
    client *http.Client
}

func NewOKXProvider() *OKXProvider {
    return &OKXProvider{
        client: &http.Client{Timeout: 10 * time.Second},
    }
}

func (p *OKXProvider) Type() string {
    return "okx"
}

// GetFills 获取成交记录
func (p *OKXProvider) GetFills(uniqueName string, since time.Time) ([]Fill, error) {
    now := time.Now()
    url := fmt.Sprintf(
        "%s?uniqueName=%s&startModify=%d&endModify=%d&instType=SWAP&limit=50&t=%d",
        OKXTradeRecordsAPI,
        uniqueName,
        since.UnixMilli(),
        now.UnixMilli(),
        now.UnixMilli(),
    )
    
    var resp OKXTradeRecordsResp
    if err := p.get(url, &resp); err != nil {
        return nil, err
    }
    
    if resp.Code != "0" {
        return nil, fmt.Errorf("OKX API error: %s", resp.Msg)
    }
    
    var fills []Fill
    for _, raw := range resp.Data {
        fill := Fill{
            ID:        raw.OrdId,
            Symbol:    normalizeOKXSymbol(raw.InstId),
            Price:     parseFloat(raw.AvgPx),
            Size:      parseFloat(raw.Sz),
            Value:     parseFloat(raw.Value),
            Timestamp: time.UnixMilli(parseInt(raw.FillTime)),
            Raw:       raw,
        }
        
        // 解析方向
        fill.Side, fill.PositionSide, fill.Action = parseOKXDirection(raw.Side, raw.PosSide)
        
        fills = append(fills, fill)
    }
    
    return fills, nil
}

// GetAccountState 获取账户状态
func (p *OKXProvider) GetAccountState(uniqueName string) (*AccountState, error) {
    // 1. 获取资产
    assetURL := fmt.Sprintf("%s?uniqueName=%s&t=%d", OKXAssetAPI, uniqueName, time.Now().UnixMilli())
    var assetResp OKXAssetResp
    if err := p.get(assetURL, &assetResp); err != nil {
        return nil, err
    }
    
    // 2. 获取持仓
    posURL := fmt.Sprintf("%s?uniqueName=%s&t=%d", OKXPositionAPI, uniqueName, time.Now().UnixMilli())
    var posResp OKXPositionResp
    if err := p.get(posURL, &posResp); err != nil {
        return nil, err
    }
    
    state := &AccountState{
        Positions: make(map[string]*Position),
        Timestamp: time.Now(),
    }
    
    // 解析资产 (USDT 为总权益)
    for _, asset := range assetResp.Data {
        if asset.Currency == "USDT" {
            state.TotalEquity = parseFloat(asset.Amount)
            state.AvailableBalance = state.TotalEquity // OKX 跟单接口不单独返回可用
            break
        }
    }
    
    // 解析持仓
    for _, pd := range posResp.Data {
        for _, pos := range pd.PosData {
            symbol := normalizeOKXSymbol(pos.InstId)
            
            state.Positions[symbol] = &Position{
                Symbol:        symbol,
                Side:          pos.PosSide,
                Size:          parseFloat(pos.Pos),
                EntryPrice:    parseFloat(pos.AvgPx),
                MarkPrice:     parseFloat(pos.MarkPx),
                Leverage:      parseInt(pos.Lever),
                MarginMode:    pos.MgnMode,
                UnrealizedPnL: parseFloat(pos.Upl),
                PositionValue: parseFloat(pos.NotionalUsd),
            }
        }
    }
    
    return state, nil
}

// parseOKXDirection 解析 OKX 交易方向
func parseOKXDirection(side, posSide string) (tradeSide, positionSide, action string) {
    positionSide = posSide // "long" | "short"
    
    // OKX: side = "buy" | "sell", posSide = "long" | "short"
    // buy + long = 开多/加多
    // sell + long = 平多/减多
    // sell + short = 开空/加空
    // buy + short = 平空/减空
    
    if side == "buy" && posSide == "long" {
        return "buy", "long", "open"
    } else if side == "sell" && posSide == "long" {
        return "sell", "long", "close"
    } else if side == "sell" && posSide == "short" {
        return "sell", "short", "open"
    } else if side == "buy" && posSide == "short" {
        return "buy", "short", "close"
    }
    
    return side, posSide, "unknown"
}
```

### 4.2 Tracker 模块 (领航员追踪器)

```go
// copytrade/tracker/interface.go

package tracker

import (
    "context"
    "nofx/copytrade/provider"
)

// LeaderTracker 领航员追踪器
type LeaderTracker interface {
    // Start 启动追踪
    Start(ctx context.Context) error
    
    // Stop 停止追踪
    Stop()
    
    // Subscribe 订阅信号
    // 返回一个 channel，有新成交时推送
    Subscribe() <-chan *TradeSignal
    
    // GetLeaderState 获取领航员当前状态
    GetLeaderState() (*provider.AccountState, error)
}

// TradeSignal 交易信号 (经过处理的成交事件)
type TradeSignal struct {
    LeaderID     string          // 领航员 ID
    ProviderType string          // "hyperliquid" | "okx"
    Fill         *provider.Fill  // 成交记录
    
    // 领航员账户快照 (用于比例计算)
    LeaderEquity     float64     // 领航员总权益
    LeaderPosition   *provider.Position // 该币种的持仓 (如有)
}
```

```go
// copytrade/tracker/hyperliquid_tracker.go

package tracker

import (
    "context"
    "sync"
    "time"
    
    "nofx/copytrade/provider"
    "nofx/logger"
)

type HyperliquidTracker struct {
    leaderID     string
    provider     *provider.HyperliquidProvider
    
    signalCh     chan *TradeSignal
    stopCh       chan struct{}
    
    // 去重（使用时间戳过期）
    seenFills    map[string]time.Time  // signal_id -> 首次看到的时间
    seenMu       sync.RWMutex
    seenTTL      time.Duration         // 过期时间（默认 1 小时）
    
    // 状态缓存
    lastState    *provider.AccountState
    lastSync     time.Time
    
    // 配置
    pollInterval time.Duration
}

func NewHyperliquidTracker(leaderID string) *HyperliquidTracker {
    return &HyperliquidTracker{
        leaderID:     leaderID,
        provider:     provider.NewHyperliquidProvider(),
        signalCh:     make(chan *TradeSignal, 100),
        stopCh:       make(chan struct{}),
        seenFills:    make(map[string]time.Time),
        seenTTL:      1 * time.Hour,      // 1 小时后自动过期
        pollInterval: 3 * time.Second,
    }
}

func (t *HyperliquidTracker) Start(ctx context.Context) error {
    logger.Infof("🎯 Starting Hyperliquid tracker for leader: %s", t.leaderID)
    
    // 初始同步状态
    state, err := t.provider.GetAccountState(t.leaderID)
    if err != nil {
        return fmt.Errorf("failed to get initial state: %w", err)
    }
    t.lastState = state
    t.lastSync = time.Now()
    
    // 启动轮询协程
    go t.pollLoop(ctx)
    
    return nil
}

func (t *HyperliquidTracker) Stop() {
    close(t.stopCh)
}

func (t *HyperliquidTracker) Subscribe() <-chan *TradeSignal {
    return t.signalCh
}

func (t *HyperliquidTracker) GetLeaderState() (*provider.AccountState, error) {
    // 如果缓存过期 (>30秒)，重新获取
    if time.Since(t.lastSync) > 30*time.Second {
        state, err := t.provider.GetAccountState(t.leaderID)
        if err != nil {
            return t.lastState, nil // 返回旧缓存
        }
        t.lastState = state
        t.lastSync = time.Now()
    }
    return t.lastState, nil
}

func (t *HyperliquidTracker) pollLoop(ctx context.Context) {
    ticker := time.NewTicker(t.pollInterval)
    defer ticker.Stop()
    
    // 首次运行：获取最近成交作为基线
    since := time.Now().Add(-5 * time.Minute)
    fills, _ := t.provider.GetFills(t.leaderID, since)
    for _, f := range fills {
        t.markSeen(f.ID)
    }
    
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.stopCh:
            return
        case <-ticker.C:
            t.poll()
        }
    }
}

func (t *HyperliquidTracker) poll() {
    // 获取最近 1 分钟的成交
    since := time.Now().Add(-1 * time.Minute)
    fills, err := t.provider.GetFills(t.leaderID, since)
    if err != nil {
        logger.Warnf("⚠️ Failed to poll Hyperliquid fills: %v", err)
        return
    }
    
    // 同步账户状态
    state, err := t.provider.GetAccountState(t.leaderID)
    if err != nil {
        logger.Warnf("⚠️ Failed to get leader state: %v", err)
    } else {
        t.lastState = state
        t.lastSync = time.Now()
    }
    
    // 处理新成交（按时间排序，确保反向开仓按顺序处理）
    sort.Slice(fills, func(i, j int) bool {
        return fills[i].Timestamp.Before(fills[j].Timestamp)
    })
    
    for _, fill := range fills {
        if t.isSeen(fill.ID) {
            continue
        }
        t.markSeen(fill.ID)
        
        // 构造信号
        signal := &TradeSignal{
            LeaderID:     t.leaderID,
            ProviderType: "hyperliquid",
            Fill:         &fill,
            LeaderEquity: state.TotalEquity,
        }
        
        // 附加该币种的持仓信息
        if pos, ok := state.Positions[fill.Symbol]; ok {
            signal.LeaderPosition = pos
        }
        
        // 推送信号
        // 🎯 反向开仓处理：Hyperliquid 会返回两条记录（先 Close 后 Open）
        // 按时间顺序推送，Engine 会自动根据本地仓位判断是否跟随
        select {
        case t.signalCh <- signal:
            logger.Infof("📡 New signal: %s %s %s @ %.4f", 
                fill.Symbol, fill.Action, fill.PositionSide, fill.Price)
        default:
            logger.Warnf("⚠️ Signal channel full, dropping: %s", fill.ID)
        }
    }
}

// 🎯 反向开仓说明：
// Hyperliquid 的反向开仓在 API 层面会返回两条成交记录：
// 1. Close Long/Short - 平掉原仓位
// 2. Open Short/Long - 开新方向仓位
//
// 我们不需要特殊处理，只需要：
// 1. 按时间顺序推送信号
// 2. Engine 根据本地仓位判断：
//    - 如果本地有该方向仓位 → 执行平仓
//    - 如果本地无该方向仓位 → 忽略平仓（历史仓位）
//    - 新方向开仓 → 当作新开仓处理

func (t *HyperliquidTracker) isSeen(id string) bool {
    t.seenMu.RLock()
    defer t.seenMu.RUnlock()
    
    seenTime, exists := t.seenFills[id]
    if !exists {
        return false
    }
    
    // 检查是否已过期
    if time.Since(seenTime) > t.seenTTL {
        return false  // 已过期，视为未见过
    }
    
    return true
}

func (t *HyperliquidTracker) markSeen(id string) {
    t.seenMu.Lock()
    defer t.seenMu.Unlock()
    
    t.seenFills[id] = time.Now()
    
    // 定期清理过期记录（每 1000 条检查一次）
    if len(t.seenFills) > 1000 && len(t.seenFills)%100 == 0 {
        t.cleanExpiredFills()
    }
}

// cleanExpiredFills 清理过期的去重记录
func (t *HyperliquidTracker) cleanExpiredFills() {
    now := time.Now()
    for id, seenTime := range t.seenFills {
        if now.Sub(seenTime) > t.seenTTL {
            delete(t.seenFills, id)
        }
    }
    logger.Debugf("🧹 清理过期去重记录，剩余 %d 条", len(t.seenFills))
}
```

### 4.3 Engine 模块 (跟单引擎)

#### 4.3.1 核心引擎

```go
// copytrade/engine/engine.go

package engine

import (
    "context"
    "fmt"
    "sync"
    "time"
    
    "nofx/copytrade/config"
    "nofx/copytrade/tracker"
    "nofx/decision"
    "nofx/logger"
    "nofx/store"
)

// CopyTradeEngine 跟单引擎
type CopyTradeEngine struct {
    traderID     string
    config       *config.CopyTradeConfig
    tracker      tracker.LeaderTracker
    
    // 跟随者账户信息 (由外部注入)
    getFollowerEquity func() float64
    
    // 决策输出
    decisionCh   chan *decision.FullDecision
    
    // 状态
    running      bool
    mu           sync.RWMutex
    
    // 统计
    stats        *EngineStats
}

type EngineStats struct {
    SignalsReceived  int64
    DecisionsGenerated int64
    LastSignalTime   time.Time
}

func NewCopyTradeEngine(
    traderID string,
    cfg *config.CopyTradeConfig,
    getFollowerEquity func() float64,
) (*CopyTradeEngine, error) {
    // 创建 tracker
    var t tracker.LeaderTracker
    switch cfg.ProviderType {
    case "hyperliquid":
        t = tracker.NewHyperliquidTracker(cfg.LeaderID)
    case "okx":
        t = tracker.NewOKXTracker(cfg.LeaderID)
    default:
        return nil, fmt.Errorf("unsupported provider type: %s", cfg.ProviderType)
    }
    
    return &CopyTradeEngine{
        traderID:          traderID,
        config:            cfg,
        tracker:           t,
        getFollowerEquity: getFollowerEquity,
        decisionCh:        make(chan *decision.FullDecision, 10),
        stats:             &EngineStats{},
    }, nil
}

// Start 启动引擎
func (e *CopyTradeEngine) Start(ctx context.Context) error {
    e.mu.Lock()
    if e.running {
        e.mu.Unlock()
        return fmt.Errorf("engine already running")
    }
    e.running = true
    e.mu.Unlock()
    
    // 启动 tracker
    if err := e.tracker.Start(ctx); err != nil {
        return err
    }
    
    // 启动信号处理协程
    go e.processSignals(ctx)
    
    logger.Infof("🚀 CopyTrade engine started for trader %s", e.traderID)
    return nil
}

// Stop 停止引擎
func (e *CopyTradeEngine) Stop() {
    e.mu.Lock()
    defer e.mu.Unlock()
    
    if !e.running {
        return
    }
    
    e.tracker.Stop()
    e.running = false
    close(e.decisionCh)
    
    logger.Infof("🛑 CopyTrade engine stopped for trader %s", e.traderID)
}

// GetDecisionChannel 获取决策输出通道
func (e *CopyTradeEngine) GetDecisionChannel() <-chan *decision.FullDecision {
    return e.decisionCh
}

// processSignals 处理交易信号
func (e *CopyTradeEngine) processSignals(ctx context.Context) {
    signals := e.tracker.Subscribe()
    
    for {
        select {
        case <-ctx.Done():
            return
        case signal, ok := <-signals:
            if !ok {
                return
            }
            
            e.stats.SignalsReceived++
            e.stats.LastSignalTime = time.Now()
            
            // 处理信号
            dec, err := e.processSignal(signal)
            if err != nil {
                logger.Warnf("⚠️ Failed to process signal: %v", err)
                continue
            }
            
            if dec != nil {
                e.stats.DecisionsGenerated++
                
                // 推送决策
                select {
                case e.decisionCh <- dec:
                default:
                    logger.Warnf("⚠️ Decision channel full, dropping")
                }
            }
        }
    }
}

// processSignal 处理单个信号，生成决策
func (e *CopyTradeEngine) processSignal(signal *tracker.TradeSignal) (*decision.FullDecision, error) {
    fill := signal.Fill
    cfg := e.config
    
    // 1. 🎯 核心规则：只跟新开仓（本地仓位对比法）
    follow, reason := e.shouldFollowSignal(signal)
    if !follow {
        logger.Infof("📋 跳过信号: %s | %s %s %s", reason, fill.Symbol, fill.Action, fill.PositionSide)
        return nil, nil
    }
    logger.Infof("✅ 跟随信号: %s | %s %s %s", reason, fill.Symbol, fill.Action, fill.PositionSide)
    
    // 2. 计算跟单仓位（带预警，不限制）
    copySize, warnings := e.calculateCopySizeWithWarnings(signal)
    
    // 3. 记录所有预警（不阻止交易）
    for _, w := range warnings {
        e.warningLogger.Log(w)
    }
    
    // 4. 构造 Decision（无论预警多少都执行）
    dec := e.buildDecision(signal, copySize)
    
    // 5. 包装为 FullDecision
    fullDec := &decision.FullDecision{
        SystemPrompt:        e.buildSystemPromptLog(),
        UserPrompt:          e.buildUserPromptLog(signal),
        CoTTrace:            e.buildCoTTrace(signal, copySize, warnings),
        Decisions:           []decision.Decision{dec},
        RawResponse:         fmt.Sprintf("Copy trade signal from %s:%s", cfg.ProviderType, cfg.LeaderID),
        Timestamp:           time.Now(),
        AIRequestDurationMs: 0, // 跟单无 AI 调用
        Warnings:            warnings, // 新增：预警列表
    }
    
    return fullDec, nil
}

// shouldFollowSignal 🎯 核心：判断是否应该跟随该信号（只跟新开仓原则）
func (e *CopyTradeEngine) shouldFollowSignal(signal *tracker.TradeSignal) (follow bool, reason string) {
    fill := signal.Fill
    
    // 获取本地仓位
    localPosition := e.getLocalPosition(fill.Symbol, fill.PositionSide)
    hasLocalPosition := localPosition != nil && localPosition.Size > 0
    
    switch fill.Action {
    case "open":
        if !hasLocalPosition {
            // 本地无仓位 + 领航员开仓 = 新开仓（跟！）
            return true, "新开仓信号，本地无持仓"
        } else {
            // 本地有仓位 + 领航员"开仓" = 其实是加仓（跟！是我们跟过的仓位）
            return true, "加仓信号，跟随已有仓位"
        }
        
    case "add":
        if !hasLocalPosition {
            // 本地无仓位 + 领航员加仓 = 历史仓位的加仓（不跟！）
            return false, "忽略：领航员历史仓位加仓，我们未跟随该仓位"
        }
        // 本地有仓位 = 是我们跟过的（跟！）
        return true, "加仓信号，跟随已有仓位"
        
    case "reduce":
        if !hasLocalPosition {
            // 本地无仓位 + 领航员减仓 = 历史仓位的减仓（不跟！）
            return false, "忽略：领航员历史仓位减仓，我们未跟随该仓位"
        }
        // 本地有仓位 = 是我们跟过的（跟！）
        return true, "减仓信号，跟随已有仓位"
        
    case "close":
        if !hasLocalPosition {
            // 本地无仓位 + 领航员平仓 = 历史仓位的平仓（不跟！）
            // 这也处理了反向开仓的"平仓部分"：如果我们没有该仓位，直接忽略
            return false, "忽略：领航员历史仓位平仓，我们未跟随该仓位"
        }
        // 本地有仓位 = 是我们跟过的（跟！）
        return true, "平仓信号，跟随已有仓位"
        
    default:
        return false, fmt.Sprintf("未知操作类型: %s", fill.Action)
    }
}

// getLocalPosition 获取本地仓位
func (e *CopyTradeEngine) getLocalPosition(symbol, side string) *Position {
    // 调用现有系统的仓位查询接口
    positions := e.followerTrader.GetPositions()
    
    for _, pos := range positions {
        if pos.Symbol == symbol && pos.Side == side {
            return pos
        }
    }
    return nil
}

// determineCloseAction 判断是减仓还是平仓（通过领航员实时持仓判断）
func (e *CopyTradeEngine) determineCloseAction(signal *tracker.TradeSignal) string {
    // 获取领航员该币种的当前持仓
    leaderPosition := signal.LeaderPosition
    
    if leaderPosition == nil || leaderPosition.Size == 0 {
        // 领航员该币种仓位已清零 = 平仓
        return "close"
    }
    // 领航员该币种仓位仍有 = 减仓
    return "reduce"
}

// calculateReduceRatio 计算减仓比例（用于部分平仓）
func (e *CopyTradeEngine) calculateReduceRatio(signal *tracker.TradeSignal) float64 {
    // 领航员本次减仓数量
    reduceSize := signal.Fill.Size
    
    // 领航员减仓前的持仓 = 当前持仓 + 本次减仓量
    leaderCurrentSize := float64(0)
    if signal.LeaderPosition != nil {
        leaderCurrentSize = signal.LeaderPosition.Size
    }
    leaderPreviousSize := leaderCurrentSize + reduceSize
    
    if leaderPreviousSize <= 0 {
        return 1.0 // 全部平仓
    }
    
    // 减仓比例 = 减仓量 / 减仓前持仓
    ratio := reduceSize / leaderPreviousSize
    
    logger.Debugf("📊 减仓比例计算: 减仓量=%.4f, 减仓前持仓=%.4f, 比例=%.2f%%",
        reduceSize, leaderPreviousSize, ratio*100)
    
    return ratio
}
```

#### 4.3.2 比例计算器

```go
// copytrade/engine/ratio.go

package engine

import (
    "nofx/copytrade/tracker"
)

// calculateCopySize 计算跟单仓位大小
// 公式: 你的开仓金额 = 跟单比例 × (交易员开仓金额 ÷ 交易员账户余额) × 你的账户余额
func (e *CopyTradeEngine) calculateCopySize(signal *tracker.TradeSignal) (float64, error) {
    cfg := e.config
    fill := signal.Fill
    
    // 领航员的成交价值
    leaderTradeValue := fill.Value
    
    // 领航员的账户权益
    leaderEquity := signal.LeaderEquity
    if leaderEquity <= 0 {
        leaderEquity = 1 // 防止除零
    }
    
    // 领航员该笔交易占其账户的比例
    leaderTradeRatio := leaderTradeValue / leaderEquity
    
    // 跟随者账户权益
    followerEquity := e.getFollowerEquity()
    if followerEquity <= 0 {
        return 0, fmt.Errorf("follower equity is zero")
    }
    
    // 计算跟单金额
    // copySize = copyRatio × leaderTradeRatio × followerEquity
    copySize := cfg.CopyRatio * leaderTradeRatio * followerEquity
    
    logger.Debugf("📊 Copy ratio calculation: "+
        "leaderTrade=%.2f, leaderEquity=%.2f (ratio=%.4f), "+
        "followerEquity=%.2f, copyRatio=%.2f → copySize=%.2f",
        leaderTradeValue, leaderEquity, leaderTradeRatio,
        followerEquity, cfg.CopyRatio, copySize)
    
    return copySize, nil
}
```

#### 4.3.3 Decision 适配器

```go
// copytrade/engine/adapter.go

package engine

import (
    "fmt"
    "time"
    
    "nofx/copytrade/tracker"
    "nofx/decision"
)

// buildDecision 构造符合现有格式的 Decision
func (e *CopyTradeEngine) buildDecision(signal *tracker.TradeSignal, copySize float64) decision.Decision {
    fill := signal.Fill
    cfg := e.config
    
    // 判断实际操作类型（特别是减仓 vs 平仓）
    actualAction := fill.Action
    if fill.Action == "close" || fill.Action == "reduce" {
        // 通过领航员实时持仓判断是减仓还是平仓
        actualAction = e.determineCloseAction(signal)
    }
    
    // 映射 action 到现有格式
    action := e.mapAction(actualAction, fill.PositionSide)
    
    dec := decision.Decision{
        Symbol:    fill.Symbol,
        Action:    action,
        Reasoning: fmt.Sprintf("Copy trading: following %s leader %s (%s)", 
            cfg.ProviderType, cfg.LeaderID, actualAction),
    }
    
    // 开仓/加仓需要额外参数
    if action == "open_long" || action == "open_short" {
        dec.PositionSizeUSD = copySize
        
        // 杠杆：同步领航员或使用默认
        if cfg.SyncLeverage && signal.LeaderPosition != nil {
            dec.Leverage = signal.LeaderPosition.Leverage
        } else {
            dec.Leverage = 5 // 默认杠杆
        }
        
        // 止损止盈：跟单模式下可选设置
        if fill.PositionSide == "long" {
            dec.StopLoss = fill.Price * 0.97
            dec.TakeProfit = fill.Price * 1.09
        } else {
            dec.StopLoss = fill.Price * 1.03
            dec.TakeProfit = fill.Price * 0.91
        }
        
        dec.Confidence = 90
    }
    
    // 减仓需要指定减仓比例
    if actualAction == "reduce" {
        dec.ReduceRatio = e.calculateReduceRatio(signal)
        dec.Reasoning = fmt.Sprintf("Copy trading: reduce %.0f%% following %s leader %s",
            dec.ReduceRatio*100, cfg.ProviderType, cfg.LeaderID)
    }
    
    // 平仓
    if actualAction == "close" {
        dec.Reasoning = fmt.Sprintf("Copy trading: close position following %s leader %s",
            cfg.ProviderType, cfg.LeaderID)
    }
    
    return dec
}

// mapAction 映射操作类型到现有 decision 格式
func (e *CopyTradeEngine) mapAction(action, posSide string) string {
    switch {
    case action == "open" && posSide == "long":
        return "open_long"
    case action == "open" && posSide == "short":
        return "open_short"
    case action == "close" && posSide == "long":
        return "close_long"
    case action == "close" && posSide == "short":
        return "close_short"
    case action == "add" && posSide == "long":
        return "open_long" // 加仓视为增加开仓
    case action == "add" && posSide == "short":
        return "open_short"
    case action == "reduce" && posSide == "long":
        return "reduce_long" // 减仓（部分平仓）
    case action == "reduce" && posSide == "short":
        return "reduce_short"
    default:
        return "hold"
    }
}

// buildCoTTrace 构建 Chain of Thought 日志 (兼容 AI 格式)
func (e *CopyTradeEngine) buildCoTTrace(signal *tracker.TradeSignal, copySize float64, warnings []Warning) string {
    fill := signal.Fill
    cfg := e.config
    
    // 构建预警部分
    warningSection := ""
    if len(warnings) > 0 {
        warningSection = "\n## ⚠️ Warnings (Not Blocking)\n"
        for _, w := range warnings {
            warningSection += fmt.Sprintf("- [%s] %s\n", w.Type, w.Message)
        }
        warningSection += "\n**Note: All warnings are for logging only. Trade will be executed.**\n"
    }
    
    return fmt.Sprintf(`# Copy Trading Decision Analysis

## Signal Source
- Provider: %s
- Leader: %s
- Signal Time: %s

## Leader Trade Details
- Symbol: %s
- Action: %s %s
- Price: %.4f
- Size: %.4f
- Value: %.2f USDT

## Leader Account State
- Total Equity: %.2f USDT
- Trade/Equity Ratio: %.4f%%

## Copy Calculation
- Copy Ratio Setting: %.0f%%
- Follower Equity: %.2f USDT
- Calculated Copy Size: %.2f USDT
%s
## Decision
Following the leader's %s %s action on %s.
Trade will be executed unconditionally (warnings are for logging only).
`,
        cfg.ProviderType,
        cfg.LeaderID,
        fill.Timestamp.Format("2006-01-02 15:04:05"),
        fill.Symbol,
        fill.Action,
        fill.PositionSide,
        fill.Price,
        fill.Size,
        fill.Value,
        signal.LeaderEquity,
        (fill.Value/signal.LeaderEquity)*100,
        cfg.CopyRatio*100,
        e.getFollowerEquity(),
        copySize,
        warningSection,
        fill.Action,
        fill.PositionSide,
        fill.Symbol,
    )
}

// buildSystemPromptLog 构建 SystemPrompt 日志
func (e *CopyTradeEngine) buildSystemPromptLog() string {
    return fmt.Sprintf(`# Copy Trading Mode

You are operating in Copy Trading mode, following a human leader's trades.

## Provider: %s
## Leader ID: %s
## Copy Ratio: %.0f%%

## Core Rules:
- **Only follow new positions**: Only copy trades for positions we've opened (not leader's historical positions)
- **Unconditional execution**: All leader's actions for tracked positions will be executed
- **Warning mode**: Risk limits only generate warnings, never block trades

## Position Tracking:
- New open: If we have no local position → follow as new position
- Add/Reduce/Close: If we have local position → follow (it's our tracked position)
- Add/Reduce/Close: If we have no local position → skip (leader's historical position)

## Sync Settings:
- Sync Leverage: %v
- Sync Margin Mode: %v
`,
        e.config.ProviderType,
        e.config.LeaderID,
        e.config.CopyRatio*100,
        e.config.SyncLeverage,
        e.config.SyncMarginMode,
    )
}

// buildUserPromptLog 构建 UserPrompt 日志
func (e *CopyTradeEngine) buildUserPromptLog(signal *tracker.TradeSignal) string {
    fill := signal.Fill
    
    return fmt.Sprintf(`## New Trade Signal Received

Time: %s
Symbol: %s
Action: %s %s
Price: %.4f
Size: %.4f (%.2f USDT)

Leader Position:
%s

Follower Equity: %.2f USDT
`,
        fill.Timestamp.Format("2006-01-02 15:04:05"),
        fill.Symbol,
        fill.Action,
        fill.PositionSide,
        fill.Price,
        fill.Size,
        fill.Value,
        formatPosition(signal.LeaderPosition),
        e.getFollowerEquity(),
    )
}
```

---

## 5. 数据模型

### 5.1 数据库表设计

> ⚠️ **设计原则**：跟单配置直接扩展现有 `traders` 表，不单独建表。

```sql
-- 方案 A：扩展现有 traders 表（推荐，更简单）
ALTER TABLE traders ADD COLUMN decision_mode TEXT DEFAULT 'ai';  -- "ai" | "copy_trade"
ALTER TABLE traders ADD COLUMN copy_config TEXT;  -- JSON 格式存储跟单配置

-- copy_config JSON 结构：
-- {
--   "provider_type": "hyperliquid",
--   "leader_id": "0x...",
--   "copy_ratio": 1.0,
--   "sync_leverage": true,
--   "sync_margin_mode": true
-- }
```

```sql
-- 方案 B：如果需要单独表（可选，便于扩展）
CREATE TABLE IF NOT EXISTS copy_trade_configs (
    trader_id TEXT PRIMARY KEY,        -- 直接用 trader_id 作为主键（1:1 关系）
    provider_type TEXT NOT NULL,       -- "hyperliquid" | "okx"
    leader_id TEXT NOT NULL,           -- 领航员地址/uniqueName
    copy_ratio REAL DEFAULT 1.0,       -- 跟单系数 (1.0 = 100%)
    
    -- 同步选项
    sync_leverage BOOLEAN DEFAULT 1,
    sync_margin_mode BOOLEAN DEFAULT 1,
    
    -- 状态由 traders 表的 decision_mode 控制，这里不需要 enabled 字段
    
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    
    FOREIGN KEY (trader_id) REFERENCES traders(id) ON DELETE CASCADE
);
-- 注意：没有单独的 id 字段，trader_id 就是主键，强调 1:1 关系
```

-- 跟单信号日志表
CREATE TABLE IF NOT EXISTS copy_trade_signals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    trader_id TEXT NOT NULL,
    leader_id TEXT NOT NULL,
    provider_type TEXT NOT NULL,
    signal_id TEXT NOT NULL,           -- 原始信号 ID
    symbol TEXT NOT NULL,
    action TEXT NOT NULL,
    position_side TEXT NOT NULL,       -- "long" | "short"
    leader_price REAL,
    leader_size REAL,
    leader_value REAL,
    copy_size REAL,                    -- 计算后的跟单大小
    
    -- 跟单判断
    followed BOOLEAN DEFAULT 0,        -- 是否跟随
    follow_reason TEXT,                -- 跟随/忽略原因
    
    -- 预警信息
    warnings_json TEXT,                -- 预警列表 JSON
    
    -- 执行状态
    decision_json TEXT,                -- 生成的决策 JSON
    status TEXT DEFAULT 'pending',     -- pending | executed | failed | skipped
    error_message TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    
    UNIQUE(trader_id, signal_id)
);

-- 预警日志表
CREATE TABLE IF NOT EXISTS copy_trade_warnings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    trader_id TEXT NOT NULL,
    leader_id TEXT NOT NULL,
    symbol TEXT NOT NULL,
    warning_type TEXT NOT NULL,        -- low_value | high_value | insufficient_balance | etc.
    message TEXT NOT NULL,
    signal_action TEXT,
    signal_value REAL,
    copy_value REAL,
    executed BOOLEAN DEFAULT 1,        -- 预警不阻止执行，始终为 true
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_warnings_trader ON copy_trade_warnings(trader_id);
CREATE INDEX idx_warnings_time ON copy_trade_warnings(created_at);
```

### 5.2 Go 结构定义

```go
// store/copytrade.go

package store

import (
    "database/sql"
    "time"
)

type CopyTradeConfig struct {
    ID               string    `json:"id"`
    TraderID         string    `json:"trader_id"`
    ProviderType     string    `json:"provider_type"`     // "hyperliquid" | "okx"
    LeaderID         string    `json:"leader_id"`
    CopyRatio        float64   `json:"copy_ratio"`        // 1.0 = 100%
    
    // 预警阈值（不限制，只预警）
    MinTradeWarn     float64   `json:"min_trade_warn"`    // 低于此金额记录预警
    MaxTradeWarn     float64   `json:"max_trade_warn"`    // 高于此金额记录预警
    
    // 同步选项
    SyncLeverage     bool      `json:"sync_leverage"`
    SyncMarginMode   bool      `json:"sync_margin_mode"`
    UseFallbackPrice bool      `json:"use_fallback_price"`
    
    Enabled          bool      `json:"enabled"`
    CreatedAt        time.Time `json:"created_at"`
    UpdatedAt        time.Time `json:"updated_at"`
    
    // ⚠️ 无条件跟随：没有 FollowOpen/Add/Reduce/Close 字段
    // 所有跟随的仓位的操作都会执行
}

// CopyTradeSignal 跟单信号记录
type CopyTradeSignal struct {
    ID            int64     `json:"id"`
    TraderID      string    `json:"trader_id"`
    LeaderID      string    `json:"leader_id"`
    ProviderType  string    `json:"provider_type"`
    SignalID      string    `json:"signal_id"`
    Symbol        string    `json:"symbol"`
    Action        string    `json:"action"`
    PositionSide  string    `json:"position_side"`
    LeaderPrice   float64   `json:"leader_price"`
    LeaderSize    float64   `json:"leader_size"`
    LeaderValue   float64   `json:"leader_value"`
    CopySize      float64   `json:"copy_size"`
    
    // 跟单判断
    Followed      bool      `json:"followed"`
    FollowReason  string    `json:"follow_reason"`
    
    // 预警
    Warnings      []Warning `json:"warnings,omitempty"`
    
    // 执行状态
    Decision      *Decision `json:"decision,omitempty"`
    Status        string    `json:"status"`
    ErrorMessage  string    `json:"error_message,omitempty"`
    CreatedAt     time.Time `json:"created_at"`
}

type CopyTradeStore struct {
    db *sql.DB
}

func (s *CopyTradeStore) Create(cfg *CopyTradeConfig) error { ... }
func (s *CopyTradeStore) Update(cfg *CopyTradeConfig) error { ... }
func (s *CopyTradeStore) Delete(id string) error { ... }
func (s *CopyTradeStore) GetByTraderID(traderID string) (*CopyTradeConfig, error) { ... }
func (s *CopyTradeStore) List(userID string) ([]*CopyTradeConfig, error) { ... }

// 信号日志
func (s *CopyTradeStore) SaveSignal(signal *CopyTradeSignal) error { ... }
func (s *CopyTradeStore) GetSignals(traderID string, limit int) ([]*CopyTradeSignal, error) { ... }

// 预警日志
func (s *CopyTradeStore) SaveWarning(warning *Warning) error { ... }
func (s *CopyTradeStore) GetWarnings(traderID string, limit int) ([]*Warning, error) { ... }
```

---

## 6. API 设计

### 6.1 跟单配置 API

```
# 获取跟单配置
GET /api/traders/:id/copy-config
Response: CopyTradeConfig

# 创建/更新跟单配置
PUT /api/traders/:id/copy-config
Body: {
    "provider_type": "hyperliquid",
    "leader_id": "0x123...",
    "copy_ratio": 1.0,              // 跟单系数 (1.0 = 100%)
    "min_trade_warn": 10,           // 预警阈值：低于此金额记录预警
    "max_trade_warn": 0,            // 预警阈值：高于此金额记录预警 (0=不预警)
    "sync_leverage": true,          // 同步杠杆
    "sync_margin_mode": true,       // 同步保证金模式
    "use_fallback_price": true,     // 缺价使用行情兜底
    "enabled": true
}
// ⚠️ 注意：没有 follow_open/add/reduce/close 选项
// 系统会无条件跟随所有操作

# 删除跟单配置 (恢复为 AI 模式)
DELETE /api/traders/:id/copy-config

# 获取领航员信息 (预览)
GET /api/copy-trade/leader-info?provider=hyperliquid&leader_id=0x123...
Response: {
    "provider": "hyperliquid",
    "leader_id": "0x123...",
    "total_equity": 50000.00,
    "positions": [...],
    "recent_trades": [...]
}

# 获取跟单信号日志
GET /api/traders/:id/copy-signals?limit=50
Response: {
    "signals": [
        {
            "signal_id": "536939280839171",
            "symbol": "BTCUSDT",
            "action": "open",
            "position_side": "long",
            "followed": true,
            "follow_reason": "新开仓信号，本地无持仓",
            "warnings": [],
            "status": "executed"
        }
    ]
}

# 获取预警日志
GET /api/traders/:id/copy-warnings?limit=50
Response: {
    "warnings": [
        {
            "type": "high_value",
            "symbol": "BTCUSDT",
            "message": "跟单金额较大 (5000 USDT)，仍执行",
            "executed": true,
            "created_at": "2025-12-16T14:30:00Z"
        }
    ]
}
```

### 6.2 API Handler

```go
// api/copytrade_handler.go

package api

import (
    "net/http"
    
    "github.com/gin-gonic/gin"
    "nofx/copytrade/provider"
    "nofx/store"
)

type CopyTradeHandler struct {
    store *store.Store
}

// HandleGetCopyConfig 获取跟单配置
func (h *CopyTradeHandler) HandleGetCopyConfig(c *gin.Context) {
    traderID := c.Param("id")
    userID := c.GetString("user_id")
    
    // 验证 trader 归属
    trader, err := h.store.Trader().GetFullConfig(userID, traderID)
    if err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "Trader not found"})
        return
    }
    
    config, err := h.store.CopyTrade().GetByTraderID(traderID)
    if err != nil {
        c.JSON(http.StatusOK, gin.H{"enabled": false})
        return
    }
    
    c.JSON(http.StatusOK, config)
}

// HandleUpdateCopyConfig 创建/更新跟单配置
func (h *CopyTradeHandler) HandleUpdateCopyConfig(c *gin.Context) {
    traderID := c.Param("id")
    userID := c.GetString("user_id")
    
    var req store.CopyTradeConfig
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    
    // 验证 trader 归属
    if _, err := h.store.Trader().GetFullConfig(userID, traderID); err != nil {
        c.JSON(http.StatusNotFound, gin.H{"error": "Trader not found"})
        return
    }
    
    // 验证 provider_type
    if req.ProviderType != "hyperliquid" && req.ProviderType != "okx" {
        c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid provider type"})
        return
    }
    
    req.TraderID = traderID
    
    // 创建或更新
    existing, _ := h.store.CopyTrade().GetByTraderID(traderID)
    if existing != nil {
        req.ID = existing.ID
        if err := h.store.CopyTrade().Update(&req); err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
    } else {
        req.ID = generateID()
        if err := h.store.CopyTrade().Create(&req); err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
    }
    
    // 重新加载 trader (触发引擎切换)
    h.reloadTrader(traderID)
    
    c.JSON(http.StatusOK, gin.H{"message": "Copy trade config updated"})
}

// HandleGetLeaderInfo 获取领航员信息
func (h *CopyTradeHandler) HandleGetLeaderInfo(c *gin.Context) {
    providerType := c.Query("provider")
    leaderID := c.Query("leader_id")
    
    var p provider.LeaderProvider
    switch providerType {
    case "hyperliquid":
        p = provider.NewHyperliquidProvider()
    case "okx":
        p = provider.NewOKXProvider()
    default:
        c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid provider"})
        return
    }
    
    state, err := p.GetAccountState(leaderID)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to fetch leader info: " + err.Error()})
        return
    }
    
    c.JSON(http.StatusOK, gin.H{
        "provider":      providerType,
        "leader_id":     leaderID,
        "total_equity":  state.TotalEquity,
        "positions":     state.Positions,
    })
}
```

---

## 7. 跟单比例算法

### 7.1 核心公式

```
你的开仓金额 = 跟单比例 × (交易员开仓金额 ÷ 交易员账户余额) × 你的账户余额
```

### 7.2 计算示例

| 参数 | 值 |
|------|-----|
| 领航员账户余额 | 100,000 USDT |
| 领航员开仓金额 | 10,000 USDT |
| 领航员仓位比例 | 10% |
| 你的账户余额 | 1,000 USDT |
| 跟单系数 | 100% (1.0) |
| **你的开仓金额** | **1.0 × 10% × 1,000 = 100 USDT** |

### 7.3 跟单系数说明

| 系数 | 含义 | 示例 (领航员 10%，你余额 1000) |
|------|------|------|
| 50% | 减半跟随 | 50 USDT |
| 100% | 完全复制 | 100 USDT |
| 200% | 双倍跟随 | 200 USDT |

### 7.4 边界处理（预警模式）

```go
// 注意：新设计中不限制交易，只做预警日志
func (e *CopyTradeEngine) calculateCopySize(signal *TradeSignal) (float64, []Warning) {
    cfg := e.config
    var warnings []Warning
    
    // 基础计算
    leaderRatio := signal.Fill.Value / signal.LeaderEquity
    followerEquity := e.getFollowerEquity()
    copySize := cfg.CopyRatio * leaderRatio * followerEquity
    
    // 最小金额预警（不阻止，只记录）
    if copySize < cfg.MinTradeSize {
        warnings = append(warnings, Warning{
            Type:    "low_value",
            Message: fmt.Sprintf("跟单金额较小 (%.2f USDT)，仍执行", copySize),
        })
        // 不返回 0，继续执行！
    }
    
    // 最大金额预警（不截断，只记录）
    if cfg.MaxTradeSize > 0 && copySize > cfg.MaxTradeSize {
        warnings = append(warnings, Warning{
            Type:    "high_value",
            Message: fmt.Sprintf("跟单金额较大 (%.2f USDT > %.2f 上限)，仍按原金额执行", copySize, cfg.MaxTradeSize),
        })
        // 不截断，按原金额执行！
    }
    
    // 可用余额不足预警（尽量执行）
    availableBalance := e.getFollowerAvailableBalance()
    if copySize > availableBalance {
        warnings = append(warnings, Warning{
            Type:    "insufficient_balance",
            Message: fmt.Sprintf("余额不足 (需 %.2f, 可用 %.2f)，以最大可用执行", copySize, availableBalance),
        })
        copySize = availableBalance // 这是唯一的调整：用尽可用余额
    }
    
    return copySize, warnings
}
```

---

## 8. 前端集成

### 8.1 类型定义

```typescript
// web/src/types.ts (新增)

export interface CopyTradeConfig {
  id: string;
  trader_id: string;
  provider_type: 'hyperliquid' | 'okx';
  leader_id: string;
  copy_ratio: number;        // 1.0 = 100%
  
  // 预警阈值（不限制交易）
  min_trade_warn: number;    // 低于此金额记录预警
  max_trade_warn: number;    // 高于此金额记录预警 (0=不预警)
  
  // 同步选项
  sync_leverage: boolean;
  sync_margin_mode: boolean;
  use_fallback_price: boolean;
  
  enabled: boolean;
  
  // ⚠️ 无条件跟随：没有 follow_open/add/reduce/close 选项
}

export interface LeaderInfo {
  provider: string;
  leader_id: string;
  total_equity: number;
  positions: Position[];
  recent_trades: LeaderTrade[];
}

export interface LeaderTrade {
  id: string;
  symbol: string;
  action: 'open' | 'close' | 'add' | 'reduce';
  position_side: 'long' | 'short';
  price: number;
  size: number;
  value: number;
  timestamp: string;
}

export interface CopyTradeSignal {
  signal_id: string;
  symbol: string;
  action: string;
  position_side: string;
  leader_price: number;
  leader_value: number;
  copy_size: number;
  followed: boolean;
  follow_reason: string;
  warnings: CopyTradeWarning[];
  status: 'pending' | 'executed' | 'failed' | 'skipped';
  created_at: string;
}

export interface CopyTradeWarning {
  type: 'low_value' | 'high_value' | 'insufficient_balance' | 'high_frequency' | 'high_leverage' | 'large_position_ratio';
  symbol: string;
  message: string;
  executed: boolean;
  created_at: string;
}
```

### 8.2 配置组件

参考你提供的 UI 设计图，主要包含：

1. **Provider 类型选择** - 下拉框：Hyperliquid 钱包 / OKX
2. **钱包/领航员 ID** - 输入框：地址或 uniqueName
3. **跟单系数** - 数字输入：百分比 (50%, 100%, 200% 等)
4. **预警阈值** - 数字输入（可选）：
   - 小额预警阈值：低于此金额记录预警日志
   - 大额预警阈值：高于此金额记录预警日志
5. **同步选项** - 复选框组：
   - 同步杠杆
   - 同步保证金模式
   - 缺价使用行情兜底

> ⚠️ **注意**：没有"跟随开仓/加仓/减仓/平仓"选项。
> 系统会**无条件跟随**领航员的所有操作（只要是我们跟随的仓位）。

```tsx
// 配置表单示例
const CopyTradeConfigForm = () => {
  return (
    <form>
      {/* Provider 选择 */}
      <Select name="provider_type" label="数据源">
        <option value="hyperliquid">Hyperliquid 钱包</option>
        <option value="okx">OKX 交易员</option>
      </Select>
      
      {/* 领航员 ID */}
      <Input 
        name="leader_id" 
        label="领航员地址/ID"
        placeholder="0x... 或 uniqueName"
      />
      
      {/* 跟单系数 */}
      <Input 
        name="copy_ratio" 
        type="number"
        label="跟单系数 (%)"
        placeholder="100"
        suffix="%"
      />
      
      {/* 预警阈值（可选） */}
      <Input 
        name="min_trade_warn" 
        type="number"
        label="小额预警阈值 (USDT)"
        placeholder="10"
        helperText="低于此金额会记录预警日志（不阻止交易）"
      />
      
      <Input 
        name="max_trade_warn" 
        type="number"
        label="大额预警阈值 (USDT)"
        placeholder="0 = 不预警"
        helperText="高于此金额会记录预警日志（不阻止交易）"
      />
      
      {/* 同步选项 */}
      <Checkbox name="sync_leverage" label="同步杠杆" defaultChecked />
      <Checkbox name="sync_margin_mode" label="同步保证金模式" defaultChecked />
      <Checkbox name="use_fallback_price" label="缺价使用行情兜底" defaultChecked />
      
      {/* 重要提示 */}
      <Alert type="info">
        💡 系统会无条件跟随领航员的所有交易操作。
        预警阈值仅用于记录日志，不会阻止任何交易。
      </Alert>
    </form>
  );
};
```

### 8.3 决策日志复用

跟单产生的决策完全兼容现有 AI 决策格式：

```typescript
// 决策日志展示无需修改
// DecisionCard 组件可直接复用

interface DecisionRecord {
  timestamp: string;
  cycle_number: number;
  input_prompt: string;        // 跟单模式下为信号描述
  cot_trace: string;           // 跟单模式下为计算过程
  decision_json: string;       // 标准 Decision JSON
  // ... 其他字段完全兼容
}
```

---

## 9. 风险预警机制

> ⚠️ **重要设计原则**：跟单系统不限制任何交易动作，只做预警日志。领航员的所有操作都会被执行。

### 9.1 预警日志系统

```go
// copytrade/warning/warning.go

package warning

import (
    "time"
    "nofx/logger"
)

// WarningType 预警类型
type WarningType string

const (
    WarnLowValue           WarningType = "low_value"            // 金额较小
    WarnHighValue          WarningType = "high_value"           // 金额较大
    WarnInsufficientBalance WarningType = "insufficient_balance" // 余额不足
    WarnHighFrequency      WarningType = "high_frequency"       // 高频操作
    WarnHighLeverage       WarningType = "high_leverage"        // 高杠杆
    WarnLargePositionRatio WarningType = "large_position_ratio" // 仓位占比大
)

// Warning 预警记录
type Warning struct {
    Timestamp   time.Time   `json:"timestamp"`
    TraderID    string      `json:"trader_id"`
    LeaderID    string      `json:"leader_id"`
    Symbol      string      `json:"symbol"`
    Type        WarningType `json:"type"`
    Message     string      `json:"message"`
    
    // 信号详情
    SignalAction string  `json:"signal_action"`
    SignalValue  float64 `json:"signal_value"`
    CopyValue    float64 `json:"copy_value"`
    
    // 执行状态（预警不阻止执行）
    Executed    bool      `json:"executed"`
    ExecutedAt  time.Time `json:"executed_at,omitempty"`
}

// WarningLogger 预警日志记录器
type WarningLogger struct {
    traderID string
    leaderID string
    warnings []Warning
}

func NewWarningLogger(traderID, leaderID string) *WarningLogger {
    return &WarningLogger{
        traderID: traderID,
        leaderID: leaderID,
        warnings: make([]Warning, 0),
    }
}

// Log 记录预警（不阻止交易）
func (wl *WarningLogger) Log(w Warning) {
    w.Timestamp = time.Now()
    w.TraderID = wl.traderID
    w.LeaderID = wl.leaderID
    w.Executed = true // 预警不阻止，总是执行
    w.ExecutedAt = time.Now()
    
    wl.warnings = append(wl.warnings, w)
    
    // 输出到日志
    logger.Warnf("⚠️ [跟单预警] %s | %s | %s", w.Type, w.Symbol, w.Message)
}

// GetRecentWarnings 获取最近的预警记录
func (wl *WarningLogger) GetRecentWarnings(limit int) []Warning {
    if len(wl.warnings) <= limit {
        return wl.warnings
    }
    return wl.warnings[len(wl.warnings)-limit:]
}
```

### 9.2 预警触发条件

| 预警类型 | 触发条件 | 处理方式 |
|---------|---------|---------|
| `low_value` | 跟单金额 < 10 USDT | 记录预警，**仍执行** |
| `high_value` | 跟单金额 > 设定上限 | 记录预警，**仍执行（不截断）** |
| `insufficient_balance` | 可用余额不足 | 记录预警，**用最大可用余额执行** |
| `high_frequency` | 同币种 30 秒内重复操作 | 记录预警，**仍执行** |
| `high_leverage` | 杠杆 > 20x | 记录预警，**仍执行** |
| `large_position_ratio` | 单仓位 > 账户 50% | 记录预警，**仍执行** |

### 9.3 熔断机制（仅用于系统异常）

> 熔断机制只在**系统层面异常**时触发（如 API 连续失败），**不用于限制交易动作**。

```go
// copytrade/engine/circuit_breaker.go

type CircuitBreaker struct {
    failureCount     int
    lastFailure      time.Time
    state            string // "closed" | "open" | "half-open"
    
    threshold        int           // 失败阈值（默认 10）
    resetTimeout     time.Duration // 重置超时（默认 60s）
}

// 仅在以下情况触发熔断：
// 1. API 调用连续失败 10 次
// 2. 网络连接中断
// 3. 交易所返回系统错误
//
// 不会因为以下原因触发熔断：
// 1. 余额不足
// 2. 跟单金额过大/过小
// 3. 高频交易
// 4. 任何领航员的交易动作
```

### 9.4 异常处理（不阻止交易）

```go
func (e *CopyTradeEngine) processSignal(signal *TradeSignal) (*decision.FullDecision, error) {
    // 系统熔断检查（仅检查 API 级别异常）
    if !e.circuitBreaker.Allow() {
        // 熔断时等待恢复，但不丢弃信号
        logger.Warnf("⚠️ 系统熔断中，等待恢复后重试...")
        time.Sleep(5 * time.Second)
    }
    
    defer func() {
        if r := recover(); r != nil {
            logger.Errorf("🔥 引擎异常: %v", r)
            e.circuitBreaker.RecordFailure()
            // 注意：即使 panic 也不丢弃信号，会在下次轮询时重试
        }
    }()
    
    // 判断是否应该跟随（只跟新开仓规则）
    follow, reason := e.shouldFollowSignal(signal)
    if !follow {
        logger.Infof("📋 跳过信号: %s", reason)
        return nil, nil // 这是正常逻辑跳过，不是错误
    }
    
    // 计算跟单金额（带预警）
    copySize, warnings := e.calculateCopySize(signal)
    
    // 记录所有预警
    for _, w := range warnings {
        e.warningLogger.Log(w)
    }
    
    // 无论有多少预警，都继续执行
    dec := e.buildDecision(signal, copySize)
    
    e.circuitBreaker.RecordSuccess()
    return dec, nil
}
```

### 9.5 预警日志展示

预警日志会在前端决策日志中展示，格式如下：

```json
{
  "timestamp": "2025-12-16T14:30:00Z",
  "trader_id": "trader_001",
  "cycle_number": 42,
  "decision_source": "copy_trade",
  "leader_id": "0x856c35...",
  "warnings": [
    {
      "type": "high_value",
      "message": "跟单金额较大 (5000 USDT > 1000 上限)，仍按原金额执行"
    },
    {
      "type": "high_leverage", 
      "message": "同步杠杆 25x 较高，仍执行"
    }
  ],
  "executed": true,
  "decision": {
    "symbol": "BTCUSDT",
    "action": "open_long",
    "position_size_usd": 5000,
    "leverage": 25
  }
}
```

---

## 10. 日志规范

### 10.1 日志分类

| 日志类别 | 标识前缀 | 用途 |
|---------|---------|------|
| **连接日志** | `🔗` | WebSocket/API 连接状态 |
| **监控日志** | `👁️` | 领航员状态监控 |
| **信号日志** | `📡` | 接收到的交易信号 |
| **判断日志** | `🎯` | 跟单判断（跟/不跟） |
| **计算日志** | `📊` | 比例计算过程 |
| **执行日志** | `⚡` | 交易执行状态 |
| **预警日志** | `⚠️` | 风控预警（不阻止） |
| **错误日志** | `❌` | 错误和异常 |
| **系统日志** | `🔧` | 系统状态变更 |

### 10.2 关键日志点

#### 10.2.1 启动阶段

```go
// 跟单服务启动
logger.Infof("🔧 [%s] 跟单服务启动 (provider=%s, leader=%s)", 
    traderID, providerType, leaderID)

// WebSocket 连接
logger.Infof("🔗 [%s] WS 连接成功 local=%s remote=%s", 
    traderID, localAddr, remoteAddr)

// 订阅成功
logger.Infof("🔗 [%s] WS 订阅成功 channel=%s user=%s", 
    traderID, channel, leaderID)

// 初始状态同步
logger.Infof("👁️ [%s] 领航员状态同步完成 equity=%.2f positions=%d", 
    traderID, leaderEquity, len(positions))
```

#### 10.2.2 信号接收

```go
// 收到新信号
logger.Infof("📡 [%s] 收到信号 | %s %s %s | 价格=%.4f 数量=%.4f 价值=%.2f", 
    traderID, symbol, action, posSide, price, size, value)

// 信号去重跳过
logger.Debugf("📡 [%s] 信号已处理，跳过 | signal_id=%s", 
    traderID, signalID)
```

#### 10.2.3 跟单判断

```go
// 决定跟随
logger.Infof("🎯 [%s] ✅ 跟随 | %s | 原因: %s", 
    traderID, symbol, reason)

// 决定不跟
logger.Infof("🎯 [%s] ❌ 跳过 | %s | 原因: %s", 
    traderID, symbol, reason)

// 动作类型判断
logger.Infof("🎯 [%s] 动作判断 | %s | 领航员仓位=%.4f → %s", 
    traderID, symbol, leaderPositionSize, actionType) // "open/add/reduce/close"
```

#### 10.2.4 比例计算

```go
// 跟单比例计算
logger.Infof("📊 [%s] 比例计算 | %s | "+
    "领航员: 交易=%.2f 权益=%.2f 占比=%.2f%% | "+
    "跟随者: 权益=%.2f 系数=%.0f%% → 跟单=%.2f", 
    traderID, symbol,
    leaderTradeValue, leaderEquity, leaderRatio*100,
    followerEquity, copyRatio*100, copySize)

// 减仓比例计算
logger.Infof("📊 [%s] 减仓计算 | %s | "+
    "减仓量=%.4f 减仓前=%.4f → 减仓比例=%.2f%%", 
    traderID, symbol, reduceSize, previousSize, reduceRatio*100)
```

#### 10.2.5 预警日志

```go
// 小额预警
logger.Warnf("⚠️ [%s] 预警:小额 | %s | 金额=%.2f (阈值=%.2f) | 仍执行", 
    traderID, symbol, copySize, minWarn)

// 大额预警
logger.Warnf("⚠️ [%s] 预警:大额 | %s | 金额=%.2f (阈值=%.2f) | 仍执行", 
    traderID, symbol, copySize, maxWarn)

// 余额不足预警
logger.Warnf("⚠️ [%s] 预警:余额不足 | %s | 需要=%.2f 可用=%.2f | 以最大可用执行", 
    traderID, symbol, copySize, availableBalance)

// 高杠杆预警
logger.Warnf("⚠️ [%s] 预警:高杠杆 | %s | 杠杆=%dx | 仍执行", 
    traderID, symbol, leverage)
```

#### 10.2.6 执行日志

```go
// 开始执行
logger.Infof("⚡ [%s] 执行开始 | %s %s | 金额=%.2f 杠杆=%dx", 
    traderID, action, symbol, positionSize, leverage)

// 执行成功
logger.Infof("⚡ [%s] 执行成功 | %s %s | 成交价=%.4f 数量=%.4f", 
    traderID, action, symbol, fillPrice, fillQty)

// 执行失败
logger.Errorf("❌ [%s] 执行失败 | %s %s | 错误: %s", 
    traderID, action, symbol, err.Error())
```

#### 10.2.7 错误日志

```go
// API 错误
logger.Errorf("❌ [%s] API错误 | %s | code=%s msg=%s", 
    traderID, apiName, code, message)

// WebSocket 断开
logger.Errorf("❌ [%s] WS断开 | 错误: %s | 将在 %ds 后重连", 
    traderID, err.Error(), reconnectDelay)

// 数据解析错误
logger.Errorf("❌ [%s] 数据解析失败 | 类型=%s | 原始数据: %s", 
    traderID, dataType, rawData)
```

### 10.3 日志格式规范

```
[级别] [时间] [文件:行号] [emoji] [trader_id] 消息 | 字段1=值1 字段2=值2
```

**示例：**
```
INFO  [2025-12-16 09:25:04] copytrade/engine.go:156 📡 [测试员] 收到信号 | BTCUSDT open long | 价格=95000.00 数量=0.1 价值=9500.00
INFO  [2025-12-16 09:25:04] copytrade/engine.go:189 🎯 [测试员] ✅ 跟随 | BTCUSDT | 原因: 新开仓信号，本地无持仓
INFO  [2025-12-16 09:25:04] copytrade/engine.go:234 📊 [测试员] 比例计算 | BTCUSDT | 领航员: 交易=9500.00 权益=100000.00 占比=9.50% | 跟随者: 权益=1000.00 系数=100% → 跟单=95.00
INFO  [2025-12-16 09:25:04] copytrade/engine.go:278 ⚡ [测试员] 执行开始 | open_long BTCUSDT | 金额=95.00 杠杆=10x
INFO  [2025-12-16 09:25:05] copytrade/engine.go:312 ⚡ [测试员] 执行成功 | open_long BTCUSDT | 成交价=95012.50 数量=0.001
```

### 10.4 日志级别使用

| 级别 | 使用场景 |
|------|---------|
| **DEBUG** | 去重跳过、详细计算过程、调试信息 |
| **INFO** | 正常流程：启动、连接、信号、判断、执行 |
| **WARN** | 预警（不阻止交易）、重连、非致命异常 |
| **ERROR** | 执行失败、API错误、解析错误 |

### 10.5 结构化日志字段

```go
// 建议使用结构化日志便于检索和分析
type CopyTradeLog struct {
    Timestamp   time.Time `json:"ts"`
    TraderID    string    `json:"trader_id"`
    LeaderID    string    `json:"leader_id"`
    LogType     string    `json:"log_type"`     // signal|judge|calc|exec|warn|error
    Symbol      string    `json:"symbol,omitempty"`
    Action      string    `json:"action,omitempty"`
    
    // 信号相关
    SignalID    string    `json:"signal_id,omitempty"`
    SignalPrice float64   `json:"signal_price,omitempty"`
    SignalValue float64   `json:"signal_value,omitempty"`
    
    // 判断相关
    Followed    *bool     `json:"followed,omitempty"`
    Reason      string    `json:"reason,omitempty"`
    
    // 计算相关
    LeaderEquity   float64 `json:"leader_equity,omitempty"`
    FollowerEquity float64 `json:"follower_equity,omitempty"`
    CopyRatio      float64 `json:"copy_ratio,omitempty"`
    CopySize       float64 `json:"copy_size,omitempty"`
    
    // 执行相关
    FillPrice   float64 `json:"fill_price,omitempty"`
    FillQty     float64 `json:"fill_qty,omitempty"`
    
    // 错误相关
    Error       string  `json:"error,omitempty"`
}

// 日志输出示例
func (e *CopyTradeEngine) logSignal(signal *TradeSignal) {
    log := CopyTradeLog{
        Timestamp:   time.Now(),
        TraderID:    e.traderID,
        LeaderID:    e.config.LeaderID,
        LogType:     "signal",
        Symbol:      signal.Fill.Symbol,
        Action:      signal.Fill.Action,
        SignalID:    signal.Fill.ID,
        SignalPrice: signal.Fill.Price,
        SignalValue: signal.Fill.Value,
    }
    
    // 输出为 JSON（便于 ELK/Loki 等日志系统检索）
    jsonBytes, _ := json.Marshal(log)
    logger.Infof("📡 [%s] %s", e.traderID, string(jsonBytes))
}
```

### 10.6 问题排查指南

| 问题现象 | 检查日志 | 关键字段 |
|---------|---------|---------|
| **没收到信号** | 连接日志、监控日志 | `WS connected`, `subscribed` |
| **信号没跟** | 判断日志 | `跳过`, `原因` |
| **金额计算错误** | 计算日志 | `领航员`, `权益`, `跟单` |
| **执行失败** | 执行日志、错误日志 | `执行失败`, `错误` |
| **仓位不同步** | 判断日志 | `本地仓位`, `领航员仓位` |

### 10.7 接入现有日志系统

> ⚠️ **重要**：直接使用项目现有的 `logger` 包，不重复造轮子。

```go
// 直接使用现有的 logger 包
import "nofx/logger"

// 跟单模块的日志直接输出到现有日志系统
func (e *CopyTradeEngine) logSignal(signal *TradeSignal) {
    logger.Infof("📡 [%s] 收到信号 | %s %s %s | 价格=%.4f",
        e.traderID, signal.Fill.Symbol, signal.Fill.Action, 
        signal.Fill.PositionSide, signal.Fill.Price)
}

// 所有日志统一由现有系统管理：
// - 日志级别控制
// - 日志文件轮转
// - 日志格式化
// - 日志输出目标
```

---

## 11. 实现路线图

### Phase 1: 基础框架 (Week 1)

- [ ] 创建 `copytrade/` 模块目录结构
- [ ] 实现 Provider 接口和 Hyperliquid Provider
- [ ] 实现基础 Tracker (轮询模式)
- [ ] 实现跟单比例计算器
- [ ] 数据库表和 Store 层
- [ ] **实现本地仓位对比逻辑（判断新开仓 vs 历史仓位）**

### Phase 2: 引擎集成 (Week 2)

- [ ] 实现 CopyTradeEngine（包含只跟新开仓规则）
- [ ] 实现 Decision 适配器
- [ ] 实现反向开仓拆分处理（Hyperliquid）
- [ ] 集成到 AutoTrader (Decision Provider 接口)
- [ ] 添加 API Handler
- [ ] 基础前端配置 UI

### Phase 3: OKX 支持 (Week 3)

- [ ] 实现 OKX Provider
- [ ] 实现 OKX Tracker
- [ ] 统一符号格式化
- [ ] 前端 Provider 切换支持

### Phase 4: 预警系统与优化 (Week 4)

- [ ] **实现预警日志系统（不阻止交易）**
- [ ] 系统级熔断机制（仅用于 API 异常）
- [ ] 信号日志记录
- [ ] 领航员预览 API
- [ ] 性能优化 (去重、缓存)
- [ ] 完整测试覆盖

### Phase 5: 文档与发布 (Week 5)

- [ ] 用户文档
- [ ] API 文档
- [ ] 示例配置
- [ ] 版本发布

---

## 12. 附录

### A. Hyperliquid API 数据结构

```go
// userFills 返回结构
type HLFillRaw struct {
    Coin          string `json:"coin"`
    Px            string `json:"px"`
    Sz            string `json:"sz"`
    Side          string `json:"side"`         // "B" | "A"
    Time          int64  `json:"time"`
    StartPosition string `json:"startPosition"`
    Dir           string `json:"dir"`          // "Open Long" | "Close Short" | ...
    ClosedPnl     string `json:"closedPnl"`
    Hash          string `json:"hash"`
    Oid           int64  `json:"oid"`
    TID           int64  `json:"tid"`
    FeeToken      string `json:"feeToken"`
}

// clearinghouseState 返回结构
type HLClearinghouseState struct {
    MarginSummary struct {
        AccountValue    string `json:"accountValue"`
        TotalNtlPos     string `json:"totalNtlPos"`
        TotalRawUsd     string `json:"totalRawUsd"`
        TotalMarginUsed string `json:"totalMarginUsed"`
    } `json:"marginSummary"`
    Withdrawable   string `json:"withdrawable"`
    AssetPositions []struct {
        Type     string `json:"type"`
        Position struct {
            Coin           string `json:"coin"`
            Szi            string `json:"szi"`
            Leverage       struct {
                Type  string `json:"type"`
                Value int    `json:"value"`
            } `json:"leverage"`
            EntryPx        string `json:"entryPx"`
            PositionValue  string `json:"positionValue"`
            UnrealizedPnl  string `json:"unrealizedPnl"`
            LiquidationPx  string `json:"liquidationPx,omitempty"`
            MarginUsed     string `json:"marginUsed"`
        } `json:"position"`
    } `json:"assetPositions"`
    Time int64 `json:"time"`
}
```

### B. OKX API 数据结构

```go
// trade-records 返回结构
type OKXTradeRecord struct {
    AvgPx     string `json:"avgPx"`
    BaseName  string `json:"baseName"`
    CTime     string `json:"cTime"`
    FillTime  string `json:"fillTime"`
    InstId    string `json:"instId"`
    InstType  string `json:"instType"`
    Lever     string `json:"lever"`
    OrdId     string `json:"ordId"`
    OrdType   string `json:"ordType"`
    PosSide   string `json:"posSide"` // "long" | "short"
    Side      string `json:"side"`    // "buy" | "sell"
    Sz        string `json:"sz"`
    Value     string `json:"value"`
}

// position-current 返回结构
type OKXPosition struct {
    AvgPx      string `json:"avgPx"`
    InstId     string `json:"instId"`
    Lever      string `json:"lever"`
    LiqPx      string `json:"liqPx"`
    Margin     string `json:"margin"`
    MarkPx     string `json:"markPx"`
    MgnMode    string `json:"mgnMode"` // "isolated" | "cross"
    NotionalUsd string `json:"notionalUsd"`
    Pos        string `json:"pos"`
    PosSide    string `json:"posSide"`
    Upl        string `json:"upl"`
}
```

### C. 符号格式化

```go
// 统一符号格式: BTCUSDT

func normalizeSymbol(coin string) string {
    coin = strings.ToUpper(coin)
    if !strings.HasSuffix(coin, "USDT") {
        coin = coin + "USDT"
    }
    return coin
}

func normalizeOKXSymbol(instId string) string {
    // OKX: "BTC-USDT-SWAP" -> "BTCUSDT"
    parts := strings.Split(instId, "-")
    if len(parts) >= 2 {
        return parts[0] + parts[1]
    }
    return instId
}
```

---

## 变更记录

| 版本 | 日期 | 变更内容 |
|------|------|----------|
| v1.0 | 2025-12-16 | 初始设计文档 |
| v1.1 | 2025-12-16 | 重大更新：<br>1. 新增「跟单规则与交易动作」章节<br>2. 明确只跟新开仓原则<br>3. 新增本地仓位对比判断逻辑<br>4. 新增 Hyperliquid 反向开仓处理方案<br>5. 风控改为预警模式（不限制交易）<br>6. 移除 follow_open/add/reduce/close 选项<br>7. 新增预警日志系统 |
| v1.2 | 2025-12-16 | 补充减仓 vs 平仓判断逻辑：<br>1. 通过领航员实时持仓判断减仓/平仓<br>2. 领航员仓位=0 → 平仓<br>3. 领航员仓位>0 → 减仓<br>4. 新增 calculateReduceRatio 计算减仓比例 |
| v1.3 | 2025-12-16 | 新增日志规范章节：<br>1. 日志分类（连接/监控/信号/判断/计算/执行/预警/错误）<br>2. 关键日志点和格式规范<br>3. 结构化日志字段定义<br>4. 问题排查指南<br>5. 日志存储建议 |
| v1.4 | 2025-12-16 | 精简模块结构：<br>1. 日志接入现有 logger 系统<br>2. 目录结构从 15+ 文件精简到 4 文件<br>3. 删除冗余的 tracker/、config/ 目录<br>4. 合并 provider、engine 逻辑<br>5. 总代码量预估 ~850 行 |
| v1.5 | 2025-12-16 | 全面复用现有系统：<br>1. 多账户隔离复用现有 trader_id 机制<br>2. 跟单配置作为 Trader 属性，不是独立实体<br>3. 复用 TraderManager、执行器、仓位查询等<br>4. 数据库扩展现有 traders 表<br>5. 跟单 = Trader 的"决策源"选项 |
| v1.6 | 2025-12-16 | 补充边界情况处理：<br>1. 系统重启：复用现有 PositionSyncManager，从交易所获取真实仓位<br>2. 用户手动操作：尊重用户行为，后续忽略是合理的<br>3. 反向开仓乱序：新增反向窗口保护（防御性编程） |
| v1.7 | 2025-12-16 | 优化去重逻辑：<br>1. 使用时间戳过期机制替代随机删除<br>2. 默认 1 小时过期<br>3. 定期清理过期记录，避免内存泄漏 |
| v1.8 | 2025-12-18 | 完善历史仓位检测与 WebSocket 混合模式：<br>1. 更新流程图，增加"历史仓位检测"分支<br>2. 历史仓位检测扩展到所有 Provider（OKX + Hyperliquid）<br>3. 检测条件：领航员当前持仓 > 本次交易量 × 1.2 则跳过<br>4. Hyperliquid 新增 WebSocket 混合模式：收到 fill 后通过 REST 获取账户状态<br>5. 解决 WebSocket 模式下权益和杠杆获取问题 |
| **v2.0** | **2025-12-20** | **🎯 posId 统一方案重大更新：**<br>1. **历史仓位处理**：启动跟单时记录领航员已有仓位为 `ignored` 状态<br>2. **100% 准确判断**：通过数据库映射状态（active/ignored/无映射）判断是否跟随<br>3. **去掉阈值检测**：不再依赖 1.2x 阈值，避免误判<br>4. **新增方法**：`InitIgnoredPositions()`、`GetMapping()`、`SaveIgnoredPosition()`<br>5. **简化流程**：查数据库 → 判断状态 → 决策，逻辑更清晰<br>6. **修改文件**：store/copytrade.go、copytrade/engine.go、copytrade/integration.go |
| **v2.1** | **2025-12-21** | **🎯 lastKnownSize 精确匹配机制：**<br>1. **核心架构明确**：trade-records 只作为通知，所有关键数据从持仓 API 获取<br>2. **新增 lastKnownSize 字段**：记录每个仓位的上次已知大小，用于精确判断操作目标<br>3. **三轮匹配逻辑**：新开仓 → 精确加仓匹配（size变化）→ 兜底匹配<br>4. **反向查找法**：减仓/平仓通过本地映射反向查找领航员持仓<br>5. **解决同币种多仓位问题**：当有 BNB 逐仓10x 和 全仓3x 两个仓位时，能精确判断加仓目标<br>6. **删除冗余代码**：移除 `findLeaderPosition` 等不再使用的函数 |

---

**文档结束**

