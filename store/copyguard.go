package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	CopyGuardFollowing         = "FOLLOWING"
	CopyGuardStopTriggered     = "STOP_TRIGGERED"
	CopyGuardStoppedWatching   = "STOPPED_WATCHING"
	CopyGuardReentryPending    = "REENTRY_PENDING"
	CopyGuardFollowingReentry  = "FOLLOWING_REENTRY"
	CopyGuardLeaderClosed      = "LEADER_CLOSED"
	CopyGuardLeaderReversed    = "LEADER_REVERSED"
	CopyGuardAttemptsExhausted = "ATTEMPTS_EXHAUSTED"
	CopyGuardWatchTimeout      = "WATCH_TIMEOUT"
	CopyGuardProtectionError   = "PROTECTION_ERROR"
)

// CopyGuardPolicy is persisted as JSON so v4 can evolve without widening the legacy v3 table.
type CopyGuardPolicy struct {
	Version              int     `json:"version"`
	StopMode             string  `json:"stop_mode"`
	ATRPeriod            int     `json:"atr_period"`
	ATRFallbackPct       float64 `json:"atr_fallback_pct"`
	TriggerPriceType     string  `json:"trigger_price_type"`
	SlippageBufferBPS    float64 `json:"slippage_buffer_bps"`
	LiquidationBufferATR float64 `json:"liquidation_buffer_atr"`
	MaxReentries         int     `json:"max_reentries"`
	ReentryBandATR       float64 `json:"reentry_band_atr"`
	ReentryCooldownSec   int     `json:"reentry_cooldown_seconds"`
	ReentryMaxChaseATR   float64 `json:"reentry_max_chase_atr"`
	MaxATRExpansion      float64 `json:"max_atr_expansion"`
	WatchTimeoutMinutes  int     `json:"watch_timeout_minutes"`
	MigrationConfirmed   bool    `json:"migration_confirmed"`
}

type CopyGuardCycle struct {
	ID                 int64      `json:"id"`
	TraderID           string     `json:"trader_id"`
	LeaderID           string     `json:"leader_id"`
	LeaderPosID        string     `json:"leader_pos_id"`
	Symbol             string     `json:"symbol"`
	Side               string     `json:"side"`
	MarginMode         string     `json:"margin_mode"`
	Status             string     `json:"status"`
	PolicySnapshot     string     `json:"policy_snapshot"`
	LeaderEntryPrice   float64    `json:"leader_entry_price"`
	FollowerEntryPrice float64    `json:"follower_entry_price"`
	FollowerNotional   float64    `json:"follower_notional"`
	BaselineNotional   float64    `json:"baseline_notional"`
	AccountEquity      float64    `json:"account_equity"`
	ATRAtEntry         float64    `json:"atr_at_entry"`
	ATRAtStop          float64    `json:"atr_at_stop"`
	LastObservedPrice  float64    `json:"last_observed_price"`
	ReentryCount       int        `json:"reentry_count"`
	StopCount          int        `json:"stop_count"`
	ActualPnL          float64    `json:"actual_pnl"`
	BaselinePnL        float64    `json:"baseline_pnl"`
	Fees               float64    `json:"fees"`
	FundingFee         float64    `json:"funding_fee"`
	Slippage           float64    `json:"slippage"`
	NetGuardEffect     float64    `json:"net_guard_effect"`
	OpenedAt           time.Time  `json:"opened_at"`
	StoppedAt          *time.Time `json:"stopped_at,omitempty"`
	ClosedAt           *time.Time `json:"closed_at,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type CopyGuardEvent struct {
	ID        int64                  `json:"id"`
	CycleID   int64                  `json:"cycle_id"`
	TraderID  string                 `json:"trader_id"`
	Type      string                 `json:"type"`
	Price     float64                `json:"price"`
	Quantity  float64                `json:"quantity"`
	Notional  float64                `json:"notional"`
	PnL       float64                `json:"pnl"`
	Fee       float64                `json:"fee"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
}

func (s *CopyTradeStore) initCopyGuardTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS copy_guard_policies (
			trader_id TEXT PRIMARY KEY,
			policy_json TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (trader_id) REFERENCES traders(id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS copy_guard_cycles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trader_id TEXT NOT NULL, leader_id TEXT NOT NULL, leader_pos_id TEXT NOT NULL,
			symbol TEXT NOT NULL, side TEXT NOT NULL, margin_mode TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'FOLLOWING', policy_snapshot TEXT NOT NULL DEFAULT '{}',
			leader_entry_price REAL DEFAULT 0, follower_entry_price REAL DEFAULT 0,
			follower_notional REAL DEFAULT 0, baseline_notional REAL DEFAULT 0, account_equity REAL DEFAULT 0,
			atr_at_entry REAL DEFAULT 0, atr_at_stop REAL DEFAULT 0, last_observed_price REAL DEFAULT 0,
			reentry_count INTEGER DEFAULT 0, stop_count INTEGER DEFAULT 0,
			actual_pnl REAL DEFAULT 0, baseline_pnl REAL DEFAULT 0, fees REAL DEFAULT 0,
			funding_fee REAL DEFAULT 0, slippage REAL DEFAULT 0, net_guard_effect REAL DEFAULT 0,
			opened_at DATETIME DEFAULT CURRENT_TIMESTAMP, stopped_at DATETIME, closed_at DATETIME,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(trader_id, leader_pos_id, opened_at)
		);
		CREATE INDEX IF NOT EXISTS idx_copy_guard_cycles_filter ON copy_guard_cycles(trader_id, opened_at, status);
		CREATE TABLE IF NOT EXISTS copy_guard_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT, cycle_id INTEGER NOT NULL, attempt_no INTEGER NOT NULL,
			status TEXT NOT NULL, entry_price REAL DEFAULT 0, exit_price REAL DEFAULT 0,
			quantity REAL DEFAULT 0, notional REAL DEFAULT 0, stop_trigger_price REAL DEFAULT 0,
			stop_fill_price REAL DEFAULT 0, stop_algo_id TEXT DEFAULT '', pnl REAL DEFAULT 0,
			fee REAL DEFAULT 0, funding_fee REAL DEFAULT 0, atr REAL DEFAULT 0,
			opened_at DATETIME DEFAULT CURRENT_TIMESTAMP, closed_at DATETIME,
			UNIQUE(cycle_id, attempt_no)
		);
		CREATE TABLE IF NOT EXISTS copy_guard_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, cycle_id INTEGER NOT NULL, trader_id TEXT NOT NULL,
			type TEXT NOT NULL, price REAL DEFAULT 0, quantity REAL DEFAULT 0, notional REAL DEFAULT 0,
			pnl REAL DEFAULT 0, fee REAL DEFAULT 0, metadata_json TEXT DEFAULT '{}',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_copy_guard_events_cycle ON copy_guard_events(cycle_id, created_at);
		CREATE TABLE IF NOT EXISTS copy_guard_protective_orders (
			cycle_id INTEGER PRIMARY KEY, trader_id TEXT NOT NULL, algo_id TEXT NOT NULL,
			algo_client_id TEXT DEFAULT '', symbol TEXT NOT NULL, side TEXT NOT NULL, margin_mode TEXT NOT NULL,
			quantity REAL NOT NULL, trigger_price REAL NOT NULL, trigger_type TEXT NOT NULL,
			status TEXT NOT NULL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err == nil {
		_, _ = s.db.Exec(`ALTER TABLE copy_guard_cycles ADD COLUMN baseline_notional REAL DEFAULT 0`)
	}
	return err
}

type CopyGuardProtectiveOrder struct {
	CycleID      int64   `json:"cycle_id"`
	TraderID     string  `json:"trader_id"`
	AlgoID       string  `json:"algo_id"`
	AlgoClientID string  `json:"algo_client_id"`
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	MarginMode   string  `json:"margin_mode"`
	Quantity     float64 `json:"quantity"`
	TriggerPrice float64 `json:"trigger_price"`
	TriggerType  string  `json:"trigger_type"`
	Status       string  `json:"status"`
}

func (s *CopyTradeStore) UpsertCopyGuardProtectiveOrder(o *CopyGuardProtectiveOrder) error {
	if o == nil {
		return fmt.Errorf("nil protective order")
	}
	_, err := s.db.Exec(`INSERT INTO copy_guard_protective_orders(cycle_id,trader_id,algo_id,algo_client_id,symbol,side,margin_mode,quantity,trigger_price,trigger_type,status) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(cycle_id) DO UPDATE SET algo_id=excluded.algo_id,algo_client_id=excluded.algo_client_id,quantity=excluded.quantity,trigger_price=excluded.trigger_price,trigger_type=excluded.trigger_type,status=excluded.status,updated_at=CURRENT_TIMESTAMP`, o.CycleID, o.TraderID, o.AlgoID, o.AlgoClientID, o.Symbol, o.Side, o.MarginMode, o.Quantity, o.TriggerPrice, o.TriggerType, o.Status)
	return err
}

func (s *CopyTradeStore) GetCopyGuardProtectiveOrder(cycleID int64) (*CopyGuardProtectiveOrder, error) {
	var o CopyGuardProtectiveOrder
	err := s.db.QueryRow(`SELECT cycle_id,trader_id,algo_id,algo_client_id,symbol,side,margin_mode,quantity,trigger_price,trigger_type,status FROM copy_guard_protective_orders WHERE cycle_id=?`, cycleID).Scan(&o.CycleID, &o.TraderID, &o.AlgoID, &o.AlgoClientID, &o.Symbol, &o.Side, &o.MarginMode, &o.Quantity, &o.TriggerPrice, &o.TriggerType, &o.Status)
	return &o, err
}

func (s *CopyTradeStore) UpdateCopyGuardProtectiveOrderStatus(cycleID int64, status string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_protective_orders SET status=?,updated_at=CURRENT_TIMESTAMP WHERE cycle_id=?`, status, cycleID)
	return err
}

func (s *CopyTradeStore) ListActiveCopyGuardProtectiveOrders(traderID string) ([]*CopyGuardProtectiveOrder, error) {
	rows, err := s.db.Query(`SELECT cycle_id,trader_id,algo_id,algo_client_id,symbol,side,margin_mode,quantity,trigger_price,trigger_type,status FROM copy_guard_protective_orders WHERE trader_id=? AND status='live'`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardProtectiveOrder{}
	for rows.Next() {
		var o CopyGuardProtectiveOrder
		if err := rows.Scan(&o.CycleID, &o.TraderID, &o.AlgoID, &o.AlgoClientID, &o.Symbol, &o.Side, &o.MarginMode, &o.Quantity, &o.TriggerPrice, &o.TriggerType, &o.Status); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func policyFromConfig(c *CopyTradeConfig) CopyGuardPolicy {
	return CopyGuardPolicy{c.RiskPolicyVersion, c.RiskStopMode, c.RiskATRPeriod, c.RiskATRFallbackPct,
		c.RiskTriggerPriceType, c.RiskSlippageBufferBPS, c.RiskLiquidationBufferATR, c.RiskMaxReentries,
		c.RiskReentryBandATR, c.RiskReentryCooldownSeconds, c.RiskReentryMaxChaseATR,
		c.RiskReentryMaxATRExpansion, c.RiskWatchTimeoutMinutes, c.RiskMigrationConfirmed}
}

func (s *CopyTradeStore) saveCopyGuardPolicy(c *CopyTradeConfig) error {
	if c == nil || c.RiskPolicyVersion < 4 {
		return nil
	}
	b, err := json.Marshal(policyFromConfig(c))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO copy_guard_policies(trader_id, policy_json, updated_at) VALUES(?,?,CURRENT_TIMESTAMP)
		ON CONFLICT(trader_id) DO UPDATE SET policy_json=excluded.policy_json, updated_at=CURRENT_TIMESTAMP`, c.TraderID, string(b))
	return err
}

func (s *CopyTradeStore) loadCopyGuardPolicy(c *CopyTradeConfig) error {
	var raw string
	if err := s.db.QueryRow(`SELECT policy_json FROM copy_guard_policies WHERE trader_id=?`, c.TraderID).Scan(&raw); err != nil {
		return err
	}
	var p CopyGuardPolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return err
	}
	c.RiskPolicyVersion, c.RiskStopMode, c.RiskATRPeriod = p.Version, p.StopMode, p.ATRPeriod
	c.RiskATRFallbackPct, c.RiskTriggerPriceType = p.ATRFallbackPct, p.TriggerPriceType
	c.RiskSlippageBufferBPS, c.RiskLiquidationBufferATR = p.SlippageBufferBPS, p.LiquidationBufferATR
	c.RiskMaxReentries, c.RiskReentryBandATR = p.MaxReentries, p.ReentryBandATR
	c.RiskReentryCooldownSeconds, c.RiskReentryMaxChaseATR = p.ReentryCooldownSec, p.ReentryMaxChaseATR
	c.RiskReentryMaxATRExpansion, c.RiskWatchTimeoutMinutes = p.MaxATRExpansion, p.WatchTimeoutMinutes
	c.RiskMigrationConfirmed = p.MigrationConfirmed
	return nil
}

func (s *CopyTradeStore) EnsureCopyGuardCycle(c *CopyGuardCycle) (*CopyGuardCycle, error) {
	if c == nil {
		return nil, fmt.Errorf("nil copy guard cycle")
	}
	var existing int64
	err := s.db.QueryRow(`SELECT id FROM copy_guard_cycles WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL ORDER BY id DESC LIMIT 1`, c.TraderID, c.LeaderPosID).Scan(&existing)
	if err == nil {
		return s.GetCopyGuardCycle(existing)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	res, err := s.db.Exec(`INSERT INTO copy_guard_cycles(trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,leader_entry_price,follower_entry_price,follower_notional,baseline_notional,account_equity,atr_at_entry,last_observed_price) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.TraderID, c.LeaderID, c.LeaderPosID, c.Symbol, c.Side, c.MarginMode, c.Status, c.PolicySnapshot, c.LeaderEntryPrice, c.FollowerEntryPrice, c.FollowerNotional, c.FollowerNotional, c.AccountEquity, c.ATRAtEntry, c.LastObservedPrice)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetCopyGuardCycle(id)
}

func (s *CopyTradeStore) GetCopyGuardCycle(id int64) (*CopyGuardCycle, error) {
	row := s.db.QueryRow(`SELECT id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,leader_entry_price,follower_entry_price,follower_notional,baseline_notional,account_equity,atr_at_entry,atr_at_stop,last_observed_price,reentry_count,stop_count,actual_pnl,baseline_pnl,fees,funding_fee,slippage,net_guard_effect,opened_at,stopped_at,closed_at,updated_at FROM copy_guard_cycles WHERE id=?`, id)
	return scanCopyGuardCycle(row)
}

func (s *CopyTradeStore) GetOpenCopyGuardCycle(traderID, leaderPosID string) (*CopyGuardCycle, error) {
	row := s.db.QueryRow(`SELECT id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,leader_entry_price,follower_entry_price,follower_notional,baseline_notional,account_equity,atr_at_entry,atr_at_stop,last_observed_price,reentry_count,stop_count,actual_pnl,baseline_pnl,fees,funding_fee,slippage,net_guard_effect,opened_at,stopped_at,closed_at,updated_at FROM copy_guard_cycles WHERE trader_id=? AND leader_pos_id=? AND closed_at IS NULL ORDER BY id DESC LIMIT 1`, traderID, leaderPosID)
	return scanCopyGuardCycle(row)
}

type rowScanner interface{ Scan(...interface{}) error }

func scanCopyGuardCycle(row rowScanner) (*CopyGuardCycle, error) {
	var c CopyGuardCycle
	var opened, updated string
	var stopped, closed sql.NullString
	err := row.Scan(&c.ID, &c.TraderID, &c.LeaderID, &c.LeaderPosID, &c.Symbol, &c.Side, &c.MarginMode, &c.Status, &c.PolicySnapshot, &c.LeaderEntryPrice, &c.FollowerEntryPrice, &c.FollowerNotional, &c.BaselineNotional, &c.AccountEquity, &c.ATRAtEntry, &c.ATRAtStop, &c.LastObservedPrice, &c.ReentryCount, &c.StopCount, &c.ActualPnL, &c.BaselinePnL, &c.Fees, &c.FundingFee, &c.Slippage, &c.NetGuardEffect, &opened, &stopped, &closed, &updated)
	if err != nil {
		return nil, err
	}
	c.OpenedAt, _ = time.Parse("2006-01-02 15:04:05", opened)
	c.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updated)
	if stopped.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", stopped.String)
		c.StoppedAt = &t
	}
	if closed.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", closed.String)
		c.ClosedAt = &t
	}
	return &c, nil
}

func (s *CopyTradeStore) UpdateCopyGuardObservation(id int64, status string, leaderEntry, lastPrice, atr float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET status=?,leader_entry_price=?,last_observed_price=?,atr_at_stop=CASE WHEN ? > 0 THEN ? ELSE atr_at_stop END,updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, leaderEntry, lastPrice, atr, atr, id)
	return err
}

func (s *CopyTradeStore) UpdateCopyGuardShadow(id int64, leaderEntry, lastPrice, baselineNotional float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET leader_entry_price=?,last_observed_price=?,baseline_notional=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, leaderEntry, lastPrice, baselineNotional, id)
	return err
}

func (s *CopyTradeStore) MarkCopyGuardStopped(id int64, atr float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET status=?,atr_at_stop=?,stop_count=stop_count+1,stopped_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, CopyGuardStoppedWatching, atr, id)
	return err
}

func (s *CopyTradeStore) MarkCopyGuardReentrySucceeded(id int64, entryPrice, notional float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET status=?,reentry_count=reentry_count+1,follower_entry_price=?,follower_notional=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, CopyGuardFollowingReentry, entryPrice, notional, id)
	return err
}

func (s *CopyTradeStore) AddCopyGuardActualPnL(id int64, pnl, fee, slippage float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET actual_pnl=actual_pnl+?,fees=fees+?,slippage=slippage+?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, pnl, fee, slippage, id)
	return err
}

func (s *CopyTradeStore) CloseCopyGuardCycle(id int64, status string, actual, baseline, fees, funding, slippage float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_cycles SET status=?,actual_pnl=?,baseline_pnl=?,fees=?,funding_fee=?,slippage=?,net_guard_effect=?-?,closed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, actual, baseline, fees, funding, slippage, actual, baseline, id)
	return err
}

func (s *CopyTradeStore) SaveCopyGuardEvent(e *CopyGuardEvent) error {
	if e == nil {
		return fmt.Errorf("nil copy guard event")
	}
	b, _ := json.Marshal(e.Metadata)
	_, err := s.db.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,quantity,notional,pnl,fee,metadata_json) VALUES(?,?,?,?,?,?,?,?,?)`, e.CycleID, e.TraderID, e.Type, e.Price, e.Quantity, e.Notional, e.PnL, e.Fee, string(b))
	return err
}

func (s *CopyTradeStore) OpenCopyGuardAttempt(cycleID int64, attempt int, entryPrice, notional, quantity, atr float64) error {
	_, err := s.db.Exec(`INSERT INTO copy_guard_attempts(cycle_id,attempt_no,status,entry_price,quantity,notional,atr) VALUES(?,?,'OPEN',?,?,?,?) ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET status='OPEN',entry_price=excluded.entry_price,quantity=excluded.quantity,notional=excluded.notional,atr=excluded.atr,opened_at=CURRENT_TIMESTAMP,closed_at=NULL`, cycleID, attempt, entryPrice, quantity, notional, atr)
	return err
}
func (s *CopyTradeStore) CloseCopyGuardAttempt(cycleID int64, attempt int, exitPrice, pnl, fee float64, algoID string) error {
	_, err := s.db.Exec(`UPDATE copy_guard_attempts SET status='STOPPED',exit_price=?,stop_fill_price=?,pnl=?,fee=?,stop_algo_id=?,closed_at=CURRENT_TIMESTAMP WHERE cycle_id=? AND attempt_no=?`, exitPrice, exitPrice, pnl, fee, algoID, cycleID, attempt)
	return err
}

type CopyGuardSummary struct {
	FollowerCount   int     `json:"follower_count"`
	CycleCount      int     `json:"cycle_count"`
	StopCount       int     `json:"stop_count"`
	ReentryCount    int     `json:"reentry_count"`
	ActualPnL       float64 `json:"actual_pnl"`
	BaselinePnL     float64 `json:"baseline_pnl"`
	AvoidedLoss     float64 `json:"avoided_loss"`
	OpportunityCost float64 `json:"opportunity_cost"`
	NetGuardEffect  float64 `json:"net_guard_effect"`
	Fees            float64 `json:"fees"`
	FundingFee      float64 `json:"funding_fee"`
	Slippage        float64 `json:"slippage"`
}

type CopyGuardFilter struct {
	LeaderID string
	Symbol   string
	Status   string
}

func appendCopyGuardFilter(q string, args []interface{}, f CopyGuardFilter) (string, []interface{}) {
	if f.LeaderID != "" {
		q += " AND leader_id=?"
		args = append(args, f.LeaderID)
	}
	if f.Symbol != "" {
		q += " AND symbol=?"
		args = append(args, strings.ToUpper(f.Symbol))
	}
	if f.Status != "" {
		q += " AND status=?"
		args = append(args, f.Status)
	}
	return q, args
}

func (s *CopyTradeStore) CopyGuardSummary(traderIDs []string, from, to time.Time, filter CopyGuardFilter) (*CopyGuardSummary, error) {
	if len(traderIDs) == 0 {
		return &CopyGuardSummary{}, nil
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+2)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, from, to)
	q := `SELECT COUNT(DISTINCT trader_id),COUNT(*),COALESCE(SUM(stop_count),0),COALESCE(SUM(reentry_count),0),COALESCE(SUM(actual_pnl),0),COALESCE(SUM(baseline_pnl),0),COALESCE(SUM(CASE WHEN net_guard_effect>0 THEN net_guard_effect ELSE 0 END),0),COALESCE(SUM(CASE WHEN net_guard_effect<0 THEN -net_guard_effect ELSE 0 END),0),COALESCE(SUM(net_guard_effect),0),COALESCE(SUM(fees),0),COALESCE(SUM(funding_fee),0),COALESCE(SUM(slippage),0) FROM copy_guard_cycles WHERE trader_id IN (` + marks + `) AND opened_at>=? AND opened_at<?`
	q, args = appendCopyGuardFilter(q, args, filter)
	var x CopyGuardSummary
	err := s.db.QueryRow(q, args...).Scan(&x.FollowerCount, &x.CycleCount, &x.StopCount, &x.ReentryCount, &x.ActualPnL, &x.BaselinePnL, &x.AvoidedLoss, &x.OpportunityCost, &x.NetGuardEffect, &x.Fees, &x.FundingFee, &x.Slippage)
	return &x, err
}

func (s *CopyTradeStore) ListCopyGuardCycles(traderIDs []string, from, to time.Time, limit, offset int, filter CopyGuardFilter) ([]*CopyGuardCycle, error) {
	if len(traderIDs) == 0 {
		return []*CopyGuardCycle{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	marks := strings.TrimRight(strings.Repeat("?,", len(traderIDs)), ",")
	args := make([]interface{}, 0, len(traderIDs)+4)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, from, to)
	q := `SELECT id,trader_id,leader_id,leader_pos_id,symbol,side,margin_mode,status,policy_snapshot,leader_entry_price,follower_entry_price,follower_notional,baseline_notional,account_equity,atr_at_entry,atr_at_stop,last_observed_price,reentry_count,stop_count,actual_pnl,baseline_pnl,fees,funding_fee,slippage,net_guard_effect,opened_at,stopped_at,closed_at,updated_at FROM copy_guard_cycles WHERE trader_id IN (` + marks + `) AND opened_at>=? AND opened_at<?`
	q, args = appendCopyGuardFilter(q, args, filter)
	q += " ORDER BY opened_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardCycle{}
	for rows.Next() {
		c, err := scanCopyGuardCycle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *CopyTradeStore) ListCopyGuardEvents(cycleID int64) ([]*CopyGuardEvent, error) {
	rows, err := s.db.Query(`SELECT id,cycle_id,trader_id,type,price,quantity,notional,pnl,fee,metadata_json,created_at FROM copy_guard_events WHERE cycle_id=? ORDER BY created_at,id`, cycleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardEvent{}
	for rows.Next() {
		var e CopyGuardEvent
		var raw, created string
		if err := rows.Scan(&e.ID, &e.CycleID, &e.TraderID, &e.Type, &e.Price, &e.Quantity, &e.Notional, &e.PnL, &e.Fee, &raw, &created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(raw), &e.Metadata)
		e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", created)
		out = append(out, &e)
	}
	return out, rows.Err()
}
