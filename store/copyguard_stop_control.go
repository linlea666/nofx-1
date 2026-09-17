package store

import (
	"database/sql"
	"fmt"
	"time"
)

type CopyGuardStopControl struct {
	Revision       int64      `json:"revision"`
	ManualPrice    float64    `json:"manual_price"`
	ObservedAlgoID string     `json:"observed_algo_id"`
	RequestedPrice float64    `json:"requested_price"`
	PreviousPrice  float64    `json:"previous_price"`
	RequestPending bool       `json:"request_pending"`
	RequestAt      *time.Time `json:"request_at,omitempty"`
}

func (s *CopyTradeStore) GetCopyGuardStopControl(cycleID int64, attempt int) (*CopyGuardStopControl, error) {
	c := &CopyGuardStopControl{}
	var at sql.NullString
	err := s.db.QueryRow(`SELECT revision,manual_price,observed_algo_id,requested_price,previous_price,request_pending,request_at FROM copy_guard_stop_controls WHERE cycle_id=? AND attempt_no=?`, cycleID, attempt).Scan(&c.Revision, &c.ManualPrice, &c.ObservedAlgoID, &c.RequestedPrice, &c.PreviousPrice, &c.RequestPending, &at)
	if err == sql.ErrNoRows {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.RequestAt, err = parseNullableDBTime(at)
	return c, err
}

func (s *CopyTradeStore) AcceptManualCopyGuardStop(cycleID int64, attempt int, algoID string, price float64) error {
	if invalidPositiveNumber(price) || algoID == "" {
		return fmt.Errorf("invalid manual protective price")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO copy_guard_stop_controls(cycle_id,attempt_no,revision,manual_price,observed_algo_id)
	 VALUES(?,?,1,?,?) ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET revision=revision+1,manual_price=excluded.manual_price,observed_algo_id=excluded.observed_algo_id,request_pending=0,updated_at=CURRENT_TIMESTAMP`, cycleID, attempt, price, algoID); err != nil {
		return err
	}
	// A new user revision may widen. Subsequent automatic changes within that
	// revision still use the existing monotonic liquidation-safety writer.
	res, err := tx.Exec(`UPDATE copy_guard_attempts SET final_stop_price=?,governed_by='manual_exchange' WHERE cycle_id=? AND attempt_no=?`, price, cycleID, attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("manual stop attempt is missing")
	}
	if _, err = tx.Exec(`INSERT INTO copy_guard_events(cycle_id,trader_id,type,price,metadata_json) SELECT id,trader_id,'MANUAL_STOP_ACCEPTED',?,'{"source":"exchange"}' FROM copy_guard_cycles WHERE id=?`, price, cycleID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CopyTradeStore) PrepareCopyGuardStopRequest(cycleID int64, attempt int, price, previous float64) error {
	_, err := s.db.Exec(`INSERT INTO copy_guard_stop_controls(cycle_id,attempt_no,requested_price,previous_price,request_pending,request_at)
	 VALUES(?,?,?,?,1,CURRENT_TIMESTAMP) ON CONFLICT(cycle_id,attempt_no) DO UPDATE SET
	 previous_price=CASE WHEN request_pending=1 AND requested_price=excluded.requested_price THEN previous_price ELSE excluded.previous_price END,
	 request_at=CASE WHEN request_pending=1 AND requested_price=excluded.requested_price THEN request_at ELSE CURRENT_TIMESTAMP END,
	 requested_price=excluded.requested_price,request_pending=1,updated_at=CURRENT_TIMESTAMP`, cycleID, attempt, price, previous)
	return err
}

func (s *CopyTradeStore) ConfirmCopyGuardStopRequest(cycleID int64, attempt int, price float64) error {
	_, err := s.db.Exec(`UPDATE copy_guard_stop_controls SET request_pending=0 WHERE cycle_id=? AND attempt_no=? AND requested_price=?`, cycleID, attempt, price)
	return err
}

// A generation is reserved before the venue call. A timeout reuses it; only a
// conclusively terminal prior order authorizes advancing to another identity.
func (s *CopyTradeStore) CopyGuardProtectionClientID(cycleID int64, attempt int, retire bool) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_guard_stop_controls(cycle_id,attempt_no) VALUES(?,?)`, cycleID, attempt); err != nil {
		return "", err
	}
	if retire {
		if _, err = tx.Exec(`UPDATE copy_guard_stop_controls SET generation=generation+1 WHERE cycle_id=? AND attempt_no=?`, cycleID, attempt); err != nil {
			return "", err
		}
	}
	var generation int64
	if err = tx.QueryRow(`SELECT generation FROM copy_guard_stop_controls WHERE cycle_id=? AND attempt_no=?`, cycleID, attempt).Scan(&generation); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if generation == 0 {
		return fmt.Sprintf("cg%da%d", cycleID, attempt), nil
	}
	return fmt.Sprintf("cg%xA%xG%x", cycleID, attempt, generation), nil
}

type CopyGuardProtectionJob struct {
	CycleID      int64
	LeaderPosID  string
	Revision     int64
	DecisionJSON string
}

func (s *CopyTradeStore) QueueCopyGuardProtection(traderID, leaderPosID string, cycleID int64, decisionJSON string) error {
	_, err := s.db.Exec(`INSERT INTO copy_guard_protection_jobs(trader_id,leader_pos_id,cycle_id,decision_json) VALUES(?,?,?,?) ON CONFLICT(trader_id,leader_pos_id) DO UPDATE SET revision=revision+1,cycle_id=excluded.cycle_id,decision_json=excluded.decision_json,updated_at=CURRENT_TIMESTAMP`, traderID, leaderPosID, cycleID, decisionJSON)
	return err
}
func (s *CopyTradeStore) ListCopyGuardProtectionJobs(traderID string) ([]CopyGuardProtectionJob, error) {
	rows, err := s.db.Query(`SELECT leader_pos_id,cycle_id,revision,decision_json FROM copy_guard_protection_jobs WHERE trader_id=? ORDER BY updated_at`, traderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []CopyGuardProtectionJob
	for rows.Next() {
		var j CopyGuardProtectionJob
		if err = rows.Scan(&j.LeaderPosID, &j.CycleID, &j.Revision, &j.DecisionJSON); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
func (s *CopyTradeStore) CompleteCopyGuardProtectionJob(traderID, leaderPosID string, revision int64) error {
	_, err := s.db.Exec(`DELETE FROM copy_guard_protection_jobs WHERE trader_id=? AND leader_pos_id=? AND revision=?`, traderID, leaderPosID, revision)
	return err
}
