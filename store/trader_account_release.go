package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// A release is permanent only for this archived identity/generation/account.
// Historical order bookkeeping cannot revoke it. An unverified legacy archive
// remains subject to the existing venue-obligation checks.
func releasedTraderAccountSQL(account string) string {
	return `(t.lifecycle_status='ARCHIVED' AND t.is_running=0 AND EXISTS (
 SELECT 1 FROM trader_account_releases r WHERE r.trader_id=t.id
 AND r.archive_generation=t.lifecycle_generation AND r.exchange_id=` + account + `))`
}

// TraderAccountReleaseCheck is captured after the runtime has drained and
// before any venue reads. The private control snapshot detects starts/stops,
// account changes and newly created owners during the reads, including ABA.
type TraderAccountReleaseCheck struct {
	UserID, TraderID string
	Generation       int64
	Accounts         []string
	control          string
}

type TraderAccountReleaseProof struct {
	Check     *TraderAccountReleaseCheck
	Snapshots []StoppedTraderFlatSnapshot
}

type releaseQuery interface {
	Query(string, ...interface{}) (*sql.Rows, error)
	QueryRow(string, ...interface{}) *sql.Row
}

// Permanent deletion has no future source baseline to advance. Cancel only
// unsubmitted work and entry authorization; do not run the stopped trader's
// historical accounting/resolution machinery or fabricate order outcomes.
func cancelArchivedWorkTx(tx *sql.Tx, traderID string) error {
	if _, err := tx.Exec(`UPDATE copy_trade_execution_intents SET status='SKIPPED',reason_code='TRADER_ARCHIVED_PRE_SUBMIT',
 last_error='trader permanently deleted before submission',terminal_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP
 WHERE trader_id=? AND status IN ('RESERVED','RECONCILING','FAILED')
 AND submitted_at IS NULL AND COALESCE(exchange_order_id,'')='' AND COALESCE(filled_quantity,0)=0 AND COALESCE(filled_notional,0)=0
 AND NOT EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts a WHERE a.intent_id=copy_trade_execution_intents.id)`, traderID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE copy_trade_current_position_tasks SET status='SKIPPED',reason='TRADER_ARCHIVED'
 WHERE trader_id=? AND status='READY' AND (intent_id=0 OR intent_id IN (
 SELECT id FROM copy_trade_execution_intents WHERE trader_id=? AND reason_code='TRADER_ARCHIVED_PRE_SUBMIT'))`, traderID, traderID); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE copy_trade_current_position_requests SET status='CANCELLED',reason='TRADER_ARCHIVED'
 WHERE trader_id=? AND status IN ('PENDING','SEALED')`, traderID)
	return err
}

func accountReleaseCheck(q releaseQuery, userID, traderID string) (*TraderAccountReleaseCheck, error) {
	var configured, original, status string
	var running bool
	check := &TraderAccountReleaseCheck{UserID: userID, TraderID: traderID}
	if err := q.QueryRow(`SELECT t.exchange_id,COALESCE(a.exchange_id,''),t.lifecycle_status,t.lifecycle_generation,t.is_running
 FROM traders t LEFT JOIN copy_trade_account_claims a ON a.trader_id=t.id
 WHERE t.id=? AND t.user_id=?`, traderID, userID).
		Scan(&configured, &original, &status, &check.Generation, &running); err != nil {
		return nil, err
	}
	if running || (status != TraderLifecycleStopped && status != TraderLifecycleArchived && status != TraderLifecycleStoppingReconcileRequired) {
		return nil, fmt.Errorf("%w: stop trader before deleting (current=%s)", ErrTraderLifecycleConflict, status)
	}
	if configured == "" {
		return nil, fmt.Errorf("execution account identity unavailable")
	}
	check.Accounts = []string{configured}
	if original != "" && original != configured {
		check.Accounts = append(check.Accounts, original)
	}
	sort.Strings(check.Accounts)
	// Lifecycle generations are monotonic. Including all potential owners closes
	// the window where another trader starts and stops between two flat reads.
	type owner struct {
		ID, User, Configured, Claimed, Status string
		Generation, ClaimGeneration           int64
		Running, Exclusive                    bool
	}
	var owners []owner
	credentials := sha256.New()
	for _, account := range check.Accounts {
		// Credential edits can change the real venue account behind a UUID.
		// Keep only a hash in the in-memory fence; never log credential material.
		var identity string
		err := q.QueryRow(`SELECT json_array(user_id,exchange_type,api_key,secret_key,passphrase,testnet)
 FROM exchanges WHERE id=?`, account).Scan(&identity)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		fmt.Fprintf(credentials, "%s:%s\n", account, identity)
		rows, err := q.Query(`SELECT t.id,t.user_id,t.exchange_id,COALESCE(a.exchange_id,''),t.lifecycle_status,
 t.lifecycle_generation,COALESCE(a.generation,0),t.is_running,COALESCE(a.exclusive,0)
 FROM traders t LEFT JOIN copy_trade_account_claims a ON a.trader_id=t.id
 WHERE t.exchange_id=? OR a.exchange_id=? ORDER BY t.id`, account, account)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var o owner
			if err = rows.Scan(&o.ID, &o.User, &o.Configured, &o.Claimed, &o.Status, &o.Generation, &o.ClaimGeneration, &o.Running, &o.Exclusive); err != nil {
				rows.Close()
				return nil, err
			}
			if o.ID != traderID && (o.Running || o.Status == TraderLifecycleStarting || o.Status == TraderLifecycleRunning || o.Status == TraderLifecycleStopping || o.Status == TraderLifecycleStoppingReconcileRequired || o.Status == TraderLifecycleArchiving) {
				rows.Close()
				return nil, fmt.Errorf("账户 %s 仍有启动中、运行中或停止对账中的交易员 %s，暂不能核验释放", account, o.ID)
			}
			owners = append(owners, o)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(owners)
	check.control = fmt.Sprintf("%s:%x", raw, credentials.Sum(nil))
	return check, err
}

func (s *TraderStore) PrepareAccountRelease(userID, traderID string) (*TraderAccountReleaseCheck, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	check, err := accountReleaseCheck(tx, userID, traderID)
	if err != nil {
		return nil, err
	}
	return check, tx.Commit()
}

func validateAccountReleaseTx(tx *sql.Tx, userID, traderID string, proof TraderAccountReleaseProof) error {
	if proof.Check == nil || proof.Check.UserID != userID || proof.Check.TraderID != traderID {
		return fmt.Errorf("%w: complete fresh account release proof required", ErrTraderLifecycleConflict)
	}
	current, err := accountReleaseCheck(tx, userID, traderID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, proof.Check) {
		return fmt.Errorf("%w: account ownership changed during verification; retry deletion", ErrTraderLifecycleConflict)
	}
	if len(proof.Snapshots) != len(current.Accounts) {
		return fmt.Errorf("all original and configured accounts must be verified")
	}
	seen := make(map[string]bool)
	for _, snapshot := range proof.Snapshots {
		if seen[snapshot.ExchangeID] || snapshot.TraderGeneration != current.Generation ||
			!snapshot.PositionsEmpty || !snapshot.OrdersEmpty || snapshot.ObservedAt.IsZero() ||
			time.Since(snapshot.ObservedAt) > time.Minute || snapshot.ObservedAt.After(time.Now().Add(time.Second)) {
			return fmt.Errorf("fresh complete flat-account evidence required for %s", snapshot.ExchangeID)
		}
		seen[snapshot.ExchangeID] = true
	}
	for _, account := range current.Accounts {
		if !seen[account] {
			return fmt.Errorf("execution account %s was not verified", account)
		}
	}
	return nil
}

// AccountReleaseComplete supports idempotent deletion without re-querying an
// account now legitimately in use by a new trader.
func (s *TraderStore) AccountReleaseComplete(userID, traderID string) (bool, error) {
	var complete bool
	err := s.db.QueryRow(`SELECT `+releasedTraderAccountSQL("t.exchange_id")+` AND
 (COALESCE(a.exchange_id,'')='' OR `+releasedTraderAccountSQL("a.exchange_id")+`)
 FROM traders t LEFT JOIN copy_trade_account_claims a ON a.trader_id=t.id
 WHERE t.id=? AND t.user_id=?`, traderID, userID).Scan(&complete)
	return complete, err
}

func (s *TraderStore) ListUnreleasedArchivedTraders(userID, exchangeID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT t.id FROM traders t
 LEFT JOIN copy_trade_account_claims a ON a.trader_id=t.id
 WHERE t.user_id=? AND t.lifecycle_status='ARCHIVED' AND (t.exchange_id=? OR a.exchange_id=?)
 AND NOT `+releasedTraderAccountSQL("?")+` AND `+unsettledTraderVenueInstructionsSQL()+` ORDER BY t.id`, userID, exchangeID, exchangeID, exchangeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
