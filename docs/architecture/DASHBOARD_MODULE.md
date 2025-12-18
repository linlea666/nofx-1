# 📊 数据大屏模块设计文档

## 1. 概述

### 1.1 目标

构建一个**公共交易数据大屏**，展示所有交易员的统计数据，包括：

- 每个交易员的日/周/月盈亏
- 交易次数、胜率、盈亏比
- 全局汇总统计（所有交易员总盈亏）
- 排行榜和趋势图表

### 1.2 设计原则

1. **复用优先**：最大化复用现有数据结构和 API
2. **无侵入性**：不破坏现有系统架构
3. **增量开发**：在现有基础上扩展，不重构核心逻辑

---

## 2. 现有数据资产分析

### 2.1 数据库表结构

| 表名 | 用途 | 关键字段 | 复用价值 |
|------|------|---------|---------|
| `trader_positions` | 交易记录（开仓/平仓） | `realized_pnl`, `fee`, `entry_time`, `exit_time`, `status` | ⭐⭐⭐ **核心数据源** |
| `trader_equity_snapshots` | 权益快照（净值曲线） | `total_equity`, `timestamp`, `unrealized_pnl` | ⭐⭐⭐ 净值变化 |
| `decision_records` | 决策日志 | `timestamp`, `success`, `decisions` | ⭐⭐ 交易频率 |
| `copy_trade_signal_logs` | 跟单信号日志 | `action`, `status`, `created_at` | ⭐⭐ 跟单统计 |
| `traders` | 交易员配置 | `id`, `name`, `initial_balance` | ⭐⭐ 基础信息 |

### 2.2 现有统计函数（`store/position.go`）

```go
// ✅ 可直接复用
GetPositionStats(traderID)     // 基础统计：总交易、胜率、总PnL、总费用
GetFullStats(traderID)         // 完整统计：Sharpe Ratio、最大回撤、平均盈亏
GetSymbolStats(traderID, n)    // 按币种统计
GetDirectionStats(traderID)    // 多空方向统计
GetRecentTrades(traderID, n)   // 最近交易记录
GetHistorySummary(traderID)    // 综合历史摘要
```

### 2.3 现有 API 端点

```
GET /api/statistics?trader_id=xxx      # 单个交易员统计
GET /api/equity-history?trader_id=xxx  # 权益历史
GET /api/equity-history-batch          # 批量权益历史
GET /api/traders                       # 公开交易员列表
GET /api/competition                   # 竞赛数据
```

---

## 3. 跟单模式兼容性检查 ⚠️

### 3.1 数据记录链路

```
跟单引擎检测到信号
       ↓
integration.go → ExecuteDecision()
       ↓
auto_trader.go → ExecuteExternalDecision()
       ↓
executeOpenLongWithRecord() / executeCloseShortWithRecord()
       ↓
recordAndConfirmOrder() → recordPositionChange()
       ↓
store.Position().Create() / ClosePosition()  ← 记录到 trader_positions
```

### 3.2 跟单模式已兼容的记录 ✅

| 记录类型 | 存储位置 | 函数 | 状态 |
|---------|---------|------|------|
| 决策日志 | `decision_records` | `saveDecisionRecord()` | ✅ 兼容 |
| 权益快照 | `trader_equity_snapshots` | `saveEquitySnapshot()` | ✅ 兼容 |
| 信号日志 | `copy_trade_signal_logs` | `SaveSignalLog()` | ✅ 兼容 |
| 交易记录 | `trader_positions` | `recordPositionChange()` | ⚠️ 部分兼容 |

### 3.3 已发现的问题 🔴

#### 问题 1：Hyperliquid 订单 ID 为空导致交易记录缺失

```go
// auto_trader.go:1746-1748
if orderID == "" || orderID == "0" {
    logger.Infof("  ⚠️ Order ID is empty, skipping record")
    return  // ← 交易记录未保存！
}
```

**影响**：
- Hyperliquid 交易所的开仓/平仓记录可能未保存到 `trader_positions`
- 统计数据（盈亏、胜率）不完整

**建议修复**：
```go
// 即使 orderID 为空，也生成一个唯一 ID 继续记录
if orderID == "" || orderID == "0" {
    orderID = fmt.Sprintf("auto_%d", time.Now().UnixNano())
    logger.Infof("  ⚠️ Order ID is empty, using auto-generated: %s", orderID)
}
```

#### 问题 2：前端决策日志格式兼容性

从截图可见，跟单模式的前端显示已兼容：
- ✅ 币种和方向显示正确（DOGE SHORT）
- ✅ 入场价、杠杆显示正确
- ✅ AI思维链分析显示跟单信息（领航员 ID、数据源、跟单比例）

**结论**：前端显示格式已适配跟单模式。

---

## 4. 大屏功能设计

### 4.1 数据维度

#### 4.1.1 时间维度

| 维度 | 计算方式 |
|------|---------|
| 今日 | `WHERE DATE(exit_time) = DATE('now')` |
| 本周 | `WHERE exit_time >= date('now', 'weekday 0', '-7 days')` |
| 本月 | `WHERE strftime('%Y-%m', exit_time) = strftime('%Y-%m', 'now')` |
| 全部 | 无时间过滤 |

#### 4.1.2 交易员维度

- 单个交易员统计
- 全局汇总（所有交易员）

### 4.2 统计指标

| 指标 | 计算方式 | 数据源 |
|------|---------|--------|
| 总盈亏 | `SUM(realized_pnl)` | `trader_positions` |
| 总手续费 | `SUM(fee)` | `trader_positions` |
| 净盈亏 | `总盈亏 - 总手续费` | 计算 |
| 交易次数 | `COUNT(*)` | `trader_positions` |
| 胜率 | `盈利交易数 / 总交易数 * 100` | `trader_positions` |
| 盈亏比 | `平均盈利 / 平均亏损` | `trader_positions` |
| 最大回撤 | 峰值到谷值的最大跌幅 | `trader_equity_snapshots` |
| 当前净值 | 最新权益 | `trader_equity_snapshots` |
| 收益率 | `(当前净值 - 初始资金) / 初始资金 * 100` | 计算 |

### 4.3 功能模块

```
┌──────────────────────────────────────────────────────────────────┐
│                        📊 交易数据大屏                            │
├──────────────────────────────────────────────────────────────────┤
│  [全局统计卡片]                                                   │
│  ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐ ┌─────────┐    │
│  │ 总盈亏   │ │ 总交易   │ │ 平均胜率 │ │ 活跃交易员│ │ 总净值   │    │
│  │ +$1,234 │ │ 156 笔  │ │ 62.5%  │ │ 8 位    │ │ $10,500│    │
│  └─────────┘ └─────────┘ └─────────┘ └─────────┘ └─────────┘    │
├──────────────────────────────────────────────────────────────────┤
│  [时间筛选] ◯ 今日  ◯ 本周  ● 本月  ◯ 全部                        │
├──────────────────────────────────────────────────────────────────┤
│  [交易员排行榜]                         [盈亏趋势图]              │
│  ┌────────────────────────┐           ┌────────────────────┐    │
│  │ 1. 飞飞    +$456 62%  │           │     📈              │    │
│  │ 2. 东东    +$234 58%  │           │   /    \            │    │
│  │ 3. xxx    -$50  45%  │           │  /      \_/\        │    │
│  │ ...                   │           │ /            \__    │    │
│  └────────────────────────┘           └────────────────────┘    │
├──────────────────────────────────────────────────────────────────┤
│  [交易员详情] (点击排行榜展开)                                     │
│  ┌──────────────────────────────────────────────────────────┐   │
│  │ 交易员: 飞飞                                              │   │
│  │ 今日盈亏: +$123  |  本周: +$456  |  本月: +$890           │   │
│  │ 胜率: 62%  |  盈亏比: 1.8  |  最大回撤: 5.2%              │   │
│  │ 最近交易: BTCUSDT LONG +$45, ETHUSDT SHORT -$12, ...     │   │
│  └──────────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────────┘
```

---

## 5. 技术方案

### 5.1 后端 API 设计

#### 5.1.1 新增 API 端点

```go
// 大屏统计 API
GET /api/dashboard/summary              // 全局汇总统计
GET /api/dashboard/traders              // 交易员列表 + 统计
GET /api/dashboard/trader/:id           // 单个交易员详细统计
GET /api/dashboard/leaderboard          // 排行榜
GET /api/dashboard/trend                // 盈亏趋势图数据
```

#### 5.1.2 数据结构

```go
// DashboardSummary 全局汇总
type DashboardSummary struct {
    TotalPnL        float64 `json:"total_pnl"`         // 总盈亏
    TotalTrades     int     `json:"total_trades"`      // 总交易数
    AvgWinRate      float64 `json:"avg_win_rate"`      // 平均胜率
    ActiveTraders   int     `json:"active_traders"`    // 活跃交易员数
    TotalEquity     float64 `json:"total_equity"`      // 总净值
    TotalFees       float64 `json:"total_fees"`        // 总手续费
    UpdatedAt       string  `json:"updated_at"`        // 更新时间
}

// TraderDashboardStats 交易员统计
type TraderDashboardStats struct {
    TraderID        string  `json:"trader_id"`
    TraderName      string  `json:"trader_name"`
    Mode            string  `json:"mode"`              // ai | copy_trade
    
    // 分时段统计
    TodayPnL        float64 `json:"today_pnl"`
    WeekPnL         float64 `json:"week_pnl"`
    MonthPnL        float64 `json:"month_pnl"`
    TotalPnL        float64 `json:"total_pnl"`
    
    // 核心指标
    TotalTrades     int     `json:"total_trades"`
    WinRate         float64 `json:"win_rate"`
    ProfitFactor    float64 `json:"profit_factor"`     // 盈亏比
    MaxDrawdown     float64 `json:"max_drawdown"`
    
    // 当前状态
    CurrentEquity   float64 `json:"current_equity"`
    InitialBalance  float64 `json:"initial_balance"`
    ReturnRate      float64 `json:"return_rate"`       // 收益率 %
    PositionCount   int     `json:"position_count"`    // 当前持仓数
    
    // 最近交易
    RecentTrades    []RecentTrade `json:"recent_trades"`
}

// PnLTrendPoint 盈亏趋势数据点
type PnLTrendPoint struct {
    Date     string  `json:"date"`      // 日期
    PnL      float64 `json:"pnl"`       // 当日盈亏
    CumPnL   float64 `json:"cum_pnl"`   // 累计盈亏
    Equity   float64 `json:"equity"`    // 净值
}
```

### 5.2 复用策略

#### 5.2.1 复用现有 Store 函数

```go
// 在 store/position.go 中新增按时间段查询
func (s *PositionStore) GetPnLByDateRange(traderID string, start, end time.Time) (float64, int, error) {
    var totalPnL float64
    var count int
    err := s.db.QueryRow(`
        SELECT COALESCE(SUM(realized_pnl), 0), COUNT(*)
        FROM trader_positions
        WHERE trader_id = ? AND status = 'CLOSED'
        AND exit_time >= ? AND exit_time < ?
    `, traderID, start.Format(time.RFC3339), end.Format(time.RFC3339)).Scan(&totalPnL, &count)
    return totalPnL, count, err
}

// 全局统计（所有交易员）
func (s *PositionStore) GetGlobalStats() (*GlobalStats, error) {
    // 复用现有的 GetFullStats 逻辑，但不按 trader_id 过滤
}
```

#### 5.2.2 复用现有 API

```go
// 在 api/dashboard_handler.go 中
func (h *DashboardHandler) handleTraderStats(c *gin.Context) {
    traderID := c.Param("id")
    
    // 复用现有函数
    fullStats, _ := h.store.Position().GetFullStats(traderID)
    recentTrades, _ := h.store.Position().GetRecentTrades(traderID, 5)
    equityHistory, _ := h.store.Equity().GetLatest(traderID, 30)
    
    // 新增：按时间段统计
    todayPnL, todayTrades, _ := h.store.Position().GetPnLByDateRange(traderID, todayStart, todayEnd)
    // ...
}
```

### 5.3 前端设计

#### 5.3.1 新增页面

```
web/src/pages/Dashboard.tsx     // 大屏页面
web/src/components/dashboard/   // 大屏组件
  ├── SummaryCards.tsx          // 顶部统计卡片
  ├── TraderLeaderboard.tsx     // 交易员排行榜
  ├── PnLTrendChart.tsx         // 盈亏趋势图
  ├── TraderDetailPanel.tsx     // 交易员详情面板
  └── TimeRangeSelector.tsx     // 时间筛选器
```

#### 5.3.2 路由配置

```tsx
// App.tsx
<Route path="/dashboard" element={<Dashboard />} />
```

---

## 6. 实施计划

### Phase 1: 数据层完善 (Day 1-2)

1. **修复 Hyperliquid 交易记录缺失问题**
   - 修改 `recordAndConfirmOrder()`，即使 orderID 为空也生成唯一 ID 继续记录
   
2. **新增按时间段统计函数**
   - `GetPnLByDateRange()` - 按日期范围统计盈亏
   - `GetGlobalStats()` - 全局统计
   - `GetDailyPnLTrend()` - 每日盈亏趋势

### Phase 2: API 层开发 (Day 3-4)

1. **新建 `api/dashboard_handler.go`**
   - `/api/dashboard/summary` - 全局汇总
   - `/api/dashboard/traders` - 交易员列表统计
   - `/api/dashboard/leaderboard` - 排行榜
   - `/api/dashboard/trend` - 趋势数据

2. **注册路由**
   - 无需认证（公开数据）

### Phase 3: 前端开发 (Day 5-7)

1. **创建大屏页面组件**
2. **集成图表库**（复用现有的 recharts）
3. **响应式布局适配**

---

## 7. 风险评估

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| Hyperliquid 交易记录缺失 | 统计不准确 | Phase 1 优先修复 |
| 数据量大导致查询慢 | 大屏加载慢 | 添加缓存 + 分页 |
| 跟单模式数据格式差异 | 统计逻辑不一致 | 统一使用 trader_positions |

---

## 8. 附录

### 8.1 现有数据记录流程图

```
┌─────────────────────────────────────────────────────────────────┐
│                        交易数据记录流程                          │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  [AI 模式]              [跟单模式]                               │
│      │                      │                                    │
│      ↓                      ↓                                    │
│  decision/engine.go    copytrade/engine.go                      │
│      │                      │                                    │
│      └──────────┬───────────┘                                   │
│                 ↓                                                │
│         auto_trader.go                                          │
│    ExecuteDecisionWithRecord()                                  │
│    ExecuteExternalDecision()                                    │
│                 │                                                │
│                 ↓                                                │
│    recordAndConfirmOrder()                                      │
│                 │                                                │
│                 ↓                                                │
│    recordPositionChange()                                       │
│                 │                                                │
│    ┌───────────┼───────────┐                                   │
│    ↓           ↓           ↓                                   │
│ Position   Equity      Decision                                 │
│  .Create() .Save()    .LogDecision()                           │
│    │           │           │                                    │
│    ↓           ↓           ↓                                    │
│ trader_   trader_equity  decision_                              │
│ positions  _snapshots    records                                │
│    │           │           │                                    │
│    └───────────┴───────────┘                                   │
│                 │                                                │
│                 ↓                                                │
│         📊 数据大屏统计                                          │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

### 8.2 SQL 查询示例

```sql
-- 今日盈亏统计
SELECT 
    trader_id,
    SUM(realized_pnl) as today_pnl,
    SUM(fee) as today_fee,
    COUNT(*) as today_trades,
    SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END) as wins
FROM trader_positions
WHERE status = 'CLOSED'
AND DATE(exit_time) = DATE('now')
GROUP BY trader_id;

-- 全局汇总
SELECT 
    SUM(realized_pnl) as total_pnl,
    SUM(fee) as total_fee,
    COUNT(*) as total_trades,
    COUNT(DISTINCT trader_id) as trader_count,
    AVG(CASE WHEN realized_pnl > 0 THEN 1.0 ELSE 0.0 END) * 100 as avg_win_rate
FROM trader_positions
WHERE status = 'CLOSED';

-- 每日盈亏趋势
SELECT 
    DATE(exit_time) as date,
    SUM(realized_pnl) as daily_pnl,
    SUM(SUM(realized_pnl)) OVER (ORDER BY DATE(exit_time)) as cum_pnl
FROM trader_positions
WHERE status = 'CLOSED'
GROUP BY DATE(exit_time)
ORDER BY date;
```

---

## 9. 更新日志

| 日期 | 版本 | 更新内容 |
|------|------|---------|
| 2025-12-19 | v1.0 | 初始设计文档 |

