package trader

import (
	"context"
	"fmt"

	"nofx/copytrade"
	"nofx/decision"
	"nofx/logger"
)

// copyTradeEngineWrapper wraps copytrade.Engine to implement copyTradeEngine interface
type copyTradeEngineWrapper struct {
	engine *copytrade.Engine
}

// newCopyTradeEngineAdapter creates a new copy trade engine adapter
func newCopyTradeEngineAdapter(at *AutoTrader, cfg *copyTradeConfig) (copyTradeEngine, error) {
	// Convert config
	engineConfig := &copytrade.CopyConfig{
		ProviderType:   copytrade.ProviderType(cfg.ProviderType),
		LeaderID:       cfg.LeaderID,
		CopyRatio:      cfg.CopyRatio,
		SyncLeverage:   cfg.SyncLeverage,
		SyncMarginMode: cfg.SyncMarginMode,
		MinTradeWarn:   cfg.MinTradeWarn,
		MaxTradeWarn:   cfg.MaxTradeWarn,
	}

	// Create balance getter function
	getBalance := func() float64 {
		info, err := at.GetAccountInfo()
		if err != nil {
			logger.Warnf("⚠️ [%s] Failed to get account balance: %v", at.name, err)
			return 0
		}

		// Extract equity from account info
		if equity, ok := info["total_equity"].(float64); ok {
			return equity
		}
		return 0
	}

	// Create positions getter function
	getPositions := func() map[string]*copytrade.Position {
		positions := make(map[string]*copytrade.Position)

		// Get positions from exchange
		exchangePositions, err := at.GetPositions()
		if err != nil {
			logger.Warnf("⚠️ [%s] Failed to get positions: %v", at.name, err)
			return positions
		}

		// Convert to copytrade.Position format
		for _, pos := range exchangePositions {
			symbol, _ := pos["symbol"].(string)
			sideStr, _ := pos["side"].(string)
			quantity, _ := pos["quantity"].(float64)
			entryPrice, _ := pos["entry_price"].(float64)
			markPrice, _ := pos["mark_price"].(float64)
			leverage, _ := pos["leverage"].(int)
			unrealizedPnl, _ := pos["unrealized_pnl"].(float64)

			if quantity == 0 {
				continue
			}

			side := copytrade.SideLong
			if sideStr == "short" || sideStr == "sell" {
				side = copytrade.SideShort
			}

			key := copytrade.PositionKey(symbol, side)
			positions[key] = &copytrade.Position{
				Symbol:        symbol,
				Side:          side,
				Size:          absValue(quantity),
				EntryPrice:    entryPrice,
				MarkPrice:     markPrice,
				Leverage:      leverage,
				UnrealizedPnL: unrealizedPnl,
				PositionValue: absValue(quantity) * markPrice,
			}
		}

		return positions
	}

	// Create engine
	engine, err := copytrade.NewEngine(
		at.id,
		engineConfig,
		getBalance,
		getPositions,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create copytrade engine: %w", err)
	}

	return &copyTradeEngineWrapper{engine: engine}, nil
}

// Start starts the copy trade engine
func (w *copyTradeEngineWrapper) Start(ctx context.Context) error {
	return w.engine.Start(ctx)
}

// Stop stops the copy trade engine
func (w *copyTradeEngineWrapper) Stop() {
	w.engine.Stop()
}

// GetDecisionChannel returns the decision channel
func (w *copyTradeEngineWrapper) GetDecisionChannel() <-chan *decision.FullDecision {
	return w.engine.GetDecisionChannel()
}

// absValue returns the absolute value (named to avoid conflict with other abs functions)
func absValue(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

