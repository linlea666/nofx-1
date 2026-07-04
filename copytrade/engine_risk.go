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
	Symbol           string   // 标准化交易对 "BTCUSDT"
	Side             SideType // SideLong | SideShort
	EntryPrice       float64  // 入场价（开仓时用 fill.Price 估算，执行后用实际成交均价）
	Leverage         int      // 杠杆倍数
	PositionValue    float64  // 仓位价值（USD，= entryPrice × size）
	FollowerEquity   float64  // 跟随者账户权益（USD）
	LiquidationPrice float64  // 跟随者强平价；0 表示交易所未返回
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
	ExpectedLossUSD  float64 // 价格损失 + 配置的滑点缓冲
	ExpectedLossPct  float64 // ExpectedLossUSD / AccountEquityUSD
	NoiseConflict    bool    // account_hard_limit 位于 ATR 噪音区内
	// LiquidationPriceIgnored: 交易所返回的强平价方向不合理（多单强平价高于
	// 入场价 / 空单低于入场价），已忽略强平价校验、按 ATR 止损继续挂单
	LiquidationPriceIgnored bool
}

func riskATRCacheMaxAge(cfg *CopyConfig) time.Duration {
	minutes := 120
	if cfg != nil && cfg.RiskATRCacheMaxAgeMinutes > 0 {
		minutes = cfg.RiskATRCacheMaxAgeMinutes
	}
	return time.Duration(minutes) * time.Minute
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
	accountDist := (input.FollowerEquity * cfg.RiskAccountPct / input.PositionValue) * input.EntryPrice
	result.AccountDistance = accountDist
	finalDist := accountDist

	// Copy Guard v4: volatility is the default authority. Account risk remains an explicit warning,
	// or becomes a hard upper bound only when account_hard_limit is selected.
	if cfg.RiskPolicyVersion >= 4 {
		period := cfg.RiskATRPeriod
		if period <= 0 {
			period = 14
		}
		atr, atrErr := market.GetOKXATRWithMaxAge(input.Symbol, cfg.RiskATRTimeframe, period, riskATRCacheMaxAge(cfg))
		if atrErr == nil && atr > 0 {
			result.ATRValue = atr
			result.ATRDistance = atr * cfg.RiskATRMultiplier
		} else {
			result.ATRDistance = input.EntryPrice * cfg.RiskATRFallbackPct
			logger.Warnf("📐 OKX ATR 获取失败 | %s: %v（使用 %.2f%% 降级距离）", input.Symbol, atrErr, cfg.RiskATRFallbackPct*100)
		}
		if result.ATRDistance <= 0 {
			return nil, fmt.Errorf("no valid ATR or fallback distance")
		}
		computed, err := ComputeRiskDistanceV4(cfg, input.EntryPrice, input.PositionValue, input.FollowerEquity, result.ATRDistance, input.Leverage)
		if err != nil {
			return nil, err
		}
		finalDist = computed.Distance
		result.GovernedBy = computed.GovernedBy
		result.NoiseConflict = computed.NoiseConflict
		if computed.GovernedBy == "margin_cap" {
			result.LeverageCapDist = finalDist
		}
		result.SLDistance = finalDist
		result.ExpectedLossUSD = computed.ExpectedLossUSD
		result.ExpectedLossPct = computed.ExpectedLossPct
		return finalizeStopLossPrice(input, result, cfg.RiskLiquidationBufferATR)
	}

	// ============================================================
	// 步骤 1：账户风险线（硬上限）
	// SL 距离 = (followerEquity × riskPct) / positionValue × entryPrice
	//
	// 含义：「这笔单价格反向走到这里时，账户亏损刚好等于 riskPct」
	// 这是「我们最多能承受的亏损」的精确等价
	// ============================================================
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

			if atrDist > accountDist {
				// ATR 是噪音下界：正常波动空间更大时必须放宽，不能反向收紧。
				finalDist = atrDist
				result.GovernedBy = "atr"
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
// 加仓账户风险预算（Copy Guard v4，v4.1 起仅告警不拦截）
//
// 背景：volatility_priority 模式下 RiskAccountPct 只是软性提示，领航员连续
// 加仓时单笔预期止损损失可以增长（WLD 实盘：5.2% → 28.4%）。这里在跟随加仓
// 时预估「加仓后总敞口按当前止损距离全损」占账户权益的比例，超过
// RiskAddonBudgetPct 时记录 ADDON_RISK_WARNING 告警。
//
// v4.1 设计修正：兜底风控不干扰领航员的开/加/减/平动作——拦截加仓会让跟随
// 仓位结构偏离领航员（实际保护由止损噪音下限 + 账户 20% 硬兜底承担），
// 因此从"拒绝加仓"降级为"仅告警"。
// ============================================================================

// addonBudgetEventInterval：同一仓位 ADDON_RISK_WARNING 事件/告警的最小间隔，
// 限频避免事件与日志刷屏。
const addonBudgetEventInterval = 60 * time.Second

// warnAddonRiskBudget 检查本次加仓是否超出账户风险预算，超出时仅记录告警
// 事件（不拦截）。任何数据不可得（无周期记录、权益为零、ATR 失败且无降级）
// 时静默跳过。
func (e *Engine) warnAddonRiskBudget(signal *TradeSignal, posID string, copySize float64) {
	cfg := e.config
	if cfg == nil || cfg.RiskPolicyVersion < 4 || cfg.ProviderType != ProviderOKX || !cfg.RiskStopLossEnabled {
		return
	}
	budget := cfg.RiskAddonBudgetPct
	if budget <= 0 || budget >= 1 {
		return
	}
	if e.store == nil || e.getFollowerEquity == nil || signal == nil || signal.Fill == nil || copySize <= 0 {
		return
	}
	cycle, err := e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, posID)
	if err != nil || cycle == nil {
		return
	}
	equity := e.getFollowerEquity()
	entryPrice := signal.Fill.Price
	if equity <= 0 || entryPrice <= 0 {
		return
	}
	period := cfg.RiskATRPeriod
	if period <= 0 {
		period = 14
	}
	var atrDistance float64
	if atr, atrErr := market.GetOKXATRWithMaxAge(signal.Fill.Symbol, cfg.RiskATRTimeframe, period, riskATRCacheMaxAge(cfg)); atrErr == nil && atr > 0 {
		atrDistance = atr * cfg.RiskATRMultiplier
	} else {
		atrDistance = entryPrice * cfg.RiskATRFallbackPct
	}
	if atrDistance <= 0 {
		return
	}
	totalNotional := cycle.FollowerNotional + copySize
	computed, err := ComputeRiskDistanceV4(cfg, entryPrice, totalNotional, equity, atrDistance, e.getLeaderLeverage(signal))
	if err != nil || computed.ExpectedLossPct <= budget {
		return
	}

	now := time.Now()
	if last, ok := e.lastAddonBudgetEvent[posID]; !ok || now.Sub(last) >= addonBudgetEventInterval {
		e.lastAddonBudgetEvent[posID] = now
		msg := fmt.Sprintf("加仓风险告警：预期止损损失 %.1f%% 超账户预算 %.1f%%（现有名义 %.2f + 加仓 %.2f，仍跟随）",
			computed.ExpectedLossPct*100, budget*100, cycle.FollowerNotional, copySize)
		logger.Warnf("🚧 [%s] %s | %s posId=%s", e.traderID, msg, signal.Fill.Symbol, posID)
		e.logWarning(Warning{
			Timestamp:    now,
			Symbol:       signal.Fill.Symbol,
			Type:         "addon_risk_warning",
			Message:      msg,
			SignalAction: string(ActionAdd),
			SignalValue:  copySize,
			CopyValue:    copySize,
			Executed:     true,
		})
		if err := e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
			CycleID:  cycle.ID,
			TraderID: e.traderID,
			Type:     "ADDON_RISK_WARNING",
			Price:    entryPrice,
			Notional: copySize,
			Metadata: map[string]interface{}{
				"expected_loss_pct": computed.ExpectedLossPct,
				"budget_pct":        budget,
				"current_notional":  cycle.FollowerNotional,
				"addon_notional":    copySize,
				"governed_by":       computed.GovernedBy,
				"blocked":           false,
			},
		}); err != nil {
			logger.Warnf("⚠️ [%s] ADDON_RISK_WARNING 事件写入失败: %v", e.traderID, err)
		}
	}
}

// isLiquidationPriceDirectionValid checks the liquidation price is on the
// plausible side of the entry price: a long liquidates below entry, a short
// above. OKX cross-margin returns account-level liquidation prices that can
// land on the wrong side (cycle 10: long entry 1717, "liquidation" 2630);
// such values must be ignored instead of blocking the ATR stop.
func isLiquidationPriceDirectionValid(side SideType, entryPrice, liquidationPrice float64) bool {
	if liquidationPrice <= 0 || entryPrice <= 0 {
		return false
	}
	if side == SideLong {
		return liquidationPrice < entryPrice
	}
	return liquidationPrice > entryPrice
}

func finalizeStopLossPrice(input *StopLossCalcInput, result *StopLossCalcResult, liquidationBufferATR float64) (*StopLossCalcResult, error) {
	if result.SLDistance/input.EntryPrice < 0.001 {
		result.OpenImmediateHit = true
		return result, nil
	}
	tickSz, _ := getOKXTickSize(input.Symbol)
	result.TickSize = tickSz
	if input.Side == SideLong {
		result.SLPrice = alignToTickSize(input.EntryPrice-result.SLDistance, tickSz, true)
	} else {
		result.SLPrice = alignToTickSize(input.EntryPrice+result.SLDistance, tickSz, false)
	}
	if result.SLPrice <= 0 || math.Abs(result.SLPrice-input.EntryPrice) < 1e-9 {
		result.SLPrice = 0
		return result, nil
	}
	if input.LiquidationPrice > 0 && result.ATRValue > 0 {
		if !isLiquidationPriceDirectionValid(input.Side, input.EntryPrice, input.LiquidationPrice) {
			// Direction-implausible liquidation price: ignore it and keep the
			// ATR stop; the caller records a diagnostic event.
			result.LiquidationPriceIgnored = true
			return result, nil
		}
		buffer := result.ATRValue * liquidationBufferATR
		if input.Side == SideLong && result.SLPrice <= input.LiquidationPrice+buffer {
			return nil, fmt.Errorf("stop %.8f is not safely above liquidation %.8f (buffer %.8f)", result.SLPrice, input.LiquidationPrice, buffer)
		}
		if input.Side == SideShort && result.SLPrice >= input.LiquidationPrice-buffer {
			return nil, fmt.Errorf("stop %.8f is not safely below liquidation %.8f (buffer %.8f)", result.SLPrice, input.LiquidationPrice, buffer)
		}
	}
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
//  1. 跟单本地仓位已消失（symbol+side+marginMode 匹配不到）
//  2. 领航员该 posId 仍然存在（不是领航员主动平仓）
//  3. mapping.Status == "active"
//  4. 连续 stopRiskSuspectThreshold 次确认（防 GetPositions API 抖动误判）
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
	if e.getFollowerPositionsResult != nil {
		var err error
		localPositions, err = e.getFollowerPositionsResult()
		if err != nil {
			logger.Warnf("⚠️ [%s] 跟随者持仓查询失败，本轮禁止仓位消失判定: %v", e.traderID, err)
			return
		}
	}
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
			// v4：跟随期也保持 last_observed_price 新鲜。周期若在跟随期直接
			// 结束（止损与领航员平仓同轮发生），估算基线才不会用到开仓时的
			// 陈旧价格（HMSTR 周期 31 基线被错记为 0 的根因）。
			if e.config.RiskPolicyVersion >= 4 {
				if lp := leaderPosMap[mapping.LeaderPosID]; lp != nil && lp.MarkPrice > 0 {
					_ = e.store.CopyTrade().UpdateCopyGuardObservedPrice(e.traderID, mapping.LeaderPosID, lp.MarkPrice)
				}
			}
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
		if e.config.RiskPolicyVersion >= 4 {
			cycle, cerr := e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, mapping.LeaderPosID)
			if cerr == nil {
				atr, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
				_ = e.store.CopyTrade().RecordCopyGuardStopObserved(cycle.ID, e.traderID, cycle.ReentryCount, atr, leaderPos.MarkPrice, leaderSize, leaderPnL, map[string]interface{}{"confirmation": "position_absent_fallback"})
				// 快照止损时的领航员均价，供重入保守锚点使用
				_ = e.store.CopyTrade().SnapshotCopyGuardLeaderEntryAtStop(cycle.ID, leaderPos.EntryPrice)
			}
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
	if !e.config.RiskReentryEnabled && e.config.RiskPolicyVersion < 4 {
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
	if e.config.RiskPolicyVersion >= 4 {
		followerEquity = e.getFollowerEquity()
	}

	for _, mapping := range stoppedMappings {
		var v4Cycle *store.CopyGuardCycle
		coolingDown := false
		terminalWatchStatus := ""
		if e.config.RiskPolicyVersion >= 4 {
			v4Cycle, err = e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, mapping.LeaderPosID)
			if err != nil {
				logger.Debugf("[%s] Copy Guard 生命周期不存在: %v", e.traderID, err)
				continue
			}
			if v4Cycle.Status == store.CopyGuardReentryPending {
				continue
			}
			// 回填开仓时因 API 限流缺失的权益快照（account_equity=0 会让
			// 账户级保护与熔断判定失效）
			if v4Cycle.AccountEquity <= 0 && followerEquity > 0 {
				if e.store.CopyTrade().BackfillCopyGuardAccountEquity(v4Cycle.ID, followerEquity) == nil {
					v4Cycle.AccountEquity = followerEquity
				}
			}
			if e.config.RiskReentryEnabled && v4Cycle.ReentryCount >= e.config.RiskMaxReentries {
				terminalWatchStatus = store.CopyGuardAttemptsExhausted
			}
			if e.config.RiskWatchTimeoutMinutes > 0 && v4Cycle.StoppedAt != nil && time.Since(*v4Cycle.StoppedAt) > time.Duration(e.config.RiskWatchTimeoutMinutes)*time.Minute {
				terminalWatchStatus = store.CopyGuardWatchTimeout
			}
			// v4.1 周期累计亏损熔断：同一周期已实现亏损触达权益 ×
			// RiskCycleMaxLossPct 后不再重入，只观察至领航员平仓
			if e.config.RiskCycleMaxLossPct > 0 && e.config.RiskCycleMaxLossPct < 1 {
				breakerEquity := v4Cycle.AccountEquity
				if breakerEquity <= 0 {
					breakerEquity = followerEquity
				}
				if breakerEquity > 0 && v4Cycle.ActualPnL <= -breakerEquity*e.config.RiskCycleMaxLossPct {
					if v4Cycle.Status != store.CopyGuardCycleLossCapped {
						logger.Warnf("⛔ [%s] 周期累计亏损熔断 | cycle=%d %s | 已亏 %.2f ≥ 权益 %.2f × %.0f%%，本周期不再重入",
							e.traderID, v4Cycle.ID, mapping.Symbol, -v4Cycle.ActualPnL, breakerEquity, e.config.RiskCycleMaxLossPct*100)
						_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
							CycleID:  v4Cycle.ID,
							TraderID: e.traderID,
							Type:     "CYCLE_LOSS_BREAKER",
							PnL:      v4Cycle.ActualPnL,
							Metadata: map[string]interface{}{
								"cycle_loss":     v4Cycle.ActualPnL,
								"account_equity": breakerEquity,
								"max_loss_pct":   e.config.RiskCycleMaxLossPct,
								"stop_count":     v4Cycle.StopCount,
								"reentry_count":  v4Cycle.ReentryCount,
							},
						})
					}
					terminalWatchStatus = store.CopyGuardCycleLossCapped
				}
			}
			// v4.1 冷却时间逐次加严：第 N 次重入的冷却 = 基础冷却 × 倍率^N。
			// 上限 7 天：无界指数（86400s × 10^10）转 time.Duration 会越过
			// int64 纳秒上限，Go 中越界 float→int 转换结果未定义（amd64 上
			// 变负数），负冷却等于冷却被绕过；7 天在实践上已等价于本周期
			// 不再重入（观察超时 / 领航员平仓会先结束周期）。
			cooldown := time.Duration(e.config.RiskReentryCooldownSeconds) * time.Second
			if v4Cycle.ReentryCount > 0 {
				esc := e.config.RiskReentryCooldownEscalation
				if esc < 1 {
					esc = 1
				}
				scaled := float64(cooldown) * math.Pow(esc, float64(v4Cycle.ReentryCount))
				if max := float64(7 * 24 * time.Hour); scaled > max {
					scaled = max
				}
				cooldown = time.Duration(scaled)
			}
			coolingDown = v4Cycle.StoppedAt != nil && time.Since(*v4Cycle.StoppedAt) < cooldown
		}
		// 安全阀：同 posId 重入限 1 次（硬阀）
		if e.config.RiskPolicyVersion < 4 && mapping.ReentryUsed {
			continue
		}

		// 条件 1: 领航员仍持有该 posId
		leaderPos := leaderPosMap[mapping.LeaderPosID]
		if leaderPos == nil || leaderPos.Size <= 0 {
			// 领航员完全平掉了 → 在 checkIgnoredPositionsClosed 里会标 closed
			continue
		}
		// 观察期领航员反手（OKX net 模式同 posId 换向）：本周期不可能再重入，
		// 直接以 LEADER_REVERSED 闭合，否则周期与 mapping 会永远停在观察态。
		if e.config.RiskPolicyVersion >= 4 && leaderPos.Side != "" && string(leaderPos.Side) != mapping.Side {
			// 观察期以反手价（当前标记价）作为"领航员离场价"汇总观察期数据
			emitWatchSummary(e.store.CopyTrade(), e.traderID, v4Cycle, leaderPos.MarkPrice)
			_ = e.store.CopyTrade().CloseCopyGuardCycle(v4Cycle.ID, store.CopyGuardLeaderReversed, v4Cycle.ActualPnL, v4Cycle.BaselinePnL, v4Cycle.Fees, v4Cycle.FundingFee, v4Cycle.LiquidationPenalty, v4Cycle.Slippage)
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: v4Cycle.ID, TraderID: e.traderID, Type: "LEADER_REVERSED", Price: leaderPos.MarkPrice, Metadata: map[string]interface{}{"old_side": mapping.Side, "new_side": string(leaderPos.Side), "phase": "watch"}})
			if err := e.store.CopyTrade().MarkStoppedByRiskAsClosed(e.traderID, mapping.LeaderPosID); err != nil {
				logger.Warnf("⚠️ [%s] 观察期反手后关闭 mapping 失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
			}
			logger.Infof("🔁 [%s] 观察期领航员反手，周期闭合 | cycle=%d posId=%s %s %s→%s", e.traderID, v4Cycle.ID, mapping.LeaderPosID, mapping.Symbol, mapping.Side, string(leaderPos.Side))
			continue
		}
		if e.config.RiskPolicyVersion >= 4 && (!e.config.RiskReentryEnabled || terminalWatchStatus != "") {
			mark := leaderPos.MarkPrice
			if mark <= 0 {
				mark = leaderPos.EntryPrice
			}
			// own-path 口径：影子名义按自身被止损时的仓位名义记账，不再按
			// 领航员比例折算（领航员加仓会把影子名义放大到自身开不出的规模，
			// 导致净保护效果虚高）。领航员减仓的 realized 逻辑保留（store 内
			// 按该名义等比例结算）。
			shadow := v4Cycle.FollowerNotional
			if shadow <= 0 {
				shadow = v4Cycle.BaselineNotional
			}
			_ = e.store.CopyTrade().UpdateCopyGuardShadow(v4Cycle.ID, leaderPos.EntryPrice, mark, shadow, leaderPos.Size)
			atr, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
			status := store.CopyGuardStoppedWatching
			if terminalWatchStatus != "" {
				status = terminalWatchStatus
			}
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, status, leaderPos.EntryPrice, mark, atr)
			// 采样：终态观察（次数用尽/超时/熔断）或重入未启用，gate 直接用状态名
			gate := watchGateReentryDisabled
			if terminalWatchStatus != "" {
				gate = terminalWatchStatus
			}
			e.recordWatchSample(v4Cycle, leaderPos, mark, atr, 0, 0, gate)
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
		if e.config.RiskPolicyVersion < 4 && e.config.RiskReentryBlockAddback {
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

		// 条件 2: 价格回归到入场价附近（v3.3 单边严格区间，仅允许优于或等于领航员入场价时重入）
		//
		// 入场价取 mapping.OpenPrice（记录跟单时的领航员开仓价；与 SL 计算用的一致）
		entryRef := mapping.OpenPrice
		if e.config.RiskPolicyVersion >= 4 {
			entryRef = leaderPos.EntryPrice
		}
		if entryRef <= 0 {
			continue
		}
		// v4.1 保守锚点：领航员止损后加仓摊均价会把实时均价拖向不利方向
		// （多单变低、空单变高），使重入边界在没有真实恢复时就被穿越。
		// 锚点取 max/min(实时均价, 止损时快照)，保证重入门槛不因领航员
		// 摊均价而变松；快照缺失（旧数据）时退回实时均价。
		reentryAnchor := entryRef
		if e.config.RiskPolicyVersion >= 4 && v4Cycle != nil && v4Cycle.LeaderEntryAtStop > 0 {
			if mapping.Side == string(SideLong) {
				reentryAnchor = math.Max(entryRef, v4Cycle.LeaderEntryAtStop)
			} else {
				reentryAnchor = math.Min(entryRef, v4Cycle.LeaderEntryAtStop)
			}
		}
		// v4.1 最小恢复幅度判据需要最近一次止损的成交价；顺带为重入定 size
		// 复用（避免二次查询）
		var stoppedAttempt *store.CopyGuardAttempt
		if e.config.RiskPolicyVersion >= 4 && v4Cycle != nil {
			if attempts, attemptErr := e.store.CopyTrade().ListCopyGuardAttempts(v4Cycle.ID); attemptErr == nil {
				for _, attempt := range attempts {
					if attempt.AttemptNo == v4Cycle.ReentryCount && attempt.Status == "STOPPED" {
						stoppedAttempt = attempt
						break
					}
				}
			}
		}
		markPrice := leaderPos.MarkPrice
		if markPrice <= 0 {
			markPrice = leaderPos.EntryPrice
		}
		if markPrice <= 0 {
			continue // 拿不到当前价，无法判断
		}
		if e.config.RiskPolicyVersion >= 4 {
			// own-path 口径：同上，影子名义按自身仓位名义记账
			shadow := v4Cycle.FollowerNotional
			if shadow <= 0 {
				shadow = v4Cycle.BaselineNotional
			}
			_ = e.store.CopyTrade().UpdateCopyGuardShadow(v4Cycle.ID, entryRef, markPrice, shadow, leaderPos.Size)
			if coolingDown {
				atr, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
				_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardStoppedWatching, entryRef, markPrice, atr)
				e.recordWatchSample(v4Cycle, leaderPos, markPrice, atr, 0, 0, watchGateCooldown)
				continue
			}
		}
		tolerance := e.config.RiskReentryTolerance

		// 单边严格触发区间：
		//   多单：[entry × (1-tolerance), entry]
		//     含义：价格至少回到入场价附近（下界）+ 不能超过领航员入场价（上界）
		//     效果：重入价不差于原入场价，避免追涨
		//   空单：[entry, entry × (1+tolerance)]
		//     含义：价格至少回到入场价附近（上界）+ 不能低于领航员入场价（下界）
		//     效果：重入价不差于原入场价，避免追跌
		//
		// 与 v3.2 之前的区别：旧逻辑只检查"下界（多单）/上界（空单）"，没有反方向约束，
		// 导致价格大幅超出入场价时（如 SL 触发 @63 后涨到 80）仍会重入，等于在更差的位置追单。
		priceReturned := false
		currentATR := float64(0)
		reentryBoundary, chaseLimit := float64(0), float64(0)
		if e.config.RiskPolicyVersion >= 4 {
			currentATR, _ = market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
			if currentATR <= 0 {
				currentATR = entryRef * e.config.RiskATRFallbackPct
			}
			if v4Cycle.ATRAtStop > 0 && currentATR > v4Cycle.ATRAtStop*e.config.RiskReentryMaxATRExpansion {
				_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardStoppedWatching, entryRef, markPrice, currentATR)
				e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, 0, 0, watchGateATRExpansion)
				continue
			}
			// 首轮观察（尚无上一 tick 价格）只记录观测、不判穿越：
			// LastObservedPrice=0 会让多单的 "0 < boundary" 恒真，造成误触发。
			// 边界与追价上限均以保守锚点为基准（见 reentryAnchor 注释）。
			// 边界/追价上限每个 tick 都计算（观察期采样要记录），穿越判定
			// 仍要求已有上一 tick 价格。
			//
			// v4.1 最小恢复幅度：把"止损成交价 ± minRecovery × ATR（第 N 次
			// 重入按倍率^N 加严）"并入穿越边界，价格必须从止损价真实恢复该
			// 幅度才算穿越——防止"刚止损又原地接回"的震荡循环。并入边界
			// （而非事后否决）保证边界抬高后穿越判定仍是单次边沿触发。
			// 止损成交价缺失（旧数据）时降级为纯带宽边界。
			reentryBoundary = reentryAnchor
			requiredRecovery := float64(0)
			beyondChase := false
			if e.config.RiskReentryMinRecoveryATR > 0 && stoppedAttempt != nil && stoppedAttempt.ExitPrice > 0 {
				recoveryEsc := e.config.RiskReentryRecoveryEscalation
				if recoveryEsc < 1 {
					recoveryEsc = 1
				}
				requiredRecovery = currentATR * e.config.RiskReentryMinRecoveryATR * math.Pow(recoveryEsc, float64(v4Cycle.ReentryCount))
			}
			if mapping.Side == string(SideLong) {
				reentryBoundary -= currentATR * e.config.RiskReentryBandATR
				if requiredRecovery > 0 {
					reentryBoundary = math.Max(reentryBoundary, stoppedAttempt.ExitPrice+requiredRecovery)
				}
				chaseLimit = reentryAnchor + currentATR*e.config.RiskReentryMaxChaseATR
				beyondChase = markPrice > chaseLimit
				if v4Cycle.LastObservedPrice > 0 {
					priceReturned = v4Cycle.LastObservedPrice < reentryBoundary && markPrice >= reentryBoundary && !beyondChase
				}
			} else {
				reentryBoundary += currentATR * e.config.RiskReentryBandATR
				if requiredRecovery > 0 {
					reentryBoundary = math.Min(reentryBoundary, stoppedAttempt.ExitPrice-requiredRecovery)
				}
				chaseLimit = reentryAnchor - currentATR*e.config.RiskReentryMaxChaseATR
				beyondChase = markPrice < chaseLimit
				if v4Cycle.LastObservedPrice > 0 {
					priceReturned = v4Cycle.LastObservedPrice > reentryBoundary && markPrice <= reentryBoundary && !beyondChase
				}
			}
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardStoppedWatching, entryRef, markPrice, currentATR)
			if !priceReturned {
				gate := watchGatePriceNotReturned
				if beyondChase {
					gate = watchGateChaseExceeded
				}
				e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, gate)
			}
		} else if mapping.Side == string(SideLong) {
			priceReturned = markPrice >= entryRef*(1-tolerance) && markPrice <= entryRef
		} else {
			priceReturned = markPrice <= entryRef*(1+tolerance) && markPrice >= entryRef
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
		if e.config.RiskPolicyVersion < 4 && mapping.LeaderPnLAtStop >= 0 {
			logger.Debugf("⏭️ [%s] 二次进场禁用：SL 触发快照不可信(LeaderPnLAtStop=%.2f >= 0) | posId=%s",
				e.traderID, mapping.LeaderPnLAtStop, mapping.LeaderPosID)
			continue
		}
		threshold := mapping.LeaderPnLAtStop * 0.5
		if e.config.RiskPolicyVersion < 4 && leaderPos.UnrealizedPnL < threshold {
			continue
		}

		// ============================================================
		// 5 条全部满足 → 触发重入
		// ============================================================
		logger.Infof("🔄 [%s] 二次进场触发 | %s %s posId=%s | 价格回归=%.4f 入场=%.4f 浮亏收窄至=%.2f",
			e.traderID, mapping.Symbol, mapping.Side, mapping.LeaderPosID,
			markPrice, entryRef, leaderPos.UnrealizedPnL)

		// 计算重入仓位大小（USD）
		//
		// v4：以"被止损时自己的仓位名义价值"为基准（× 重入系数）。
		// 旧逻辑按领航员当前总仓位占比折算，当领航员在跟随期间加过仓（跟随者
		// 未等比例跟进）时，重入会远大于被止损的仓位——实盘出现过重入 8 倍于
		// 首仓、57 倍杠杆、止损价落入强平区导致保护单挂不上的事故（cycle 15）。
		// 被止损仓位基准让重入风险严格有界：永远不超过上一段仓位的名义 × 系数。
		var reentrySize float64
		stoppedNotional := float64(0)
		if e.config.RiskPolicyVersion >= 4 && v4Cycle != nil {
			if stoppedAttempt != nil {
				stoppedNotional = stoppedAttempt.Notional
			}
			if stoppedNotional <= 0 {
				// 旧数据/回填周期可能没有 attempt 名义，用周期跟随名义兜底
				stoppedNotional = v4Cycle.FollowerNotional
			}
			reentrySize = stoppedNotional * e.config.RiskReentryRatio
		}
		if reentrySize <= 0 {
			// v3 及无名义快照的兜底：按领航员当前仓位占比折算（原有公式）
			leaderTradeValue := leaderPos.Size * markPrice
			leaderEquity := float64(1)
			e.leaderStateMu.RLock()
			if e.leaderState != nil && e.leaderState.TotalEquity > 0 {
				leaderEquity = e.leaderState.TotalEquity
			}
			e.leaderStateMu.RUnlock()
			leaderRatio := leaderTradeValue / leaderEquity
			reentrySize = e.config.CopyRatio * e.config.RiskReentryRatio * leaderRatio * followerEquity
		}
		if reentrySize <= 0 {
			logger.Warnf("⚠️ [%s] 重入金额非正(%.4f)，跳过 | posId=%s", e.traderID, reentrySize, mapping.LeaderPosID)
			if e.config.RiskPolicyVersion >= 4 {
				e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateMinNotional)
			}
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
			if e.config.RiskPolicyVersion >= 4 {
				e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateMinNotional)
			}
			continue
		}

		// 标记 reentry_used + 状态 → active（先标后下单：避免重复触发；如果下单失败，由 mapping 失败计数熔断兜底）
		// 同步刷新 mapping.OpenPrice 和 OpenSizeUSD 为重入时的入场基准
		// （重要：updatePositionMapping 看到 status=active 的 mapping 会走"加仓"分支，
		//        不会刷新 OpenPrice/OpenSizeUSD，所以必须在这里先刷新）
		if e.config.RiskPolicyVersion < 4 {
			if err := e.store.CopyTrade().MarkReentryUsed(e.traderID, mapping.LeaderPosID, markPrice, reentrySize); err != nil {
				logger.Errorf("❌ [%s] 标记 reentry_used 失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
				continue
			}
		} else {
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardReentryPending, entryRef, markPrice, currentATR)
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateReentryTriggered)
			stopFillPrice := float64(0)
			if stoppedAttempt != nil {
				stopFillPrice = stoppedAttempt.ExitPrice
			}
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: v4Cycle.ID, TraderID: e.traderID, Type: "REENTRY_REQUESTED", Price: markPrice, Notional: reentrySize, Metadata: map[string]interface{}{
				"reentry_no":           v4Cycle.ReentryCount + 1,
				"reentry_ratio":        e.config.RiskReentryRatio,
				"stopped_notional":     stoppedNotional,
				"stop_fill_price":      stopFillPrice,
				"anchor_price":         reentryAnchor,
				"leader_entry_live":    entryRef,
				"leader_entry_at_stop": v4Cycle.LeaderEntryAtStop,
				"current_atr":          currentATR,
				"min_recovery_atr":     e.config.RiskReentryMinRecoveryATR,
				"cooldown_seconds":     e.config.RiskReentryCooldownSeconds,
			}})
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
		if !e.emitReentryDecision(mapping, leaderPos, reentrySize, markPrice) && e.config.RiskPolicyVersion >= 4 {
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardStoppedWatching, entryRef, markPrice, currentATR)
		}
	}
}

// ============================================================================
// 观察期采样（v4.1）
//
// 目的：止损出局进入观察后，UpdateCopyGuardObservation 只覆盖最新价，没有
// 历史轨迹，也不知道每个时刻"为什么没有重入"。采样表记录价格/ATR/领航员
// 仓位/重入边界/追价上限/主导门控原因的时间序列，供复盘（出局后是挽回了
// 损失还是错过了行情）、参数调优（哪个 gate 挡住了本可盈利的重入）与离线
// 回测（重放不同参数）。
// ============================================================================

// 观察期未重入的主导门控原因（写入 copy_guard_watch_samples.gate）。
// 判定优先级从上到下：终态 > 冷却 > 波动扩张 > 追价超限 > 价格未回归 >
// 金额过小 > 全部通过（REENTRY_TRIGGERED）。
const (
	watchGateReentryDisabled  = "REENTRY_DISABLED"   // 重入功能未启用，观察至领航员平仓
	watchGateCooldown         = "COOLDOWN"           // 冷却期内（含逐次加严）
	watchGateATRExpansion     = "ATR_EXPANSION"      // 波动扩张超上限，市场环境已变
	watchGateChaseExceeded    = "CHASE_EXCEEDED"     // 价格超出追价上限（行情跑远）
	watchGatePriceNotReturned = "PRICE_NOT_RETURNED" // 价格尚未回归重入边界
	watchGateMinNotional      = "MIN_NOTIONAL"       // 重入金额低于最小阈值
	watchGateReentryTriggered = "REENTRY_TRIGGERED"  // 全部条件满足，已请求重入
	watchSampleInterval       = 60 * time.Second     // 固定间隔采样（gate 不变时）
	watchResumeGapMultiplier  = 5                    // 采样断档 > 间隔×该倍数 → 记 WATCH_RESUMED
)

// recordWatchSample 观察期采样：固定间隔必采 + 门控原因变化立即补采。
// 门控变化时写 REENTRY_GATE_CHANGED 事件（只在变化沿写入，事件流保持可读）；
// 采样断档远超间隔时写 WATCH_RESUMED（引擎重启/停摆标记）。
// 降噪基于表内最近一条采样判断，跨引擎重启依然成立。
func (e *Engine) recordWatchSample(cycle *store.CopyGuardCycle, leaderPos *Position, markPrice, atr, boundary, chaseLimit float64, gate string) {
	if cycle == nil || e.store == nil {
		return
	}
	leaderEntry, leaderSize := float64(0), float64(0)
	if leaderPos != nil {
		leaderEntry, leaderSize = leaderPos.EntryPrice, leaderPos.Size
	}
	last, err := e.store.CopyTrade().GetLatestCopyGuardWatchSample(cycle.ID)
	if err == nil && last != nil {
		gap := time.Since(last.CreatedAt)
		if last.Gate == gate && gap < watchSampleInterval {
			return
		}
		if gap > watchSampleInterval*watchResumeGapMultiplier {
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
				CycleID: cycle.ID, TraderID: e.traderID, Type: "WATCH_RESUMED", Price: markPrice,
				Metadata: map[string]interface{}{"gap_seconds": gap.Seconds(), "last_gate": last.Gate},
			})
		}
		if last.Gate != gate {
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
				CycleID: cycle.ID, TraderID: e.traderID, Type: "REENTRY_GATE_CHANGED", Price: markPrice,
				Metadata: map[string]interface{}{
					"from": last.Gate, "to": gate, "atr": atr,
					"reentry_boundary": boundary, "chase_limit": chaseLimit,
					"leader_size": leaderSize, "reentry_count": cycle.ReentryCount,
				},
			})
		}
	}
	_ = e.store.CopyTrade().SaveCopyGuardWatchSample(&store.CopyGuardWatchSample{
		CycleID: cycle.ID, TraderID: e.traderID, MarkPrice: markPrice, ATR: atr,
		LeaderEntryPrice: leaderEntry, LeaderSize: leaderSize,
		ReentryBoundary: boundary, ChaseLimit: chaseLimit, Gate: gate,
	})
}

// emitWatchSummary 周期关闭（领航员平仓/反手）时汇总观察期数据 → WATCH_SUMMARY 事件。
// 只对触发过止损的周期有意义（stop_count=0 直接跳过）。
//
// 关键结论字段：
//   - price_saved_usd：以价格口径回答"出局后是挽回了损失还是错过了行情"——
//     对每段被止损的 attempt 按 (止损成交价 vs 领航员最终平仓价) × 数量求和，
//     >0 = 止损帮忙少亏（价格继续恶化），<0 = 错过（价格后来恢复）。
//     与 net_guard_effect（含费用/滑点的记账口径）互补。
//   - max_favorable/adverse_excursion：观察期内价格相对止损价的最大有利/不利
//     偏移（有利 = 朝原持仓方向恢复），衡量观察期错过的最大幅度。
//   - first_recovery_seconds：止损后价格首次触及重入边界的耗时（-1 = 从未）。
//   - blocked_when_recovered：价格已回归边界但被其他 gate 挡住的采样计数
//     （按 gate 分组）——直接指出哪个参数太紧。
//   - gate_seconds：各 gate 占用的观察时长（按相邻采样间隔近似）。
//   - leader_addons/reductions：观察期领航员加/减仓次数（按相邻采样 size 变化 >1%）。
func emitWatchSummary(cs *store.CopyTradeStore, traderID string, cycle *store.CopyGuardCycle, closePrice float64) {
	if cs == nil || cycle == nil || cycle.StopCount == 0 {
		return
	}
	long := cycle.Side == "long"
	attempts, _ := cs.ListCopyGuardAttempts(cycle.ID)
	samples, _ := cs.ListCopyGuardWatchSamples(cycle.ID)

	// 价格口径的挽回/错过：Σ (止损成交价 − 领航员平仓价) × 数量（多单；空单取反）
	priceSaved, lastStopPrice := float64(0), float64(0)
	for _, a := range attempts {
		if a.Status != "STOPPED" || a.ExitPrice <= 0 {
			continue
		}
		lastStopPrice = a.ExitPrice
		if closePrice > 0 && a.Quantity > 0 {
			if long {
				priceSaved += (a.ExitPrice - closePrice) * a.Quantity
			} else {
				priceSaved += (closePrice - a.ExitPrice) * a.Quantity
			}
		}
	}

	// 观察期价格轨迹统计（相对最后一次止损成交价；有利 = 朝原持仓方向恢复）
	var maxFavorable, maxAdverse float64
	firstRecoverySec := float64(-1)
	blockedWhenRecovered := map[string]int{}
	gateSeconds := map[string]float64{}
	leaderAddons, leaderReductions := 0, 0
	var prev *store.CopyGuardWatchSample
	for _, w := range samples {
		if lastStopPrice > 0 && w.MarkPrice > 0 {
			diff := w.MarkPrice - lastStopPrice
			if !long {
				diff = -diff
			}
			if diff > maxFavorable {
				maxFavorable = diff
			}
			if -diff > maxAdverse {
				maxAdverse = -diff
			}
		}
		recovered := w.ReentryBoundary > 0 && w.MarkPrice > 0 &&
			((long && w.MarkPrice >= w.ReentryBoundary) || (!long && w.MarkPrice <= w.ReentryBoundary))
		if recovered {
			if firstRecoverySec < 0 && cycle.StoppedAt != nil {
				firstRecoverySec = w.CreatedAt.Sub(*cycle.StoppedAt).Seconds()
			}
			if w.Gate != watchGateReentryTriggered && w.Gate != watchGatePriceNotReturned {
				blockedWhenRecovered[w.Gate]++
			}
		}
		if prev != nil {
			if dt := w.CreatedAt.Sub(prev.CreatedAt).Seconds(); dt > 0 {
				gateSeconds[prev.Gate] += dt
			}
			if prev.LeaderSize > 0 && w.LeaderSize > prev.LeaderSize*1.01 {
				leaderAddons++
			}
			if prev.LeaderSize > 0 && w.LeaderSize < prev.LeaderSize*0.99 {
				leaderReductions++
			}
		}
		prev = w
	}

	watchSeconds := float64(0)
	if cycle.StoppedAt != nil {
		watchSeconds = time.Since(*cycle.StoppedAt).Seconds()
	}
	meta := map[string]interface{}{
		"stop_count":              cycle.StopCount,
		"reentry_count":           cycle.ReentryCount,
		"last_stop_price":         lastStopPrice,
		"leader_close_price":      closePrice,
		"price_saved_usd":         priceSaved,
		"watch_seconds":           watchSeconds,
		"sample_count":            len(samples),
		"max_favorable_excursion": maxFavorable,
		"max_adverse_excursion":   maxAdverse,
		"first_recovery_seconds":  firstRecoverySec,
	}
	if len(blockedWhenRecovered) > 0 {
		meta["blocked_when_recovered"] = blockedWhenRecovered
	}
	if len(gateSeconds) > 0 {
		meta["gate_seconds"] = gateSeconds
	}
	if leaderAddons+leaderReductions > 0 {
		meta["leader_addons"] = leaderAddons
		meta["leader_reductions"] = leaderReductions
	}
	_ = cs.SaveCopyGuardEvent(&store.CopyGuardEvent{
		CycleID: cycle.ID, TraderID: traderID, Type: "WATCH_SUMMARY",
		Price: closePrice, PnL: priceSaved, Metadata: meta,
	})
}

// emitReentryDecision 构造并推送一个二次进场的 Open 决策
func (e *Engine) emitReentryDecision(mapping *store.CopyTradePositionMapping, leaderPos *Position, copySize, entryPrice float64) bool {
	if leaderPos == nil {
		return false
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
		return true
	default:
		logger.Warnf("⚠️ [%s] 决策通道已满，二次进场决策被丢弃 | posId=%s", e.traderID, mapping.LeaderPosID)
		return false
	}
}
