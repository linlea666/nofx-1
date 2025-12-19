package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/logger"
)

// ========== 缓存结构 ==========

// dashboardCache 大屏数据缓存
type dashboardCache struct {
	sync.RWMutex
	summary        *DashboardSummary
	summaryTime    time.Time
	traders        []TraderDashboardStats
	tradersTime    time.Time
	cacheDuration  time.Duration
}

// 全局缓存实例
var dbCache = &dashboardCache{
	cacheDuration: 30 * time.Second, // 30秒缓存
}

// isCacheValid 检查缓存是否有效
func (c *dashboardCache) isSummaryValid() bool {
	c.RLock()
	defer c.RUnlock()
	return c.summary != nil && time.Since(c.summaryTime) < c.cacheDuration
}

func (c *dashboardCache) isTradersValid() bool {
	c.RLock()
	defer c.RUnlock()
	return c.traders != nil && time.Since(c.tradersTime) < c.cacheDuration
}

// getSummary 获取缓存的汇总数据
func (c *dashboardCache) getSummary() *DashboardSummary {
	c.RLock()
	defer c.RUnlock()
	return c.summary
}

// setSummary 设置汇总缓存
func (c *dashboardCache) setSummary(s *DashboardSummary) {
	c.Lock()
	defer c.Unlock()
	c.summary = s
	c.summaryTime = time.Now()
}

// getTraders 获取缓存的交易员数据
func (c *dashboardCache) getTraders() []TraderDashboardStats {
	c.RLock()
	defer c.RUnlock()
	return c.traders
}

// setTraders 设置交易员缓存
func (c *dashboardCache) setTraders(t []TraderDashboardStats) {
	c.Lock()
	defer c.Unlock()
	c.traders = t
	c.tradersTime = time.Now()
}

// ========== 数据结构 ==========

// DashboardSummary 全局汇总统计
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

// SystemMonitor 系统监控统计
type SystemMonitor struct {
	// 跟单统计 (今日)
	TodaySignals    int     `json:"today_signals"`     // 今日信号总数
	TodayExecuted   int     `json:"today_executed"`    // 执行成功
	TodaySkipped    int     `json:"today_skipped"`     // 跳过
	TodayFailed     int     `json:"today_failed"`      // 失败
	ExecutionRate   float64 `json:"execution_rate"`    // 执行率 %
	
	// API 错误统计 (最近24小时)
	RateLimitErrors int     `json:"rate_limit_errors"` // 频率限制 (429)
	NetworkErrors   int     `json:"network_errors"`    // 网络错误
	AuthErrors      int     `json:"auth_errors"`       // 认证错误
	OtherErrors     int     `json:"other_errors"`      // 其他错误
	
	// 系统健康
	HealthScore     int     `json:"health_score"`      // 健康度 0-100
	
	// 风险预警
	Alerts          []RiskAlert `json:"alerts"`        // 风险预警列表
	
	UpdatedAt       string  `json:"updated_at"`
}

// RiskAlert 风险预警
type RiskAlert struct {
	Level      string  `json:"level"`       // critical | warning | info
	Type       string  `json:"type"`        // consecutive_loss | max_drawdown | api_error | low_win_rate
	TraderID   string  `json:"trader_id"`
	TraderName string  `json:"trader_name"`
	Message    string  `json:"message"`
	Value      float64 `json:"value"`       // 相关数值
	Timestamp  string  `json:"timestamp"`
}

// ========== 辅助函数 ==========

// getTimeRangeStart 获取时间范围起始时间
func getTimeRangeStart(timeRange string) time.Time {
	now := time.Now()
	switch timeRange {
	case "today":
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case "week":
		// 本周一
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7
		}
		return time.Date(now.Year(), now.Month(), now.Day()-weekday+1, 0, 0, 0, 0, now.Location())
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	default:
		return time.Time{} // 全部
	}
}

// ========== 数据查询 ==========

// getDashboardSummary 获取全局汇总统计
func (s *Server) getDashboardSummary() (*DashboardSummary, error) {
	summary := &DashboardSummary{
		UpdatedAt: time.Now().Format("2006-01-02 15:04:05"),
	}
	
	db := s.store.DB()
	
	// 全局统计
	err := db.QueryRow(`
		SELECT 
			COALESCE(SUM(realized_pnl), 0),
			COALESCE(SUM(fee), 0),
			COUNT(*)
		FROM trader_positions
		WHERE status = 'CLOSED'
	`).Scan(&summary.TotalPnL, &summary.TotalFees, &summary.TotalTrades)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询全局统计失败: %v", err)
	}
	
	// 计算胜率
	var winTrades int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions
		WHERE status = 'CLOSED' AND realized_pnl > 0
	`).Scan(&winTrades)
	if err == nil && summary.TotalTrades > 0 {
		summary.AvgWinRate = float64(winTrades) / float64(summary.TotalTrades) * 100
	}
	
	// 活跃交易员数（有持仓的）
	err = db.QueryRow(`
		SELECT COUNT(DISTINCT trader_id) FROM trader_positions WHERE status = 'OPEN'
	`).Scan(&summary.ActiveTraders)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询活跃交易员失败: %v", err)
	}
	
	// 今日盈亏
	todayStart := getTimeRangeStart("today")
	err = db.QueryRow(`
		SELECT COALESCE(SUM(realized_pnl), 0) FROM trader_positions
		WHERE status = 'CLOSED' AND exit_time >= ?
	`, todayStart.Format("2006-01-02 15:04:05")).Scan(&summary.TodayPnL)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询今日盈亏失败: %v", err)
	}
	
	// 本周盈亏
	weekStart := getTimeRangeStart("week")
	err = db.QueryRow(`
		SELECT COALESCE(SUM(realized_pnl), 0) FROM trader_positions
		WHERE status = 'CLOSED' AND exit_time >= ?
	`, weekStart.Format("2006-01-02 15:04:05")).Scan(&summary.WeekPnL)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询本周盈亏失败: %v", err)
	}
	
	// 本月盈亏
	monthStart := getTimeRangeStart("month")
	err = db.QueryRow(`
		SELECT COALESCE(SUM(realized_pnl), 0) FROM trader_positions
		WHERE status = 'CLOSED' AND exit_time >= ?
	`, monthStart.Format("2006-01-02 15:04:05")).Scan(&summary.MonthPnL)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询本月盈亏失败: %v", err)
	}
	
	// 获取总净值（从 equity snapshots）
	err = db.QueryRow(`
		SELECT COALESCE(SUM(total_equity), 0) FROM (
			SELECT trader_id, total_equity,
				ROW_NUMBER() OVER (PARTITION BY trader_id ORDER BY timestamp DESC) as rn
			FROM trader_equity_snapshots
		) t WHERE rn = 1
	`).Scan(&summary.TotalEquity)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询总净值失败: %v", err)
	}
	
	return summary, nil
}

// getTraderDashboardStats 获取单个交易员的大屏统计
func (s *Server) getTraderDashboardStats(traderID string) (*TraderDashboardStats, error) {
	stats := &TraderDashboardStats{
		TraderID: traderID,
	}
	
	db := s.store.DB()
	
	// 获取交易员基本信息
	var name, exchange, decisionMode, aiModel sql.NullString
	var initialBalance sql.NullFloat64
	err := db.QueryRow(`
		SELECT name, exchange, decision_mode, initial_balance, ai_model FROM traders WHERE id = ?
	`, traderID).Scan(&name, &exchange, &decisionMode, &initialBalance, &aiModel)
	if err == nil {
		// 优先使用 name，如果为空则尝试构建友好名称
		if name.String != "" {
			stats.TraderName = name.String
		} else {
			// 尝试用 ai_model + exchange 构建名称，如 "DEEPSEEK + HYPERLIQUID"
			if aiModel.String != "" && exchange.String != "" {
				stats.TraderName = aiModel.String + " + " + exchange.String
			} else if len(traderID) >= 8 {
				// 最后使用 trader_id 前8位
				stats.TraderName = traderID[:8]
			} else {
				stats.TraderName = traderID
			}
		}
		stats.Exchange = exchange.String
		stats.Mode = decisionMode.String
		if stats.Mode == "" {
			stats.Mode = "ai"
		}
		stats.InitialBalance = initialBalance.Float64
	}
	
	// 检查是否运行中
	stats.IsRunning = s.isTraderRunning(traderID)
	
	// 全部统计
	var totalWin, totalLoss float64
	err = db.QueryRow(`
		SELECT 
			COALESCE(SUM(realized_pnl), 0),
			COALESCE(SUM(fee), 0),
			COUNT(*),
			COALESCE(SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN realized_pnl < 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN realized_pnl > 0 THEN realized_pnl ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN realized_pnl < 0 THEN ABS(realized_pnl) ELSE 0 END), 0)
		FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED'
	`, traderID).Scan(
		&stats.TotalPnL, &stats.TotalFees, &stats.TotalTrades,
		&stats.WinTrades, &stats.LossTrades, &totalWin, &totalLoss,
	)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询交易员统计失败: %v", err)
	}
	
	// 计算胜率和盈亏比
	if stats.TotalTrades > 0 {
		stats.WinRate = float64(stats.WinTrades) / float64(stats.TotalTrades) * 100
	}
	if totalLoss > 0 {
		stats.ProfitFactor = totalWin / totalLoss
	}
	
	// 今日统计
	todayStart := getTimeRangeStart("today")
	err = db.QueryRow(`
		SELECT COALESCE(SUM(realized_pnl), 0), COUNT(*)
		FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED' AND exit_time >= ?
	`, traderID, todayStart.Format("2006-01-02 15:04:05")).Scan(&stats.TodayPnL, &stats.TodayTrades)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询今日统计失败: %v", err)
	}
	
	// 本周统计
	weekStart := getTimeRangeStart("week")
	err = db.QueryRow(`
		SELECT COALESCE(SUM(realized_pnl), 0), COUNT(*)
		FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED' AND exit_time >= ?
	`, traderID, weekStart.Format("2006-01-02 15:04:05")).Scan(&stats.WeekPnL, &stats.WeekTrades)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询本周统计失败: %v", err)
	}
	
	// 本月统计
	monthStart := getTimeRangeStart("month")
	err = db.QueryRow(`
		SELECT COALESCE(SUM(realized_pnl), 0), COUNT(*)
		FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED' AND exit_time >= ?
	`, traderID, monthStart.Format("2006-01-02 15:04:05")).Scan(&stats.MonthPnL, &stats.MonthTrades)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询本月统计失败: %v", err)
	}
	
	// 当前持仓数
	err = db.QueryRow(`
		SELECT COUNT(*) FROM trader_positions WHERE trader_id = ? AND status = 'OPEN'
	`, traderID).Scan(&stats.PositionCount)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询持仓数失败: %v", err)
	}
	
	// 当前净值（最新快照）
	err = db.QueryRow(`
		SELECT total_equity FROM trader_equity_snapshots
		WHERE trader_id = ? ORDER BY timestamp DESC LIMIT 1
	`, traderID).Scan(&stats.CurrentEquity)
	if err != nil && err != sql.ErrNoRows {
		logger.Warnf("Dashboard: 查询净值失败: %v", err)
	}
	
	// 计算收益率
	if stats.InitialBalance > 0 {
		stats.ReturnRate = (stats.CurrentEquity - stats.InitialBalance) / stats.InitialBalance * 100
	}
	
	// 计算最大回撤（简化版：使用累计 PnL）
	stats.MaxDrawdown = s.calculateMaxDrawdown(traderID)
	
	return stats, nil
}

// calculateMaxDrawdown 计算最大回撤
func (s *Server) calculateMaxDrawdown(traderID string) float64 {
	db := s.store.DB()
	
	rows, err := db.Query(`
		SELECT realized_pnl FROM trader_positions
		WHERE trader_id = ? AND status = 'CLOSED'
		ORDER BY exit_time ASC
	`, traderID)
	if err != nil {
		return 0
	}
	defer rows.Close()
	
	var cumPnL, peak, maxDrawdown float64
	for rows.Next() {
		var pnl float64
		if err := rows.Scan(&pnl); err != nil {
			continue
		}
		cumPnL += pnl
		if cumPnL > peak {
			peak = cumPnL
		}
		drawdown := peak - cumPnL
		if drawdown > maxDrawdown {
			maxDrawdown = drawdown
		}
	}
	
	if peak > 0 {
		return maxDrawdown / peak * 100
	}
	return 0
}

// getAllTradersDashboardStats 获取所有交易员统计
func (s *Server) getAllTradersDashboardStats() ([]TraderDashboardStats, error) {
	db := s.store.DB()
	
	// 获取所有交易员 ID
	rows, err := db.Query(`SELECT DISTINCT id FROM traders`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var traderIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		traderIDs = append(traderIDs, id)
	}
	
	// 获取每个交易员的统计
	var result []TraderDashboardStats
	for _, id := range traderIDs {
		stats, err := s.getTraderDashboardStats(id)
		if err != nil {
			logger.Warnf("Dashboard: 获取交易员 %s 统计失败: %v", id, err)
			continue
		}
		result = append(result, *stats)
	}
	
	return result, nil
}

// getSystemMonitor 获取系统监控数据
func (s *Server) getSystemMonitor() (*SystemMonitor, error) {
	monitor := &SystemMonitor{
		UpdatedAt: time.Now().Format("2006-01-02 15:04:05"),
		Alerts:    []RiskAlert{},
	}
	
	db := s.store.DB()
	todayStart := getTimeRangeStart("today")
	todayStr := todayStart.Format("2006-01-02 15:04:05")
	
	// ========== 跟单信号统计 (今日) ==========
	// 总信号数
	db.QueryRow(`
		SELECT COUNT(*) FROM copy_trade_signal_logs WHERE created_at >= ?
	`, todayStr).Scan(&monitor.TodaySignals)
	
	// 执行成功
	db.QueryRow(`
		SELECT COUNT(*) FROM copy_trade_signal_logs 
		WHERE created_at >= ? AND status = 'executed'
	`, todayStr).Scan(&monitor.TodayExecuted)
	
	// 跳过
	db.QueryRow(`
		SELECT COUNT(*) FROM copy_trade_signal_logs 
		WHERE created_at >= ? AND status = 'skipped'
	`, todayStr).Scan(&monitor.TodaySkipped)
	
	// 失败
	db.QueryRow(`
		SELECT COUNT(*) FROM copy_trade_signal_logs 
		WHERE created_at >= ? AND status = 'failed'
	`, todayStr).Scan(&monitor.TodayFailed)
	
	// 执行率
	if monitor.TodaySignals > 0 {
		monitor.ExecutionRate = float64(monitor.TodayExecuted) / float64(monitor.TodaySignals) * 100
	}
	
	// ========== API 错误统计 (最近24小时) ==========
	last24h := time.Now().Add(-24 * time.Hour).Format("2006-01-02 15:04:05")
	
	// 从 copy_trade_signal_logs 和 decision_records 提取错误
	rows, err := db.Query(`
		SELECT error_message FROM copy_trade_signal_logs 
		WHERE created_at >= ? AND error_message != '' AND error_message IS NOT NULL
		UNION ALL
		SELECT error_message FROM decision_records 
		WHERE timestamp >= ? AND error_message != '' AND error_message IS NOT NULL
	`, last24h, last24h)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var errMsg string
			if rows.Scan(&errMsg) == nil && errMsg != "" {
				// 分类错误
				errLower := strings.ToLower(errMsg)
				switch {
				case strings.Contains(errLower, "429") || strings.Contains(errLower, "rate") || strings.Contains(errLower, "limit"):
					monitor.RateLimitErrors++
				case strings.Contains(errLower, "timeout") || strings.Contains(errLower, "network") || strings.Contains(errLower, "connection"):
					monitor.NetworkErrors++
				case strings.Contains(errLower, "auth") || strings.Contains(errLower, "401") || strings.Contains(errLower, "403") || strings.Contains(errLower, "key"):
					monitor.AuthErrors++
				default:
					monitor.OtherErrors++
				}
			}
		}
	}
	
	// ========== 计算健康度 ==========
	monitor.HealthScore = 100
	totalErrors := monitor.RateLimitErrors + monitor.NetworkErrors + monitor.AuthErrors + monitor.OtherErrors
	if totalErrors > 0 {
		monitor.HealthScore -= min(totalErrors*5, 30) // 每个错误扣5分，最多扣30分
	}
	if monitor.ExecutionRate < 80 && monitor.TodaySignals > 5 {
		monitor.HealthScore -= 20 // 执行率低于80%扣20分
	}
	if monitor.HealthScore < 0 {
		monitor.HealthScore = 0
	}
	
	// ========== 风险预警计算 ==========
	monitor.Alerts = s.calculateRiskAlerts()
	
	// 根据预警调整健康度
	for _, alert := range monitor.Alerts {
		if alert.Level == "critical" {
			monitor.HealthScore -= 15
		} else if alert.Level == "warning" {
			monitor.HealthScore -= 5
		}
	}
	if monitor.HealthScore < 0 {
		monitor.HealthScore = 0
	}
	
	return monitor, nil
}

// calculateRiskAlerts 计算风险预警
func (s *Server) calculateRiskAlerts() []RiskAlert {
	var alerts []RiskAlert
	db := s.store.DB()
	
	// 获取所有交易员
	rows, err := db.Query(`SELECT DISTINCT id FROM traders`)
	if err != nil {
		return alerts
	}
	defer rows.Close()
	
	var traderIDs []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			traderIDs = append(traderIDs, id)
		}
	}
	
	for _, traderID := range traderIDs {
		// 获取交易员名称
		var traderName string
		var name, aiModel, exchange sql.NullString
		db.QueryRow(`SELECT name, ai_model, exchange FROM traders WHERE id = ?`, traderID).Scan(&name, &aiModel, &exchange)
		if name.String != "" {
			traderName = name.String
		} else if aiModel.String != "" {
			traderName = aiModel.String
		} else if len(traderID) >= 8 {
			traderName = traderID[:8]
		} else {
			traderName = traderID
		}
		
		// 1. 检查连续亏损 (最近5笔交易)
		recentPnLs := []float64{}
		pnlRows, err := db.Query(`
			SELECT realized_pnl FROM trader_positions 
			WHERE trader_id = ? AND status = 'CLOSED'
			ORDER BY exit_time DESC LIMIT 5
		`, traderID)
		if err == nil {
			for pnlRows.Next() {
				var pnl float64
				if pnlRows.Scan(&pnl) == nil {
					recentPnLs = append(recentPnLs, pnl)
				}
			}
			pnlRows.Close()
		}
		
		// 计算连续亏损次数
		consecutiveLosses := 0
		for _, pnl := range recentPnLs {
			if pnl < 0 {
				consecutiveLosses++
			} else {
				break
			}
		}
		
		if consecutiveLosses >= 3 {
			level := "warning"
			if consecutiveLosses >= 5 {
				level = "critical"
			}
			alerts = append(alerts, RiskAlert{
				Level:      level,
				Type:       "consecutive_loss",
				TraderID:   traderID,
				TraderName: traderName,
				Message:    fmt.Sprintf("连续亏损 %d 笔交易", consecutiveLosses),
				Value:      float64(consecutiveLosses),
				Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
			})
		}
		
		// 2. 检查胜率过低 (至少10笔交易)
		var totalTrades, winTrades int
		db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END), 0)
			FROM trader_positions WHERE trader_id = ? AND status = 'CLOSED'
		`, traderID).Scan(&totalTrades, &winTrades)
		
		if totalTrades >= 10 {
			winRate := float64(winTrades) / float64(totalTrades) * 100
			if winRate < 30 {
				alerts = append(alerts, RiskAlert{
					Level:      "warning",
					Type:       "low_win_rate",
					TraderID:   traderID,
					TraderName: traderName,
					Message:    fmt.Sprintf("胜率过低: %.1f%% (%d/%d)", winRate, winTrades, totalTrades),
					Value:      winRate,
					Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
				})
			}
		}
		
		// 3. 检查最大回撤
		maxDrawdown := s.calculateMaxDrawdown(traderID)
		if maxDrawdown > 20 {
			level := "warning"
			if maxDrawdown > 40 {
				level = "critical"
			}
			alerts = append(alerts, RiskAlert{
				Level:      level,
				Type:       "max_drawdown",
				TraderID:   traderID,
				TraderName: traderName,
				Message:    fmt.Sprintf("最大回撤: %.1f%%", maxDrawdown),
				Value:      maxDrawdown,
				Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
			})
		}
	}
	
	// 4. 检查 API 错误频繁
	var recentErrors int
	last1h := time.Now().Add(-1 * time.Hour).Format("2006-01-02 15:04:05")
	db.QueryRow(`
		SELECT COUNT(*) FROM copy_trade_signal_logs 
		WHERE created_at >= ? AND status = 'failed'
	`, last1h).Scan(&recentErrors)
	
	if recentErrors >= 5 {
		alerts = append(alerts, RiskAlert{
			Level:      "warning",
			Type:       "api_error",
			TraderID:   "",
			TraderName: "系统",
			Message:    fmt.Sprintf("最近1小时内 %d 次跟单失败", recentErrors),
			Value:      float64(recentErrors),
			Timestamp:  time.Now().Format("2006-01-02 15:04:05"),
		})
	}
	
	return alerts
}

// getPnLTrend 获取盈亏趋势（按天）
func (s *Server) getPnLTrend(traderID string, days int) ([]PnLTrendPoint, error) {
	db := s.store.DB()
	
	// 构建查询
	query := `
		SELECT 
			DATE(exit_time) as date,
			COALESCE(SUM(realized_pnl), 0) as daily_pnl,
			COUNT(*) as trades
		FROM trader_positions
		WHERE status = 'CLOSED'
	`
	args := []interface{}{}
	
	if traderID != "" {
		query += " AND trader_id = ?"
		args = append(args, traderID)
	}
	
	if days > 0 {
		startDate := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
		query += " AND DATE(exit_time) >= ?"
		args = append(args, startDate)
	}
	
	query += " GROUP BY DATE(exit_time) ORDER BY date ASC"
	
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var result []PnLTrendPoint
	var cumPnL float64
	
	for rows.Next() {
		var point PnLTrendPoint
		if err := rows.Scan(&point.Date, &point.PnL, &point.Trades); err != nil {
			continue
		}
		cumPnL += point.PnL
		point.CumPnL = cumPnL
		result = append(result, point)
	}
	
	return result, nil
}

// ========== API Handler ==========

// handleDashboardSummary 处理全局汇总请求（带缓存）
func (s *Server) handleDashboardSummary(c *gin.Context) {
	// 检查缓存
	if dbCache.isSummaryValid() {
		logger.Debugf("📊 Dashboard: 使用缓存的汇总数据")
		c.JSON(http.StatusOK, dbCache.getSummary())
		return
	}
	
	// 缓存失效，重新查询
	summary, err := s.getDashboardSummary()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "获取统计数据失败",
		})
		return
	}
	
	// 更新缓存
	dbCache.setSummary(summary)
	logger.Debugf("📊 Dashboard: 更新汇总数据缓存")
	
	c.JSON(http.StatusOK, summary)
}

// handleDashboardTraders 处理交易员列表统计请求（带缓存）
func (s *Server) handleDashboardTraders(c *gin.Context) {
	// 检查缓存
	if dbCache.isTradersValid() {
		logger.Debugf("📊 Dashboard: 使用缓存的交易员数据")
		c.JSON(http.StatusOK, dbCache.getTraders())
		return
	}
	
	// 缓存失效，重新查询
	traders, err := s.getAllTradersDashboardStats()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "获取交易员数据失败",
		})
		return
	}
	
	// 更新缓存
	dbCache.setTraders(traders)
	logger.Debugf("📊 Dashboard: 更新交易员数据缓存，共 %d 位", len(traders))
	
	c.JSON(http.StatusOK, traders)
}

// handleDashboardTrader 处理单个交易员统计请求
func (s *Server) handleDashboardTrader(c *gin.Context) {
	traderID := c.Param("id")
	if traderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "缺少 trader_id",
		})
		return
	}
	
	stats, err := s.getTraderDashboardStats(traderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "获取交易员数据失败",
		})
		return
	}
	c.JSON(http.StatusOK, stats)
}

// handleDashboardTrend 处理盈亏趋势请求
func (s *Server) handleDashboardTrend(c *gin.Context) {
	traderID := c.Query("trader_id") // 可选，为空则全局
	days := 30 // 默认30天
	if d := c.Query("days"); d != "" {
		if parsed, err := time.ParseDuration(d + "h"); err == nil {
			days = int(parsed.Hours() / 24)
		}
	}
	
	trend, err := s.getPnLTrend(traderID, days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "获取趋势数据失败",
		})
		return
	}
	c.JSON(http.StatusOK, trend)
}

// handleDashboardMonitor 处理系统监控请求
func (s *Server) handleDashboardMonitor(c *gin.Context) {
	monitor, err := s.getSystemMonitor()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "获取监控数据失败",
		})
		return
	}
	c.JSON(http.StatusOK, monitor)
}

// ========== 路由注册 ==========

// RegisterDashboardRoutes 注册大屏路由（在 setupRoutes 中调用）
func (s *Server) RegisterDashboardRoutes(api *gin.RouterGroup) {
	dashboard := api.Group("/dashboard")
	{
		dashboard.GET("/summary", s.handleDashboardSummary)
		dashboard.GET("/traders", s.handleDashboardTraders)
		dashboard.GET("/trader/:id", s.handleDashboardTrader)
		dashboard.GET("/trend", s.handleDashboardTrend)
		dashboard.GET("/monitor", s.handleDashboardMonitor)
	}
	
	logger.Infof("📊 Dashboard API 路由已注册:")
	logger.Infof("  • GET /api/dashboard/summary   - 全局汇总统计")
	logger.Infof("  • GET /api/dashboard/traders   - 所有交易员统计")
	logger.Infof("  • GET /api/dashboard/trader/:id - 单个交易员统计")
	logger.Infof("  • GET /api/dashboard/trend     - 盈亏趋势数据")
	logger.Infof("  • GET /api/dashboard/monitor   - 系统监控与风险预警")
}

