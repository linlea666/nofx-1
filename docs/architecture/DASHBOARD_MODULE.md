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
3. **独立文件**：后端使用单文件方案，便于维护和删除

### 1.3 实现状态 ✅

| 模块 | 状态 | 文件 |
|------|------|------|
| 后端 API | ✅ 已完成 | `api/dashboard.go` |
| 前端页面 | ✅ 已完成 | `web/src/pages/DashboardPage.tsx` |
| 路由注册 | ✅ 已完成 | `api/server.go` |
| 数据记录修复 | ✅ 已完成 | `trader/auto_trader.go` |

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

---

## 3. 跟单模式兼容性 ✅

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

### 3.2 已修复的问题 ✅

#### 问题：Hyperliquid 订单 ID 为空导致交易记录缺失

**原代码**：
```go
if orderID == "" || orderID == "0" {
    logger.Infof("  ⚠️ Order ID is empty, skipping record")
    return  // ← 交易记录未保存！
}
```

**修复后**（`trader/auto_trader.go`）：
```go
if orderID == "" || orderID == "0" || orderID == "<nil>" {
    // Hyperliquid 不返回订单 ID，生成唯一 ID 确保记录被保存
    orderID = fmt.Sprintf("%s_%s_%s_%d", at.exchange, symbol, action, time.Now().UnixNano())
    logger.Infof("  📝 Order ID is empty, using auto-generated: %s", orderID)
}
// 继续记录，不再 return
```

**效果**：所有交易记录都能正确保存到 `trader_positions`，大屏统计数据完整。

---

## 4. 后端 API 实现 ✅

### 4.1 文件结构

采用**单文件独立方案**，所有 Dashboard 相关代码集中在 `api/dashboard.go`：

```
api/dashboard.go
├── 数据结构定义 (DashboardSummary, TraderDashboardStats, PnLTrendPoint)
├── 辅助函数 (getTimeRangeStart)
├── 数据查询函数 (直接 SQL)
├── API Handler
└── 路由注册 (RegisterDashboardRoutes)
```

### 4.2 API 端点

| 端点 | 方法 | 描述 | 认证 |
|------|------|------|------|
| `/api/dashboard/summary` | GET | 全局汇总统计 | 无需 |
| `/api/dashboard/traders` | GET | 所有交易员统计列表 | 无需 |
| `/api/dashboard/trader/:id` | GET | 单个交易员详细统计 | 无需 |
| `/api/dashboard/trend` | GET | 盈亏趋势数据 | 无需 |

### 4.3 数据结构

```go
// DashboardSummary 全局汇总
type DashboardSummary struct {
    TotalPnL      float64 `json:"total_pnl"`       // 总盈亏
    TotalTrades   int     `json:"total_trades"`    // 总交易数
    AvgWinRate    float64 `json:"avg_win_rate"`    // 平均胜率
    ActiveTraders int     `json:"active_traders"`  // 活跃交易员数
    TotalEquity   float64 `json:"total_equity"`    // 总净值
    TotalFees     float64 `json:"total_fees"`      // 总手续费
    TodayPnL      float64 `json:"today_pnl"`       // 今日盈亏
    WeekPnL       float64 `json:"week_pnl"`        // 本周盈亏
    MonthPnL      float64 `json:"month_pnl"`       // 本月盈亏
    UpdatedAt     string  `json:"updated_at"`      // 更新时间
}

// TraderDashboardStats 交易员大屏统计
type TraderDashboardStats struct {
    TraderID       string  `json:"trader_id"`
    TraderName     string  `json:"trader_name"`
    Mode           string  `json:"mode"`            // ai | copy_trade
    Exchange       string  `json:"exchange"`        // 交易所
    IsRunning      bool    `json:"is_running"`      // 是否运行中
    
    // 分时段统计
    TodayPnL       float64 `json:"today_pnl"`
    TodayTrades    int     `json:"today_trades"`
    WeekPnL        float64 `json:"week_pnl"`
    WeekTrades     int     `json:"week_trades"`
    MonthPnL       float64 `json:"month_pnl"`
    MonthTrades    int     `json:"month_trades"`
    TotalPnL       float64 `json:"total_pnl"`
    
    // 核心指标
    TotalTrades    int     `json:"total_trades"`
    WinRate        float64 `json:"win_rate"`
    WinTrades      int     `json:"win_trades"`
    LossTrades     int     `json:"loss_trades"`
    ProfitFactor   float64 `json:"profit_factor"`   // 盈亏比
    MaxDrawdown    float64 `json:"max_drawdown"`    // 最大回撤 %
    TotalFees      float64 `json:"total_fees"`      // 总手续费
    
    // 当前状态
    CurrentEquity  float64 `json:"current_equity"`
    InitialBalance float64 `json:"initial_balance"`
    ReturnRate     float64 `json:"return_rate"`     // 收益率 %
    PositionCount  int     `json:"position_count"`  // 当前持仓数
}

// PnLTrendPoint 盈亏趋势数据点
type PnLTrendPoint struct {
    Date   string  `json:"date"`    // 日期
    PnL    float64 `json:"pnl"`     // 当日盈亏
    CumPnL float64 `json:"cum_pnl"` // 累计盈亏
    Trades int     `json:"trades"`  // 交易数
}
```

### 4.4 路由注册

在 `api/server.go` 的 `setupRoutes()` 中：

```go
// Dashboard 数据大屏 API (无需认证)
s.RegisterDashboardRoutes(api)
```

---

## 5. 前端实现 ✅

### 5.1 文件结构

```
web/src/pages/DashboardPage.tsx    // 主页面（包含所有组件）
```

### 5.2 主要组件

| 组件 | 功能 |
|------|------|
| `ParticleBackground` | 粒子动画背景 |
| `NeonCard` | 霓虹发光边框卡片 |
| `CircleProgress` | 圆环进度图（胜率等） |
| `MiniBarChart` | 迷你柱状图 |
| `MiniLineChart` | 迷你折线图 |
| `BigStatCard` | 顶部大数字统计卡片 |
| `TraderLeaderboard` | 交易员排行榜 |
| `TraderDetailPanel` | 交易员详情面板 |
| `RealtimePanel` | 实时数据面板 |
| `GlobalPnLChart` | 全局盈亏图表 |
| `AnimatedNumber` | 数字动画效果 |

### 5.3 数据获取

```tsx
// 获取大屏交易员统计数据
const { data: dashboardTraders, isLoading, error } = useSWR(
  'dashboard-traders', 
  fetchDashboardTraders, 
  { refreshInterval: 30000 }
)

// 获取全局汇总数据
const { data: summaryData } = useSWR(
  'dashboard-summary', 
  fetchDashboardSummary, 
  { refreshInterval: 30000 }
)
```

### 5.4 路由配置

```tsx
// App.tsx
{ path: '/data-dashboard', element: <DashboardPage /> }

// HeaderBar.tsx
导航按钮: "数据大屏" → /data-dashboard
```

### 5.5 UI 特性

- 🎨 深蓝科技风格主题
- ✨ 粒子背景动画
- 💫 霓虹发光边框效果
- 📊 圆环进度图显示胜率
- 📈 迷你柱状图和折线图
- 🏆 前三名金银铜发光效果
- 🖥️ 三栏布局（排行榜/详情/实时数据）
- 🔄 30秒自动刷新
- ⚡ 加载状态和错误处理

---

## 6. 统计指标

| 指标 | 计算方式 | 数据源 |
|------|---------|--------|
| 总盈亏 | `SUM(realized_pnl)` | `trader_positions` |
| 总手续费 | `SUM(fee)` | `trader_positions` |
| 交易次数 | `COUNT(*)` | `trader_positions` |
| 胜率 | `盈利交易数 / 总交易数 * 100` | `trader_positions` |
| 盈亏比 | `总盈利 / 总亏损` | `trader_positions` |
| 最大回撤 | `(峰值 - 谷值) / 峰值 * 100` | 累计 PnL 计算 |
| 当前净值 | 最新权益快照 | `trader_equity_snapshots` |
| 收益率 | `(当前净值 - 初始资金) / 初始资金 * 100` | 计算 |
| 活跃交易员 | 有持仓的交易员数 | `trader_positions (status='OPEN')` |

---

## 7. 时间维度计算

```go
func getTimeRangeStart(timeRange string) time.Time {
    now := time.Now()
    switch timeRange {
    case "today":
        // 今天 00:00:00
        return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
    case "week":
        // 本周一 00:00:00
        weekday := int(now.Weekday())
        if weekday == 0 { weekday = 7 }
        return time.Date(now.Year(), now.Month(), now.Day()-weekday+1, 0, 0, 0, 0, now.Location())
    case "month":
        // 本月1号 00:00:00
        return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
    default:
        return time.Time{} // 全部
    }
}
```

---

## 8. 部署说明

### 8.1 后端

```bash
# 拉取代码
git pull

# 编译
go build -o nofx

# 重启服务
./nofx
```

### 8.2 前端

```bash
cd web
npm install
npm run build
```

### 8.3 访问地址

- 数据大屏：`https://your-domain/data-dashboard`

---

## 9. 附录

### 9.1 数据流程图

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
│    recordAndConfirmOrder()  ← 已修复：空 orderID 自动生成       │
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
│    ┌─────────────────────────────┐                             │
│    │    api/dashboard.go         │                             │
│    │    直接 SQL 查询统计        │                             │
│    └─────────────────────────────┘                             │
│                 │                                                │
│                 ↓                                                │
│         📊 数据大屏展示                                          │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

### 9.2 SQL 查询示例

```sql
-- 全局统计
SELECT 
    COALESCE(SUM(realized_pnl), 0) as total_pnl,
    COALESCE(SUM(fee), 0) as total_fees,
    COUNT(*) as total_trades
FROM trader_positions
WHERE status = 'CLOSED';

-- 今日盈亏
SELECT COALESCE(SUM(realized_pnl), 0) 
FROM trader_positions
WHERE status = 'CLOSED' AND exit_time >= '2025-12-19 00:00:00';

-- 交易员分时段统计
SELECT 
    COALESCE(SUM(realized_pnl), 0) as pnl,
    COALESCE(SUM(fee), 0) as fees,
    COUNT(*) as trades,
    COALESCE(SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END), 0) as wins,
    COALESCE(SUM(CASE WHEN realized_pnl < 0 THEN 1 ELSE 0 END), 0) as losses
FROM trader_positions
WHERE trader_id = ? AND status = 'CLOSED' AND exit_time >= ?;

-- 每日盈亏趋势
SELECT 
    DATE(exit_time) as date,
    COALESCE(SUM(realized_pnl), 0) as daily_pnl,
    COUNT(*) as trades
FROM trader_positions
WHERE status = 'CLOSED'
GROUP BY DATE(exit_time)
ORDER BY date ASC;
```

---

## 10. 更新日志

| 日期 | 版本 | 更新内容 |
|------|------|---------|
| 2025-12-19 | v1.0 | 初始设计文档 |
| 2025-12-19 | v2.0 | 完成实现，更新文档：<br>- 后端 API 实现 (`api/dashboard.go`)<br>- 前端页面实现 (`DashboardPage.tsx`)<br>- 修复 Hyperliquid 交易记录缺失<br>- 采用单文件独立方案 |
