package trader

import (
	"fmt"
	"math"
)

func (at *AutoTrader) CloseCopyGuardPosition(req CopyGuardExitRequest) (map[string]interface{}, error) {
	if executor, ok := at.trader.(CopyGuardScopedCloser); ok {
		return executor.CloseCopyGuardPosition(req)
	}
	return nil, fmt.Errorf("execution exchange does not support scoped Copy Guard exits")
}

func validateCopyGuardExit(req CopyGuardExitRequest) error {
	if req.CycleID <= 0 || req.ClientOrderID == "" || req.Symbol == "" || req.Quantity <= 0 || math.IsNaN(req.Quantity) || math.IsInf(req.Quantity, 0) || (req.Side != "long" && req.Side != "short") || (req.MarginMode != "cross" && req.MarginMode != "isolated") {
		return fmt.Errorf("incomplete Copy Guard exit scope")
	}
	return nil
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
