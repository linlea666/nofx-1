// Package store provides unified database storage layer
// All database operations should go through this package
package store

import (
	"database/sql"
	"fmt"
	"nofx/logger"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store unified data storage interface
type Store struct {
	db *sql.DB
	// instanceLock is held for the lifetime of the Store. Trading against one
	// SQLite database from two NOFX processes can duplicate exchange mutations,
	// so startup waits for the previous process to release ownership.
	instanceLock *processLock

	// Sub-stores (lazy initialization)
	user         *UserStore
	aiModel      *AIModelStore
	exchange     *ExchangeStore
	trader       *TraderStore
	decision     *DecisionStore
	backtest     *BacktestStore
	position     *PositionStore
	strategy     *StrategyStore
	equity       *EquityStore
	copyTrade    *CopyTradeStore
	binanceCreds *BinanceCredentialsStore
	reentryAI    *ReentryAIStore

	// Encryption functions
	encryptFunc func(string) string
	decryptFunc func(string) string

	mu sync.RWMutex
}

// New creates new Store instance
func New(dbPath string) (*Store, error) {
	lock, err := acquireProcessLock(dbPath, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire database process lock: %w", err)
	}
	releaseLock := true
	defer func() {
		if releaseLock {
			_ = lock.Close()
		}
	}()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// SQLite configuration
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// busy_timeout is connection-local and must be installed before any pragma
	// that may need a database lock. The old order changed journal_mode first,
	// so an overlapping service restart failed immediately with SQLITE_BUSY.
	if _, err := db.Exec("PRAGMA busy_timeout = 30000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set busy_timeout: %w", err)
	}

	// Enable foreign key constraints
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to enable foreign keys: %w", err)
	}

	// Use DELETE mode (traditional mode) to ensure Docker bind mount compatibility
	// Note: WAL mode causes data sync issues on macOS Docker
	if err := ensureSQLiteDeleteJournalMode(db, 30*time.Second); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set journal_mode: %w", err)
	}

	// Set synchronous=FULL
	if _, err := db.Exec("PRAGMA synchronous=FULL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set synchronous: %w", err)
	}

	s := &Store{db: db, instanceLock: lock}

	// Initialize all table structures
	if err := s.initTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize table structure: %w", err)
	}

	// Initialize default data
	if err := s.initDefaultData(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize default data: %w", err)
	}

	logger.Info("✅ Database enabled DELETE mode and FULL sync")
	releaseLock = false
	return s, nil
}

func ensureSQLiteDeleteJournalMode(db *sql.DB, timeout time.Duration) error {
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if strings.EqualFold(mode, "delete") {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		var selected string
		err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&selected)
		if err == nil {
			if !strings.EqualFold(selected, "delete") {
				return fmt.Errorf("SQLite selected unexpected journal mode %q", selected)
			}
			return nil
		}
		message := strings.ToLower(err.Error())
		if (!strings.Contains(message, "database is locked") && !strings.Contains(message, "sqlite_busy")) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ensureSQLiteColumn performs an additive migration without hiding real I/O or
// schema errors. Many legacy migrations ignored every ALTER error, which can
// leave a production database partially upgraded and fail only on a trade.
func ensureSQLiteColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if strings.EqualFold(name, column) {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

// NewFromDB creates Store from existing database connection
func NewFromDB(db *sql.DB) *Store {
	return &Store{db: db}
}

// SetCryptoFuncs sets encryption/decryption functions
func (s *Store) SetCryptoFuncs(encrypt, decrypt func(string) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.encryptFunc = encrypt
	s.decryptFunc = decrypt

	// Update already initialized sub-stores
	if s.aiModel != nil {
		s.aiModel.encryptFunc = encrypt
		s.aiModel.decryptFunc = decrypt
	}
	if s.exchange != nil {
		s.exchange.encryptFunc = encrypt
		s.exchange.decryptFunc = decrypt
	}
	if s.trader != nil {
		s.trader.decryptFunc = decrypt
	}
}

// initTables initializes all database tables
func (s *Store) initTables() error {
	// Initialize in dependency order
	if err := s.User().initTables(); err != nil {
		return fmt.Errorf("failed to initialize user tables: %w", err)
	}
	if err := s.AIModel().initTables(); err != nil {
		return fmt.Errorf("failed to initialize AI model tables: %w", err)
	}
	if err := s.Exchange().initTables(); err != nil {
		return fmt.Errorf("failed to initialize exchange tables: %w", err)
	}
	if err := s.Trader().initTables(); err != nil {
		return fmt.Errorf("failed to initialize trader tables: %w", err)
	}
	if err := s.Decision().initTables(); err != nil {
		return fmt.Errorf("failed to initialize decision log tables: %w", err)
	}
	if err := s.Backtest().initTables(); err != nil {
		return fmt.Errorf("failed to initialize backtest tables: %w", err)
	}
	if err := s.Position().InitTables(); err != nil {
		return fmt.Errorf("failed to initialize position tables: %w", err)
	}
	if err := s.Strategy().initTables(); err != nil {
		return fmt.Errorf("failed to initialize strategy tables: %w", err)
	}
	if err := s.Equity().initTables(); err != nil {
		return fmt.Errorf("failed to initialize equity tables: %w", err)
	}
	if err := s.CopyTrade().initTables(); err != nil {
		return fmt.Errorf("failed to initialize copy trade tables: %w", err)
	}
	if err := s.CopyTrade().initSignalLogTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade signal log table: %w", err)
	}
	if err := s.CopyTrade().initPositionMappingTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade position mapping table: %w", err)
	}
	if err := s.CopyTrade().initExecutionIntentTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade execution intent table: %w", err)
	}
	if err := s.CopyTrade().initCopyGuardTables(); err != nil {
		return fmt.Errorf("failed to initialize copy guard tables: %w", err)
	}
	if err := s.CopyTrade().finalizeExecutionIntentTerminalMigration(); err != nil {
		return fmt.Errorf("failed to finalize execution intent terminal migration: %w", err)
	}
	if err := s.CopyTrade().initCopyEventTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade event table: %w", err)
	}
	if err := s.CopyTrade().migratePositionMarginStops(); err != nil {
		return fmt.Errorf("failed to migrate position margin stops: %w", err)
	}
	if err := s.CopyTrade().initSourceHealthTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade source health table: %w", err)
	}
	if err := s.CopyTrade().initSourceIncidentTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade source incident table: %w", err)
	}
	if err := s.CopyTrade().initSourceBaselineTable(); err != nil {
		return fmt.Errorf("failed to initialize copy trade source baseline table: %w", err)
	}
	if err := s.CopyTrade().initUnsupportedExecutionInstrumentTable(); err != nil {
		return fmt.Errorf("failed to initialize unsupported execution instrument table: %w", err)
	}
	if err := s.BinanceCreds().initTables(); err != nil {
		return fmt.Errorf("failed to initialize binance credentials tables: %w", err)
	}
	if err := s.ReentryAI().initTables(); err != nil {
		return fmt.Errorf("failed to initialize reentry ai tables: %w", err)
	}
	if err := s.Trader().ReconcileOrphanTombstones(); err != nil {
		return fmt.Errorf("failed to reconcile legacy trader tombstones: %w", err)
	}
	return nil
}

// initDefaultData initializes default data
func (s *Store) initDefaultData() error {
	if err := s.AIModel().initDefaultData(); err != nil {
		return err
	}
	if err := s.Exchange().initDefaultData(); err != nil {
		return err
	}
	if err := s.Strategy().initDefaultData(); err != nil {
		return err
	}
	// Migrate old decision_account_snapshots data to new trader_equity_snapshots table
	if migrated, err := s.Equity().MigrateFromDecision(); err != nil {
		logger.Warnf("failed to migrate equity data: %v", err)
	} else if migrated > 0 {
		logger.Infof("✅ Migrated %d equity records to new table", migrated)
	}
	// Migrate latest per-trader Binance credentials to global store (one-time on upgrade)
	if migrated, err := s.BinanceCreds().MigrateFromCopyTradeConfigs(); err != nil {
		logger.Warnf("failed to migrate binance credentials: %v", err)
	} else if migrated {
		logger.Infof("✅ Migrated Binance credentials from per-trader to global store")
	}
	return nil
}

// User gets user storage
func (s *Store) User() *UserStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.user == nil {
		s.user = &UserStore{db: s.db}
	}
	return s.user
}

// AIModel gets AI model storage
func (s *Store) AIModel() *AIModelStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aiModel == nil {
		s.aiModel = &AIModelStore{
			db:          s.db,
			encryptFunc: s.encryptFunc,
			decryptFunc: s.decryptFunc,
		}
	}
	return s.aiModel
}

// Exchange gets exchange storage
func (s *Store) Exchange() *ExchangeStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exchange == nil {
		s.exchange = &ExchangeStore{
			db:          s.db,
			encryptFunc: s.encryptFunc,
			decryptFunc: s.decryptFunc,
		}
	}
	return s.exchange
}

// Trader gets trader storage
func (s *Store) Trader() *TraderStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trader == nil {
		s.trader = &TraderStore{
			db:          s.db,
			decryptFunc: s.decryptFunc,
		}
	}
	return s.trader
}

// Decision gets decision log storage
func (s *Store) Decision() *DecisionStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decision == nil {
		s.decision = &DecisionStore{db: s.db}
	}
	return s.decision
}

// Backtest gets backtest data storage
func (s *Store) Backtest() *BacktestStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backtest == nil {
		s.backtest = &BacktestStore{db: s.db}
	}
	return s.backtest
}

// Position gets position storage
func (s *Store) Position() *PositionStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.position == nil {
		s.position = NewPositionStore(s.db)
	}
	return s.position
}

// Strategy gets strategy storage
func (s *Store) Strategy() *StrategyStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strategy == nil {
		s.strategy = &StrategyStore{db: s.db}
	}
	return s.strategy
}

// Equity gets equity storage
func (s *Store) Equity() *EquityStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.equity == nil {
		s.equity = &EquityStore{db: s.db}
	}
	return s.equity
}

// CopyTrade gets copy trade storage
func (s *Store) CopyTrade() *CopyTradeStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.copyTrade == nil {
		s.copyTrade = &CopyTradeStore{db: s.db}
	}
	return s.copyTrade
}

// BinanceCreds gets binance global credentials storage
func (s *Store) BinanceCreds() *BinanceCredentialsStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binanceCreds == nil {
		s.binanceCreds = &BinanceCredentialsStore{db: s.db}
	}
	return s.binanceCreds
}

// ReentryAI gets reentry advisor storage
func (s *Store) ReentryAI() *ReentryAIStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reentryAI == nil {
		s.reentryAI = &ReentryAIStore{db: s.db}
	}
	return s.reentryAI
}

// Close closes database connection
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var dbErr error
	if s.db != nil {
		dbErr = s.db.Close()
	}
	if s.instanceLock != nil {
		if lockErr := s.instanceLock.Close(); dbErr == nil {
			dbErr = lockErr
		}
		s.instanceLock = nil
	}
	return dbErr
}

// DB gets underlying database connection (for legacy code compatibility, gradually deprecated)
// Deprecated: use Store methods instead
func (s *Store) DB() *sql.DB {
	return s.db
}

// Transaction executes transaction
func (s *Store) Transaction(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}
