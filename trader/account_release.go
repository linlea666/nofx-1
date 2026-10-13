package trader

import (
	"fmt"
	"time"

	"nofx/store"
)

// ArchiveTrader is the permanent deletion path. The caller must first stop and
// wait for every runtime belonging to this identity. Unlike stopped accounting
// reconciliation, this needs no historical fills and submits no venue requests
// other than uncached reads of positions and ordinary/conditional orders.
func (m *PositionSyncManager) ArchiveTrader(userID, traderID string) ([]store.TraderLifecycleBlocker, error) {
	complete, err := m.store.Trader().AccountReleaseComplete(userID, traderID)
	if err != nil || complete {
		return nil, err
	}
	check, err := m.store.Trader().PrepareAccountRelease(userID, traderID)
	if err != nil {
		return nil, err
	}
	// Never borrow a potentially stale runtime client for the deletion proof.
	m.InvalidateCache(traderID)
	proof := store.TraderAccountReleaseProof{Check: check}
	for _, accountID := range check.Accounts {
		executor, err := m.stoppedQueryTrader(traderID, userID, accountID, "")
		if err != nil {
			return nil, err
		}
		snapshot := store.StoppedTraderFlatSnapshot{
			ExchangeID: accountID, TraderGeneration: check.Generation, ObservedAt: time.Now(),
		}
		_, _, blockers, err := readStoppedAccountSnapshot(executor)
		if err != nil {
			return nil, fmt.Errorf("账户 %s 当前风险核验失败，未释放：%w", accountID, err)
		}
		if len(blockers) > 0 {
			for i := range blockers {
				blockers[i].ResourceID = accountID + ":" + blockers[i].ResourceID
			}
			return blockers, nil
		}
		snapshot.PositionsEmpty, snapshot.OrdersEmpty = true, true
		proof.Snapshots = append(proof.Snapshots, snapshot)
	}
	return m.store.Trader().Archive(userID, traderID, proof)
}

// ReleaseArchivedAccounts upgrades legacy archives only when an account is
// about to be reused and the old records still claim it. Already settled
// archives must not add a new prerequisite to ordinary startup/recovery.
// A durable release avoids all later historical lookups for that identity.
// drain runs before the proof and may only stop local runtimes, never submit or
// cancel orders. Calling before BeginStart also fences concurrent start races.
func (m *PositionSyncManager) ReleaseArchivedAccounts(userID, accountID string, drain func(string) error) error {
	ids, err := m.store.Trader().ListUnreleasedArchivedTraders(userID, accountID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if drain == nil {
			return fmt.Errorf("runtime drain unavailable for archived trader %s", id)
		}
		if err = drain(id); err != nil {
			return err
		}
		blockers, err := m.ArchiveTrader(userID, id)
		if err != nil {
			return fmt.Errorf("已删除交易员 %s 的账户释放核验未完成：%w", id, err)
		}
		if len(blockers) > 0 {
			return fmt.Errorf("已删除交易员 %s 对应账户仍有真实仓位或挂单：%s；处理后可重试启动", id, store.FormatLifecycleBlockers(blockers))
		}
	}
	return nil
}
