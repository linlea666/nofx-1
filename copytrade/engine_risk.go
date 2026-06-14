package copytrade

import (
	"fmt"
	"math"
	"time"

	"nofx/decision"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
)

// ============================================================================
// 账户保护止损兜底（v3 风控）
//
// 设计哲学：跟单平时 100% 跟随领航员，不主动止盈/分批/干预；
// 仅在「价格反向走到账户风险线」时由交易所托管的 algo 条件单兜底平仓。
//
// 算法（calcStopLossPrice）：账户风险线 + ATR 下界 + 杠杆 cap 三层取严
//   - 账户风险线：硬上限，单笔最多亏账户的 riskPct（用户配，默认 0.5%）
//   - ATR 下界：噪音防护，避免被币种正常波动扫出（默认 1.5×ATR_1h_14）
//   - 杠杆兜底：保证金亏损封顶（默认 50%），防止超高杠杆时账户线失效
//
// 仅 OKX 路径调用，HL/Binance 完全不走这里。
// ============================================================================

// StopLossCalcInput 止损价计算输入
type StopLossCalcInput struct {
	Symbol         string   // 标准化交易对 "BTCUSDT"
	Side           SideType // SideLong | SideShort
	EntryPrice     float64  // 入场价（开仓时用 fill.Price 估算，执行后用实际成交均价）
	Leverage       int      // 杠杆倍数
	PositionValue  float64  // 仓位价值（USD，= entryPrice × size）
	FollowerEquity float64  // 跟随者账户权益（USD）
}

// StopLossCalcResult 止损价计算结果（含完整决策追踪，便于日志和调试）
type StopLossCalcResult struct {
	SLPrice          float64 // 最终 SL 价格（tickSz 对齐后）
	SLDistance       float64 // 最终 SL 距离（|entry - SL|）
	AccountDistance  float64 // 账户风险线对应距离
	ATRDistance      float64 // ATR 下界对应距离（0 = 未启用 / 获取失败）
	LeverageCapDist  float64 // 杠杆兜底 cap 对应距离（0 = 未启用）
	ATRValue         float64 // 实际 ATR 值（参考用）
	GovernedBy       string  // 哪条规则最终生效："account" | "atr" | "leverage_cap"
	TickSize         float64 // 实际使用的 tickSz（0 = fallback 到 1e-4）
	OpenImmediateHit bool    // 是否开仓即触发风险（SL 距离 < 0.1%）
}

// calcStopLossPrice 计算账户保护止损价
//
// 返回值：
//   - result.SLPrice > 0: 算出有效 SL 价
//   - result.SLPrice = 0: 不应该挂 SL（开仓即触发风险 / 输入异常）
//   - error: 严重错误（输入不合法），调用方应记日志降级
func calcStopLossPrice(cfg *CopyConfig, input *StopLossCalcInput) (*StopLossCalcResult, error) {
	if cfg == nil || input == nil {
		return nil, fmt.Errorf("nil config or input")
	}
	if input.EntryPrice <= 0 {
		return nil, fmt.Errorf("invalid entry price: %f", input.EntryPrice)
	}
	if input.PositionValue <= 0 {
		return nil, fmt.Errorf("invalid position value: %f", input.PositionValue)
	}
	if input.FollowerEquity <= 0 {
		return nil, fmt.Errorf("invalid follower equity: %f", input.FollowerEquity)
	}
	if input.Leverage <= 0 {
		input.Leverage = 1
	}
	if input.Side != SideLong && input.Side != SideShort {
		return nil, fmt.Errorf("invalid side: %s", input.Side)
	}

	result := &StopLossCalcResult{}

	// ============================================================
	// 步骤 1：账户风险线（硬上限）
	// SL 距离 = (followerEquity × riskPct) / positionValue × entryPrice
	//
	// 含义：「这笔单价格反向走到这里时，账户亏损刚好等于 riskPct」
	// 这是「我们最多能承受的亏损」的精确等价
	// ============================================================
	accountDist := (input.FollowerEquity * cfg.RiskAccountPct / input.PositionValue) * input.EntryPrice
	result.AccountDistance = accountDist
	finalDist := accountDist
	result.GovernedBy = "account"

	// ============================================================
	// 步骤 2：ATR 下界（噪音防护）
	// 如果启用 ATR 且 ATR 下界 > 账户线 → 用账户线（坚持账户保护）
	// 如果启用 ATR 且 ATR 下界 < 账户线 → 用 ATR 下界（防噪音扫损）
	// ============================================================
	if cfg.RiskATREnabled {
		atr, err := market.GetATR(input.Symbol, cfg.RiskATRTimeframe, 14)
		if err == nil && atr > 0 {
			result.ATRValue = atr
			atrDist := cfg.RiskATRMultiplier * atr
			result.ATRDistance = atrDist

			if atrDist <= accountDist {
				// ATR 下界比账户线还紧 → 放宽到 ATR（防噪音）
				finalDist = atrDist
				result.GovernedBy = "atr"
			} else {
				// ATR 比账户线还大 → 仍坚持账户线，但是个警告信号
				logger.Warnf("📐 ATR(%.4f×%.2f=%.4f) > 账户线(%.4f) | %s 波动较大，SL 可能频繁被扫出",
					atr, cfg.RiskATRMultiplier, atrDist, accountDist, input.Symbol)
			}
		} else if err != nil {
			// 降级：ATR 获取失败 → 仅用账户线
			logger.Debugf("📐 ATR 获取失败 | %s: %v（降级为纯账户线）", input.Symbol, err)
		}
	}

	// ============================================================
	// 步骤 3：杠杆兜底 cap（最外层封顶）
	// 不管前面算出多大，保证金亏损都不能超过 leverageMaxLoss（默认 50%）
	// 杠杆 cap 距离 = entryPrice × (leverageMaxLoss / leverage)
	// 含义：「价格反向 leverageMaxLoss/leverage 时，保证金亏损刚好等于 leverageMaxLoss」
	// ============================================================
	if cfg.RiskLeverageFallback && cfg.RiskLeverageMaxLoss > 0 {
		leverageCapDist := input.EntryPrice * (cfg.RiskLeverageMaxLoss / float64(input.Leverage))
		result.LeverageCapDist = leverageCapDist
		if leverageCapDist < finalDist {
			finalDist = leverageCapDist
			result.GovernedBy = "leverage_cap"
		}
	}

	result.SLDistance = finalDist

	// ============================================================
	// 步骤 4：开仓即触发检测
	// 如果 SL 距离 / entryPrice < 0.1% → 极有可能开仓滑点立即触发
	// 此时不挂 SL（裸跑），等下一轮 poll 由对账逻辑判断
	// ============================================================
	if finalDist/input.EntryPrice < 0.001 {
		result.OpenImmediateHit = true
		return result, nil // SLPrice = 0，调用方应跳过挂单
	}

	// ============================================================
	// 步骤 5：tickSz 对齐
	// 多单 SL：向下取整（让 SL 更紧 / 提前触发）
	// 空单 SL：向上取整（同样让 SL 更紧）
	// ============================================================
	tickSz, _ := getOKXTickSize(input.Symbol) // 失败时 tickSz=0，alignToTickSize 会 fallback 到 0.0001
	result.TickSize = tickSz

	var slPrice float64
	if input.Side == SideLong {
		slPrice = alignToTickSize(input.EntryPrice-finalDist, tickSz, true)
	} else {
		slPrice = alignToTickSize(input.EntryPrice+finalDist, tickSz, false)
	}

	// 防御：对齐后可能因为 tickSz 大于 finalDist 出现 SL = entry（无效）
	if slPrice <= 0 || math.Abs(slPrice-input.EntryPrice) < 1e-9 {
		return result, nil // SLPrice = 0，跳过挂单
	}

	result.SLPrice = slPrice
	return result, nil
}

// ============================================================================
// 对账逻辑：识别 SL 被触发（仓位消失但 mapping 还 active）
// ============================================================================

// stopRiskSuspectThreshold 多少次连续命中"本地无+领航员有"后才正式标 stopped_by_risk
// 设计：3 × 3s(poll 周期) ≈ 9 秒，能滤掉单次 GetPositions API 抖动 / 网络瞬断的假阳性
//
// 如果未来 poll 周期变化，确认 stopRiskSuspectThreshold × pollInterval ≈ 10-15s
const stopRiskSuspectThreshold = 3

// findLocalPositionForMapping 在跟随者本地持仓中查找对应 mapping 的仓位
//
// 关键认知：跟随者账户的 OKX posId（pos.PosID）与领航员账户的 OKX posId
// （mapping.LeaderPosID）属于不同账户，**永远不会相等**。
// 因此本地匹配只能用 symbol + side + marginMode 三元组。
//
// 返回值：
//   - found = true: 找到匹配的本地仓位（且 Size > 0）
//   - found = false: 本地无该 mapping 对应的仓位
func (e *Engine) findLocalPositionForMapping(localPositions map[string]*Position, mapping *store.CopyTradePositionMapping) (found bool) {
	for _, pos := range localPositions {
		if pos == nil || pos.Size <= 0 {
			continue
		}
		if pos.Symbol != mapping.Symbol {
			continue
		}
		if string(pos.Side) != mapping.Side {
			continue
		}
		// marginMode 严格匹配：cross 与 isolated 在 OKX 是独立 posId，必须区分
		// mapping.MarginMode 为空时（旧数据），仅按 symbol+side 兜底匹配
		if mapping.MarginMode != "" && pos.MarginMode != "" && pos.MarginMode != mapping.MarginMode {
			continue
		}
		return true
	}
	return false
}

// checkStoppedByRisk 对账识别 SL 被交易所触发
//
// 触发条件（同时满足）：
//   1. 跟单本地仓位已消失（symbol+side+marginMode 匹配不到）
//   2. 领航员该 posId 仍然存在（不是领航员主动平仓）
//   3. mapping.Status == "active"
//   4. 连续 stopRiskSuspectThreshold 次确认（防 GetPositions API 抖动误判）
//
// 调用时机：每个 poll 周期同步领航员状态后
// 仅 OKX 路径生效；HL/Binance 在 plan v1 暂不实现 SL 兜底
func (e *Engine) checkStoppedByRisk() {
	if e.store == nil || e.config == nil {
		return
	}
	if e.config.ProviderType != ProviderOKX {
		return
	}
	if !e.config.RiskStopLossEnabled {
		return
	}

	// 拉本地仓位（跟随者）
	// 注意：getFollowerPositions 失败时返回空 map（无法区分"真的空"和"API 抖动"），
	// 必须配合 stopRiskSuspectCount 多次确认机制做防御
	localPositions := e.getFollowerPositions()
	if localPositions == nil {
		// 真正的 nil 才跳过（不会发生，integration 实现总是返回非 nil）
		return
	}

	// 拉领航员持仓
	leaderPosMap := e.buildLeaderPosMap()

	activeMappings, err := e.store.CopyTrade().ListActiveMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 拉 active 映射失败（SL 对账跳过）: %v", e.traderID, err)
		return
	}

	// 防御：如果本地 trader API 异常返回空 map 但 mapping 又很多，是 API 抖动的强信号
	// 此时跳过整轮对账，避免批量错误熔断
	if len(localPositions) == 0 && len(activeMappings) > 1 {
		logger.Warnf("⚠️ [%s] 本地仓位为空但 %d 个 active mapping，疑似 API 抖动，本轮跳过 SL 对账",
			e.traderID, len(activeMappings))
		return
	}

	// 收集本轮所有"疑似 SL 触发"的 mapping
	suspectPosIds := make(map[string]bool)

	for _, mapping := range activeMappings {
		// 本地是否还有这个仓位？（用 symbol+side+marginMode 三元组匹配）
		if e.findLocalPositionForMapping(localPositions, mapping) {
			// 本地仓位还在，重置该 mapping 的疑似计数（如果之前累积过）
			delete(e.stopRiskSuspectCount, mapping.LeaderPosID)
			continue
		}

		// 本地没有，看领航员是否还持有
		leaderPos := leaderPosMap[mapping.LeaderPosID]
		if leaderPos == nil || leaderPos.Size <= 0 {
			// 领航员也没了 → 是领航员平仓 + 我方跟随平仓导致的 active 残留，
			// 走 matchCloseReduceSignal 路径正常关闭；这里不处理
			delete(e.stopRiskSuspectCount, mapping.LeaderPosID)
			continue
		}

		// 跟单本地仓位消失 + 领航员仍持有 = 疑似 SL 触发
		// 排除场景：领航员近期减仓导致我方也减仓平掉（用 mapping.LastKnownSize vs 领航员当前 size 对比）
		// 如果领航员减仓过（current < lastKnown），说明 mapping.LastKnownSize 是减仓前的旧值，
		// 那么我方本地仓位很可能是被跟随减仓信号清掉的，不算 SL 触发
		if mapping.LastKnownSize > 0 && leaderPos.Size < mapping.LastKnownSize*0.95 {
			logger.Debugf("⏭️ [%s] 本地仓位消失但领航员近期减仓过（last=%.4f cur=%.4f），可能是跟随减仓导致，跳过 SL 判定 | posId=%s",
				e.traderID, mapping.LastKnownSize, leaderPos.Size, mapping.LeaderPosID)
			delete(e.stopRiskSuspectCount, mapping.LeaderPosID)
			continue
		}

		// 累计疑似次数
		suspectPosIds[mapping.LeaderPosID] = true
		e.stopRiskSuspectCount[mapping.LeaderPosID]++
		count := e.stopRiskSuspectCount[mapping.LeaderPosID]

		if count < stopRiskSuspectThreshold {
			logger.Debugf("⏳ [%s] 疑似 SL 触发（%d/%d 次确认中） | posId=%s %s %s",
				e.traderID, count, stopRiskSuspectThreshold, mapping.LeaderPosID, mapping.Symbol, mapping.Side)
			continue
		}

		// 抓快照并标记为 stopped_by_risk
		leaderPnL := leaderPos.UnrealizedPnL
		leaderSize := leaderPos.Size
		addCount := mapping.AddCount

		if err := e.store.CopyTrade().MarkStoppedByRisk(e.traderID, mapping.LeaderPosID, leaderPnL, leaderSize, addCount); err != nil {
			logger.Errorf("❌ [%s] 标记 stopped_by_risk 失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
			continue
		}

		// 清除疑似计数（已转 stopped_by_risk 状态）
		delete(e.stopRiskSuspectCount, mapping.LeaderPosID)

		logger.Warnf("🛑 [%s] 账户保护止损触发 | %s %s posId=%s | 领航员仍持仓 size=%.4f pnl=%.2f addCount=%d (连续 %d 次确认)",
			e.traderID, mapping.Symbol, mapping.Side, mapping.LeaderPosID, leaderSize, leaderPnL, addCount, count)

		// 推风控事件给 integration 层发邮件告警
		e.emitRiskEvent(&RiskEvent{
			Type:        RiskEventStopLossTriggered,
			Timestamp:   time.Now(),
			Symbol:      mapping.Symbol,
			Side:        mapping.Side,
			MarginMode:  mapping.MarginMode,
			LeaderPosID: mapping.LeaderPosID,
			LeaderPnL:   leaderPnL,
			LeaderSize:  leaderSize,
			AddCount:    addCount,
		})
	}

	// 清理不再疑似的 mapping 的计数（释放内存）
	for posId := range e.stopRiskSuspectCount {
		if !suspectPosIds[posId] {
			delete(e.stopRiskSuspectCount, posId)
		}
	}
}

// ============================================================================
// 二次进场监控（判据 E 双门控）
//
// 判据：领航员仍持仓 + 价格回归 + 浮亏收窄 + 未加仓 + 重入次数<1 → 重入 50%
// 详见 plan §2.2
// ============================================================================

// checkReentryConditions 检查所有 stopped_by_risk 映射是否满足二次进场条件
// 满足时通过 e.decisionCh 推一个 Open 决策出去
func (e *Engine) checkReentryConditions() {
	if e.store == nil || e.config == nil {
		return
	}
	if e.config.ProviderType != ProviderOKX {
		return
	}
	if !e.config.RiskReentryEnabled {
		return
	}

	stoppedMappings, err := e.store.CopyTrade().ListStoppedByRiskMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 拉 stopped_by_risk 映射失败（二次进场跳过）: %v", e.traderID, err)
		return
	}
	if len(stoppedMappings) == 0 {
		return
	}

	leaderPosMap := e.buildLeaderPosMap()
	followerEquity := e.getFollowerBalance()

	for _, mapping := range stoppedMappings {
		// 安全阀：同 posId 重入限 1 次（硬阀）
		if mapping.ReentryUsed {
			continue
		}

		// 条件 1: 领航员仍持有该 posId
		leaderPos := leaderPosMap[mapping.LeaderPosID]
		if leaderPos == nil || leaderPos.Size <= 0 {
			// 领航员完全平掉了 → 在 checkIgnoredPositionsClosed 里会标 closed
			continue
		}

		// ============================================================
		// 条件 4: 反加仓铁律（v3.2 可配置）
		//
		// 背景：止损触发后领航员"亏损加仓救单"是赌徒型行为，是 CFTC 等监管机构
		// 警告的爆仓主因。但完全禁止任何加仓也会误伤"优秀领航员小幅摊均价"场景。
		//
		// v3.2 改进：用 RiskReentryBlockAddback 开关 + RiskReentryAddbackTolerance
		//   倍数允许灵活配置：
		//   - RiskReentryBlockAddback=false → 完全无视加仓，仅看价格回归+浮亏收窄
		//   - RiskReentryBlockAddback=true + Tolerance=1.0 → 严格：完全不允许加仓
		//   - RiskReentryBlockAddback=true + Tolerance=1.20 → 默认：允许 ≤20% 加仓
		//   - RiskReentryBlockAddback=true + Tolerance=1.50 → 宽松：允许 ≤50% 加仓
		//
		// 实施细节：不能用 mapping.AddCount > AddCountAtStop 判断（stopped_by_risk
		// 状态下 matchSignalWithMapping 早返，AddCount 永不增加），必须直接比较
		// 领航员当前 size 与 SL 触发时的 size。
		// ============================================================
		if e.config.RiskReentryBlockAddback {
			if mapping.LeaderSizeAtStop > 0 {
				// Tolerance 兜底（极旧库或前端未传时为 0，按默认 1.20 处理）
				addbackTolerance := e.config.RiskReentryAddbackTolerance
				if addbackTolerance <= 0 {
					addbackTolerance = 1.20
				}
				if leaderPos.Size > mapping.LeaderSizeAtStop*addbackTolerance {
					logger.Debugf("⏭️ [%s] 二次进场禁用：领航员 SL 后加仓超出容差 | posId=%s size %.4f → %.4f (容差倍数=%.2fx)",
						e.traderID, mapping.LeaderPosID, mapping.LeaderSizeAtStop, leaderPos.Size, addbackTolerance)
					continue
				}
			} else {
				// 旧数据兼容：v3 之前的 mapping 没有 LeaderSizeAtStop 快照，无法判定加仓
				// 此时降级为"不拦截"（与 v3.1 行为一致），但记 Debug 让用户知道
				logger.Debugf("⏭️ [%s] 反加仓判据降级：LeaderSizeAtStop=0（v3 之前的旧 mapping） | posId=%s",
					e.traderID, mapping.LeaderPosID)
			}
		}

		// 条件 2: 价格回归到入场价 ± tolerance
		// 入场价取 mapping.OpenPrice（记录跟单时的领航员开仓价；与 SL 计算用的一致）
		entryRef := mapping.OpenPrice
		if entryRef <= 0 {
			continue
		}
		markPrice := leaderPos.MarkPrice
		if markPrice <= 0 {
			markPrice = leaderPos.EntryPrice
		}
		if markPrice <= 0 {
			continue // 拿不到当前价，无法判断
		}
		tolerance := e.config.RiskReentryTolerance

		priceReturned := false
		if mapping.Side == string(SideLong) {
			priceReturned = markPrice >= entryRef*(1-tolerance)
		} else {
			priceReturned = markPrice <= entryRef*(1+tolerance)
		}
		if !priceReturned {
			continue
		}

		// ============================================================
		// 条件 3: 领航员浮亏收窄到 SL 触发时的 50% 以内
		//
		// 修复：LeaderPnLAtStop 必须 < 0 才有意义（SL 触发时领航员应该是浮亏）
		// 异常情况：LeaderPnLAtStop >= 0 → 状态不可信（可能是数据延迟/极端 SL 触发场景）
		// 处理：拒绝重入（保守优先）
		// ============================================================
		if mapping.LeaderPnLAtStop >= 0 {
			logger.Debugf("⏭️ [%s] 二次进场禁用：SL 触发快照不可信(LeaderPnLAtStop=%.2f >= 0) | posId=%s",
				e.traderID, mapping.LeaderPnLAtStop, mapping.LeaderPosID)
			continue
		}
		threshold := mapping.LeaderPnLAtStop * 0.5
		if leaderPos.UnrealizedPnL < threshold {
			continue
		}

		// ============================================================
		// 5 条全部满足 → 触发重入
		// ============================================================
		logger.Infof("🔄 [%s] 二次进场触发 | %s %s posId=%s | 价格回归=%.4f 入场=%.4f 浮亏收窄至=%.2f",
			e.traderID, mapping.Symbol, mapping.Side, mapping.LeaderPosID,
			markPrice, entryRef, leaderPos.UnrealizedPnL)

		// 计算重入仓位大小（USD）
		leaderTradeValue := leaderPos.Size * markPrice
		leaderEquity := float64(1)
		e.leaderStateMu.RLock()
		if e.leaderState != nil && e.leaderState.TotalEquity > 0 {
			leaderEquity = e.leaderState.TotalEquity
		}
		e.leaderStateMu.RUnlock()
		leaderRatio := leaderTradeValue / leaderEquity
		reentrySize := e.config.CopyRatio * e.config.RiskReentryRatio * leaderRatio * followerEquity
		if reentrySize <= 0 {
			logger.Warnf("⚠️ [%s] 重入金额非正(%.4f)，跳过 | posId=%s", e.traderID, reentrySize, mapping.LeaderPosID)
			continue
		}
		// 最小金额防御：低于交易所最小订单价值会导致下单失败 → 触发熔断；
		// 优先级用配置的 MinTradeWarn（与开仓金额最小阈值同一概念），未配置时用 10 USDT 兜底
		minReentry := e.config.MinTradeWarn
		if minReentry <= 0 {
			minReentry = 10.0
		}
		if reentrySize < minReentry {
			logger.Infof("⏭️ [%s] 重入金额 %.2f < 阈值 %.2f，跳过本次（条件保持，下轮再判） | posId=%s",
				e.traderID, reentrySize, minReentry, mapping.LeaderPosID)
			continue
		}

		// 标记 reentry_used + 状态 → active（先标后下单：避免重复触发；如果下单失败，由 mapping 失败计数熔断兜底）
		// 同步刷新 mapping.OpenPrice 和 OpenSizeUSD 为重入时的入场基准
		// （重要：updatePositionMapping 看到 status=active 的 mapping 会走"加仓"分支，
		//        不会刷新 OpenPrice/OpenSizeUSD，所以必须在这里先刷新）
		if err := e.store.CopyTrade().MarkReentryUsed(e.traderID, mapping.LeaderPosID, markPrice, reentrySize); err != nil {
			logger.Errorf("❌ [%s] 标记 reentry_used 失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
			continue
		}

		// 统计：重入也是一次"信号驱动"的决策
		e.stats.SignalsReceived++
		e.stats.SignalsFollowed++

		// 重要：清零该 posId 的 SL 疑似计数
		// 理由：emitReentryDecision 推决策到 channel 后，consumer 异步执行 ExecuteDecision 需要时间（1-3s）；
		// 这段窗口期内 mapping=active 但本地仍无仓位，会再次累计 suspect。
		// 清零后从 0 起算（再累 3 次需 9 秒），通常 ExecuteDecision 已完成 → 本地仓位出现 → suspect 自然清零。
		// 如果 ExecuteDecision 失败超过 9 秒，suspect 才会再次到阈值 → 重新标 stopped_by_risk（已 ReentryUsed=true，
		// 不会再重入，但状态正确反映了"重入失败"）。
		delete(e.stopRiskSuspectCount, mapping.LeaderPosID)

		// 推一个 Open 决策出去
		e.emitReentryDecision(mapping, leaderPos, reentrySize, markPrice)
	}
}

// emitReentryDecision 构造并推送一个二次进场的 Open 决策
func (e *Engine) emitReentryDecision(mapping *store.CopyTradePositionMapping, leaderPos *Position, copySize, entryPrice float64) {
	if leaderPos == nil {
		return
	}

	side := SideLong
	if mapping.Side == string(SideShort) {
		side = SideShort
	}

	// 取领航员权益用于日志展示（不参与决策金额计算——重入金额已固定）
	// 不传 0 是为了避免 buildCoTTrace / buildUserPromptLog 出现 0 或 NaN 误导日志
	e.leaderStateMu.RLock()
	displayLeaderEquity := float64(0)
	if e.leaderState != nil {
		displayLeaderEquity = e.leaderState.TotalEquity
	}
	e.leaderStateMu.RUnlock()

	// 构造 SignalMatchResult 复用 buildDecisionV2
	match := &SignalMatchResult{
		ShouldFollow:   true,
		Reason:         "二次进场（判据 E 双门控）",
		Action:         ActionOpen,
		PosID:          mapping.LeaderPosID,
		MarginMode:     mapping.MarginMode,
		LeaderPosition: leaderPos,
	}

	// 构造一个虚拟 fill（emitReentry 是引擎主动生成的，不来自领航员真实成交）
	virtualFill := &Fill{
		ID:           fmt.Sprintf("reentry_%s_%d", mapping.LeaderPosID, time.Now().UnixNano()),
		Symbol:       mapping.Symbol,
		Side:         "buy", // 仅占位，buildDecisionV2 会用 match.Action + PositionSide 映射 Action
		PositionSide: side,
		Action:       ActionOpen,
		Price:        entryPrice,
		Size:         leaderPos.Size,
		Value:        copySize,
		Timestamp:    time.Now(),
	}
	if side == SideShort {
		virtualFill.Side = "sell"
	}

	signal := &TradeSignal{
		LeaderID:       e.config.LeaderID,
		ProviderType:   e.config.ProviderType,
		Fill:           virtualFill,
		LeaderEquity:   displayLeaderEquity, // 仅供日志展示，金额已固定为 copySize
		LeaderPosition: leaderPos,
		LeaderPosID:    mapping.LeaderPosID,
	}

	// 走标准 buildDecisionV2 流程（不重新计算金额，直接传 copySize）
	dec := e.buildDecisionV2(signal, match, copySize)
	dec.Reasoning = fmt.Sprintf("Copy trading: reentry (judge E) following %s leader %s | posId=%s ratio=%.2f×%.2f",
		e.config.ProviderType, e.config.LeaderID, mapping.LeaderPosID, e.config.CopyRatio, e.config.RiskReentryRatio)

	fullDec := &decision.FullDecision{
		SystemPrompt:        e.buildSystemPromptLog(),
		UserPrompt:          e.buildUserPromptLog(signal),
		CoTTrace:            e.buildCoTTrace(signal, match.Action, copySize, nil),
		Decisions:           []decision.Decision{dec},
		RawResponse:         fmt.Sprintf("Copy trade reentry (judge E) from %s:%s posId=%s", e.config.ProviderType, e.config.LeaderID, mapping.LeaderPosID),
		Timestamp:           time.Now(),
		AIRequestDurationMs: 0,
	}

	select {
	case e.decisionCh <- fullDec:
		e.stats.DecisionsGenerated++
		logger.Infof("⚡ [%s] 二次进场决策已生成 | %s %s | 金额=%.2f", e.traderID, dec.Action, dec.Symbol, copySize)

		// 推风控事件给 integration 层发邮件告警
		// 注意：这是"决策生成"事件，不代表执行成功。实际执行成功的告警由 integration 在 ExecuteDecision 后发
		e.emitRiskEvent(&RiskEvent{
			Type:              RiskEventReentryInitiated,
			Timestamp:         time.Now(),
			Symbol:            mapping.Symbol,
			Side:              mapping.Side,
			MarginMode:        mapping.MarginMode,
			LeaderPosID:       mapping.LeaderPosID,
			LeaderPnL:         leaderPos.UnrealizedPnL,
			LeaderSize:        leaderPos.Size,
			ReentryEntryPrice: entryPrice,
			ReentrySize:       copySize,
		})
	default:
		logger.Warnf("⚠️ [%s] 决策通道已满，二次进场决策被丢弃 | posId=%s", e.traderID, mapping.LeaderPosID)
	}
}
