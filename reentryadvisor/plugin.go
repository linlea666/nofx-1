// Package reentryadvisor 重入 AI 助手插件。
//
// Phase 1（半自动数据透明化）：Copy Guard 人工重入信号
// （copy_guard_manual_reentry_signals）产生后，为其生成"决策数据包 + Prompt"
// 并落库（reentry_ai_analyses），前端三段折叠可复制展示，用户复制给外部 AI
// 判断后手工确认入场。
//
// Phase 2（内置 AI 分析）：同一快照同一 Prompt 直接喂内置模型（密钥复用
// ai_models 表），结论写回同一条记录并邮件通知；周期闭合后回填重入尝试的
// 真实盈亏，内外部 AI 结论准确率可对比。AI 只给建议，入场仍需人工确认。
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
	// backfillEvery 结局盈亏回填频率：每 12 轮（约 60s）扫一次已执行信号
	backfillEvery = 12
)

// Advisor 插件实例（进程内单例）
type Advisor struct {
	st *store.Store
	bn *binanceClient

	stopCh chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	started bool

	// Phase 2：内置 AI 分析进行中标记（防止同一记录并发分析）
	inflightMu sync.Mutex
	inflight   map[int64]bool

	// Phase 2：结局回填节流计数（每 backfillEvery 轮跑一次）
	pollCount int
}

var (
	defaultAdvisor   *Advisor
	defaultAdvisorMu sync.RWMutex
)

// Start 创建并启动插件（main.go 调用一次）。返回实例供 Stop。
func Start(st *store.Store) *Advisor {
	a := &Advisor{
		st:       st,
		bn:       newBinanceClient(),
		stopCh:   make(chan struct{}),
		inflight: map[int64]bool{},
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

	// Phase 2：结局盈亏回填（低频，独立于新信号处理，失败互不影响）
	a.pollCount++
	if a.pollCount%backfillEvery == 0 {
		a.backfillOutcomes()
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
		analysis, err := a.generateForSignal(sig, cfg)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] 信号 %d (%s %s) 数据包生成失败: %v", sig.ID, sig.Symbol, sig.Side, err)
			continue
		}
		logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 数据包已生成 (analysis=%d, 市场层=%v, 缺失字段=%s)",
			sig.ID, sig.Symbol, sig.Side, analysis.ID, analysis.MarketDataAvailable, orNone(analysis.MissingFields))
		// Phase 2：自动内置 AI 分析（异步，AI 慢不阻塞轮询；完成后邮件通知）
		if cfg.AIEnabled {
			go a.runAnalysis(analysis.ID, true)
		}
	}
}

// backfillOutcomes 为已执行（EXECUTED）信号回填重入尝试的真实结局盈亏。
// 归属规则：人工重入成交后引擎把周期 reentry_count 自增并开出新尝试，
// 故本信号对应 attempt_no = 信号快照时的 reentry_count + 1；等该尝试
// 闭合且对账完成（reconciled）后，以 pnl − fee 作为结局净额回填。
func (a *Advisor) backfillOutcomes() {
	signalIDs, err := a.st.ReentryAI().ListExecutedSignalIDsPendingOutcome(50)
	if err != nil {
		logger.Warnf("[ReentryAdvisor] 结局回填查询失败: %v", err)
		return
	}
	for _, sigID := range signalIDs {
		sig, err := a.st.CopyTrade().GetManualReentrySignal(sigID)
		if err != nil {
			continue
		}
		attempts, err := a.st.CopyTrade().ListCopyGuardAttempts(sig.CycleID)
		if err != nil {
			logger.Warnf("[ReentryAdvisor] 结局回填读取尝试失败 (signal=%d cycle=%d): %v", sigID, sig.CycleID, err)
			continue
		}
		targetNo := sig.ReentryCount + 1
		for _, at := range attempts {
			if at.AttemptNo != targetNo || at.ClosedAt == nil || !at.Reconciled {
				continue
			}
			outcome := at.PnL - at.Fee
			if err := a.st.ReentryAI().SetReentryOutcomeForSignal(sigID, outcome); err != nil {
				logger.Warnf("[ReentryAdvisor] 结局回填写入失败 (signal=%d): %v", sigID, err)
				break
			}
			logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 结局已回填: %.4f USDT (attempt_no=%d)",
				sigID, sig.Symbol, sig.Side, outcome, targetNo)
			break
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "无"
	}
	return s
}

// generateForSignal 组装数据包 → 生成 Prompt（含配置页自定义模板）→ 落库一条分析记录
func (a *Advisor) generateForSignal(sig *store.CopyGuardManualReentrySignal, cfg *store.ReentryAIConfig) (*store.ReentryAIAnalysis, error) {
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
		SystemPrompt:        buildSystemPrompt(cfg.PromptTemplate),
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
	analysis, err := a.generateForSignal(sig, cfg)
	if err != nil {
		return nil, err
	}
	logger.Infof("[ReentryAdvisor] 信号 %d (%s %s) 数据包已手动重新生成 (analysis=%d)", sig.ID, sig.Symbol, sig.Side, analysis.ID)
	// 自动分析开启时对新快照顺带跑内置 AI（用户在界面上，不发邮件）
	if cfg.AIEnabled {
		go a.runAnalysis(analysis.ID, false)
	}
	return analysis, nil
}
