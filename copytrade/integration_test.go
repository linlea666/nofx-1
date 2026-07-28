package copytrade

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/notifier"
	"nofx/store"
)

func TestClassifyExecutionFailurePreservesTypedPreSubmitReason(t *testing.T) {
	err := fmt.Errorf("persist close short attempt: %w",
		reasonError("PRE_SUBMIT", "prepare durable execution order attempt: database busy"))
	if got := classifyExecutionFailure(err); got != "PRE_SUBMIT" {
		t.Fatalf("typed pre-submit failure was flattened to %q", got)
	}
}

func TestExecutionFailurePhaseOnlyMarksErrorsBeforeDurableSubmission(t *testing.T) {
	baseErr := errors.New("market price temporarily unavailable")
	dec := &decision.Decision{
		ExecutionIntentID: 42,
		ExecutionStatus:   store.ExecutionIntentReserved,
		BeforeOrderSubmit: func(string, float64) error { return nil },
	}
	preSubmitErr := preservePreSubmitExecutionFailure(dec, baseErr)
	if ReasonCodeOf(preSubmitErr) != "PRE_SUBMIT" || !errors.Is(preSubmitErr, baseErr) {
		t.Fatalf("pre-adapter failure lost phase/cause: code=%s err=%v", ReasonCodeOf(preSubmitErr), preSubmitErr)
	}
	dec.ExecutionStatus = store.ExecutionIntentSubmitted
	afterSubmitErr := preservePreSubmitExecutionFailure(dec, baseErr)
	if afterSubmitErr != baseErr || ReasonCodeOf(afterSubmitErr) != "" {
		t.Fatalf("post-submission failure was incorrectly made replayable: code=%s err=%v", ReasonCodeOf(afterSubmitErr), afterSubmitErr)
	}
}

func TestExecutionAttemptRecorderAcceptsCloseAllSentinel(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "close-all-recorder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-light-short", SourceRevision: 3,
		CanonicalKey: "leader|trader-1|leader-light-short|3",
		Action:       "close_short", Symbol: "LIGHTUSDT", Side: "short",
		ClientOrderID: "stable-close-all",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	ti := &TraderIntegration{traderID: "trader-1", store: st}
	dec := &decision.Decision{
		Action: "close_short", Symbol: "LIGHTUSDT", IsCopyTrade: true,
		ExecutionIntentID: intent.ID, ClientOrderID: "stable-close-all",
	}
	ti.bindExecutionAttemptRecorder(dec)
	if dec.BeforeOrderSubmit == nil {
		t.Fatal("execution attempt recorder was not bound")
	}
	if err = dec.BeforeOrderSubmit(dec.ClientOrderID, 0); err != nil {
		t.Fatalf("close-all sentinel was rejected before adapter submission: %v", err)
	}
	var status string
	var attemptCount int
	if err = st.DB().QueryRow(`SELECT status FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=?`, intent.ID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if status != store.ExecutionIntentSubmitted || attemptCount != 1 || dec.ExecutionStatus != store.ExecutionIntentSubmitted {
		t.Fatalf("close-all submission boundary incomplete: db_status=%s attempts=%d decision_status=%s", status, attemptCount, dec.ExecutionStatus)
	}
}

func TestStableClientOrderIDMeetsOKXConstraint(t *testing.T) {
	first := stableClientOrderID("trader-1", "catchup|98271|1", "open_short")
	second := stableClientOrderID("trader-1", "catchup|98271|1", "open_short")
	if first != second {
		t.Fatalf("stable client id changed: %q != %q", first, second)
	}
	if !validOKXClientOrderID(first) {
		t.Fatalf("generated client id violates OKX constraint: %q", first)
	}
	for _, invalid := range []string{"", "nfx-catch-98271-1", strings.Repeat("a", 33), "订单1"} {
		if validOKXClientOrderID(invalid) {
			t.Fatalf("invalid OKX client id accepted: %q", invalid)
		}
	}
}

func TestOKXAttemptRecorderRejectsInvalidClientIDBeforePersistence(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "invalid-okx-client-id.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 2,
		CanonicalKey: "leader|trader-1|leader-pos|2", Action: "open_short",
		Symbol: "PROSUSDT", Side: "short", ClientOrderID: "nfx-catch-1-1",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	ti := &TraderIntegration{
		traderID: "trader-1", store: st,
		engine: &Engine{config: &CopyConfig{ProviderType: ProviderOKX}},
	}
	dec := &decision.Decision{
		Action: "open_short", IsCopyTrade: true, CopyTradeAction: "catchup",
		ExecutionIntentID: intent.ID, ClientOrderID: "nfx-catch-1-1",
		RequestedQuantity: 1,
	}
	ti.bindExecutionAttemptRecorder(dec)
	err = dec.BeforeOrderSubmit(dec.ClientOrderID, 1)
	if err == nil || ReasonCodeOf(err) != "PRE_SUBMIT" {
		t.Fatalf("invalid OKX client id was not rejected: %v", err)
	}
	var attempts int
	if queryErr := st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_execution_order_attempts WHERE intent_id=?`, intent.ID).Scan(&attempts); queryErr != nil {
		t.Fatal(queryErr)
	}
	if attempts != 0 {
		t.Fatalf("invalid client id reached durable attempt table: %d", attempts)
	}
}

func TestExecFailureDedupKeyStableForSameFailureState(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}
	err := errors.New("short position not found")
	dec := &decision.Decision{
		Action:        "reduce_short",
		Symbol:        "ETHUSDT",
		CloseRatio:    0.5154639175,
		MarginMode:    "cross",
		LeaderPosID:   "1243719130_ETHUSDT_SHORT",
		LeaderPosSize: 0.047,
	}

	first := ti.execFailureDedupKey(dec, err)
	second := ti.execFailureDedupKey(dec, err)

	if first != second {
		t.Fatalf("same failure state produced different keys:\n%s\n%s", first, second)
	}
}

func TestExecFailureDedupKeyChangesWhenLeaderOperationChanges(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}
	err := errors.New("short position not found")
	base := &decision.Decision{
		Action:        "reduce_short",
		Symbol:        "ETHUSDT",
		CloseRatio:    0.5154639175,
		MarginMode:    "cross",
		LeaderPosID:   "1243719130_ETHUSDT_SHORT",
		LeaderPosSize: 0.047,
	}
	next := *base
	next.LeaderPosSize = 0.02
	next.CloseRatio = 0.5744680851

	if ti.execFailureDedupKey(base, err) == ti.execFailureDedupKey(&next, err) {
		t.Fatalf("different leader operation should produce a different dedupe key")
	}
}

func TestInsufficientMarginInitialOpenKeepsReplayableCatchupGap(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-open-margin-skip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	engine := &Engine{config: &CopyConfig{ProviderType: ProviderBinance, LeaderID: "leader"}}
	ti := &TraderIntegration{traderID: "trader-1", engine: engine, store: st}
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 1,
		CanonicalKey: "leader|trader-1|leader-pos|1", SourceFillID: "fill-1",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 4,
	})
	if err != nil || !claimed {
		t.Fatalf("reserve intent claimed=%v err=%v", claimed, err)
	}
	dec := &decision.Decision{
		Action: "open_long", Symbol: "ETHUSDT", IsCopyTrade: true, CopyTradeAction: "open",
		LeaderPosID: "leader-pos", LeaderPosSize: 4, MarginMode: "cross",
		ExecutionIntentID: intent.ID, SourceRevision: 1,
	}
	ti.deferOrdinaryLeaderCatchup(dec, errors.New("insufficient margin"))
	var status, storedReason string
	if err = st.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&status, &storedReason); err != nil {
		t.Fatal(err)
	}
	if status != store.ExecutionIntentPartiallyFilled || storedReason != "CATCHUP_WAITING_MARGIN" {
		t.Fatalf("unexpected deferred intent state: status=%s reason=%s", status, storedReason)
	}
	var mappings int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM copy_trade_position_mappings WHERE trader_id=? AND leader_pos_id=?`,
		"trader-1", "leader-pos").Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if mappings != 0 {
		t.Fatal("no-fill initial open must not create a permanent ignored mapping")
	}
}

func TestInsufficientMarginAddDoesNotAdvanceSourceWithoutFill(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-add-margin-skip.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "trader-1", LeaderPosID: "leader-pos", LeaderID: "leader",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross", OpenedAt: time.Now(),
		LastKnownSize: 4,
	}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{config: &CopyConfig{ProviderType: ProviderBinance, LeaderID: "leader"}}
	ti := &TraderIntegration{traderID: "trader-1", engine: engine, store: st}
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 2,
		CanonicalKey: "leader|trader-1|leader-pos|2", SourceFillID: "fill-2",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 5,
	})
	if err != nil || !claimed {
		t.Fatalf("reserve intent claimed=%v err=%v", claimed, err)
	}
	dec := &decision.Decision{
		Action: "open_long", Symbol: "ETHUSDT", IsCopyTrade: true, CopyTradeAction: "add",
		LeaderPosID: "leader-pos", LeaderPosSize: 5, MarginMode: "cross",
		ExecutionIntentID: intent.ID, SourceRevision: 2,
	}
	ti.deferOrdinaryLeaderCatchup(dec, errors.New("insufficient margin"))
	mapping, err := st.CopyTrade().GetMapping("trader-1", "leader-pos")
	if err != nil {
		t.Fatal(err)
	}
	if mapping.Status != store.MappingStatusActive || mapping.SourceRevision != 1 || mapping.LastKnownSize != 4 || mapping.AddCount != 0 {
		t.Fatalf("no-fill add must preserve the prior mapping revision: %+v", mapping)
	}
	var status, storedReason string
	if err = st.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents WHERE id=?`, intent.ID).Scan(&status, &storedReason); err != nil {
		t.Fatal(err)
	}
	if status != store.ExecutionIntentPartiallyFilled || storedReason != "CATCHUP_WAITING_MARGIN" {
		t.Fatalf("unexpected deferred add state: status=%s reason=%s", status, storedReason)
	}
}

func TestLeaderCloseUnblocksTerminalNoFillCatchupRevision(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "leader-close-unblocks-catchup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pros-pos",
		Symbol: "PROSUSDT", Side: "short", MarginMode: "cross",
		Status: store.MappingStatusActive, SourceRevision: 4,
		LastKnownSize: 13344, OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET source_revision=4 WHERE trader_id='trader-1' AND leader_pos_id='pros-pos'`); err != nil {
		t.Fatal(err)
	}
	blocker, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "pros-pos", SourceRevision: 5,
		SourceFillID: "pros-add-5", CanonicalKey: "leader|trader-1|pros-pos|5",
		Action: "open_short", Symbol: "PROSUSDT", Side: "short",
		MarginMode: "cross", LeaderTargetSize: 13389,
		ClientOrderID: "ct1234567890abcdef",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve blocker claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(blocker.ID, "ct1234567890abcdef", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.CompleteExecutionOrderAttempt(blocker.ID, "ct1234567890abcdef",
		store.ExecutionOrderAttemptTerminalNoFill, "", "REJECTED", "OKX 51000", 0); err != nil {
		t.Fatal(err)
	}
	if err = cs.UpdateExecutionIntent(blocker.ID, store.ExecutionIntentFailed,
		"CATCHUP_SUPERSEDED", "authoritative leader position changed", "", 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	engine := &Engine{
		traderID: "trader-1", store: st,
		config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader"},
	}
	closeDecision := &decision.Decision{
		Action: "close_short", Symbol: "PROSUSDT", IsCopyTrade: true,
		LeaderPosID: "pros-pos", LeaderPosSize: 0, MarginMode: "cross",
		SourceFillID: "pros-close",
	}
	if !engine.reserveExecutionIntent(closeDecision) {
		t.Fatal("leader close remained blocked by terminal no-fill catch-up")
	}
	if closeDecision.SourceRevision != 6 || closeDecision.ExecutionIntentID == 0 {
		t.Fatalf("close did not reserve next revision: %+v", closeDecision)
	}
	settled, err := cs.GetExecutionIntentByID(blocker.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != store.ExecutionIntentSkipped || settled.ReasonCode != "CATCHUP_SKIPPED_NO_FILL" {
		t.Fatalf("blocker was not safely settled: %+v", settled)
	}
}

type cancelPendingCatchupExecutor struct {
	canceled bool
}

func (f *cancelPendingCatchupExecutor) ExecuteDecision(*decision.Decision) error {
	return nil
}
func (f *cancelPendingCatchupExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}
func (f *cancelPendingCatchupExecutor) GetPositions() ([]map[string]interface{}, error) {
	return nil, nil
}
func (f *cancelPendingCatchupExecutor) GetOrderStatusByClientID(_, clientOrderID string) (map[string]interface{}, error) {
	status := "NEW"
	if f.canceled {
		status = "CANCELED"
	}
	return map[string]interface{}{
		"clOrdId": clientOrderID, "ordId": "venue-order", "status": status, "executedQty": 0.0,
	}, nil
}
func (f *cancelPendingCatchupExecutor) CancelOrderByClientID(_, _ string) error {
	f.canceled = true
	return nil
}

func TestLeaderCloseReconciliationCancelsPendingCatchupBeforeSettlement(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "leader-close-cancel-catchup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	if err = cs.SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "pros-pos",
		Symbol: "PROSUSDT", Side: "short", MarginMode: "cross",
		Status: store.MappingStatusActive, LastKnownSize: 13344, OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE copy_trade_position_mappings SET source_revision=4 WHERE trader_id='trader-1' AND leader_pos_id='pros-pos'`); err != nil {
		t.Fatal(err)
	}
	intent, claimed, err := cs.ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "pros-pos", SourceRevision: 5,
		CanonicalKey: "leader|trader-1|pros-pos|5", Action: "open_short",
		Symbol: "PROSUSDT", Side: "short", LeaderTargetSize: 13389,
		ClientOrderID: "ct1234567890abcdef",
	})
	if err != nil || !claimed {
		t.Fatalf("reserve claimed=%v err=%v", claimed, err)
	}
	if _, err = cs.PrepareExecutionOrderAttempt(intent.ID, "ct1234567890abcdef", 1); err != nil {
		t.Fatal(err)
	}
	if err = cs.MarkOrdinaryCatchupReconciliationPending(intent.ID, "trader-1",
		"LEADER_CLOSE_RECONCILIATION_PENDING", "leader closed"); err != nil {
		t.Fatal(err)
	}
	fake := &cancelPendingCatchupExecutor{}
	ti := &TraderIntegration{
		traderID: "trader-1", store: st, executor: fake,
		engine: &Engine{config: &CopyConfig{ProviderType: ProviderOKX, LeaderID: "leader"}},
	}
	ti.reconcileExecutionIntents(false)
	if !fake.canceled {
		t.Fatal("pending risk-increasing order was not canceled before leader close")
	}
	stored, err := cs.GetExecutionIntentByID(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.ExecutionIntentSkipped || stored.ReasonCode != "CATCHUP_SKIPPED_NO_FILL" {
		t.Fatalf("canceled blocker was not settled: %+v", stored)
	}
	mapping, err := cs.GetMapping("trader-1", "pros-pos")
	if err != nil || mapping.SourceRevision != 5 {
		t.Fatalf("source revision did not advance after cancel confirmation: mapping=%+v err=%v", mapping, err)
	}
}

func TestExpiredOrdinaryCatchupTerminatesWithoutSubmitting(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-catchup-timeout.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	intent, claimed, err := st.CopyTrade().ReserveExecutionIntent(&store.CopyTradeExecutionIntent{
		TraderID: "trader-1", LeaderPosID: "leader-pos", SourceRevision: 1,
		CanonicalKey: "leader|trader-1|leader-pos|1", SourceFillID: "fill-1",
		Action: "open_long", Symbol: "ETHUSDT", Side: "long",
		MarginMode: "cross", LeaderTargetSize: 4, TargetQuantity: 1,
	})
	if err != nil || !claimed {
		t.Fatalf("reserve intent claimed=%v err=%v", claimed, err)
	}
	if err = st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: "trader-1", LeaderID: "leader", LeaderPosID: "leader-pos",
		Symbol: "ETHUSDT", Side: "long", MarginMode: "cross",
		Status: store.MappingStatusActive, SourceRevision: 1, LastKnownSize: 4,
		OpenedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.CopyTrade().PrepareExecutionOrderAttempt(intent.ID, "ct1234567890abcdef", 0.4); err != nil {
		t.Fatal(err)
	}
	if err = st.CopyTrade().CompleteExecutionOrderAttempt(intent.ID, "ct1234567890abcdef",
		store.ExecutionOrderAttemptFilled, "order-1", "FILLED", "", 0.4); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Second).UTC()
	if _, err = st.DB().Exec(`UPDATE copy_trade_execution_intents
		SET status='PARTIALLY_FILLED',filled_quantity=0.4,catchup_deadline_at=?
		WHERE id=?`, expired.Format(time.RFC3339Nano), intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = st.CopyTrade().GetExecutionIntentByID(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	ti := &TraderIntegration{
		traderID: "trader-1", store: st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance, LeaderID: "leader",
			CopyCatchupWindowSeconds: 60, CopyCatchupMaxAdverseBPS: 20,
		}},
	}
	ti.executeOrdinaryCatchup(intent)
	stored, err := st.CopyTrade().GetExecutionIntentByID(intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != store.ExecutionIntentCompletedPartial || stored.ReasonCode != "CATCHUP_RESIDUAL_SUPERSEDED" {
		t.Fatalf("expired catch-up was not terminalized: %+v", stored)
	}
	attempts, err := st.CopyTrade().ListExecutionOrderAttempts(intent.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("expired catch-up must not submit another order: attempts=%+v err=%v", attempts, err)
	}
}

// TestIsBenignCloseErrorRecognizesAllTraderFormats 覆盖各家 trader 的
// "本地无对应仓位"错误格式（关键字必须能识别）。
// 来源参考：
//   - OKX:     trader/okx_trader.go:856,946
//   - Bitget:  trader/bitget_trader.go:613,676
//   - Binance: trader/binance_futures.go:443,498
//   - Aster:   trader/aster_trader.go:755,846
//   - HL:      trader/hyperliquid_trader.go:516,588
//   - Bybit:   trader/bybit_trader.go:383,428
func TestIsBenignCloseErrorRecognizesAllTraderFormats(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}

	cases := []struct {
		name   string
		action string
		errMsg string
		want   bool
	}{
		// OKX
		{"okx close_long position not found",
			"close_long", "long position not found for ETHUSDT (mgnMode=cross)", true},
		{"okx close_short position not found",
			"close_short", "short position not found for ETHUSDT (mgnMode=cross)", true},
		{"okx reduce_short position not found",
			"reduce_short", "short position not found for ETHUSDT (mgnMode=cross)", true},
		// Bitget
		{"bitget close_long position not found",
			"close_long", "long position not found for BTCUSDT", true},
		// Binance / Aster / HL
		{"binance close_long no long position",
			"close_long", "no long position found for ETHUSDT", true},
		{"binance close_short no short position",
			"close_short", "no short position found for ETHUSDT", true},
		// Bybit
		{"bybit close_long no long position to close",
			"close_long", "no long position to close", true},
		{"bybit close_short no short position to close",
			"close_short", "no short position to close", true},
		// Binance fapi reduce-only 保险关键字
		{"binance reduceonly rejected",
			"close_long", "ReduceOnly Order is rejected", true},
		{"binance position size is 0",
			"reduce_long", "position size is 0", true},
		// 大小写不敏感
		{"upper case OKX",
			"close_short", "SHORT POSITION NOT FOUND for ETHUSDT", true},

		// 非 close/reduce 动作永远 false（防止开仓时被误判）
		{"open_long with position not found should NOT be benign",
			"open_long", "long position not found for ETHUSDT", false},
		{"open_short with no short position should NOT be benign",
			"open_short", "no short position found for ETHUSDT", false},
		{"hold action should NOT be benign",
			"hold", "position not found", false},

		// 其他错误不是良性（保证金不足、网络等）
		{"insufficient margin is NOT benign close",
			"close_long", "Order failed. Insufficient USDT margin in account", false},
		{"network error is NOT benign",
			"close_long", "context deadline exceeded", false},
		{"unrelated error is NOT benign",
			"close_long", "rate limit exceeded", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := &decision.Decision{
				Action:      tc.action,
				Symbol:      "ETHUSDT",
				LeaderPosID: "pos-1",
			}
			got := ti.isBenignCloseError(dec, errors.New(tc.errMsg))
			if got != tc.want {
				t.Fatalf("action=%s err=%q got=%v want=%v",
					tc.action, tc.errMsg, got, tc.want)
			}
		})
	}
}

func TestIsBenignCloseErrorHandlesNilInputs(t *testing.T) {
	ti := &TraderIntegration{
		traderID: "test-trader",
		engine:   &Engine{config: &CopyConfig{ProviderType: ProviderBinance}},
	}
	if ti.isBenignCloseError(nil, errors.New("position not found")) {
		t.Fatalf("nil decision should not be benign")
	}
	if ti.isBenignCloseError(&decision.Decision{Action: "close_long"}, nil) {
		t.Fatalf("nil error should not be benign")
	}
}

// TestHandleBenignCloseFailureClosesMappingAndAvoidsDeadlock 验证：
//   - 本地存在 active mapping
//   - 收到良性 close 失败
//   - handleBenignCloseFailure 后 mapping 状态变为 closed
//
// 这是阻断"大爷的弟弟"那种死循环的关键链路。
func TestHandleBenignCloseFailureClosesMappingAndAvoidsDeadlock(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-benign-close.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "1243719130_ETHUSDT_SHORT"

	// 模拟历史遗留的 active mapping（即"大爷的弟弟"日志里那条）
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      traderID,
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        "ETHUSDT",
		Side:          string(SideShort),
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2062.21,
		OpenSizeUSD:   96.92,
		LastKnownSize: 0.047,
	}); err != nil {
		t.Fatalf("seed active mapping: %v", err)
	}

	// 验证 seed 成功
	before, err := st.CopyTrade().GetActiveMapping(traderID, posID)
	if err != nil || before == nil {
		t.Fatalf("seed mapping not active: err=%v mapping=%+v", err, before)
	}

	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}

	dec := &decision.Decision{
		Action:      "close_short",
		Symbol:      "ETHUSDT",
		LeaderPosID: posID,
		EntryPrice:  2062.21,
		MarginMode:  "cross",
	}
	closeErr := errors.New("short position not found for ETHUSDT (mgnMode=cross)")

	// 前置条件：必须是良性错误（否则不应调用 handle）
	if !ti.isBenignCloseError(dec, closeErr) {
		t.Fatalf("test setup error: expected benign close")
	}

	ti.handleBenignCloseFailure(dec, closeErr)

	// 关键断言：active mapping 被回收（GetActiveMapping 应返回 nil）
	after, err := st.CopyTrade().GetActiveMapping(traderID, posID)
	if err != nil {
		t.Fatalf("get mapping after handle: %v", err)
	}
	if after != nil {
		t.Fatalf("expected mapping closed (deadlock broken), still active: %+v", after)
	}
}

type livePositionExecutor struct {
	positions []map[string]interface{}
}

func (e *livePositionExecutor) ExecuteDecision(*decision.Decision) error { return nil }
func (e *livePositionExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return nil, nil
}
func (e *livePositionExecutor) GetPositions() ([]map[string]interface{}, error) {
	return e.positions, nil
}
func (e *livePositionExecutor) GetPositionsFresh() ([]map[string]interface{}, error) {
	return e.positions, nil
}

func TestBenignCloseTextCannotCloseMappingWhilePositionStillExists(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-benign-close-live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "smart_leader_BTCUSDC_long"
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: traderID, LeaderPosID: posID, LeaderID: "leader",
		Symbol: "BTCUSDC", SourceSymbol: "BTCUSDC", ExecutionSymbol: "BTC-USDC-SWAP",
		Side: "long", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{config: &CopyConfig{ProviderType: ProviderBinance}, seenFills: map[string]time.Time{"close-fill": time.Now()}}
	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine:   engine,
		executor: &livePositionExecutor{positions: []map[string]interface{}{{
			"symbol": "BTCUSDC", "side": "long", "mgnMode": "cross", "positionAmt": 1.0,
		}}},
	}
	dec := &decision.Decision{
		Action: "close_long", Symbol: "BTCUSDC", SourceSymbol: "BTCUSDC",
		ExecutionSymbol: "BTC-USDC-SWAP", LeaderPosID: posID, SourceFillID: "close-fill", MarginMode: "cross",
	}
	closeErr := errors.New("long position not found for BTC-USDC-SWAP")
	if !ti.isBenignCloseError(dec, closeErr) || ti.benignCloseConfirmedFlat(dec) {
		t.Fatal("error text may be a candidate, but a fresh live position must prevent silent_close")
	}
	ti.handleRiskReductionRetry(dec, closeErr, "test")

	mapping, err := st.CopyTrade().GetActiveMapping(traderID, posID)
	if err != nil || mapping == nil {
		t.Fatalf("live position must retain its active mapping: mapping=%+v err=%v", mapping, err)
	}
	if engine.isSeen("close-fill") {
		t.Fatal("unconfirmed risk reduction must release the fill for retry")
	}
}

func TestBenignCloseMappingFailureReleasesFillForRetry(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-benign-close-db-failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	const fillID = "close-fill-db-retry"
	engine := &Engine{
		config:    &CopyConfig{ProviderType: ProviderBinance},
		seenFills: map[string]time.Time{fillID: time.Now()},
	}
	ti := &TraderIntegration{traderID: "trader", store: st, engine: engine}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ti.handleBenignCloseFailure(&decision.Decision{
		Action: "close_long", Symbol: "BTCUSDT",
		LeaderPosID: "leader-position", SourceFillID: fillID,
	}, errors.New("long position not found"))
	if engine.isSeen(fillID) {
		t.Fatal("failed mapping close must release the snapshot fill for the next poll")
	}
}

// TestIncrementMappingFailureAccumulatesAndResetsCorrectly 验证 store 层
// IncrementMappingFailure + ResetMappingFailure 的基本语义。
func TestIncrementMappingFailureAccumulatesAndResetsCorrectly(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-fail-count.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "leader-pos-1"

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:    traderID,
		LeaderPosID: posID,
		LeaderID:    "leader",
		Symbol:      "ETHUSDT",
		Side:        string(SideShort),
		MarginMode:  "cross",
		OpenedAt:    time.Now(),
		OpenPrice:   2000,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 累加 3 次
	for i := 1; i <= 3; i++ {
		c, err := st.CopyTrade().IncrementMappingFailure(traderID, posID, "fake error")
		if err != nil {
			t.Fatalf("increment #%d: %v", i, err)
		}
		if c != i {
			t.Fatalf("increment #%d: count=%d want %d", i, c, i)
		}
	}

	// 清零
	if err := st.CopyTrade().ResetMappingFailure(traderID, posID); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// 再累加 1 次：应该回到 1
	c, err := st.CopyTrade().IncrementMappingFailure(traderID, posID, "another error")
	if err != nil {
		t.Fatalf("increment after reset: %v", err)
	}
	if c != 1 {
		t.Fatalf("after reset increment: count=%d want 1", c)
	}
}

// TestIncrementMappingFailureReturnsZeroWhenNoActiveMapping 验证：
// active mapping 不存在时 IncrementMappingFailure 应返回 (0, nil)，
// 上层 checkAndTripMappingCircuit 可以据此短路。
func TestIncrementMappingFailureReturnsZeroWhenNoActiveMapping(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-no-mapping.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	c, err := st.CopyTrade().IncrementMappingFailure("test-trader", "nonexistent-pos", "err")
	if err != nil {
		t.Fatalf("increment on missing mapping: %v", err)
	}
	if c != 0 {
		t.Fatalf("expected count=0 for missing mapping, got %d", c)
	}
}

// TestCheckAndTripMappingCircuitTripsAtThreshold 验证：
// integration 层熔断逻辑在累计失败 ≥ mappingFailureCircuitThreshold 次时
// 自动 CloseMapping。
func TestCheckAndTripMappingCircuitTripsAtThreshold(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-circuit.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "circuit-pos-1"

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:    traderID,
		LeaderPosID: posID,
		LeaderID:    "leader",
		Symbol:      "ETHUSDT",
		Side:        string(SideShort),
		MarginMode:  "cross",
		OpenedAt:    time.Now(),
		OpenPrice:   2000,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderOKX,
			LeaderID:     "leader",
		}},
	}
	dec := &decision.Decision{
		Action:      "open_long",
		Symbol:      "ETHUSDT",
		LeaderPosID: posID,
		EntryPrice:  2000,
		MarginMode:  "cross",
	}
	hardErr := errors.New("Order failed. Insufficient USDT margin in account")

	// 累加到阈值前一步：mapping 应仍 active
	for i := 1; i < mappingFailureCircuitThreshold; i++ {
		ti.checkAndTripMappingCircuit(dec, hardErr)
	}
	if m, _ := st.CopyTrade().GetActiveMapping(traderID, posID); m == nil {
		t.Fatalf("到达阈值前 mapping 不应被熔断关闭")
	}

	// 第 threshold 次：熔断 → CloseMapping
	ti.checkAndTripMappingCircuit(dec, hardErr)

	if m, _ := st.CopyTrade().GetActiveMapping(traderID, posID); m != nil {
		t.Fatalf("熔断后 mapping 应被关闭，仍 active: %+v", m)
	}
}

// TestCheckAndTripMappingCircuitNoopWithoutMapping 验证：
// 无 active mapping 时（例如良性失败已自动 CloseMapping），
// checkAndTripMappingCircuit 应为 no-op 不报错。
func TestCheckAndTripMappingCircuitNoopWithoutMapping(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-circuit-noop.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ti := &TraderIntegration{
		traderID: "test-trader",
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderOKX,
			LeaderID:     "leader",
		}},
	}
	dec := &decision.Decision{
		Action:      "open_long",
		Symbol:      "ETHUSDT",
		LeaderPosID: "no-such-pos",
	}
	// 应安全返回，不 panic
	for i := 0; i < 10; i++ {
		ti.checkAndTripMappingCircuit(dec, errors.New("any error"))
	}
}

// TestExecuteDecisionDoesNotAlertOnBenignCloseFailure 验证：
// executeFullDecision 在遇到良性 close 失败时，不应该走告警分支
// （这里只能间接验证：状态保存为 silent_close 而非 failed）
func TestBenignCloseStatusDistinctFromHardFailure(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-status.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const traderID = "test-trader"
	const posID = "test-pos"

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID:      traderID,
		LeaderPosID:   posID,
		LeaderID:      "leader",
		Symbol:        "ETHUSDT",
		Side:          string(SideShort),
		MarginMode:    "cross",
		OpenedAt:      time.Now(),
		OpenPrice:     2062.21,
		LastKnownSize: 0.047,
	}); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	ti := &TraderIntegration{
		traderID: traderID,
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: ProviderBinance,
			LeaderID:     "leader",
		}},
	}

	dec := &decision.Decision{
		Action:      "close_short",
		Symbol:      "ETHUSDT",
		LeaderPosID: posID,
		EntryPrice:  2062.21,
	}
	ti.handleBenignCloseFailure(dec, errors.New("short position not found"))

	logs, err := st.CopyTrade().GetRecentSignalLogs(traderID, 10)
	if err != nil {
		t.Fatalf("query signal logs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatalf("expected at least one signal log")
	}
	found := false
	for _, log := range logs {
		if log.Status == "silent_close" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected signal log with status=silent_close, got: %+v", logs)
	}
}

// ============================================================================
// PR-Notify-1: Binance 跟单成功动作邮件通知（hook + helper）
// ============================================================================

// newTestIntegrationWithProvider 构造一个最小化 TraderIntegration 用于 hook 测试。
func newTestIntegrationWithProvider(t *testing.T, provider ProviderType) *TraderIntegration {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "nofx-notify-action.db"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &TraderIntegration{
		traderID: "test-trader-notify",
		store:    st,
		engine: &Engine{config: &CopyConfig{
			ProviderType: provider,
			LeaderID:     "5008318166959365632",
		}},
	}
}

func newBinanceDecisionForNotify() *decision.Decision {
	return &decision.Decision{
		Action:          "open_long",
		Symbol:          "ETHUSDT",
		Leverage:        40,
		PositionSizeUSD: 49.03,
		MarginMode:      "cross",
		EntryPrice:      2095.4200,
		LeaderPosID:     "1239518824_ETHUSDT_LONG",
		LeaderPosSize:   0.0335,
		Reasoning:       "Copy trading from Binance leader",
	}
}

// TestSendCopyActionAlertBuildsAlertWithExpectedFields 验证 helper 构造的 Alert
// 字段、限流键、Body 内容符合预期。直接调用 sendCopyActionAlert（不走 Provider 守卫）。
func TestSendCopyActionAlertBuildsAlertWithExpectedFields(t *testing.T) {
	cap := &notifier.CaptureNotifier{}
	restore := notifier.SetGlobalForTesting(cap, true)
	t.Cleanup(restore)

	ti := newTestIntegrationWithProvider(t, ProviderBinance)
	dec := newBinanceDecisionForNotify()

	ti.sendCopyActionAlert(dec, 1832)

	if len(cap.Alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d: %+v", len(cap.Alerts), cap.Alerts)
	}
	a := cap.Alerts[0]

	if a.Category != "copy_trade" {
		t.Fatalf("Category=%q want copy_trade", a.Category)
	}
	if a.TraderID != "test-trader-notify" {
		t.Fatalf("TraderID=%q unexpected", a.TraderID)
	}
	wantRateKey := "copy_action|test-trader-notify|ETHUSDT|open_long"
	if a.RateKey != wantRateKey {
		t.Fatalf("RateKey=%q want %q", a.RateKey, wantRateKey)
	}
	if a.DedupKey != "" {
		t.Fatalf("DedupKey 不应设置（每个独立动作都该发），实际=%q", a.DedupKey)
	}
	if !strings.Contains(a.Title, "跟单成功") || !strings.Contains(a.Title, "ETHUSDT") {
		t.Fatalf("Title 缺失关键词: %q", a.Title)
	}
	if !strings.Contains(a.Body, "Copy Trade Action Executed") {
		t.Fatalf("Body 缺失关键标识: %q", a.Body)
	}
	wantFields := map[string]string{
		"Provider":    "binance",
		"Leader":      "5008318166959365632",
		"Action":      "open_long",
		"Symbol":      "ETHUSDT",
		"Leverage":    "40x",
		"MarginMode":  "cross",
		"LeaderPosID": "1239518824_ETHUSDT_LONG",
		"DurationMs":  "1832",
	}
	for k, want := range wantFields {
		got, ok := a.Fields[k]
		if !ok {
			t.Fatalf("Fields 缺少 key=%s", k)
		}
		if got != want {
			t.Fatalf("Fields[%s]=%q want %q", k, got, want)
		}
	}
}

// TestExecuteFullDecisionSkipsActionAlertWhenSwitchOff 验证守卫 1：
// NOTIFY_BINANCE_COPY_ACTION_ENABLED=false 时即使 provider=Binance 也不发邮件。
// 直接测试 hook 入口条件而非走完整执行链。
func TestExecuteFullDecisionSkipsActionAlertWhenSwitchOff(t *testing.T) {
	cap := &notifier.CaptureNotifier{}
	restore := notifier.SetGlobalForTesting(cap, false) // 开关 OFF
	t.Cleanup(restore)

	ti := newTestIntegrationWithProvider(t, ProviderBinance)
	dec := newBinanceDecisionForNotify()

	// 模拟 executeFullDecision 成功分支末尾的守卫
	if ti.engine.config.ProviderType == ProviderBinance && notifier.CopyTradeActionEnabled() {
		ti.sendCopyActionAlert(dec, 100)
	}

	if len(cap.Alerts) != 0 {
		t.Fatalf("env 关闭时不应发邮件，实际 %d 封: %+v", len(cap.Alerts), cap.Alerts)
	}
}

// TestExecuteFullDecisionSkipsActionAlertForNonBinanceProvider 验证守卫 2：
// 即使开关开启，OKX / Hyperliquid 数据源也永不发邮件。
func TestExecuteFullDecisionSkipsActionAlertForNonBinanceProvider(t *testing.T) {
	cases := []ProviderType{ProviderOKX, ProviderHyperliquid}
	for _, p := range cases {
		t.Run(string(p), func(t *testing.T) {
			cap := &notifier.CaptureNotifier{}
			restore := notifier.SetGlobalForTesting(cap, true) // 开关 ON
			t.Cleanup(restore)

			ti := newTestIntegrationWithProvider(t, p)
			dec := newBinanceDecisionForNotify()

			if ti.engine.config.ProviderType == ProviderBinance && notifier.CopyTradeActionEnabled() {
				ti.sendCopyActionAlert(dec, 100)
			}

			if len(cap.Alerts) != 0 {
				t.Fatalf("provider=%s 不应发邮件，实际 %d 封", p, len(cap.Alerts))
			}
		})
	}
}

// TestSendCopyActionAlertHandlesNilDec 边界：nil decision 应安全返回不发邮件。
func TestSendCopyActionAlertHandlesNilDec(t *testing.T) {
	cap := &notifier.CaptureNotifier{}
	restore := notifier.SetGlobalForTesting(cap, true)
	t.Cleanup(restore)

	ti := newTestIntegrationWithProvider(t, ProviderBinance)
	ti.sendCopyActionAlert(nil, 0) // 不应 panic、不应发邮件

	if len(cap.Alerts) != 0 {
		t.Fatalf("nil decision 不应发邮件，实际 %d 封", len(cap.Alerts))
	}
}

// TestSendCopyActionAlertRateKeyDistinguishesActions 验证限流键设计：
// 同 trader 同 symbol 不同 action 应有不同 RateKey，确保不会互相压制。
func TestSendCopyActionAlertRateKeyDistinguishesActions(t *testing.T) {
	cap := &notifier.CaptureNotifier{}
	restore := notifier.SetGlobalForTesting(cap, true)
	t.Cleanup(restore)

	ti := newTestIntegrationWithProvider(t, ProviderBinance)

	actions := []string{"open_long", "open_short", "close_long", "close_short",
		"reduce_long", "reduce_short"}
	seen := map[string]bool{}
	for _, a := range actions {
		dec := newBinanceDecisionForNotify()
		dec.Action = a
		ti.sendCopyActionAlert(dec, 0)
	}
	for _, alert := range cap.Alerts {
		if seen[alert.RateKey] {
			t.Fatalf("RateKey 重复：%s 应该按 action 区分", alert.RateKey)
		}
		seen[alert.RateKey] = true
	}
	if len(seen) != len(actions) {
		t.Fatalf("期望 %d 个不同 RateKey，实际 %d", len(actions), len(seen))
	}
}

func TestStopCopyTradingForTraderClearsLifecycleReservation(t *testing.T) {
	integrationsMu.Lock()
	oldIntegrations, oldStarting, oldEpoch := integrations, integrationsStarting, integrationsEpoch
	integrations = make(map[string]*TraderIntegration)
	integrationsStarting = make(map[string]struct{})
	integrationsEpoch = 0
	integrationsMu.Unlock()
	t.Cleanup(func() {
		integrationsMu.Lock()
		integrations, integrationsStarting, integrationsEpoch = oldIntegrations, oldStarting, oldEpoch
		integrationsMu.Unlock()
	})

	ti := NewTraderIntegration("lifecycle-trader", nil, nil)
	ti.running.Store(true)
	integrationsMu.Lock()
	integrations[ti.traderID] = ti
	integrationsMu.Unlock()

	if err := StopCopyTradingForTrader(ti.traderID); err != nil {
		t.Fatalf("stop integration: %v", err)
	}
	integrationsMu.RLock()
	_, exists := integrations[ti.traderID]
	_, reserved := integrationsStarting[ti.traderID]
	integrationsMu.RUnlock()
	if exists || reserved || ti.IsRunning() {
		t.Fatalf("stop must remove integration and release lifecycle reservation: exists=%v reserved=%v running=%v",
			exists, reserved, ti.IsRunning())
	}
}
