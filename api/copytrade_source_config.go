package api

import (
	"fmt"
	"strings"

	"nofx/copytrade"
	"nofx/store"
)

// prepareCopyTradeSource centralizes all three configuration entry points so
// source identity, generation, and active-position protection cannot drift.
func prepareCopyTradeSource(st *store.Store, cfg, existing *store.CopyTradeConfig) error {
	running := false
	if cfg != nil && cfg.TraderID != "" {
		running = copytrade.IsCopyTradingRunning(cfg.TraderID)
	}
	return prepareCopyTradeSourceWithRuntime(st, cfg, existing, running)
}

func prepareCopyTradeSourceWithRuntime(st *store.Store, cfg, existing *store.CopyTradeConfig, running bool) error {
	if cfg == nil {
		return fmt.Errorf("copy trade config is required")
	}
	cfg.ProviderType = strings.TrimSpace(cfg.ProviderType)
	cfg.LeaderID = strings.TrimSpace(cfg.LeaderID)
	switch copytrade.ProviderType(cfg.ProviderType) {
	case copytrade.ProviderHyperliquid, copytrade.ProviderOKX, copytrade.ProviderBinance:
	default:
		return fmt.Errorf("unsupported copy trade provider %q", cfg.ProviderType)
	}
	mode := strings.TrimSpace(cfg.BinanceSourceMode)
	if mode == "" {
		mode = string(copytrade.BinanceSourceCopyManagement)
	}

	if cfg.ProviderType == string(copytrade.ProviderBinance) {
		switch copytrade.BinanceSourceMode(mode) {
		case copytrade.BinanceSourceCopyManagement:
			if cfg.LeaderID == "" {
				return fmt.Errorf("Binance 已跟单模式需要 portfolioId")
			}
			if existing != nil &&
				copytrade.BinanceSourceMode(strings.TrimSpace(existing.BinanceSourceMode)) == copytrade.BinanceSourceSmartMoney &&
				(cfg.LeaderID == existing.LeaderID || cfg.LeaderID == existing.BinanceTopTraderID) {
				return fmt.Errorf("从公开领航员切换到 Binance 跟单管理时必须显式提供新的 portfolioId，不能复用 topTraderId")
			}
			cfg.BinanceTopTraderID = ""
		case copytrade.BinanceSourceSmartMoney:
			raw := strings.TrimSpace(cfg.BinanceTopTraderID)
			if raw == "" {
				existingMode := copytrade.BinanceSourceCopyManagement
				if existing != nil && strings.TrimSpace(existing.BinanceSourceMode) != "" {
					existingMode = copytrade.BinanceSourceMode(strings.TrimSpace(existing.BinanceSourceMode))
				}
				if existing != nil && existingMode != copytrade.BinanceSourceSmartMoney {
					return fmt.Errorf("从 Binance 跟单管理切换到公开领航员时必须显式提供 topTraderId，不能复用 portfolioId")
				}
				raw = cfg.LeaderID
			}
			topTraderID, err := copytrade.NormalizeTopTraderID(raw)
			if err != nil {
				return fmt.Errorf("Binance 公开领航员配置无效: %w", err)
			}
			cfg.BinanceTopTraderID = topTraderID
			// The engine has one provider identity parameter. Persisting the same
			// canonical ID in both fields avoids ever mixing portfolioId and
			// topTraderId at runtime or in health/event keys.
			cfg.LeaderID = topTraderID
		default:
			return fmt.Errorf("unsupported Binance source mode %q", mode)
		}
	} else {
		if cfg.LeaderID == "" {
			return fmt.Errorf("copy trade config requires leader_id")
		}
		mode = string(copytrade.BinanceSourceCopyManagement)
		cfg.BinanceTopTraderID = ""
	}
	cfg.BinanceSourceMode = mode

	if existing == nil {
		cfg.SourceGeneration = 1
		return nil
	}
	identityChanged := copyTradeSourceIdentityChanged(existing, cfg)
	if !identityChanged {
		cfg.SourceGeneration = existing.SourceGeneration
		if cfg.SourceGeneration <= 0 {
			cfg.SourceGeneration = 1
		}
		return nil
	}
	if running {
		return fmt.Errorf("跟单引擎正在运行，请先停止跟单后再切换领航员数据源")
	}
	if st != nil {
		live, err := st.CopyTrade().HasLiveSourceState(cfg.TraderID)
		if err != nil {
			return fmt.Errorf("check active source state: %w", err)
		}
		if live {
			return fmt.Errorf("当前仍有活动跟单映射或 Copy Guard 周期，请先平仓/清理旧来源后再切换领航员数据源")
		}
	}
	cfg.SourceGeneration = existing.SourceGeneration + 1
	if cfg.SourceGeneration <= 1 {
		cfg.SourceGeneration = 2
	}
	return nil
}

func copyTradeSourceIdentityChanged(existing, next *store.CopyTradeConfig) bool {
	if existing == nil || next == nil {
		return false
	}
	return existing.ProviderType != next.ProviderType ||
		existing.LeaderID != next.LeaderID ||
		existing.BinanceSourceMode != next.BinanceSourceMode ||
		existing.BinanceTopTraderID != next.BinanceTopTraderID
}
