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
// 账户保护止损兜底（Copy Guard v5）
//
// 设计哲学：跟单平时 100% 跟随领航员，不主动止盈/分批/干预；
// 仅在「价格反向走到风险线」时由交易所托管的 algo 条件单兜底平仓。
//
// 算法（calcStopLossPrice → ComputeRiskDistanceV4）：各项纯取严（min）
//   - ATR 基线：噪音参考线（默认 RiskATRMultiplier=2.0 × ATR_1h_14），不再放宽硬 cap
//   - 仓位保证金止损（margin cap）：仅 RiskLeverageFallback 开启时参与（默认关，
//     开启时默认 20% 保证金）
//   - 风险预算：单次尝试默认最多亏账户的 RiskAccountPct（v7 默认 2%）
//
// 由 SupportsCopyGuard 的数据源（OKX / Binance 领航员）在 v4+ 配置下调用；
// Hyperliquid 完全不走这里。保护单始终挂在跟随者执行交易所（须支持交易所托管
// 条件单，启动时由 validateV4ExecutorCapabilities 校验）。v3 旧三层算法已于 v5 下线。
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
	PriceTickSize    float64  // 执行交易所精确价格步长；0 时仅兼容旧 OKX 路径
	BaseQuantityStep float64  // 执行交易所精确基础币数量步长
}

// StopLossCalcResult 止损价计算结果（含完整决策追踪，便于日志和调试）
type StopLossCalcResult struct {
	SLPrice          float64 // 最终 SL 价格（tickSz 对齐后）
	SLDistance       float64 // 最终 SL 距离（|entry - SL|）
	ATRDistance      float64 // ATR 基线对应距离（fallback 时为等效近似）
	ATRValue         float64 // 实际 ATR 值（参考用）
	GovernedBy       string  // 哪条规则最终生效："atr" | "margin_cap" | "account_cap" | "clamp"
	TickSize         float64 // 实际使用的 tickSz（0 = fallback 到 1e-4）
	QuantityStep     float64 // lotSz * ctVal，以币数表示的最小仓位步长
	OpenImmediateHit bool    // 是否开仓即触发风险（SL 距离 < 0.1%）
	ExpectedLossUSD  float64 // 价格损失 + 配置的滑点缓冲
	ExpectedLossPct  float64 // ExpectedLossUSD / AccountEquityUSD（权益口径）
	// ExpectedMarginLossPct 保证金口径：预期止损损失 / 该仓位保证金
	ExpectedMarginLossPct float64
	// DistanceATRRatio 止损距离/原始 ATR 比值；< 0.5 表示止损落在正常噪音
	// 区内，预计高频止损（UI 易扫损提示 + 重入自适应加严的输入）
	DistanceATRRatio float64
	NoiseConflict    bool // 硬 cap 比 ATR 基线更紧（止损落在噪音区内）
	// LiquidationPriceIgnored: 交易所返回的强平价方向不合理（多单强平价高于
	// 入场价 / 空单低于入场价），已忽略强平价校验、按 ATR 止损继续挂单
	LiquidationPriceIgnored bool
	// Clamped: v5 可保护性状态机——正常止损价落入强平缓冲区内，已被 clamp
	// 到强平安全线上的极紧止损（触发概率高，但保护真实存在）
	Clamped bool
	// Unprotectable: clamp 后距离仍 < 0.1%（连极紧止损都挂不出），调用方
	// 必须走 GUARD_UNPROTECTABLE 处置（close/follow），禁止静默裸跑
	Unprotectable bool
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
	result.SLDistance = computed.Distance
	result.GovernedBy = computed.GovernedBy
	result.NoiseConflict = computed.NoiseConflict
	result.ExpectedLossUSD = computed.ExpectedLossUSD
	result.ExpectedLossPct = computed.ExpectedLossPct
	result.ExpectedMarginLossPct = computed.ExpectedMarginLossPct
	result.DistanceATRRatio = computed.DistanceATRRatio
	return finalizeStopLossPrice(input, result, cfg.RiskLiquidationBufferATR)
}

// ============================================================================
// 加仓账户风险预算（Copy Guard v7：超限自动缩量）
//
// 背景：volatility_priority 模式下 RiskAccountPct 只是软性提示，领航员连续
// 加仓时单笔预期止损损失可以增长（WLD 实盘：5.2% → 28.4%）。这里在跟随加仓
// 时预估「加仓后总敞口按当前止损距离全损」占账户权益的比例，超过
// RiskAddonBudgetPct 时记录 ADDON_RISK_WARNING 告警。
//
// v7 把费用与滑点纳入预算，超限时缩小本次加仓；不能安全达到交易所最小量
// 时拒绝，避免仅告警却继续穿透风险预算。
// ============================================================================

// addonBudgetEventInterval：同一仓位 ADDON_RISK_WARNING 事件/告警的最小间隔，
// 限频避免事件与日志刷屏。
const addonBudgetEventInterval = 60 * time.Second

// structuralInvalidationPrice uses only completed 5m/15m OKX mark candles.
// The most recent confirmed two-sided swing is preferred; unavailable market
// data safely degrades to the ATR-only plan.
func structuralInvalidationPrice(symbol string, side SideType, entryPrice float64) float64 {
	best, bestTime := float64(0), int64(0)
	for _, timeframe := range []string{"5m", "15m"} {
		klines, err := market.GetOKXCompletedMarkCandles(symbol, timeframe, 40)
		if err != nil || len(klines) < 5 {
			continue
		}
		for i := len(klines) - 3; i >= 2; i-- {
			k := klines[i]
			valid := false
			price := float64(0)
			if side == SideLong && k.Low < entryPrice && k.Low < klines[i-1].Low && k.Low <= klines[i-2].Low && k.Low < klines[i+1].Low && k.Low <= klines[i+2].Low {
				valid, price = true, k.Low
			}
			if side == SideShort && k.High > entryPrice && k.High > klines[i-1].High && k.High >= klines[i-2].High && k.High > klines[i+1].High && k.High >= klines[i+2].High {
				valid, price = true, k.High
			}
			if valid && k.OpenTime > bestTime {
				best, bestTime = price, k.OpenTime
			}
			if valid {
				break
			}
		}
	}
	return best
}

// limitAddonRiskBudget 检查本次加仓后的风险，超预算时缩量。Copy Guard
// 数据缺失时保守拒绝，避免旧逻辑“只告警仍然穿透预算”。
func (e *Engine) limitAddonRiskBudget(signal *TradeSignal, posID string, copySize float64) float64 {
	cfg := e.config
	// 显式 v4+ 门槛：加仓预算是 Copy Guard v4 特性。version<4 的存量配置
	// 目前也进不来（budget 默认 0 + 无 open cycle 双重隐式短路），此检查
	// 把门控从"依赖下游数据缺失"改为与 shouldManageStopLoss 等一致的
	// 显式判定，防止未来重构 cycle 创建时机时悄悄漏风。
	if cfg == nil || !SupportsCopyGuard(cfg.ProviderType) || cfg.RiskPolicyVersion < 4 || !cfg.RiskStopLossEnabled {
		return copySize
	}
	budget := cfg.RiskAddonBudgetPct
	if budget <= 0 || budget >= 1 {
		return copySize
	}
	if e.store == nil || e.getFollowerEquity == nil || signal == nil || signal.Fill == nil || copySize <= 0 {
		return 0
	}
	cycle, err := e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, posID)
	if err != nil || cycle == nil {
		return 0
	}
	equity := e.getFollowerEquity()
	entryPrice := signal.Fill.Price
	if equity <= 0 || entryPrice <= 0 {
		return 0
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
		return 0
	}
	totalNotional := cycle.FollowerNotional + copySize
	computed, err := ComputeRiskDistanceV4(cfg, entryPrice, totalNotional, equity, atrDistance, e.getLeaderLeverage(signal))
	if err != nil {
		return 0
	}
	if computed.ExpectedLossPct <= budget {
		return copySize
	}
	lossRate := computed.ExpectedLossUSD / totalNotional
	allowed := equity*budget/lossRate - cycle.FollowerNotional
	if allowed < 0 {
		allowed = 0
	}
	if allowed > copySize {
		allowed = copySize
	}

	now := time.Now()
	if last, ok := e.lastAddonBudgetEvent[posID]; !ok || now.Sub(last) >= addonBudgetEventInterval {
		e.lastAddonBudgetEvent[posID] = now
		msg := fmt.Sprintf("加仓风险缩量：预期止损损失 %.1f%% 超预算 %.1f%%（请求 %.2f，允许 %.2f）",
			computed.ExpectedLossPct*100, budget*100, copySize, allowed)
		logger.Warnf("🚧 [%s] %s | %s posId=%s", e.traderID, msg, signal.Fill.Symbol, posID)
		e.logWarning(Warning{
			Timestamp:    now,
			Symbol:       signal.Fill.Symbol,
			Type:         "addon_risk_shrunk",
			Message:      msg,
			SignalAction: string(ActionAdd),
			SignalValue:  copySize,
			CopyValue:    allowed,
			Executed:     allowed > 0,
		})
		if err := e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{
			CycleID:  cycle.ID,
			TraderID: e.traderID,
			Type:     "ADDON_RISK_SHRUNK",
			Price:    entryPrice,
			Notional: copySize,
			Metadata: map[string]interface{}{
				"expected_loss_pct": computed.ExpectedLossPct,
				"budget_pct":        budget,
				"current_notional":  cycle.FollowerNotional,
				"addon_notional":    copySize,
				"allowed_notional":  allowed,
				"governed_by":       computed.GovernedBy,
				"blocked":           allowed <= 0,
			},
		}); err != nil {
			logger.Warnf("⚠️ [%s] ADDON_RISK_SHRUNK 事件写入失败: %v", e.traderID, err)
		}
	}
	return allowed
}

func (e *Engine) limitAIGuardedTradeRisk(signal *TradeSignal, posID string, action ActionType, requested float64) float64 {
	if e.config == nil || e.config.RiskReentryDecisionMode != "ai_guarded" || !e.config.RiskStopLossEnabled || signal == nil || signal.Fill == nil || requested <= 0 || e.getFollowerEquity == nil {
		return requested
	}
	entryPrice, equity := signal.Fill.Price, e.getFollowerEquity()
	if entryPrice <= 0 || equity <= 0 {
		return 0
	}
	atr, err := market.GetOKXATRWithMaxAge(signal.Fill.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
	if err != nil || atr <= 0 {
		atr = entryPrice * e.config.RiskATRFallbackPct
	}
	if atr <= 0 {
		return 0
	}
	currentNotional := float64(0)
	var cycle *store.CopyGuardCycle
	cycleID := int64(0)
	if action == ActionAdd {
		cycle, _ = e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, posID)
		if cycle == nil {
			return 0
		}
		cycleID = cycle.ID
		currentNotional = cycle.FollowerNotional
	}
	usage, err := e.store.ReentryAI().GetCopyGuardRiskUsageExcludingAttempt(e.traderID, cycleID, 0)
	if err != nil {
		return 0
	}
	availableRisk, err := AvailableCopyGuardRiskUSD(e.config, equity, usage)
	if err != nil {
		return 0
	}
	side := signal.Fill.PositionSide
	structure := structuralInvalidationPrice(signal.Fill.Symbol, side, entryPrice)
	plan, err := BuildProtectionPlan(e.config, side, entryPrice, atr, structure, equity, availableRisk/equity, currentNotional+requested)
	if err != nil {
		logger.Warnf("[CopyGuard] trader=%s event=ENTRY_RISK_REJECTED symbol=%s reason=%v", e.traderID, signal.Fill.Symbol, err)
		return 0
	}
	allowed := plan.MaxNotional - currentNotional
	if allowed < 0 {
		allowed = 0
	}
	if allowed > requested {
		allowed = requested
	}
	if allowed+1e-9 < requested {
		logger.Warnf("[CopyGuard] trader=%s event=ENTRY_RISK_SHRUNK symbol=%s requested=%.2f allowed=%.2f stop=%.8f", e.traderID, signal.Fill.Symbol, requested, allowed, plan.StopPrice)
		if cycle != nil {
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: e.traderID, Type: "ADDON_RISK_SHRUNK", Price: entryPrice, Notional: requested, Metadata: map[string]interface{}{"allowed_notional": allowed, "stop_price": plan.StopPrice, "stop_distance": plan.StopDistance, "structure_invalidation": plan.StructureInvalidation, "expected_loss_usd": plan.ExpectedLossUSD, "risk_budget": plan.MaxRiskUSD}})
		}
	}
	return allowed
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

// liquidationSafetyBuffer v5 强平安全缓冲：max(2×tickSize, min(bufferATR×ATR, 0.15%×entry))。
// v4.1 的纯 0.5×ATR 缓冲在高杠杆下会吃掉全部可用空间（周期 65/66 保护单被拒
// 的"拒单放大器"），0.15% 价格封顶保证缓冲永远不会大到让止损挂不出去。
// ATR 不可得（fallback 距离）时直接用 0.15% 封顶值。
func liquidationSafetyBuffer(tickSize, atrValue, entryPrice, bufferATR float64) float64 {
	if bufferATR <= 0 {
		bufferATR = 0.25
	}
	buffer := entryPrice * 0.0015
	if atrValue > 0 {
		if atrComponent := atrValue * bufferATR; atrComponent < buffer {
			buffer = atrComponent
		}
	}
	if minBuffer := 2 * tickSize; buffer < minBuffer {
		buffer = minBuffer
	}
	return buffer
}

// finalizeStopLossPrice 把止损距离落成可挂单的触发价：tickSz 对齐 + 强平安全
// 校验。v5 可保护性状态机的入口段：
//   - 正常：SLPrice 高于（多）/低于（空）强平安全线 → 直接挂单
//   - clamp：正常价落入强平缓冲区 → 钳到强平安全线上的极紧止损（Clamped=true，
//     调用方记 PROTECTION_CLAMPED + 告警），保护真实存在
//   - 不可保护：clamp 后距离 < 0.1%（连极紧止损都无意义）→ Unprotectable=true，
//     调用方必须走 GUARD_UNPROTECTABLE 处置，禁止静默裸跑
func finalizeStopLossPrice(input *StopLossCalcInput, result *StopLossCalcResult, liquidationBufferATR float64) (*StopLossCalcResult, error) {
	if result.SLDistance/input.EntryPrice < 0.001 {
		result.OpenImmediateHit = true
		return result, nil
	}
	tickSz := input.PriceTickSize
	if tickSz > 0 && input.BaseQuantityStep > 0 {
		result.QuantityStep = input.BaseQuantityStep
	} else {
		// Backward-compatible path for existing OKX-only calculations and unit
		// tests. Production AutoTrader integrations inject the resolved target
		// venue specification before a protective order is placed.
		spec, specErr := getOKXInstrumentSpec(input.Symbol)
		tickSz = spec.tickSz
		if specErr != nil {
			logger.Warnf("⚠️ 获取 %s 合约规格失败（止损价不做档位对齐）: %v", input.Symbol, specErr)
		} else {
			result.QuantityStep = spec.lotSz * spec.ctVal
		}
	}
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
	if input.LiquidationPrice > 0 {
		if !isLiquidationPriceDirectionValid(input.Side, input.EntryPrice, input.LiquidationPrice) {
			// Direction-implausible liquidation price: ignore it and keep the
			// ATR stop; the caller records a diagnostic event.
			result.LiquidationPriceIgnored = true
			return result, nil
		}
		buffer := liquidationSafetyBuffer(tickSz, result.ATRValue, input.EntryPrice, liquidationBufferATR)
		needClamp := (input.Side == SideLong && result.SLPrice <= input.LiquidationPrice+buffer) ||
			(input.Side == SideShort && result.SLPrice >= input.LiquidationPrice-buffer)
		if needClamp {
			var clamped float64
			if input.Side == SideLong {
				// 多单：钳到安全线上方（向上取整保证不落回缓冲区）
				clamped = alignToTickSize(input.LiquidationPrice+buffer, tickSz, false)
			} else {
				clamped = alignToTickSize(input.LiquidationPrice-buffer, tickSz, true)
			}
			dist := math.Abs(input.EntryPrice - clamped)
			validSide := (input.Side == SideLong && clamped > 0 && clamped < input.EntryPrice) ||
				(input.Side == SideShort && clamped > input.EntryPrice)
			if !validSide || dist/input.EntryPrice < 0.001 {
				result.SLPrice = 0
				result.Unprotectable = true
				return result, nil
			}
			result.SLPrice = clamped
			result.SLDistance = dist
			result.GovernedBy = "clamp"
			result.Clamped = true
			// 极紧止损的实际损失小于原预估（预估字段保留原值即偏保守），但
			// 易扫损比值必须反映真实距离
			if result.ATRValue > 0 {
				result.DistanceATRRatio = dist / result.ATRValue
			}
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
// 由 SupportsCopyGuard 的数据源（OKX / Binance）在 v4+ 配置下生效；Hyperliquid 不走
func (e *Engine) checkStoppedByRisk() {
	if e.store == nil || e.config == nil {
		return
	}
	if !SupportsCopyGuard(e.config.ProviderType) {
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
			// 跟随期也保持 last_observed_price 新鲜。周期若在跟随期直接
			// 结束（止损与领航员平仓同轮发生），估算基线才不会用到开仓时的
			// 陈旧价格（HMSTR 周期 31 基线被错记为 0 的根因）。
			if lp := leaderPosMap[mapping.LeaderPosID]; lp != nil && lp.MarkPrice > 0 {
				_ = e.store.CopyTrade().UpdateCopyGuardObservedPrice(e.traderID, mapping.LeaderPosID, lp.MarkPrice)
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

		// The lifecycle ledger must commit before the mapping leaves active.
		// Previously MarkStoppedByRisk ran first and Record... errors were ignored;
		// one DB failure permanently removed the mapping from this retry loop while
		// leaving the cycle falsely FOLLOWING. Keeping it active on failure makes
		// the next confirmed poll retry the same idempotent transition.
		cycle, cerr := e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, mapping.LeaderPosID)
		if cerr != nil {
			logger.Errorf("❌ [%s] 仓位已消失但 Copy Guard 周期不可用: %v | posId=%s", e.traderID, cerr, mapping.LeaderPosID)
			continue
		}
		atr, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
		// 统计口径修正（v5）：领航员浮亏是参考信息，写 metadata；事件
		// pnl 字段留给跟随者自身盈亏（此路径无法得知，由 attempt 对账补）
		if recordErr := e.store.CopyTrade().RecordCopyGuardStopObserved(cycle.ID, e.traderID, cycle.ReentryCount, atr, leaderPos.MarkPrice, leaderSize, map[string]interface{}{"confirmation": "position_absent_fallback", "leader_unrealized_pnl": leaderPnL}); recordErr != nil {
			logger.Errorf("[CopyGuard] trader=%s cycle=%d attempt=%d event=STOP_PERSIST_FAILED reason=%v", e.traderID, cycle.ID, cycle.ReentryCount, recordErr)
			continue
		}
		if err := e.store.CopyTrade().MarkStoppedByRisk(e.traderID, mapping.LeaderPosID, leaderPnL, leaderSize, addCount); err != nil {
			logger.Errorf("❌ [%s] 标记 stopped_by_risk 失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
			continue
		}
		// 快照止损时的领航员均价，供重入保守锚点使用
		_ = e.store.CopyTrade().SnapshotCopyGuardLeaderEntryAtStop(cycle.ID, leaderPos.EntryPrice)

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
// 二次进场监控（Copy Guard v5 确认式重入）
//
// 流程：STOPPED_WATCHING → 既有门控全部通过（冷却/恢复幅度/锚点带宽/追价/
// ATR 扩张）→ REENTRY_CANDIDATE（内存态：连续 N 个轮询 tick 条件持续满足，
// 中途破坏即清零重来）→ 可保护性预检（预算重入后连极紧止损都挂不出 → 不重入）
// → 重入下单（size = 被止损名义 × ratio，多次重入名义几何衰减，累计亏损有界）。
//
// 自适应加严（按止损时 distance/ATR 比值，v5）：
//   - < 0.5：谨慎档——确认 tick 数翻倍、最小恢复幅度 ×1.5
//   - < 0.3：默认禁用自动重入（gate=REENTRY_DISABLED_NOISE）；
//     RiskReentryNoiseOverride=true 时强制放行（按谨慎档执行）
//
// v3 判据（ReentryUsed 硬阀/反加仓铁律/价格容差/浮亏收窄）与 v4.1 周期熔断
// 已于 v5 下线。
// ============================================================================

// reentryConfirmTicksBase 连续确认基础 tick 数（poll 周期 3s → 约 9s 持续满足）
const reentryConfirmTicksBase = 3

// 噪音自适应阈值：止损时 distance/ATR 低于 cautious 进谨慎档，低于 disable
// 默认禁用自动重入（该配置天然易扫损，自动重入长期大概率磨损）
const (
	reentryNoiseCautiousRatio = 0.5
	reentryNoiseDisableRatio  = 0.3
)

// checkReentryConditions 检查所有 stopped_by_risk 映射是否满足二次进场条件
// 满足时通过 e.decisionCh 推一个 Open 决策出去
func (e *Engine) checkReentryConditions() {
	if e.store == nil || e.config == nil {
		return
	}
	if !SupportsCopyGuard(e.config.ProviderType) {
		return
	}
	if e.reentryCandidateTicks == nil {
		e.reentryCandidateTicks = make(map[string]int)
	}

	stoppedMappings, err := e.store.CopyTrade().ListStoppedByRiskMappings(e.traderID)
	if err != nil {
		logger.Warnf("⚠️ [%s] 拉 stopped_by_risk 映射失败（二次进场跳过）: %v", e.traderID, err)
		return
	}
	// 清理已离开观察态的 posId 的确认计数（防止 map 无界增长 / 陈旧计数复用）
	watching := make(map[string]bool, len(stoppedMappings))
	for _, m := range stoppedMappings {
		watching[m.LeaderPosID] = true
	}
	for posID := range e.reentryCandidateTicks {
		if !watching[posID] {
			delete(e.reentryCandidateTicks, posID)
		}
	}
	if len(stoppedMappings) == 0 {
		return
	}

	leaderPosMap := e.buildLeaderPosMap()
	followerEquity := e.getFollowerEquity()

	for _, mapping := range stoppedMappings {
		terminalWatchStatus := ""
		v4Cycle, err := e.store.CopyTrade().GetOpenCopyGuardCycle(e.traderID, mapping.LeaderPosID)
		if err != nil {
			logger.Debugf("[%s] Copy Guard 生命周期不存在: %v", e.traderID, err)
			continue
		}
		if v4Cycle.Status == store.CopyGuardReentryPending {
			continue
		}
		// 回填开仓时因 API 限流缺失的权益快照（account_equity=0 会让
		// 账户级保护判定失效）
		if v4Cycle.AccountEquity <= 0 && followerEquity > 0 {
			if e.store.CopyTrade().BackfillCopyGuardAccountEquity(v4Cycle.ID, followerEquity) == nil {
				v4Cycle.AccountEquity = followerEquity
			}
		}
		if e.config.RiskReentryEnabled && v4Cycle.ReentryCount >= e.config.RiskMaxReentries {
			terminalWatchStatus = store.CopyGuardAttemptsExhausted
		}
		// 窗口空集时自动路径已把周期持久化为 ATTEMPTS_EXHAUSTED（见
		// checkReentryConditions A 层）。此处让持久化终态回灌内存态，否则下一
		// tick 内存不认、又重新进入自动观察——自动重入次数虽未达上限，但可行
		// 窗口已不存在=实质用尽，须保持终态呈现并短路后续门控。
		if e.config.RiskReentryEnabled && v4Cycle.Status == store.CopyGuardAttemptsExhausted {
			terminalWatchStatus = store.CopyGuardAttemptsExhausted
		}
		if e.config.RiskWatchTimeoutMinutes > 0 && v4Cycle.StoppedAt != nil && time.Since(*v4Cycle.StoppedAt) > time.Duration(e.config.RiskWatchTimeoutMinutes)*time.Minute {
			terminalWatchStatus = store.CopyGuardWatchTimeout
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
		coolingDown := v4Cycle.StoppedAt != nil && time.Since(*v4Cycle.StoppedAt) < cooldown

		// 条件 1: 领航员仍持有该 posId
		leaderPos := leaderPosMap[mapping.LeaderPosID]
		if leaderPos == nil || leaderPos.Size <= 0 {
			// 领航员完全平掉了 → 在 checkIgnoredPositionsClosed 里会标 closed
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}
		// 观察期领航员反手（OKX net 模式同 posId 换向）：本周期不可能再重入，
		// 直接以 LEADER_REVERSED 闭合，否则周期与 mapping 会永远停在观察态。
		if leaderPos.Side != "" && string(leaderPos.Side) != mapping.Side {
			// 观察期以反手价（当前标记价）作为"领航员离场价"汇总观察期数据
			emitWatchSummary(e.store.CopyTrade(), e.traderID, v4Cycle, leaderPos.MarkPrice)
			_ = e.store.CopyTrade().CloseCopyGuardCycle(v4Cycle.ID, store.CopyGuardLeaderReversed, v4Cycle.ActualPnL, v4Cycle.BaselinePnL, v4Cycle.Fees, v4Cycle.FundingFee, v4Cycle.LiquidationPenalty, v4Cycle.Slippage)
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: v4Cycle.ID, TraderID: e.traderID, Type: "LEADER_REVERSED", Price: leaderPos.MarkPrice, Metadata: map[string]interface{}{"old_side": mapping.Side, "new_side": string(leaderPos.Side), "phase": "watch"}})
			if err := e.store.CopyTrade().MarkStoppedByRiskAsClosed(e.traderID, mapping.LeaderPosID); err != nil {
				logger.Warnf("⚠️ [%s] 观察期反手后关闭 mapping 失败: %v | posId=%s", e.traderID, err, mapping.LeaderPosID)
			}
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			logger.Infof("🔁 [%s] 观察期领航员反手，周期闭合 | cycle=%d posId=%s %s %s→%s", e.traderID, v4Cycle.ID, mapping.LeaderPosID, mapping.Symbol, mapping.Side, string(leaderPos.Side))
			continue
		}
		// v5 噪音档判定：止损时的实际止损距离 / 当时 ATR。数据缺失（旧周期
		// 无 attempt 或 ATR 快照）时按正常档处理。
		stoppedAttempt := e.findStoppedAttempt(v4Cycle)
		noiseRatio := stopDistanceATRRatio(v4Cycle, stoppedAttempt)
		noiseDisabled := noiseRatio > 0 && noiseRatio < reentryNoiseDisableRatio && !e.config.RiskReentryNoiseOverride
		cautious := noiseRatio > 0 && noiseRatio < reentryNoiseCautiousRatio

		// vNext AI 模式完全绕开旧价格带自动执行：规则只负责生成安全候选，
		// 是否值得重入交给持久化 AI 调度器。AI 失败时不会回退到下方旧规则。
		if e.config.RiskReentryDecisionMode == "ai_guarded" {
			e.handleAIGuardedReentry(mapping, leaderPos, v4Cycle, stoppedAttempt, followerEquity, coolingDown, terminalWatchStatus)
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}
		if e.config.RiskReentryDecisionMode == "disabled" {
			terminalWatchStatus = store.CopyGuardAttemptsExhausted
		}

		// v7 retires per-order human approval（v5.1 manualMode 死链已随 L1
		// 清理删除）：exhausted attempts never create a manual execution
		// signal. Existing PENDING rows are migrated to durable AI candidates
		// at startup and the old confirm endpoint returns 410.
		watchStatus := store.CopyGuardStoppedWatching

		if !e.config.RiskReentryEnabled || terminalWatchStatus != "" || noiseDisabled {
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
			// 采样：终态观察（次数用尽/超时）、重入未启用或噪音档禁入，
			// gate 直接用状态名
			gate := watchGateReentryDisabled
			switch {
			case terminalWatchStatus != "":
				gate = terminalWatchStatus
			case noiseDisabled:
				gate = watchGateDisabledNoise
			}
			e.recordWatchSample(v4Cycle, leaderPos, mark, atr, 0, 0, gate)
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}

		// 条件 2: 价格回归（确认式）——入场基准取领航员实时均价
		entryRef := leaderPos.EntryPrice
		if entryRef <= 0 {
			continue
		}
		// v4.1 保守锚点：领航员止损后加仓摊均价会把实时均价拖向不利方向
		// （多单变低、空单变高），使重入边界在没有真实恢复时就被穿越。
		// 锚点取 max/min(实时均价, 止损时快照)，保证重入门槛不因领航员
		// 摊均价而变松；快照缺失（旧数据）时退回实时均价。
		reentryAnchor := entryRef
		if v4Cycle.LeaderEntryAtStop > 0 {
			if mapping.Side == string(SideLong) {
				reentryAnchor = math.Max(entryRef, v4Cycle.LeaderEntryAtStop)
			} else {
				reentryAnchor = math.Min(entryRef, v4Cycle.LeaderEntryAtStop)
			}
		}
		markPrice := leaderPos.MarkPrice
		if markPrice <= 0 {
			markPrice = leaderPos.EntryPrice
		}
		if markPrice <= 0 {
			continue // 拿不到当前价，无法判断
		}
		// own-path 口径：同上，影子名义按自身仓位名义记账
		shadow := v4Cycle.FollowerNotional
		if shadow <= 0 {
			shadow = v4Cycle.BaselineNotional
		}
		_ = e.store.CopyTrade().UpdateCopyGuardShadow(v4Cycle.ID, entryRef, markPrice, shadow, leaderPos.Size)
		if coolingDown {
			atr, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, watchStatus, entryRef, markPrice, atr)
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, atr, 0, 0, watchGateCooldown)
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}

		currentATR, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
		if currentATR <= 0 {
			currentATR = entryRef * e.config.RiskATRFallbackPct
		}
		if v4Cycle.ATRAtStop > 0 && currentATR > v4Cycle.ATRAtStop*e.config.RiskReentryMaxATRExpansion {
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, watchStatus, entryRef, markPrice, currentATR)
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, 0, 0, watchGateATRExpansion)
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}
		// 重入边界（价格判据）：
		//   - 锚点带宽：锚点 ∓ band × ATR（多单减、空单加）
		//   - v4.1 最小恢复幅度：止损成交价 ± minRecovery × ATR（第 N 次重入
		//     按倍率^N 加严；v5 谨慎档再 ×1.5），取两者更严
		//   - 追价上限：锚点 ± maxChase × ATR，超出即"行情跑远"
		// 止损成交价缺失（旧数据）时降级为纯带宽边界。
		stopPrice := float64(0)
		if stoppedAttempt != nil {
			stopPrice = stoppedAttempt.ExitPrice
		}
		reentryBoundary, chaseLimit, requiredRecovery := e.reentryObservationBounds(mapping.Side, reentryAnchor, currentATR, stopPrice, v4Cycle.ReentryCount, cautious)
		priceReturned := (mapping.Side == string(SideLong) && markPrice >= reentryBoundary) || (mapping.Side != string(SideLong) && markPrice <= reentryBoundary)
		beyondChase := (mapping.Side == string(SideLong) && markPrice > chaseLimit) || (mapping.Side != string(SideLong) && markPrice < chaseLimit)
		// A 层——自动重入窗口可行性不变量：恢复下界越过追价上限时可行区间为空集
		// （多单 下界>上界；空单 下界<上界；epsilon 相对容差避免裸浮点误判）。
		// 自动路径下窗口空集意味着自动重入永远打不出，判定为"实质用尽"→ 持久化
		// ATTEMPTS_EXHAUSTED 关闭旧规则自动窗口；v7 不再转人工确认。
		// leader 平仓/反手、冷却期（chaseLimit=0）、noiseDisabled 均已在前面提前
		// continue；到此必为"应继续自动观察但可行区间不存在"。
		windowInfeasible := reentryWindowInfeasible(mapping.Side, reentryBoundary, chaseLimit, reentryAnchor)
		if windowInfeasible {
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardAttemptsExhausted, entryRef, markPrice, currentATR)
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateReentryWindowInfeasible)
			_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: v4Cycle.ID, TraderID: e.traderID, Type: "REENTRY_WINDOW_COLLAPSED", Price: markPrice, Metadata: map[string]interface{}{
				"reentry_boundary":  reentryBoundary,
				"chase_limit":       chaseLimit,
				"required_recovery": requiredRecovery,
				"reentry_count":     v4Cycle.ReentryCount,
				"stop_count":        v4Cycle.StopCount,
				"atr":               currentATR,
				"mark_price":        markPrice,
			}})
			logger.Warnf("🚫 [%s] legacy 自动重入窗口塌缩为空集（下界=%.4f 上界=%.4f），本周期不再自动重入 | cycle=%d %s %s",
				e.traderID, reentryBoundary, chaseLimit, v4Cycle.ID, mapping.Symbol, mapping.Side)
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}
		conditionsMet := priceReturned && !beyondChase
		// 首轮观察（尚无上一 tick 价格）只记录观测、不计确认：重启后的第一
		// 个 tick 状态不完整，直接计数会把重启前的行情当作已确认。
		if v4Cycle.LastObservedPrice <= 0 {
			conditionsMet = false
		}
		_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, watchStatus, entryRef, markPrice, currentATR)
		if !conditionsMet {
			// 条件破坏 → 确认计数清零重来（v5 确认式重入的核心：单 tick
			// 触碰边界不再直接重入，杜绝"止损 @1773 → 单 tick 回归重入
			// @1779 → 再止损 @1766"的 whipsaw）
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			gate := watchGatePriceNotReturned
			if beyondChase {
				gate = watchGateChaseExceeded
			}
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, gate)
			continue
		}

		// REENTRY_CANDIDATE：条件满足但需连续 N 个 tick 确认（谨慎档翻倍）
		requiredTicks := reentryConfirmTicksBase
		if cautious {
			requiredTicks *= 2
		}
		e.reentryCandidateTicks[mapping.LeaderPosID]++
		if ticks := e.reentryCandidateTicks[mapping.LeaderPosID]; ticks < requiredTicks {
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateReentryCandidate)
			logger.Debugf("⏳ [%s] 重入候选确认中（%d/%d tick） | posId=%s %s", e.traderID, ticks, requiredTicks, mapping.LeaderPosID, mapping.Symbol)
			continue
		}

		// 计算重入仓位大小（USD）
		//
		// 以"被止损时自己的仓位名义价值"为基准（× 重入系数）。
		// 旧逻辑按领航员当前总仓位占比折算，当领航员在跟随期间加过仓（跟随者
		// 未等比例跟进）时，重入会远大于被止损的仓位——实盘出现过重入 8 倍于
		// 首仓、57 倍杠杆、止损价落入强平区导致保护单挂不上的事故（cycle 15）。
		// 被止损仓位基准让重入风险严格有界：多次重入名义按 ratio 几何衰减
		// （1 + 0.5 + 0.25… ≤ 2 倍首仓），周期累计亏损结构性有界——这也是
		// v4.1 周期熔断下线的前提。
		stoppedNotional := float64(0)
		if stoppedAttempt != nil {
			stoppedNotional = stoppedAttempt.Notional
		}
		if stoppedNotional <= 0 {
			// 旧数据/回填周期可能没有 attempt 名义，用周期跟随名义兜底
			stoppedNotional = v4Cycle.FollowerNotional
		}
		reentrySize := stoppedNotional * e.config.RiskReentryRatio
		// 最小金额阈值：低于交易所最小订单价值会导致下单失败 → 触发熔断；
		// 优先级用配置的 MinTradeWarn（与开仓金额最小阈值同一概念），未配置时统一兜底
		minReentry := minTradeNotionalOrDefault(e.config.MinTradeWarn)
		if reentrySize <= 0 {
			logger.Warnf("⚠️ [%s] 重入金额非正(%.4f)，跳过 | posId=%s", e.traderID, reentrySize, mapping.LeaderPosID)
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateMinNotional)
			delete(e.reentryCandidateTicks, mapping.LeaderPosID)
			continue
		}
		if reentrySize < minReentry {
			logger.Infof("⏭️ [%s] 重入金额 %.2f < 阈值 %.2f，跳过本次（条件保持，下轮再判） | posId=%s",
				e.traderID, reentrySize, minReentry, mapping.LeaderPosID)
			e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateMinNotional)
			continue
		}
		// v5 可保护性预检：预算"重入成交后按当前配置算出的止损距离"是否连
		// 极紧止损都挂不出（distance < 0.1% 即 OpenImmediateHit）。强平价在
		// 下单前不可知，这里检查的是保护单可行的必要条件；成交后的完整可行
		// 性（含强平缓冲/clamp）仍由 upsertV4Protection 状态机把关。
		equityForCheck := followerEquity
		if equityForCheck <= 0 {
			equityForCheck = v4Cycle.AccountEquity
		}
		if equityForCheck > 0 {
			atrBaseline := currentATR
			if e.config.RiskATRMultiplier > 0 {
				atrBaseline = currentATR * e.config.RiskATRMultiplier
			}
			precheck, precheckErr := ComputeRiskDistanceV4(e.config, markPrice, reentrySize, equityForCheck, atrBaseline, leaderPos.Leverage)
			if precheckErr != nil || precheck.Distance/markPrice < 0.001 {
				logger.Warnf("⚠️ [%s] 重入可保护性预检未通过（预算止损距离过近/计算失败），本轮不重入 | posId=%s err=%v", e.traderID, mapping.LeaderPosID, precheckErr)
				e.recordWatchSample(v4Cycle, leaderPos, markPrice, currentATR, reentryBoundary, chaseLimit, watchGateUnprotectable)
				delete(e.reentryCandidateTicks, mapping.LeaderPosID)
				continue
			}
		}

		// ============================================================
		// 连续确认完成 + 可保护性预检通过 → 触发重入
		// ============================================================
		logger.Infof("🔄 [%s] 二次进场触发（连续 %d tick 确认） | %s %s posId=%s | 价格=%.4f 边界=%.4f 锚点=%.4f",
			e.traderID, requiredTicks, mapping.Symbol, mapping.Side, mapping.LeaderPosID,
			markPrice, reentryBoundary, reentryAnchor)
		delete(e.reentryCandidateTicks, mapping.LeaderPosID)

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
			"confirm_ticks":        requiredTicks,
			"noise_ratio":          noiseRatio,
			"cautious_mode":        cautious,
		}})

		// 统计：重入也是一次"信号驱动"的决策
		e.stats.SignalsReceived++
		e.stats.SignalsFollowed++

		// 重要：清零该 posId 的 SL 疑似计数
		// 理由：emitReentryDecision 推决策到 channel 后，consumer 异步执行 ExecuteDecision 需要时间（1-3s）；
		// 这段窗口期内 mapping=active 但本地仍无仓位，会再次累计 suspect。
		// 清零后从 0 起算（再累 3 次需 9 秒），通常 ExecuteDecision 已完成 → 本地仓位出现 → suspect 自然清零。
		// 如果 ExecuteDecision 失败超过 9 秒，suspect 才会再次到阈值 → 重新标 stopped_by_risk
		// （周期已进 REENTRY_PENDING，恢复逻辑会兜底，不会重复重入）。
		delete(e.stopRiskSuspectCount, mapping.LeaderPosID)

		// 推一个 Open 决策出去
		if !e.emitReentryDecision(mapping, leaderPos, reentrySize, markPrice) {
			_ = e.store.CopyTrade().UpdateCopyGuardObservation(v4Cycle.ID, store.CopyGuardStoppedWatching, entryRef, markPrice, currentATR)
		}
	}
}

// handleAIGuardedReentry 把已完全止损的周期转换为 AI 候选。这里不调用模型、
// 不下单，只负责确定性数据快照、额度上限和可保护性预检。
func (e *Engine) handleAIGuardedReentry(mapping *store.CopyTradePositionMapping, leaderPos *Position, cycle *store.CopyGuardCycle, stoppedAttempt *store.CopyGuardAttempt, followerEquity float64, coolingDown bool, terminalStatus string) {
	if mapping == nil || leaderPos == nil || cycle == nil || e.store == nil {
		return
	}
	// The exchange has confirmed this attempt flat. Remove it from live
	// portfolio exposure, but retain the spent reservation against the cycle's
	// cumulative loss budget before evaluating any terminal/reentry branch.
	_ = e.store.ReentryAI().ConsumeCopyGuardRisk(cycle.ID, cycle.ReentryCount)
	if !e.config.RiskReentryEnabled {
		if c, err := e.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); err == nil {
			_ = e.store.ReentryAI().MarkReentryCandidateStatus(c.ID, store.ReentryCandidateInvalidated, "reentry disabled")
		}
		_ = e.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, store.CopyGuardAttemptsExhausted, leaderPos.EntryPrice, leaderPos.MarkPrice, 0)
		return
	}
	if terminalStatus != "" {
		status := store.ReentryCandidateInvalidated
		if terminalStatus == store.CopyGuardWatchTimeout {
			status = store.ReentryCandidateExpired
		}
		if c, err := e.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID); err == nil {
			_ = e.store.ReentryAI().MarkReentryCandidateStatus(c.ID, status, terminalStatus)
		}
		_ = e.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, terminalStatus, leaderPos.EntryPrice, leaderPos.MarkPrice, 0)
		return
	}
	mark := leaderPos.MarkPrice
	if mark <= 0 {
		mark = leaderPos.EntryPrice
	}
	if mark <= 0 {
		return
	}
	atr, _ := market.GetOKXATRWithMaxAge(mapping.Symbol, e.config.RiskATRTimeframe, e.config.RiskATRPeriod, riskATRCacheMaxAge(e.config))
	if atr <= 0 {
		atr = mark * e.config.RiskATRFallbackPct
	}
	if atr <= 0 {
		return
	}
	if cycle.BaselineLeaderSize <= 0 && leaderPos.Size > 0 {
		if err := e.store.CopyTrade().InitializeCopyGuardLeaderBaseline(cycle.ID, leaderPos.Size); err == nil {
			cycle.BaselineLeaderSize = leaderPos.Size
		}
	}
	stoppedNotional, lastStop, noiseRatio := cycle.FollowerNotional, float64(0), float64(0)
	if stoppedAttempt != nil {
		if stoppedAttempt.Notional > 0 {
			stoppedNotional = stoppedAttempt.Notional
		}
		lastStop = attemptStopFillPrice(stoppedAttempt)
		noiseRatio = stopDistanceATRRatio(cycle, stoppedAttempt)
	}
	reentryAnchor := leaderPos.EntryPrice
	if reentryAnchor <= 0 {
		reentryAnchor = mark
	}
	if cycle.LeaderEntryAtStop > 0 {
		if mapping.Side == string(SideLong) {
			reentryAnchor = math.Max(reentryAnchor, cycle.LeaderEntryAtStop)
		} else {
			reentryAnchor = math.Min(reentryAnchor, cycle.LeaderEntryAtStop)
		}
	}
	reentryBoundary, chaseLimit, _ := e.reentryObservationBounds(mapping.Side, reentryAnchor, atr, lastStop, cycle.ReentryCount, noiseRatio > 0 && noiseRatio < reentryNoiseCautiousRatio)
	nominalCap := stoppedNotional * e.config.RiskReentryRatio
	maxNotional := nominalCap
	equity := followerEquity
	if equity <= 0 {
		equity = cycle.AccountEquity
	}
	protectable := false
	if nominalCap > 0 && equity > 0 {
		usage, usageErr := e.store.ReentryAI().GetCopyGuardRiskUsageExcludingAttempt(e.traderID, cycle.ID, cycle.ReentryCount+1)
		availableRisk, capacityErr := AvailableCopyGuardRiskUSD(e.config, equity, usage)
		if usageErr == nil && capacityErr == nil {
			if sized, sizeErr := MaxNotionalForRiskDistance(e.config, equity, mark, atr*e.config.RiskATRMultiplier, availableRisk/equity, nominalCap); sizeErr == nil {
				maxNotional = sized
				p, err := ComputeRiskDistanceV4(e.config, mark, maxNotional, equity, atr*e.config.RiskATRMultiplier, leaderPos.Leverage)
				protectable = err == nil && p.Distance/mark >= 0.001 && p.Distance <= 4*atr
			}
		}
	}
	before, beforeErr := e.store.ReentryAI().GetReentryCandidateByCycle(cycle.ID)
	// Only a meaningful threshold crossing changes the hash. Every tick still
	// refreshes the snapshot, but it cannot wake the model merely because price
	// moved to another arbitrary bucket.
	priceBucket := math.Floor(mark / math.Max(atr*0.25, mark*0.0001))
	leaderBucket := math.Floor(leaderPos.Size / math.Max(cycle.BaselineLeaderSize*0.05, 1e-9))
	featureHash := fmt.Sprintf("initial|p%.0f|l%.0f|r%d|s%d", priceBucket, leaderBucket, cycle.ReentryCount, cycle.StopCount)
	pendingTrigger := "STOP_FLAT_CONFIRMED"
	if beforeErr == nil && before != nil {
		featureHash = before.FeatureHash
		pendingTrigger = before.PendingTrigger
		prevATR := before.ATR
		if prevATR <= 0 {
			prevATR = atr
		}
		inRange := func(price, low, high float64) bool { return low > 0 && high >= low && price >= low && price <= high }
		currentNearLeader := leaderPos.EntryPrice > 0 && math.Abs(mark-leaderPos.EntryPrice) <= 0.5*atr
		previousNearLeader := before.LeaderEntryPrice > 0 && math.Abs(before.TriggerPrice-before.LeaderEntryPrice) <= 0.5*prevATR
		currentRecovered, previousRecovered := false, false
		if lastStop > 0 {
			if mapping.Side == string(SideLong) {
				currentRecovered = mark >= lastStop+0.5*atr
				previousRecovered = before.TriggerPrice >= before.LastStopPrice+0.5*prevATR
			} else {
				currentRecovered = mark <= lastStop-0.5*atr
				previousRecovered = before.TriggerPrice <= before.LastStopPrice-0.5*prevATR
			}
		}
		leaderStep := math.Max(cycle.BaselineLeaderSize*0.05, 1e-9)
		prevLeaderBucket := math.Floor(before.LeaderSize / leaderStep)
		atrReference := cycle.ATRAtStop
		if atrReference <= 0 {
			atrReference = before.ATR
		}
		atrStep := math.Max(atrReference*0.20, mark*0.000001)
		currentATRBucket, previousATRBucket := math.Floor(atr/atrStep), math.Floor(before.ATR/atrStep)
		costStep := math.Max(atr*0.25, mark*0.0001)
		currentCostBucket, previousCostBucket := math.Floor(leaderPos.EntryPrice/costStep), math.Floor(before.LeaderEntryPrice/costStep)
		candleTrigger, candleHash := e.aiClosedCandleFeature(cycle.ID, mapping.Symbol, mapping.Side, before)
		switch {
		case inRange(mark, before.AttentionPriceLow, before.AttentionPriceHigh) && !inRange(before.TriggerPrice, before.AttentionPriceLow, before.AttentionPriceHigh):
			pendingTrigger = "AI_ATTENTION_ZONE"
		case candleTrigger != "":
			pendingTrigger = candleTrigger
			featureHash = candleHash
		case leaderBucket != prevLeaderBucket:
			pendingTrigger = "LEADER_SIZE_CHANGE"
		case before.LeaderEntryPrice > 0 && currentCostBucket != previousCostBucket:
			pendingTrigger = "LEADER_COST_CHANGE"
		case before.ATR > 0 && currentATRBucket != previousATRBucket:
			pendingTrigger = "ATR_CHANGE"
		case currentNearLeader && !previousNearLeader:
			pendingTrigger = "LEADER_ENTRY_ZONE"
		case currentRecovered && !previousRecovered:
			pendingTrigger = "STOP_RECOVERY"
		default:
			pendingTrigger = before.PendingTrigger
		}
		if pendingTrigger != before.PendingTrigger {
			featureHash = fmt.Sprintf("%s|p%.0f|a%.8f|l%.0f|r%d|s%d", pendingTrigger, priceBucket, atr, leaderBucket, cycle.ReentryCount, cycle.StopCount)
		}
	}
	firstReview := time.Now()
	if cycle.StoppedAt != nil {
		firstReview = cycle.StoppedAt.Add(time.Duration(e.config.RiskReentryCooldownSeconds) * time.Second)
		if firstReview.Before(time.Now()) {
			firstReview = time.Now()
		}
	}
	candidate, err := e.store.ReentryAI().EnsureReentryCandidate(&store.CopyGuardReentryCandidate{
		CycleID: cycle.ID, TraderID: e.traderID, LeaderPosID: mapping.LeaderPosID,
		Symbol: mapping.Symbol, Side: mapping.Side, MarginMode: mapping.MarginMode,
		TriggerPrice: mark, ATR: atr, MaxNotional: maxNotional, StopCount: cycle.StopCount,
		ReentryCount: cycle.ReentryCount, LeaderSize: leaderPos.Size, LeaderEntryPrice: leaderPos.EntryPrice,
		LastStopPrice: lastStop, DistanceATRRatio: noiseRatio, Protectable: protectable,
		FeatureHash: featureHash, PendingTrigger: pendingTrigger,
	}, firstReview)
	if err != nil {
		logger.Warnf("[CopyGuard] trader=%s cycle=%d event=AI_CANDIDATE_CREATE_FAILED reason=%v", e.traderID, cycle.ID, err)
		return
	}
	cycleStatus := copyGuardCycleStatusForCandidate(candidate.Status)
	_ = e.store.CopyTrade().UpdateCopyGuardObservation(cycle.ID, cycleStatus, leaderPos.EntryPrice, mark, atr)
	gate := "AI_WATCHING"
	if coolingDown {
		gate = watchGateCooldown
	}
	e.recordWatchSample(cycle, leaderPos, mark, atr, reentryBoundary, chaseLimit, gate)
	if beforeErr != nil {
		_ = e.store.CopyTrade().SaveCopyGuardEvent(&store.CopyGuardEvent{CycleID: cycle.ID, TraderID: e.traderID, Type: "AI_CANDIDATE_CREATED", Price: mark, Notional: maxNotional, Metadata: map[string]interface{}{"candidate_id": candidate.ID, "attempt_no": cycle.ReentryCount + 1, "next_review_at": candidate.NextReviewAt, "protectable": protectable, "feature_hash": featureHash}})
	}
}

// reentryObservationBounds is the single deterministic definition used by
// both legacy rule reentry and ai_guarded datapacks. Unknown inputs remain
// invalid (zero), so callers can mark them unavailable instead of teaching AI
// that an unknown boundary is a real price of zero.
func (e *Engine) reentryObservationBounds(side string, anchor, atr, lastStop float64, reentryCount int, cautious bool) (boundary, chaseLimit, requiredRecovery float64) {
	if e == nil || e.config == nil || anchor <= 0 || atr <= 0 {
		return 0, 0, 0
	}
	recoveryEsc := e.config.RiskReentryRecoveryEscalation
	if recoveryEsc < 1 {
		recoveryEsc = 1
	}
	if e.config.RiskReentryMinRecoveryATR > 0 && lastStop > 0 {
		requiredRecovery = atr * e.config.RiskReentryMinRecoveryATR * math.Pow(recoveryEsc, float64(reentryCount))
		if cautious {
			requiredRecovery *= 1.5
		}
	}
	if side == string(SideLong) {
		boundary = anchor - atr*e.config.RiskReentryBandATR
		if requiredRecovery > 0 {
			boundary = math.Max(boundary, lastStop+requiredRecovery)
		}
		chaseLimit = anchor + atr*e.config.RiskReentryMaxChaseATR
		return boundary, chaseLimit, requiredRecovery
	}
	boundary = anchor + atr*e.config.RiskReentryBandATR
	if requiredRecovery > 0 {
		boundary = math.Min(boundary, lastStop-requiredRecovery)
	}
	chaseLimit = anchor - atr*e.config.RiskReentryMaxChaseATR
	return boundary, chaseLimit, requiredRecovery
}

func copyGuardCycleStatusForCandidate(candidateStatus string) string {
	switch candidateStatus {
	case store.ReentryCandidateReviewing:
		return store.CopyGuardAIReviewing
	case store.ReentryCandidateWaiting, store.ReentryCandidatePaused:
		return store.CopyGuardAIWaiting
	case store.ReentryCandidateEntryPending:
		return store.CopyGuardReentryPending
	case store.ReentryCandidateAbandoned, store.ReentryCandidateInvalidated:
		return store.CopyGuardAIAbandoned
	case store.ReentryCandidateExpired:
		return store.CopyGuardWatchTimeout
	case store.ReentryCandidateBudgetSuspended:
		return store.CopyGuardBudgetSuspended
	default:
		return store.CopyGuardAIWatching
	}
}

// aiClosedCandleFeature emits only meaningful, original-direction reversal
// evidence from a newly closed 5m candle. It only decides when another paid
// review is worth scheduling; the AI still evaluates the complete datapack.
func (e *Engine) aiClosedCandleFeature(cycleID int64, symbol, side string, candidate *store.CopyGuardReentryCandidate) (string, string) {
	if candidate == nil || candidate.LastReviewAt == nil {
		return "", ""
	}
	if e.lastAICandleFeatureCheck == nil {
		e.lastAICandleFeatureCheck = make(map[int64]time.Time)
	}
	now := time.Now()
	if last := e.lastAICandleFeatureCheck[cycleID]; !last.IsZero() && now.Sub(last) < 30*time.Second {
		return "", ""
	}
	e.lastAICandleFeatureCheck[cycleID] = now
	klines, err := market.GetOKXCompletedCandles(symbol, "5m", 21)
	if err != nil {
		// Price structure remains useful if the volume endpoint is temporarily
		// unavailable; volume confirmation simply stays disabled for this tick.
		klines, err = market.GetOKXCompletedMarkCandles(symbol, "5m", 8)
	}
	if err != nil || len(klines) < 7 {
		return "", ""
	}
	latest := klines[len(klines)-1]
	// A candle that closed after the previous review may justify a new call,
	// even if that review happened after the candle opened.
	if !time.UnixMilli(latest.OpenTime).Add(5 * time.Minute).After(*candidate.LastReviewAt) {
		return "", ""
	}
	prev := klines[len(klines)-6 : len(klines)-1]
	maxHigh, minLow := prev[0].High, prev[0].Low
	for _, k := range prev[1:] {
		if k.High > maxHigh {
			maxHigh = k.High
		}
		if k.Low < minLow {
			minLow = k.Low
		}
	}
	recentSlope := latest.Close - klines[len(klines)-3].Close
	priorSlope := klines[len(klines)-3].Close - klines[len(klines)-6].Close
	breakout, slopeFlip, volumeConfirmed := false, false, false
	if side == string(SideLong) {
		breakout = latest.Close > maxHigh
		slopeFlip = recentSlope > 0 && priorSlope <= 0
	} else {
		breakout = latest.Close < minLow
		slopeFlip = recentSlope < 0 && priorSlope >= 0
	}
	if latest.Volume > 0 && len(klines) >= 21 {
		avgVolume := float64(0)
		for _, k := range klines[len(klines)-21 : len(klines)-1] {
			avgVolume += k.Volume
		}
		avgVolume /= 20
		directional := (side == string(SideLong) && latest.Close > latest.Open) || (side == string(SideShort) && latest.Close < latest.Open)
		volumeConfirmed = directional && avgVolume > 0 && latest.Volume >= 1.5*avgVolume
	}
	if !breakout && !slopeFlip && !volumeConfirmed {
		return "", ""
	}
	kind := "MA_SLOPE_REVERSAL"
	if breakout {
		kind = "STRUCTURE_BREAK"
	} else if volumeConfirmed {
		kind = "VOLUME_CONFIRMATION"
	}
	return kind, fmt.Sprintf("%s|c%d|close%.8f|vol%.4f|side%s", kind, latest.OpenTime, latest.Close, latest.Volume, side)
}

// findStoppedAttempt 找到当前观察期对应的被止损 attempt（attempt_no =
// 当前 reentry_count 且状态 STOPPED），供恢复幅度判据 / 重入定 size /
// 噪音档判定复用。
func (e *Engine) findStoppedAttempt(cycle *store.CopyGuardCycle) *store.CopyGuardAttempt {
	if cycle == nil || e.store == nil {
		return nil
	}
	attempts, err := e.store.CopyTrade().ListCopyGuardAttempts(cycle.ID)
	if err != nil {
		return nil
	}
	for _, attempt := range attempts {
		if attempt.AttemptNo == cycle.ReentryCount && attempt.Status == "STOPPED" {
			return attempt
		}
	}
	return nil
}

// attemptStopFillPrice 被止损 attempt 的止损成交价（数据缺失返回 0）
func attemptStopFillPrice(attempt *store.CopyGuardAttempt) float64 {
	if attempt == nil {
		return 0
	}
	return attempt.ExitPrice
}

// reentryWindowEpsilonRatio 窗口可行性判定的相对价格容差（锚点×该比例），
// 避免边界差极小的浮点误差被误判为空集。
const reentryWindowEpsilonRatio = 1e-6

// reentryWindowInfeasible 判定自动重入的价格窗口是否为空集：恢复下界越过追价
// 上限（多单 下界>上界；空单 下界<上界）。chaseLimit<=0（冷却期/降级边界，尚
// 未计算追价上限）时不判定。用相对 epsilon 容差避免裸浮点比较误判。
func reentryWindowInfeasible(side string, reentryBoundary, chaseLimit, anchor float64) bool {
	if chaseLimit <= 0 {
		return false
	}
	eps := math.Abs(anchor) * reentryWindowEpsilonRatio
	if side == string(SideLong) {
		return reentryBoundary > chaseLimit+eps
	}
	return reentryBoundary < chaseLimit-eps
}

// stopDistanceATRRatio 止损时的实际止损距离 / 当时 ATR（v5 重入自适应加严的
// 输入）。< 0.5 说明该配置的止损落在正常噪音区内（易扫损），< 0.3 说明极易
// 扫损。数据缺失（旧周期无 attempt 快照或 ATR）时返回 0（按正常档处理）。
func stopDistanceATRRatio(cycle *store.CopyGuardCycle, stopped *store.CopyGuardAttempt) float64 {
	if cycle == nil || stopped == nil || cycle.ATRAtStop <= 0 {
		return 0
	}
	if stopped.EntryPrice <= 0 || stopped.ExitPrice <= 0 {
		return 0
	}
	return math.Abs(stopped.EntryPrice-stopped.ExitPrice) / cycle.ATRAtStop
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
// 判定优先级从上到下：终态 > 噪音禁入 > 冷却 > 波动扩张 > 追价超限 >
// 价格未回归 > 确认中 > 金额过小/不可保护 > 全部通过（REENTRY_TRIGGERED）。
const (
	watchGateReentryDisabled  = "REENTRY_DISABLED"       // 重入功能未启用，观察至领航员平仓
	watchGateDisabledNoise    = "REENTRY_DISABLED_NOISE" // v5：止损距离/ATR < 0.3，自动重入默认禁用
	watchGateCooldown         = "COOLDOWN"               // 冷却期内（含逐次加严）
	watchGateATRExpansion     = "ATR_EXPANSION"          // 波动扩张超上限，市场环境已变
	watchGateChaseExceeded    = "CHASE_EXCEEDED"         // 价格超出追价上限（行情跑远）
	watchGatePriceNotReturned = "PRICE_NOT_RETURNED"     // 价格尚未回归重入边界
	watchGateReentryCandidate = "REENTRY_CANDIDATE"      // v5：条件满足，连续确认累计中
	watchGateMinNotional      = "MIN_NOTIONAL"           // 重入金额低于最小阈值
	watchGateUnprotectable    = "REENTRY_UNPROTECTABLE"  // v5：预算重入后止损不可行，不重入
	watchGateReentryTriggered = "REENTRY_TRIGGERED"      // 全部条件满足，已请求重入
	// 自动重入价格窗口塌缩为空集（恢复下界越过追价上限）→ 自动重入实质用尽
	watchGateReentryWindowInfeasible = "REENTRY_WINDOW_INFEASIBLE"
	watchSampleInterval              = 60 * time.Second // 固定间隔采样（gate 不变时）
	watchResumeGapMultiplier         = 5                // 采样断档 > 间隔×该倍数 → 记 WATCH_RESUMED
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
		CycleID: cycle.ID, TraderID: e.traderID, AttemptNo: cycle.ReentryCount, MarkPrice: markPrice, ATR: atr,
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
	attemptRecovery := make([]map[string]interface{}, 0, len(attempts))
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
				if elapsed := w.CreatedAt.Sub(*cycle.StoppedAt).Seconds(); elapsed >= 0 {
					firstRecoverySec = elapsed
				}
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
	// 每次止损分别计算恢复时间与观察期 MAE/MFE，防止第一次、第二次重入
	// 的 watch 样本混在一起。旧 schema 的 attempt_no=0 只保留周期级统计。
	for _, attempt := range attempts {
		if attempt.Status != "STOPPED" || attempt.ClosedAt == nil || attempt.ExitPrice <= 0 {
			continue
		}
		first := float64(-1)
		mfe, mae, sampleCount := float64(0), float64(0), 0
		for _, w := range samples {
			if w.AttemptNo != attempt.AttemptNo || w.CreatedAt.Before(*attempt.ClosedAt) {
				continue
			}
			sampleCount++
			diff := w.MarkPrice - attempt.ExitPrice
			if !long {
				diff = -diff
			}
			if diff > mfe {
				mfe = diff
			}
			if -diff > mae {
				mae = -diff
			}
			recovered := w.ReentryBoundary > 0 && ((long && w.MarkPrice >= w.ReentryBoundary) || (!long && w.MarkPrice <= w.ReentryBoundary))
			if recovered && first < 0 {
				first = w.CreatedAt.Sub(*attempt.ClosedAt).Seconds()
			}
		}
		attemptRecovery = append(attemptRecovery, map[string]interface{}{
			"attempt_no": attempt.AttemptNo, "first_recovery_seconds": first,
			"max_favorable_excursion": mfe, "max_adverse_excursion": mae,
			"max_favorable_excursion_usd": mfe * attempt.Quantity,
			"max_adverse_excursion_usd":   mae * attempt.Quantity,
			"sample_count":                sampleCount,
		})
	}

	watchSeconds := float64(0)
	if cycle.StoppedAt != nil {
		if elapsed := time.Since(*cycle.StoppedAt).Seconds(); elapsed > 0 {
			watchSeconds = elapsed
		}
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
		"attempt_recovery":        attemptRecovery,
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
	// 清空引擎默认的 fill 级稳定 clOrdId：重入的幂等 ID 必须是 integration
	// 分配的周期级 cgr ID（executeFullDecision 仅在 ClientOrderID 为空时分配，
	// 重启恢复 recoverV4PendingStates 也按 cgr ID 对账），不能被此处抢占。
	// 且重入虚拟 fill ID 含纳秒时间戳，本身不具备跨次重放的稳定性。
	dec.ClientOrderID = ""

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
