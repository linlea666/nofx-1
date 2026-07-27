package copytrade

import (
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/decision"
	"nofx/store"
)

type smartEngineProvider struct {
	state *AccountState
	err   error
}

func (p *smartEngineProvider) GetFills(string, time.Time) ([]Fill, error)    { return nil, nil }
func (p *smartEngineProvider) GetAccountState(string) (*AccountState, error) { return p.state, p.err }
func (p *smartEngineProvider) Type() ProviderType                            { return ProviderBinance }

func makeSmartPosition(id, symbol string, size float64, valid bool) *Position {
	return &Position{PosID: id, Symbol: symbol, Side: SideLong, Size: size, EntryPrice: 100, MarkPrice: 100,
		MarginMode: "cross", PositionValue: size * 100, RawPositionValue: size * 100,
		ValueCurrency: "USDC", ValueUSDValid: valid, ValueError: "USDC/USD rate unavailable"}
}

func TestAttachRawFillSourcesKeepsUniqueMatchingEvidence(t *testing.T) {
	snapshot := []Fill{{ID: "snapshot", Symbol: "SOXSUSDT", PositionSide: SideLong}}
	raw := []Fill{
		{ID: "f1", Symbol: "SOXSUSDT", PositionSide: SideLong},
		{ID: "f1", Symbol: "SOXSUSDT", PositionSide: SideLong},
		{ID: "f2", Symbol: "SOXSUSDT", PositionSide: SideLong},
		{ID: "other-side", Symbol: "SOXSUSDT", PositionSide: SideShort},
		{ID: "other-symbol", Symbol: "BTCUSDT", PositionSide: SideLong},
	}
	attachRawFillSources(snapshot, raw)
	if len(snapshot[0].SourceFillIDs) != 2 ||
		snapshot[0].SourceFillIDs[0] != "f1" ||
		snapshot[0].SourceFillIDs[1] != "f2" {
		t.Fatalf("snapshot source evidence must be unique and narrowly matched: %+v", snapshot[0].SourceFillIDs)
	}
}

func TestOKXSnapshotUsesContractAwareUSDNotional(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderOKX)
	pos := &Position{
		PosID: "okx-pos", Symbol: "TESTUSDT", Side: SideLong,
		Size: 100, MarkPrice: 10, PositionValue: 50, MarginMode: "cross",
	}
	e.leaderState = &AccountState{TotalEquity: 1000, Positions: map[string]*Position{pos.PosID: pos}}
	open := e.detectBinancePositionSnapshotFills()
	if len(open) != 1 || open[0].Value != 50 || !open[0].ValueUSDValid || open[0].ValueCurrency != "USD" {
		t.Fatalf("OKX open must use ctVal-aware notionalUsd, not contracts*price: %+v", open)
	}
	saveActiveMapping(t, st, pos.PosID, 80)
	add := e.detectBinancePositionSnapshotFills()
	if len(add) != 1 || add[0].Action != ActionAdd || add[0].Size != 20 || add[0].Value != 10 || !add[0].ValueUSDValid {
		t.Fatalf("OKX add must scale authoritative total notional by contract delta: %+v", add)
	}
}

func TestOKXSnapshotMissingUSDNotionalFailsClosedForRiskIncrease(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderOKX)
	pos := &Position{
		PosID: "okx-pos", Symbol: "TESTUSDT", Side: SideLong,
		Size: 100, MarkPrice: 10, PositionValue: 0, MarginMode: "cross",
	}
	fill := e.buildBinanceSnapshotFillForPosition(pos, pos.PosID, ActionOpen, pos.Size, 0, pos.Size, 0)
	signal := &TradeSignal{Fill: &fill}
	match := &SignalMatchResult{Action: ActionOpen, PosID: pos.PosID, LeaderPosition: pos}
	if fill.Value != 0 || fill.ValueUSDValid || !e.blockInvalidSourceRiskIncrease(signal, match) {
		t.Fatalf("missing OKX notionalUsd must block risk increase: %+v", fill)
	}
}

func TestSmartMoneySnapshotCarriesValueIdentityAndFailsClosedOnOpen(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.SourceGeneration = 1
	e.config.LeaderID = "5082050984257986817"
	pos := makeSmartPosition("smart-pos", "BTCUSDC", 2, false)
	e.leaderState = &AccountState{TotalEquity: 1000, Positions: map[string]*Position{pos.PosID: pos}}
	e.provider = &smartEngineProvider{state: e.leaderState}
	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 1 || fills[0].ValueCurrency != "USDC" || fills[0].ValueUSDValid || fills[0].Value != 0 || fills[0].RawValue != 200 {
		t.Fatalf("Smart Money value identity lost: %+v", fills)
	}
	e.markSeen(fills[0].ID)
	e.processSignal(e.buildSignal(&fills[0]))
	select {
	case got := <-e.decisionCh:
		t.Fatalf("invalid USD value must not open: %+v", got)
	default:
	}
	mapping, err := st.CopyTrade().GetMapping(e.traderID, pos.PosID)
	if err != nil || mapping != nil {
		t.Fatalf("temporarily unvalued open must not create a permanent mapping: %+v err=%v", mapping, err)
	}
	var intentStatus, reasonCode string
	if err = st.DB().QueryRow(`SELECT status,reason_code FROM copy_trade_execution_intents
		WHERE trader_id=? AND leader_pos_id=? ORDER BY id DESC LIMIT 1`, e.traderID, pos.PosID).Scan(&intentStatus, &reasonCode); err != nil {
		t.Fatal(err)
	}
	if intentStatus != store.ExecutionIntentReconciling || reasonCode != "SOURCE_VALUE_UNAVAILABLE" {
		t.Fatalf("recognized but unvalued open must stay replayable: %s/%s", intentStatus, reasonCode)
	}
}

func TestSmartMoneySizingUsesLeaderMarginBalanceAndFollowerTotalEquity(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.CopyRatio = 0.5
	e.config.MinTradeWarn = 1
	e.getFollowerBalance = func() float64 { return 20 }
	e.getFollowerEquity = func() float64 { return 100 }
	signal := &TradeSignal{
		ProviderType: ProviderBinance,
		LeaderEquity: 1000,
		Fill: &Fill{
			Symbol: "BTCUSDC", PositionSide: SideLong, Action: ActionOpen,
			Price: 100, Size: 2, Value: 200, ValueUSDValid: true,
		},
	}
	copySize, warnings := e.calculateCopySizeByPositionChange(signal, &SignalMatchResult{Action: ActionOpen})
	if len(warnings) != 0 || copySize != 10 {
		t.Fatalf("copy ratio must be 0.5 × (200/1000) × follower equity 100 = 10, got %.2f warnings=%+v", copySize, warnings)
	}

	signal.LeaderEquity = 0
	if blocked, _ := e.calculateCopySizeByPositionChange(signal, &SignalMatchResult{Action: ActionOpen}); blocked != 0 {
		t.Fatalf("missing/stale leader equity must block risk increase, got %.2f", blocked)
	}
}

func TestSmartMoneyRecoveryAbsorbsNewRiskButPreservesReductions(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.LeaderID = "5082050984257986817"
	saveActiveMapping(t, st, "added", 10)
	saveActiveMapping(t, st, "reduced", 10)
	state := &AccountState{Positions: map[string]*Position{
		"added":   makeSmartPosition("added", "ETHUSDC", 15, true),
		"reduced": makeSmartPosition("reduced", "BTCUSDC", 6, true),
		"new":     makeSmartPosition("new", "SOLUSDC", 4, true),
	}}
	if err := e.rebaselineSmartMoneyRecovery(state); err != nil {
		t.Fatal(err)
	}
	added, _ := st.CopyTrade().GetMapping(e.traderID, "added")
	reduced, _ := st.CopyTrade().GetMapping(e.traderID, "reduced")
	newMapping, _ := st.CopyTrade().GetMapping(e.traderID, "new")
	if added.LastKnownSize != 15 {
		t.Fatalf("private-window add must be absorbed: %+v", added)
	}
	if reduced.LastKnownSize != 10 {
		t.Fatalf("private-window reduction must remain actionable: %+v", reduced)
	}
	if newMapping == nil || newMapping.Status != "ignored" || newMapping.LastKnownSize != 4 {
		t.Fatalf("private-window new position baseline: %+v", newMapping)
	}
}

type smartObservingProvider struct {
	state *AccountState
	obs   SourceHealthObservation
}

func (p *smartObservingProvider) GetFills(string, time.Time) ([]Fill, error) { return nil, nil }
func (p *smartObservingProvider) GetAccountState(string) (*AccountState, error) {
	return p.state, nil
}
func (p *smartObservingProvider) Type() ProviderType { return ProviderBinance }
func (p *smartObservingProvider) LastSourceHealthObservation() SourceHealthObservation {
	return p.obs
}

// TestSmartMoneyHealthySteadyStateDoesNotAbsorbNewPosition 复现 GLWUSDT 漏跟
// 单根因的引擎级回归：健康状态下相邻两轮快照之间出现的新仓位，必须走开仓
// 信号路径，绝不能被"断供恢复 rebaseline"吸收为 ignored 基线。旧实现因
// 健康表时间戳落库损坏（LastCompleteSnapshotAt 读回恒 nil）导致每轮都误判
// 需要恢复，新仓在信号生成前就被吸收。
func TestSmartMoneyHealthySteadyStateDoesNotAbsorbNewPosition(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.SourceGeneration = 1
	e.config.LeaderID = "5082050984257986817"

	existing := makeSmartPosition("smart_old", "BTCUSDC", 2, true)
	provider := &smartObservingProvider{
		state: &AccountState{
			TotalEquity: 1000,
			Positions:   map[string]*Position{existing.PosID: existing},
			Timestamp:   time.Now(), // 故意保留单调时钟，与生产一致
		},
		obs: SourceHealthObservation{Status: "HEALTHY", CompleteSnapshot: true, CheckedAt: time.Now()},
	}
	e.provider = provider

	// 第一轮：建立 HEALTHY 健康记录与完整快照时间
	if err := e.syncLeaderState(); err != nil {
		t.Fatalf("first healthy sync: %v", err)
	}

	// 第二轮（3 秒后的下一次轮询）：领航员新开 GLWUSDT
	newPos := makeSmartPosition("smart_glw", "GLWUSDT", 500, true)
	next := time.Now()
	provider.state = &AccountState{
		TotalEquity: 1000,
		Positions:   map[string]*Position{existing.PosID: existing, newPos.PosID: newPos},
		Timestamp:   next,
	}
	provider.obs = SourceHealthObservation{Status: "HEALTHY", CompleteSnapshot: true, CheckedAt: next}
	if err := e.syncLeaderState(); err != nil {
		t.Fatalf("second healthy sync: %v", err)
	}

	mapping, err := st.CopyTrade().GetMapping(e.traderID, newPos.PosID)
	if err != nil {
		t.Fatal(err)
	}
	if mapping != nil {
		t.Fatalf("healthy steady-state new position must not be absorbed into baseline: %+v", mapping)
	}

	fills := e.detectBinancePositionSnapshotFills()
	var open *Fill
	for i := range fills {
		if fills[i].Symbol == "GLWUSDT" && fills[i].Action == ActionOpen {
			open = &fills[i]
		}
	}
	if open == nil || open.Size != 500 {
		t.Fatalf("new position must emit an open signal, fills=%+v", fills)
	}
}

func TestSmartMoneyDirectionReversalClosesBeforeOpening(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.LeaderID = "5082050984257986817"
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: e.traderID, LeaderPosID: "old-long", LeaderID: e.config.LeaderID,
		Symbol: "BTCUSDC", Side: string(SideLong), MarginMode: "cross",
		OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	short := makeSmartPosition("new-short", "BTCUSDC", 1, true)
	short.Side = SideShort
	e.leaderState = &AccountState{TotalEquity: 1000, Positions: map[string]*Position{short.PosID: short}}

	fills := e.detectBinancePositionSnapshotFills()
	if len(fills) != 2 || fills[0].Action != ActionClose || fills[0].PositionSide != SideLong ||
		fills[1].Action != ActionOpen || fills[1].PositionSide != SideShort {
		t.Fatalf("direction reversal must close old side before opening new side: %+v", fills)
	}
}

func TestSmartMoneyProviderConfirmedEmptyDoesNotWaitTwice(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	saveActiveMapping(t, st, "active-position", 1)
	state := &AccountState{
		Positions:              map[string]*Position{},
		Timestamp:              time.Now(),
		EmptySnapshotConfirmed: true,
	}
	if err := e.confirmSmartMoneyEmptySnapshot(state, state.Timestamp); err != nil {
		t.Fatalf("provider-confirmed empty snapshot must not enter a second confirmation window: %v", err)
	}
}

func TestSmartMoneyVisibilityRaceCannotExecuteCachedSignal(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.LeaderID = "5082050984257986817"
	pos := makeSmartPosition("race", "BTCUSDC", 1, true)
	e.leaderState = &AccountState{TotalEquity: 1000, Positions: map[string]*Position{"race": pos}}
	e.provider = &smartEngineProvider{err: ErrBinanceSmartMoneyPrivate}
	fill := e.buildBinanceSnapshotFillForPosition(pos, "race", ActionOpen, 1, 0, 1, 0)
	e.markSeen(fill.ID)
	e.processSignal(e.buildSignal(&fill))
	select {
	case got := <-e.decisionCh:
		t.Fatalf("cached signal executed after privacy transition: %+v", got)
	default:
	}
	if e.isSeen(fill.ID) {
		t.Fatal("privacy race must release fill for healthy replay")
	}
	if !errors.Is(e.provider.(*smartEngineProvider).err, ErrBinanceSmartMoneyPrivate) {
		t.Fatal("test setup")
	}
}

func TestSmartMoneySnapshotRevisionIsRetryStableAndDistinguishesRepeatedTransitions(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.SourceGeneration = 7
	e.config.LeaderID = "5082050984257986817"
	const posID = "smart-repeat"
	saveActiveMapping(t, st, posID, 1)

	e.leaderState.Positions[posID] = makeSmartPosition(posID, "BTCUSDC", 2, true)
	firstAdd := e.detectBinancePositionSnapshotFills()
	retryAdd := e.detectBinancePositionSnapshotFills()
	if len(firstAdd) != 1 || len(retryAdd) != 1 || firstAdd[0].ID != retryAdd[0].ID {
		t.Fatalf("unacknowledged transition must keep one retry identity: first=%+v retry=%+v", firstAdd, retryAdd)
	}
	if !strings.Contains(firstAdd[0].ID, "|g7|r1|") {
		t.Fatalf("Smart Money identity must include persisted generation/revision: %s", firstAdd[0].ID)
	}
	firstClientID := stableClientOrderID(e.traderID, firstAdd[0].ID, "open_long")

	if err := st.CopyTrade().UpdateLastKnownSize(e.traderID, posID, 2); err != nil {
		t.Fatal(err)
	}
	e.leaderState.Positions[posID] = makeSmartPosition(posID, "BTCUSDC", 1, true)
	reduced := e.detectBinancePositionSnapshotFills()
	if len(reduced) != 1 || reduced[0].Action != ActionReduce {
		t.Fatalf("expected acknowledged add to expose reduction: %+v", reduced)
	}
	if err := st.CopyTrade().UpdateLastKnownSize(e.traderID, posID, 1); err != nil {
		t.Fatal(err)
	}

	e.leaderState.Positions[posID] = makeSmartPosition(posID, "BTCUSDC", 2, true)
	secondAdd := e.detectBinancePositionSnapshotFills()
	if len(secondAdd) != 1 || secondAdd[0].Action != ActionAdd {
		t.Fatalf("expected repeated add transition: %+v", secondAdd)
	}
	if firstAdd[0].ID == secondAdd[0].ID {
		t.Fatalf("1->2->1->2 must not reuse a synthetic fill ID: %s", firstAdd[0].ID)
	}
	secondClientID := stableClientOrderID(e.traderID, secondAdd[0].ID, "open_long")
	if firstClientID == secondClientID {
		t.Fatalf("repeated lifecycle transition must not reuse client order ID: %s", firstClientID)
	}
}

func TestSmartMoneySnapshotRevisionDistinguishesCloseReopenAndGeneration(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.SourceGeneration = 3
	e.config.LeaderID = "5082050984257986817"
	const posID = "smart-reopen"
	pos := makeSmartPosition(posID, "ETHUSDC", 1, true)
	e.leaderState.Positions[posID] = pos

	initialOpen := e.detectBinancePositionSnapshotFills()
	initialOpenRetry := e.detectBinancePositionSnapshotFills()
	if len(initialOpen) != 1 || len(initialOpenRetry) != 1 || initialOpen[0].ID != initialOpenRetry[0].ID {
		t.Fatalf("first open identity must remain stable before acknowledgement: first=%+v retry=%+v", initialOpen, initialOpenRetry)
	}
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: e.traderID, LeaderPosID: posID, LeaderID: e.config.LeaderID,
		Symbol: pos.Symbol, Side: string(pos.Side), MarginMode: pos.MarginMode,
		OpenedAt: time.Now(), OpenPrice: pos.EntryPrice, LastKnownSize: pos.Size,
	}); err != nil {
		t.Fatal(err)
	}

	e.leaderState.Positions = map[string]*Position{}
	closed := e.detectBinancePositionSnapshotFills()
	closedRetry := e.detectBinancePositionSnapshotFills()
	if len(closed) != 1 || len(closedRetry) != 1 || closed[0].ID != closedRetry[0].ID {
		t.Fatalf("close identity must remain stable before acknowledgement: close=%+v retry=%+v", closed, closedRetry)
	}
	if err := st.CopyTrade().CloseMapping(e.traderID, posID, pos.MarkPrice); err != nil {
		t.Fatal(err)
	}

	e.leaderState.Positions[posID] = pos
	reopened := e.detectBinancePositionSnapshotFills()
	if len(reopened) != 1 || reopened[0].Action != ActionOpen {
		t.Fatalf("expected reopened lifecycle: %+v", reopened)
	}
	if reopened[0].ID == initialOpen[0].ID {
		t.Fatalf("close/reopen must not reuse the first lifecycle fill ID: %s", reopened[0].ID)
	}
	if stableClientOrderID(e.traderID, reopened[0].ID, "open_long") == stableClientOrderID(e.traderID, initialOpen[0].ID, "open_long") {
		t.Fatal("close/reopen must not reuse the first lifecycle client order ID")
	}

	e.config.SourceGeneration++
	nextGeneration := e.detectBinancePositionSnapshotFills()
	if len(nextGeneration) != 1 || nextGeneration[0].ID == reopened[0].ID {
		t.Fatalf("source generation change must produce a distinct identity: old=%+v new=%+v", reopened, nextGeneration)
	}
}

func TestCopyManagementSnapshotIdentityFormatRemainsBackwardCompatible(t *testing.T) {
	e, _ := newTestCopyTradeEngine(t, ProviderBinance)
	fill := e.buildBinanceSnapshotFill("legacy-pos", "BTCUSDT", SideLong, ActionAdd, 1, 100, 1, 2, 99)
	want := "binance_snapshot|legacy-pos|add|1.00000000|2.00000000"
	if fill.ID != want {
		t.Fatalf("copy_management snapshot identity changed: got %q want %q", fill.ID, want)
	}
}

func TestSmartMoneyRiskReductionCarriesStoredExecutionContract(t *testing.T) {
	e, st := newTestCopyTradeEngine(t, ProviderBinance)
	e.config.BinanceSourceMode = BinanceSourceSmartMoney
	e.config.LeaderID = "5082050984257986817"
	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: e.traderID, LeaderPosID: "mapped", LeaderID: e.config.LeaderID,
		Symbol: "BTCUSDC", SourceSymbol: "BTCUSDC", ExecutionSymbol: "BTC-USDC-SWAP",
		SourceQuoteAsset: "USDC", ExecutionSettleAsset: "USDC",
		Side: "long", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 2,
	}); err != nil {
		t.Fatal(err)
	}
	position := makeSmartPosition("mapped", "BTCUSDC", 1, true)
	e.leaderState = &AccountState{Positions: map[string]*Position{"mapped": position}}
	fill := e.buildBinanceSnapshotFillForPosition(position, "mapped", ActionReduce, 1, 2, 1, 1)
	signal := e.buildSignal(&fill)
	match := e.matchSignalWithMapping(signal)
	if !match.ShouldFollow || match.Action != ActionReduce {
		t.Fatalf("expected mapped reduction, got %+v", match)
	}
	dec := e.buildDecisionV2(signal, match, 0)
	if dec.SourceSymbol != "BTCUSDC" || dec.ExecutionSymbol != "BTC-USDC-SWAP" ||
		dec.ValueCurrency != "USDC" || dec.ExecutionSettleAsset != "USDC" {
		t.Fatalf("stored execution identity not carried to reduction: %+v", dec)
	}

	if err := st.CopyTrade().SavePositionMapping(&store.CopyTradePositionMapping{
		TraderID: e.traderID, LeaderPosID: "mapped-add", LeaderID: e.config.LeaderID,
		Symbol: "ETHUSDC", SourceSymbol: "ETHUSDC", ExecutionSymbol: "ETH-USDC-SWAP",
		SourceQuoteAsset: "USDC", ExecutionSettleAsset: "USDC",
		Side: "long", MarginMode: "cross", OpenedAt: time.Now(), LastKnownSize: 1,
	}); err != nil {
		t.Fatal(err)
	}
	addPosition := makeSmartPosition("mapped-add", "ETHUSDC", 2, true)
	e.leaderState.Positions["mapped-add"] = addPosition
	addFill := e.buildBinanceSnapshotFillForPosition(addPosition, "mapped-add", ActionAdd, 1, 1, 2, 1)
	addSignal := e.buildSignal(&addFill)
	addMatch := e.matchSignalWithMapping(addSignal)
	addDecision := e.buildDecisionV2(addSignal, addMatch, 100)
	if !addMatch.ShouldFollow || addMatch.Action != ActionAdd ||
		addDecision.SourceSymbol != "ETHUSDC" || addDecision.ExecutionSymbol != "ETH-USDC-SWAP" {
		t.Fatalf("stored execution identity not carried to add: match=%+v decision=%+v", addMatch, addDecision)
	}
}

type captureDecisionExecutor struct {
	symbol string
}

func (e *captureDecisionExecutor) ExecuteDecision(dec *decision.Decision) error {
	e.symbol = dec.Symbol
	dec.ExchangeOrderID = "executed-order"
	return nil
}
func (e *captureDecisionExecutor) GetAccountInfo() (map[string]interface{}, error) {
	return nil, nil
}
func (e *captureDecisionExecutor) GetPositions() ([]map[string]interface{}, error) {
	return nil, nil
}

func TestExecutionKeepsNormalizedSymbolAndPreservesNativeContractMetadata(t *testing.T) {
	executor := &captureDecisionExecutor{}
	ti := &TraderIntegration{executor: executor}
	dec := &decision.Decision{Symbol: "BTCUSDC", SourceSymbol: "BTCUSDC", ExecutionSymbol: "BTC-USDC-SWAP", Action: "close_long"}
	if err := ti.executeDecisionWithRetry(dec); err != nil {
		t.Fatal(err)
	}
	if executor.symbol != "BTCUSDC" {
		t.Fatalf("executor received %q; normalized symbol must reach AutoTrader position lookup", executor.symbol)
	}
	if dec.Symbol != "BTCUSDC" || dec.ExecutionSymbol != "BTC-USDC-SWAP" || dec.ExchangeOrderID != "executed-order" {
		t.Fatalf("source/native identity or execution result lost: %+v", dec)
	}
}
