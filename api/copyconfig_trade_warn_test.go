package api

// M18 回归：min/max_trade_warn 必须能通过创建/更新交易员的 CopyConfigReq
// 透传（此前请求结构缺字段，前端传值被 JSON unmarshal 静默丢弃）。

import (
	"encoding/json"
	"testing"

	"nofx/store"
)

func TestCopyConfigReqCarriesTradeWarnThresholds(t *testing.T) {
	var req CopyConfigReq
	payload := `{"provider_type":"okx","leader_id":"leader","copy_ratio":1,"min_trade_warn":15,"max_trade_warn":0}`
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.MinTradeWarn == nil || *req.MinTradeWarn != 15 {
		t.Fatalf("min_trade_warn must be carried, got %v", req.MinTradeWarn)
	}
	if req.MaxTradeWarn == nil || *req.MaxTradeWarn != 0 {
		t.Fatalf("explicit max_trade_warn=0 (no warn) must be distinguishable from unset, got %v", req.MaxTradeWarn)
	}

	var unset CopyConfigReq
	if err := json.Unmarshal([]byte(`{"provider_type":"okx","leader_id":"leader"}`), &unset); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if unset.MinTradeWarn != nil || unset.MaxTradeWarn != nil {
		t.Fatal("absent fields must stay nil so existing/default values are preserved")
	}
}

func TestNewCopyGuardDefaultsMinTradeWarn(t *testing.T) {
	if got := store.NewCopyGuardDefaults().MinTradeWarn; got != 12 {
		t.Fatalf("NewCopyGuardDefaults MinTradeWarn must align with the engine fallback (12), got %v", got)
	}
}
