package trader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// Quantities and relative clocks are the 14 immutable fills observed in the
// ZEC incident. IDs are anonymous. The full adapter test shifts the date into
// the venue's retention window; the one-millisecond discrepancy is unchanged.
func zecIncidentRows(opened time.Time) []map[string]string {
	var rows []map[string]string
	appendEntry := func(order string, at time.Time, quantities []int) {
		for _, quantity := range quantities {
			id := strconv.Itoa(len(rows) + 1)
			rows = append(rows, map[string]string{"billId": id, "tradeId": id, "ordId": order, "instId": "ZEC-USDT-SWAP", "side": "buy", "posSide": "long", "fillSz": strconv.Itoa(quantity), "fillPx": "1477.1441", "fillPnl": "0", "fee": "0", "fillTime": strconv.FormatInt(at.UnixMilli(), 10), "ts": strconv.FormatInt(at.Add(time.Millisecond).UnixMilli(), 10)})
		}
	}
	appendEntry("first", opened.Add(-time.Millisecond), []int{587, 160, 10, 44, 159, 230, 60, 1, 121})
	appendEntry("add-1", opened.Add(15613*time.Millisecond), []int{55, 17})
	appendEntry("add-2", opened.Add(33360*time.Millisecond), []int{3, 1})
	closed := opened.Add(576793 * time.Millisecond)
	rows = append(rows, map[string]string{"billId": "14", "tradeId": "liquidation", "ordId": "liquidation-order", "instId": "ZEC-USDT-SWAP", "side": "sell", "posSide": "long", "fillSz": "1448", "fillPx": "1462.93", "fillPnl": "-205.8198", "fee": "-10.5916132", "fillTime": strconv.FormatInt(closed.UnixMilli(), 10), "ts": strconv.FormatInt(closed.UnixMilli(), 10)})
	return rows
}

func zecHistoryRow(opened time.Time) map[string]string {
	return map[string]string{"posId": "incident-pos", "instId": "ZEC-USDT-SWAP", "direction": "long", "mgnMode": "isolated", "cTime": strconv.FormatInt(opened.UnixMilli(), 10), "uTime": strconv.FormatInt(opened.Add(576793*time.Millisecond).UnixMilli(), 10), "closeTotalPos": "1448", "pnl": "-205.8198", "realizedPnl": "-438.9382003", "fee": "-21.2861363", "fundingFee": "0", "liqPenalty": "-211.832264", "type": "3", "openAvgPx": "1477.1441", "closeAvgPx": "1462.93", "lever": "50"}
}

func okxSettlementResponse(rows interface{}) (int, string) {
	raw, _ := json.Marshal(map[string]interface{}{"code": "0", "data": rows})
	return 200, string(raw)
}

func TestZECLiquidationRawAPIProofAndThreeLotSettlement(t *testing.T) {
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	closed := opened.Add(576793 * time.Millisecond)
	rows := zecIncidentRows(opened)
	modeQueries := map[string]int{}
	missingFirstFill := true
	ex := newOKXTestServer(t, func(path string) (int, string) {
		u, _ := url.Parse(path)
		switch u.Path {
		case "/api/v5/account/positions-history":
			return okxSettlementResponse([]map[string]string{zecHistoryRow(opened)})
		case okxFillsHistoryPath:
			if u.Query().Get("begin") != strconv.FormatInt(opened.Add(-time.Second).UnixMilli(), 10) || u.Query().Get("end") != strconv.FormatInt(closed.Add(time.Second).UnixMilli(), 10) {
				t.Fatalf("settlement requested unbounded fill evidence: %s", path)
			}
			copyRows := append([]map[string]string{}, rows...)
			if missingFirstFill {
				copyRows = copyRows[1:]
			}
			// Deliberately return a remote out-of-range fill. It must be removed
			// before querying an unrelated order whose lookup would fail.
			copyRows = append(copyRows, map[string]string{"billId": "future", "tradeId": "future", "ordId": "unrelated-future", "instId": "ZEC-USDT-SWAP", "side": "buy", "posSide": "long", "fillSz": "1", "fillPx": "1500", "fillPnl": "0", "fillTime": strconv.FormatInt(closed.Add(time.Minute).UnixMilli(), 10)})
			return okxSettlementResponse(copyRows)
		case okxInstrumentsPath:
			return okxSettlementResponse([]map[string]string{{"instId": "ZEC-USDT-SWAP", "ctVal": "0.01", "lotSz": "1", "minSz": "1", "tickSz": "0.01"}})
		case okxOrderPath:
			id := u.Query().Get("ordId")
			modeQueries[id]++
			if id == "unrelated-future" {
				t.Fatal("unrelated later order blocked old settlement")
			}
			return okxSettlementResponse([]map[string]string{{"ordId": id, "state": "filled", "tdMode": "isolated", "accFillSz": "1", "avgPx": "1477"}})
		default:
			return okxSettlementResponse([]map[string]string{})
		}
	})
	st, err := store.New(filepath.Join(t.TempDir(), "incident.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var lots []*store.TraderPosition
	for i, qty := range []float64{13.72, .72, .04} {
		p := &store.TraderPosition{TraderID: "incident-trader", ExchangeID: "incident-account", ExchangeType: "okx", Symbol: "ZECUSDT", Side: "LONG", Quantity: qty, EntryPrice: 1477.1441, EntryOrderID: []string{"first", "add-1", "add-2"}[i], EntryTime: []time.Time{opened.Add(-time.Millisecond), opened.Add(15613 * time.Millisecond), opened.Add(33360 * time.Millisecond)}[i]}
		if err = st.Position().Create(p); err != nil {
			t.Fatal(err)
		}
		lots = append(lots, p)
	}
	fill := store.PositionCloseFill{TradeID: "trade:ZEC-USDT-SWAP:liquidation", Symbol: "ZECUSDT", Side: "LONG", Quantity: 14.48, ExitPrice: 1462.93, RealizedPnL: -205.8198, Fee: 10.5916132, FillTime: closed}
	if _, err = st.Position().ClosePositionWithAllocations(lots[0].ID, "incident-account", []store.PositionCloseFill{fill}, 1462.93, "unknown"); err != nil {
		t.Fatal(err)
	}
	m := NewPositionSyncManager(st, time.Second)
	m.reconcileOKXSettlements("incident-account", ex)
	diagnostics, err := st.Position().ListSettlementDiagnostics("incident-account", false, 100)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].ReasonCode != "OPENING_FILLS_INCOMPLETE" || diagnostics[0].Stage != "LIFECYCLE_PROOF" {
		t.Fatalf("missing proof not observable: %+v %v", diagnostics, err)
	}
	missingFirstFill = false
	records, err := ex.closedPnLSettlementWindow(opened.Add(-time.Second))
	if err != nil || len(records) != 1 {
		t.Fatalf("history: %+v %v", records, err)
	}
	trades, err := ReadPositionSettlementTrades(ex, records[0])
	if err != nil || len(trades) != 14 {
		t.Fatalf("scoped fills: count=%d %v", len(trades), err)
	}
	settlement, err := BuildPositionSettlement("incident-account", records[0], trades)
	if err != nil {
		t.Fatal(err)
	}
	if !settlement.OpenedAt.Equal(opened) || !settlement.ExchangeClosedAt.Equal(closed) || !settlement.FirstFillAt.Equal(opened.Add(-time.Millisecond)) || !trades[0].RecordTime.Equal(opened) {
		t.Fatalf("record and execution clocks were conflated: %+v first=%+v", settlement, trades[0])
	}
	for i := 0; i < 3; i++ {
		m.reconcileOKXSettlements("incident-account", ex)
	}
	var count, verified int
	var gross, net, fee, penalty float64
	if err = st.DB().QueryRow(`SELECT COUNT(*),SUM(settlement_verified),SUM(gross_pnl),SUM(net_pnl),SUM(fee),SUM(liquidation_penalty) FROM trader_positions WHERE trader_id='incident-trader'`).Scan(&count, &verified, &gross, &net, &fee, &penalty); err != nil {
		t.Fatal(err)
	}
	if count != 3 || verified != 3 || math.Abs(gross+205.8198) > 1e-7 || math.Abs(net+438.9382003) > 1e-7 || math.Abs(fee-21.2861363) > 1e-7 || math.Abs(penalty-211.832264) > 1e-7 {
		t.Fatalf("wrong incident accounting: count=%d verified=%d gross=%v net=%v fee=%v penalty=%v", count, verified, gross, net, fee, penalty)
	}
	var identities int
	if err = st.DB().QueryRow(`SELECT COUNT(*) FROM position_settlements`).Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("clock correction changed identity: %d %v", identities, err)
	}
	diagnostics, err = st.Position().ListSettlementDiagnostics("incident-account", true, 100)
	if err != nil || len(diagnostics) != 1 || diagnostics[0].Status != "RESOLVED" {
		t.Fatalf("resolved diagnostic missing: %+v %v", diagnostics, err)
	}
	for _, p := range lots {
		var when, reason string
		if err = st.DB().QueryRow(`SELECT exit_time,close_reason FROM trader_positions WHERE id=?`, p.ID).Scan(&when, &reason); err != nil || reason != "liquidation" {
			t.Fatalf("wrong terminal evidence %s %s %v", when, reason, err)
		}
		parsed, parseErr := time.Parse(time.RFC3339Nano, when)
		if parseErr != nil || !parsed.Equal(closed) {
			t.Fatalf("close time changed %s %v", when, parseErr)
		}
	}
}

func TestSettlementBoundedClocksDuplicateEvidenceAndAmbiguousOrdering(t *testing.T) {
	start := time.UnixMilli(1790022269524)
	end := start.Add(time.Minute)
	r := ClosedPnLRecord{Symbol: "ZECUSDT", Side: "long", MarginMode: "isolated", QuantityCoins: 2, GrossPnL: -20, RealizedPnL: -33, Fee: 3, LiquidationPenalty: 10, EntryTime: start, ExitTime: end, ExchangeID: "pos"}
	entry := TradeRecord{TradeID: "entry", OrderID: "first", Symbol: r.Symbol, Side: "BUY", PositionSide: "LONG", MarginMode: r.MarginMode, Quantity: 2, Time: start.Add(-time.Millisecond), RecordTime: start}
	exit := TradeRecord{TradeID: "exit", OrderID: "close", Symbol: r.Symbol, Side: "SELL", PositionSide: "LONG", MarginMode: r.MarginMode, Quantity: 2, RealizedPnL: -20, Time: end}
	if _, err := BuildPositionSettlement("account", r, []TradeRecord{exit, entry, entry}); err != nil {
		t.Fatalf("shuffled identical duplicate rejected: %v", err)
	}
	conflict := entry
	conflict.Quantity = 3
	if _, err := BuildPositionSettlement("account", r, []TradeRecord{entry, conflict, exit}); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	tooEarly := entry
	tooEarly.Time = start.Add(-time.Second - time.Millisecond)
	if _, err := BuildPositionSettlement("account", r, []TradeRecord{tooEarly, exit}); err == nil {
		t.Fatal("unbounded early fill accepted")
	}
	reopen := entry
	reopen.TradeID = "reopen"
	reopen.OrderID = "next"
	reopen.Time = end
	for _, fills := range [][]TradeRecord{{entry, exit, reopen}, {reopen, exit, entry}} {
		_, err := BuildPositionSettlement("account", r, fills)
		var proof *SettlementProofError
		if !errors.As(err, &proof) || proof.Code != "FILL_SEQUENCE_AMBIGUOUS" {
			t.Fatalf("same millisecond ambiguity guessed: %v", err)
		}
	}
	older := entry
	older.TradeID = "older"
	older.OrderID = "older"
	older.Time = start.Add(-500 * time.Millisecond)
	olderClose := exit
	olderClose.TradeID = "older-close"
	olderClose.OrderID = "older-close"
	olderClose.Time = start.Add(-400 * time.Millisecond)
	if _, err := BuildPositionSettlement("account", r, []TradeRecord{older, olderClose, entry, exit}); err == nil {
		t.Fatal("adjacent lifecycle merged inside candidate tolerance")
	}
}

func TestOKXFillDuplicateConflictRejectsPartialEvidence(t *testing.T) {
	queries := 0
	ex := newOKXTestServer(t, func(path string) (int, string) {
		if strings.Contains(path, "public/instruments") {
			return okxSettlementResponse([]map[string]string{{"instId": "ZEC-USDT-SWAP", "ctVal": "0.01", "lotSz": "1"}})
		}
		if strings.Contains(path, "fills-history") {
			queries++
			row := zecIncidentRows(time.Now().Add(-time.Minute))[0]
			row["fillTime"] = "1000"
			row["ts"] = "1001"
			if queries > 1 {
				row["fillSz"] = "588"
			}
			return okxSettlementResponse([]map[string]string{row})
		}
		return okxSettlementResponse([]map[string]string{})
	})
	if _, err := ex.GetTradesForSymbol("ZECUSDT", time.Now().Add(-time.Hour), 1); err == nil || !strings.Contains(err.Error(), "conflicting OKX fill identity") {
		t.Fatalf("conflicting page silently deduped: %v", err)
	}
}

func TestOKXSettlementPaginationPreservesRawBoundaryClock(t *testing.T) {
	opened := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	boundary := opened.Add(576793 * time.Millisecond).UnixMilli()
	calls := 0
	ex := newOKXTestServer(t, func(path string) (int, string) {
		u, _ := url.Parse(path)
		if u.Path == okxInstrumentsPath {
			return okxSettlementResponse([]map[string]string{{"instId": "ZEC-USDT-SWAP", "ctVal": "0.01", "lotSz": "1"}})
		}
		if u.Path != "/api/v5/account/positions-history" {
			return okxSettlementResponse([]map[string]string{})
		}
		calls++
		if calls == 1 {
			var rows []map[string]string
			for i := 0; i < 100; i++ {
				r := zecHistoryRow(opened)
				r["posId"] = fmt.Sprintf("pos-%d", i)
				r["uTime"] = strconv.FormatInt(boundary+int64(99-i), 10)
				rows = append(rows, r)
			}
			return okxSettlementResponse(rows)
		}
		if u.Query().Get("after") != strconv.FormatInt(boundary+1, 10) {
			t.Fatalf("raw uTime boundary lost: %s", path)
		}
		tied := zecHistoryRow(opened)
		tied["posId"] = "same-ms-unseen"
		repeated := zecHistoryRow(opened)
		repeated["posId"] = "pos-99"
		return okxSettlementResponse([]map[string]string{repeated, tied})
	})
	rows, err := ex.closedPnLSettlementWindow(opened.Add(-time.Second))
	if err != nil || len(rows) != 101 || calls != 2 {
		t.Fatalf("timestamp pagination lost tied row: rows=%d calls=%d err=%v", len(rows), calls, err)
	}
}
