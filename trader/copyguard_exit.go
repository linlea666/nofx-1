package trader

import (
	"fmt"
	"math"
)

// Validate the underlying venue, not just AutoTrader's wrapper methods. This
// exit contract is also required when both protection switches are disabled.
func (at *AutoTrader) ValidateLeaderExitCapabilities() error {
	checks := []struct {
		name string
		ok   bool
	}{
		{"scoped leader exits", implementsTraderCapability[ScopedPositionCloser](at.trader)},
		{"uncached position refresh", implementsTraderCapability[FreshPositionProvider](at.trader)},
		{"client order lookup", implementsTraderCapability[ClientOrderStatusProvider](at.trader)},
		{"exact execution instrument resolution", implementsTraderCapability[ExecutionInstrumentResolver](at.trader)},
	}
	for _, check := range checks {
		if !check.ok {
			return fmt.Errorf("execution exchange does not support %s", check.name)
		}
	}
	return nil
}

func (at *AutoTrader) CloseCopyGuardPosition(req CopyGuardExitRequest) (map[string]interface{}, error) {
	if executor, ok := at.trader.(CopyGuardScopedCloser); ok {
		return executor.CloseCopyGuardPosition(req)
	}
	return nil, fmt.Errorf("execution exchange does not support scoped Copy Guard exits")
}

func validateCopyGuardExit(req CopyGuardExitRequest) error {
	if (req.CycleID <= 0 && req.ExecutionIntentID <= 0) || req.ClientOrderID == "" || req.Symbol == "" || req.Quantity <= 0 || math.IsNaN(req.Quantity) || math.IsInf(req.Quantity, 0) || (req.Side != "long" && req.Side != "short") || (req.MarginMode != "cross" && req.MarginMode != "isolated") {
		return fmt.Errorf("incomplete Copy Guard exit scope")
	}
	return nil
}

func (at *AutoTrader) CloseScopedPosition(req ScopedPositionCloseRequest) (map[string]interface{}, error) {
	if executor, ok := at.trader.(ScopedPositionCloser); ok {
		return executor.CloseScopedPosition(req)
	}
	return nil, fmt.Errorf("execution exchange does not support scoped leader exits")
}

func (t *OKXTrader) CloseScopedPosition(req ScopedPositionCloseRequest) (map[string]interface{}, error) {
	return t.CloseCopyGuardPosition(req)
}

func (t *FuturesTrader) CloseScopedPosition(req ScopedPositionCloseRequest) (map[string]interface{}, error) {
	return t.CloseCopyGuardPosition(req)
}

func (t *OKXTrader) CloseCopyGuardPosition(req CopyGuardExitRequest) (map[string]interface{}, error) {
	if err := validateCopyGuardExit(req); err != nil {
		return nil, err
	}
	if req.Side == "long" {
		return t.closeLong(req.Symbol, req.Quantity, false, req.ClientOrderID, req.BeforeSubmit, &req)
	}
	return t.closeShort(req.Symbol, req.Quantity, false, req.ClientOrderID, req.BeforeSubmit, &req)
}

func (t *FuturesTrader) CloseCopyGuardPosition(req CopyGuardExitRequest) (map[string]interface{}, error) {
	if err := validateCopyGuardExit(req); err != nil {
		return nil, err
	}
	return t.ExecuteCopyTradeMarketOrder(CopyTradeMarketOrderRequest{Action: "close_" + req.Side, Symbol: req.Symbol, Quantity: req.Quantity, ClientOrderID: req.ClientOrderID, BeforeSubmit: req.BeforeSubmit})
}

func (t *OKXTrader) copyGuardClosePositions(mode *string, scopes []*CopyGuardExitRequest) ([]map[string]interface{}, error) {
	if len(scopes) > 0 && scopes[0] != nil {
		*mode = scopes[0].MarginMode
		return t.GetPositionsFresh()
	}
	return t.GetPositions()
}

func copyGuardPositionIDMatches(pos map[string]interface{}, scopes []*CopyGuardExitRequest) bool {
	if len(scopes) == 0 || scopes[0] == nil || scopes[0].PositionID == "" {
		return true
	}
	return pos["posId"] == scopes[0].PositionID
}
