// Package reentryadvisor 重入 AI 助手插件（Phase 1：半自动数据透明化）。
//
// 定位：Copy Guard 人工重入信号（copy_guard_manual_reentry_signals）产生后，
// 为其生成"决策数据包 + Prompt"并落库（reentry_ai_analyses），前端三段折叠
// 可复制展示，用户复制给外部 AI 判断后手工确认入场。
//
// 插件化铁律（方案 A）：
//   - 通过 DB 轮询（5s）发现新 PENDING 信号，copytrade 引擎零改动；
//   - 只消费公共接口：store 读表、market.GetOKXATRWithMaxAge、Binance 公共 REST；
//   - 全局开关（reentry_ai_config.enabled）关闭或插件内部 panic 均不影响跟单
//     与既有人工重入确认/邮件流程。
package reentryadvisor

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"nofx/logger"
	"nofx/store"
)

const (
	pollInterval = 5 * time.Second
	// regenerateCooldown 手动"重新生成"冷却：防止高频刷新打爆 Binance 公共接口
	regenerateCooldown = 60 * time.Second
)

// Advisor 插件实例（进程内单例）
type Advisor struct {
	st *store.Store
	bn *binanceClient

	stopCh chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	started bool
}

var (
	defaultAdvisor   *Advisor
	defaultAdvisorMu sync.RWMutex
)

// Start 创建并启动插件（main.go 调用一次）。返回实例供 Stop。
func Start(st *store.Store) *Advisor {
	a := &Advisor{
		st:     st,
		bn:     newBinanceClient(),
		stopCh: make(chan struct{}),
	}
	defaultAdvisorMu.Lock()
	defaultAdvisor = a
	defaultAdvisorMu.Unlock()

	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	a.wg.Add(1)
	go a.pollLoop()
	logger.Info("[ReentryAdvisor] 插件已启动（轮询人工重入信号，5s 间隔）")
	return a
}

// Stop 停止轮询（幂等）
func (a *Advisor) Stop() {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return
	}
	a.started = false
	a.mu.Unlock()
	close(a.stopCh)
	a.wg.Wait()
	logger.Info("[ReentryAdvisor] 插件已停止")
}

func (a *Advisor) pollLoop() {
	defer a.wg.Done()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			a.pollOnce()
		}
	}
}

// pollOnce 单次轮询：为尚无分析记录的 PENDING 信号生成数据包。
// 任何异常（含 panic）只影响本轮，绝不外泄影响宿主进程。
func (a *Advisor) pollOnce() {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("[ReentryAdvisor] 轮询 panic 已恢复: %v", r)
		}
	}()

	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 读取配置失败: %v", err)
		return
	}
	if !cfg.Enabled {
		return
	}

	signals, err := a.st.ReentryAI().ListManualReentrySignalsByStatus(store.ManualReentryStatusPending, 50)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 读取待处理信号失败: %v", err)
		return
	}
	for _, sig := range signals {
		has, err := a.st.ReentryAI().HasReentryAnalysisForSignal(sig.ID)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] 查询信号 %d 分析记录失败: %v", sig.ID, err)
			continue
		}
		if has {
			continue
		}
		analysis, err := a.generateForSignal(sig)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] 信号 %d (%s %s) 数据包生成失败: %v", sig.ID, sig.Symbol, sig.Side, err)
			continue
		}
		logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 数据包已生成 (analysis=%d, 市场层=%v, 缺失字段=%s)",
			sig.ID, sig.Symbol, sig.Side, analysis.ID, analysis.MarketDataAvailable, orNone(analysis.MissingFields))
	}
}

func orNone(s string) string {
	if s == "" {
		return "无"
	}
	return s
}

// generateForSignal 组装数据包 → 生成 Prompt → 落库一条分析记录
func (a *Advisor) generateForSignal(sig *store.CopyGuardManualReentrySignal) (*store.ReentryAIAnalysis, error) {
	pack, err := buildDataPack(a.st, a.bn, sig)
	if err != nil {
		return nil, err
	}
	packJSON, err := json.MarshalIndent(pack, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("数据包序列化失败: %w", err)
	}
	analysis := &store.ReentryAIAnalysis{
		SignalID:            sig.ID,
		TraderID:            sig.TraderID,
		CycleID:             sig.CycleID,
		Symbol:              sig.Symbol,
		Side:                sig.Side,
		SystemPrompt:        buildSystemPrompt(),
		UserPrompt:          buildUserPrompt(sig, string(packJSON)),
		DatapackJSON:        string(packJSON),
		MarketDataAvailable: pack.Meta.FuturesAvailable,
		MissingFields:       joinMissing(pack.Meta.MissingFields),
		PromptVersion:       promptVersion,
	}
	return a.st.ReentryAI().SaveReentryAnalysis(analysis)
}

func joinMissing(fields []string) string {
	out := ""
	for i, f := range fields {
		if i > 0 {
			out += ","
		}
		out += f
	}
	return out
}

// RegenerateForSignal 手动重新生成数据包（新快照新记录），API 层调用。
// 约束：插件已启动且启用；信号处于 PENDING/EXECUTING；距最近快照 ≥ 60s。
func RegenerateForSignal(signalID int64) (*store.ReentryAIAnalysis, error) {
	defaultAdvisorMu.RLock()
	a := defaultAdvisor
	defaultAdvisorMu.RUnlock()
	if a == nil {
		return nil, fmt.Errorf("重入 AI 助手插件未启动")
	}
	cfg, err := a.st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, fmt.Errorf("重入 AI 助手已在配置中关闭")
	}
	sig, err := a.st.CopyTrade().GetManualReentrySignal(signalID)
	if err != nil {
		return nil, fmt.Errorf("信号不存在: %d", signalID)
	}
	if sig.Status != store.ManualReentryStatusPending && sig.Status != store.ManualReentryStatusExecuting {
		return nil, fmt.Errorf("信号当前状态为 %s，不支持重新生成分析数据", sig.Status)
	}
	if latest, err := a.st.ReentryAI().LatestReentryAnalysisBySignal(signalID); err == nil && latest != nil {
		if since := time.Since(latest.SnapshotAt); since < regenerateCooldown {
			return nil, fmt.Errorf("重新生成过于频繁，请 %d 秒后再试", int((regenerateCooldown-since).Seconds())+1)
		}
	}
	analysis, err := a.generateForSignal(sig)
	if err != nil {
		return nil, err
	}
	logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 数据包已手动重新生成 (analysis=%d)", sig.ID, sig.Symbol, sig.Side, analysis.ID)
	return analysis, nil
}
