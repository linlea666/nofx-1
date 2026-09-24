package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Protection failures share one incident per lifecycle attempt. SMTP delivery,
// rather than the wall-clock hour or enqueue time, starts the reminder interval.
type ProtectionMailClaim struct {
	ID, CycleID, Sequence int64
	Attempt, Level        int
	Recovery              bool
}

func (s *CopyTradeStore) initProtectionIncidentTable() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS copy_guard_protection_incidents (
 id INTEGER PRIMARY KEY AUTOINCREMENT,cycle_id INTEGER NOT NULL,attempt_no INTEGER NOT NULL,
 opened_ms INTEGER NOT NULL,resolved_ms INTEGER NOT NULL DEFAULT 0,healthy_since_ms INTEGER NOT NULL DEFAULT 0,
 last_sent_ms INTEGER NOT NULL DEFAULT 0,sent_level INTEGER NOT NULL DEFAULT 0,
 recovery_sent_ms INTEGER NOT NULL DEFAULT 0,next_attempt_ms INTEGER NOT NULL DEFAULT 0,
 claim_sequence INTEGER NOT NULL DEFAULT 0,claim_until_ms INTEGER NOT NULL DEFAULT 0,
 delivery_status TEXT NOT NULL DEFAULT '',delivery_error TEXT NOT NULL DEFAULT '');
 CREATE UNIQUE INDEX IF NOT EXISTS idx_guard_protection_incident_open
 ON copy_guard_protection_incidents(cycle_id,attempt_no) WHERE resolved_ms=0;`)
	return err
}

// Resolving an incident only ends its notifications. It never settles an order,
// changes custody or claims a strategy exit. Old queued claims are invalidated.
func (s *CopyTradeStore) ResolveInactiveProtectionIncidents(traderID string, now time.Time) error {
	// A brief successful re-arm followed by another coverage failure belongs
	// to the same incident, otherwise flapping would create fresh first alerts.
	if _, err := s.db.Exec(`UPDATE copy_guard_protection_incidents SET healthy_since_ms=CASE
 WHEN EXISTS(SELECT 1 FROM copy_guard_cycles c WHERE c.id=cycle_id AND c.protection_status IN ('VERIFIED','CLAMPED'))
 THEN CASE WHEN healthy_since_ms=0 THEN ? ELSE healthy_since_ms END ELSE 0 END
 WHERE resolved_ms=0 AND cycle_id IN (SELECT id FROM copy_guard_cycles WHERE trader_id=?)`, now.UnixMilli(), traderID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE copy_guard_protection_incidents SET resolved_ms=?,claim_until_ms=0,next_attempt_ms=0
 WHERE resolved_ms=0 AND cycle_id IN (SELECT c.id FROM copy_guard_cycles c WHERE c.trader_id=? AND (
 c.closed_at IS NOT NULL OR c.protection_status IN ('POSITION_ABSENT','FLAT_RECONCILING','CANCELED')
 OR (c.protection_status IN ('VERIFIED','CLAMPED') AND healthy_since_ms>0 AND healthy_since_ms<=?)
 OR c.status NOT IN ('FOLLOWING','FOLLOWING_REENTRY')
 OR EXISTS(SELECT 1 FROM copy_trade_position_custody p WHERE p.trader_id=c.trader_id AND p.leader_pos_id=c.leader_pos_id AND (p.state='RELEASED' OR p.cycle_id<>c.id OR p.attempt_no<>c.reentry_count))))`, now.UnixMilli(), traderID, now.Add(-time.Minute).UnixMilli())
	return err
}

func protectionFailureStatus(status string) bool {
	switch status {
	case CopyGuardProtectionPending, CopyGuardProtectionUnknown, CopyGuardProtectionDegraded, CopyGuardProtectionUnprotectable, CopyGuardProtectionUnprotectedWarning:
		return true
	}
	return false
}

func (s *CopyTradeStore) ClaimProtectionMail(cycleID int64, attempt, level int, recovery bool, now time.Time) (*ProtectionMailClaim, error) {
	c, err := s.GetCopyGuardCycle(cycleID)
	if err != nil {
		return nil, err
	}
	if err = s.ResolveInactiveProtectionIncidents(c.TraderID, now); err != nil {
		return nil, err
	}
	authority, err := s.CopyGuardHasPositionAuthority(cycleID, attempt)
	if err != nil {
		return nil, err
	}
	if !authority || c.ReentryCount != attempt || (c.Status != CopyGuardFollowing && c.Status != CopyGuardFollowingReentry) {
		return nil, nil
	}
	healthy := c.ProtectionStatus == CopyGuardProtectionVerified || c.ProtectionStatus == CopyGuardProtectionClamped
	if (recovery && !healthy) || (!recovery && !protectionFailureStatus(c.ProtectionStatus)) {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ms := now.UnixMilli()
	if !recovery {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO copy_guard_protection_incidents(cycle_id,attempt_no,opened_ms) VALUES(?,?,?)`, cycleID, attempt, ms); err != nil {
			return nil, err
		}
	}
	claim := &ProtectionMailClaim{CycleID: cycleID, Attempt: attempt, Level: level, Recovery: recovery}
	var sent, resolved, recoverySent, next, until int64
	var sentLevel int
	err = tx.QueryRow(`SELECT id,claim_sequence,last_sent_ms,sent_level,resolved_ms,recovery_sent_ms,next_attempt_ms,claim_until_ms FROM copy_guard_protection_incidents WHERE cycle_id=? AND attempt_no=? ORDER BY id DESC LIMIT 1`, cycleID, attempt).Scan(&claim.ID, &claim.Sequence, &sent, &sentLevel, &resolved, &recoverySent, &next, &until)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	eligible := !recovery && resolved == 0 && (sent == 0 || level > sentLevel || ms-sent >= time.Hour.Milliseconds())
	if recovery {
		eligible = resolved > 0 && sent > 0 && recoverySent == 0
	}
	if !eligible || ms < next || ms < until {
		return nil, nil
	}
	claim.Sequence++
	_, err = tx.Exec(`UPDATE copy_guard_protection_incidents SET claim_sequence=?,claim_until_ms=?,delivery_status='claimed',delivery_error='' WHERE id=?`, claim.Sequence, ms+(5*time.Minute).Milliseconds(), claim.ID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return claim, nil
}

// Re-read immediately before SMTP; an alert queued before a close/recovery
// must not claim that the old position is still unprotected.
func (s *CopyTradeStore) ProtectionMailStillRelevant(claim *ProtectionMailClaim, now time.Time) bool {
	if claim == nil {
		return false
	}
	c, err := s.GetCopyGuardCycle(claim.CycleID)
	if err != nil || c.ClosedAt != nil || c.ReentryCount != claim.Attempt || (c.Status != CopyGuardFollowing && c.Status != CopyGuardFollowingReentry) {
		return false
	}
	ok, err := s.CopyGuardHasPositionAuthority(c.ID, c.ReentryCount)
	if err != nil || !ok {
		return false
	}
	if claim.Recovery {
		if c.ProtectionStatus != CopyGuardProtectionVerified && c.ProtectionStatus != CopyGuardProtectionClamped {
			return false
		}
	} else if !protectionFailureStatus(c.ProtectionStatus) {
		return false
	}
	var current bool
	err = s.db.QueryRow(`SELECT claim_sequence=? AND claim_until_ms>? AND ((? AND resolved_ms>0) OR (NOT ? AND resolved_ms=0)) FROM copy_guard_protection_incidents WHERE id=?`, claim.Sequence, now.UnixMilli(), claim.Recovery, claim.Recovery, claim.ID).Scan(&current)
	return err == nil && current
}

func (s *CopyTradeStore) RecordProtectionMailDelivery(claim *ProtectionMailClaim, status, detail string, now time.Time) error {
	if claim == nil {
		return fmt.Errorf("missing protection mail claim")
	}
	if status == "queued" {
		return nil
	}
	ms := now.UnixMilli()
	// Failure/disabled/full queue retry uses a persisted backoff, not a new
	// incident. Sequence checks reject callbacks from expired queue leases.
	_, err := s.db.Exec(`UPDATE copy_guard_protection_incidents SET
 delivery_status=?,delivery_error=?,claim_until_ms=0,next_attempt_ms=?,
 last_sent_ms=CASE WHEN ?='sent' AND NOT ? THEN ? ELSE last_sent_ms END,
 sent_level=CASE WHEN ?='sent' AND NOT ? THEN MAX(sent_level,?) ELSE sent_level END,
 recovery_sent_ms=CASE WHEN ?='sent' AND ? THEN ? ELSE recovery_sent_ms END
 WHERE id=? AND claim_sequence=? AND ((? AND resolved_ms>0) OR (NOT ? AND resolved_ms=0))`,
		status, detail, ms+time.Minute.Milliseconds(), status, claim.Recovery, ms, status, claim.Recovery, claim.Level, status, claim.Recovery, ms, claim.ID, claim.Sequence, claim.Recovery, claim.Recovery)
	return err
}
