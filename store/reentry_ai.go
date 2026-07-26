package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// 重入 AI 助手（Reentry Advisor）存储层
//
// 定位：Copy Guard 持久化候选的每次模型调用都保存数据快照、Prompt、
// 原始回复、严格解析结果、调用状态和最终结局。历史人工信号字段仅保留为
// 只读兼容，v7 执行路径不再依赖人工确认。
//
// 四张表：
//   reentry_ai_analyses  每个候选/历史信号的完整调用审计记录
//   reentry_ai_config    单行全局配置；生产 Prompt 只允许 analysis_focus 补充
//   reentry_ai_diagnostics 零交易模型连接与严格 Schema 自检，独立于交易统计
//   reentry_ai_decision_evaluations 版本化、确定性的后验决策效果
//
// AI 关闭时分析表静默闲置；候选与订单的确定性风控状态由 Copy Guard 表维护。
// ============================================================================

// 外部/内部 AI 结论标签（准确率对比用；空串=未标注）
const (
	ReentryVerdictEnter   = "ENTER"
	ReentryVerdictWait    = "WAIT"
	ReentryVerdictSkip    = "SKIP"
	ReentryVerdictAbandon = "ABANDON"
)

// ReentryAIAnalysis 一次数据包快照 + 双 AI 结果载体
type ReentryAIAnalysis struct {
	ID                 int64  `json:"id"`
	SignalID           int64  `json:"signal_id"`
	CandidateID        int64  `json:"candidate_id"`
	TraderID           string `json:"trader_id"`
	CycleID            int64  `json:"cycle_id"`
	Symbol             string `json:"symbol"`
	Side               string `json:"side"`
	AttemptNo          int    `json:"attempt_no"`
	DecisionGeneration int    `json:"decision_generation"`
	CallStatus         string `json:"call_status"`
	CallError          string `json:"call_error,omitempty"`
	DataHash           string `json:"data_hash"`

	// 透明化三段的持久化载体
	SystemPrompt string `json:"system_prompt"`
	UserPrompt   string `json:"user_prompt"`
	DatapackJSON string `json:"datapack_json"` // 纯数据 JSON（喂外部 AI 用）

	// 市场层可用性（Binance 无该币种合约时 false，仓位层数据仍完整）
	MarketDataAvailable bool   `json:"market_data_available"`
	MissingFields       string `json:"missing_fields"` // 逗号分隔的降级字段列表

	// 内部 AI（Phase 2 起填充）
	RawResponse string  `json:"raw_response"`
	Verdict     string  `json:"verdict"`
	Confidence  float64 `json:"confidence"`
	Reasons     string  `json:"reasons"`

	// 外部 AI（用户粘贴，永久可编辑）
	ExternalResponse string `json:"external_response"`
	ExternalVerdict  string `json:"external_verdict"`

	PromptVersion     string     `json:"prompt_version"`
	SnapshotPrice     float64    `json:"snapshot_price"`
	SnapshotAt        time.Time  `json:"snapshot_at"`
	ModelStartedAt    *time.Time `json:"model_started_at,omitempty"`
	ModelCompletedAt  *time.Time `json:"model_completed_at,omitempty"`
	DecisionExpiresAt *time.Time `json:"decision_expires_at,omitempty"`
	OutcomePnL        *float64   `json:"outcome_pnl,omitempty"` // 周期闭合后回填（Phase 2）
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
}

// ReentryAIConfig 全局配置（单行，id 恒为 1）
type ReentryAIConfig struct {
	Enabled             bool    `json:"enabled"`              // 插件总开关（控制数据包自动生成）
	AIEnabled           bool    `json:"ai_enabled"`           // Phase 2：新信号自动触发内置 AI 分析（默认关；手动"AI 分析"按钮不受此开关限制）
	AutoEntryEnabled    bool    `json:"auto_entry_enabled"`   // ai_guarded 候选真实执行的全局安全开关（默认关；依赖 ai_enabled）
	Provider            string  `json:"provider"`             // 展示用 provider（实际密钥由 model 指向的 ai_models 行决定）
	Model               string  `json:"model"`                // ai_models 表的模型 ID（空=自动选用已启用的默认模型）
	PromptTemplate      string  `json:"prompt_template"`      // 仅历史人工信号分析兼容；ai_guarded 不读取此字段
	AnalysisFocus       string  `json:"analysis_focus"`       // 追加到不可覆盖的生产 Prompt 后，仅用于补充分析关注点
	ConfidenceThreshold float64 `json:"confidence_threshold"` // 仅历史分析兼容；ai_guarded 使用交易员级 risk_ai_confidence_threshold
	TimeoutSeconds      int     `json:"timeout_seconds"`      // AI 调用超时
}

// ReentryAIStore 重入 AI 助手存储
type ReentryAIStore struct {
	db *sql.DB
}

func (s *ReentryAIStore) initTables() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS reentry_ai_analyses (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			signal_id INTEGER NOT NULL,
			candidate_id INTEGER NOT NULL DEFAULT 0,
			trader_id TEXT NOT NULL,
			cycle_id INTEGER NOT NULL,
			symbol TEXT NOT NULL DEFAULT '',
			side TEXT NOT NULL DEFAULT '',
			attempt_no INTEGER NOT NULL DEFAULT 0,
			decision_generation INTEGER NOT NULL DEFAULT 0,
			call_status TEXT NOT NULL DEFAULT 'PENDING',
			call_error TEXT NOT NULL DEFAULT '',
			data_hash TEXT NOT NULL DEFAULT '',
			system_prompt TEXT NOT NULL DEFAULT '',
			user_prompt TEXT NOT NULL DEFAULT '',
			datapack_json TEXT NOT NULL DEFAULT '',
			market_data_available BOOLEAN DEFAULT 1,
			missing_fields TEXT DEFAULT '',
			raw_response TEXT DEFAULT '',
			verdict TEXT DEFAULT '',
			confidence REAL DEFAULT 0,
			reasons TEXT DEFAULT '',
			external_response TEXT DEFAULT '',
			external_verdict TEXT DEFAULT '',
			prompt_version TEXT DEFAULT '',
			snapshot_price REAL DEFAULT 0,
			snapshot_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			model_started_at DATETIME,
			model_completed_at DATETIME,
			decision_expires_at DATETIME,
			outcome_pnl REAL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS idx_reentry_ai_analyses_signal ON reentry_ai_analyses(signal_id, id);
		CREATE INDEX IF NOT EXISTS idx_reentry_ai_analyses_trader ON reentry_ai_analyses(trader_id, id);
		CREATE TABLE IF NOT EXISTS reentry_ai_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			enabled BOOLEAN DEFAULT 1,
			ai_enabled BOOLEAN DEFAULT 0,
			auto_entry_enabled BOOLEAN DEFAULT 0,
			provider TEXT DEFAULT 'deepseek',
			model TEXT DEFAULT '',
			prompt_template TEXT DEFAULT '',
			analysis_focus TEXT DEFAULT '',
			confidence_threshold REAL DEFAULT 0.7,
			timeout_seconds INTEGER DEFAULT 60,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return err
	}
	for _, migration := range []struct {
		table, column, definition string
	}{
		{"reentry_ai_analyses", "candidate_id", "INTEGER NOT NULL DEFAULT 0"},
		{"reentry_ai_analyses", "snapshot_price", "REAL DEFAULT 0"},
		{"reentry_ai_analyses", "attempt_no", "INTEGER NOT NULL DEFAULT 0"},
		{"reentry_ai_analyses", "decision_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"reentry_ai_analyses", "call_status", "TEXT NOT NULL DEFAULT 'PENDING'"},
		{"reentry_ai_analyses", "call_error", "TEXT NOT NULL DEFAULT ''"},
		{"reentry_ai_analyses", "data_hash", "TEXT NOT NULL DEFAULT ''"},
		{"reentry_ai_analyses", "model_started_at", "DATETIME"},
		{"reentry_ai_analyses", "model_completed_at", "DATETIME"},
		{"reentry_ai_analyses", "decision_expires_at", "DATETIME"},
		{"reentry_ai_config", "ai_enabled", "BOOLEAN DEFAULT 0"},
		{"reentry_ai_config", "auto_entry_enabled", "BOOLEAN DEFAULT 0"},
		{"reentry_ai_config", "analysis_focus", "TEXT DEFAULT ''"},
	} {
		if err := ensureSQLiteColumn(s.db, migration.table, migration.column, migration.definition); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", migration.table, migration.column, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE reentry_ai_analyses SET call_status=CASE WHEN verdict<>'' THEN 'COMPLETED' WHEN raw_response<>'' THEN 'INVALID' ELSE call_status END WHERE call_status='PENDING'`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_reentry_ai_analyses_candidate ON reentry_ai_analyses(candidate_id,id)`); err != nil {
		return err
	}
	if err := s.initReentryCandidateTables(); err != nil {
		return err
	}
	if err := s.initReentryDiagnosticTable(); err != nil {
		return err
	}
	if err := s.initReentryDecisionEvaluationTable(); err != nil {
		return err
	}
	// Candidate PnL belongs only to the exact analysis whose durable execution
	// intent filled. Older code matched on attempt_no alone, so a rejected ENTER
	// could inherit a later analysis' PnL. Clear only rows whose contamination is
	// positively proven by another exact filled analysis on the same candidate
	// attempt; preserve legacy rows that lack enough evidence and exclude them
	// from v3 verified statistics instead of destructively guessing.
	_, err = s.db.Exec(`UPDATE reentry_ai_analyses SET outcome_pnl=NULL,updated_at=CURRENT_TIMESTAMP
		WHERE candidate_id>0 AND outcome_pnl IS NOT NULL AND NOT EXISTS (
			SELECT 1 FROM copy_trade_execution_intents i
			WHERE i.analysis_id=reentry_ai_analyses.id AND i.status IN ('FILLED','PROTECTED')
		) AND EXISTS (
			SELECT 1 FROM reentry_ai_analyses exact_analysis
			JOIN copy_trade_execution_intents exact_intent ON exact_intent.analysis_id=exact_analysis.id
			WHERE exact_analysis.candidate_id=reentry_ai_analyses.candidate_id
			  AND exact_analysis.attempt_no=reentry_ai_analyses.attempt_no
			  AND exact_analysis.id<>reentry_ai_analyses.id
			  AND exact_intent.status IN ('FILLED','PROTECTED')
		)`)
	return err
}

const reentryAnalysisColumns = `id, signal_id, candidate_id, trader_id, cycle_id, symbol, side, attempt_no, decision_generation, call_status, call_error, data_hash,
	system_prompt, user_prompt, datapack_json, market_data_available, missing_fields,
	raw_response, verdict, confidence, reasons, external_response, external_verdict,
	prompt_version, snapshot_price, snapshot_at, model_started_at, model_completed_at, decision_expires_at, outcome_pnl, created_at, updated_at`

func scanReentryAnalysis(row rowScanner) (*ReentryAIAnalysis, error) {
	var a ReentryAIAnalysis
	var snapshot, created string
	var modelStarted, modelCompleted, decisionExpires, updated sql.NullString
	var outcome sql.NullFloat64
	if err := row.Scan(&a.ID, &a.SignalID, &a.CandidateID, &a.TraderID, &a.CycleID, &a.Symbol, &a.Side,
		&a.AttemptNo, &a.DecisionGeneration, &a.CallStatus, &a.CallError, &a.DataHash,
		&a.SystemPrompt, &a.UserPrompt, &a.DatapackJSON, &a.MarketDataAvailable, &a.MissingFields,
		&a.RawResponse, &a.Verdict, &a.Confidence, &a.Reasons, &a.ExternalResponse, &a.ExternalVerdict,
		&a.PromptVersion, &a.SnapshotPrice, &snapshot, &modelStarted, &modelCompleted, &decisionExpires, &outcome, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if a.SnapshotAt, err = parseDBTime(snapshot); err != nil {
		return nil, fmt.Errorf("reentry analysis %d snapshot_at: %w", a.ID, err)
	}
	if a.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, fmt.Errorf("reentry analysis %d created_at: %w", a.ID, err)
	}
	if a.ModelStartedAt, err = parseNullableDBTime(modelStarted); err != nil {
		return nil, fmt.Errorf("reentry analysis %d model_started_at: %w", a.ID, err)
	}
	if a.ModelCompletedAt, err = parseNullableDBTime(modelCompleted); err != nil {
		return nil, fmt.Errorf("reentry analysis %d model_completed_at: %w", a.ID, err)
	}
	if a.DecisionExpiresAt, err = parseNullableDBTime(decisionExpires); err != nil {
		return nil, fmt.Errorf("reentry analysis %d decision_expires_at: %w", a.ID, err)
	}
	if a.UpdatedAt, err = parseNullableDBTime(updated); err != nil {
		return nil, fmt.Errorf("reentry analysis %d updated_at: %w", a.ID, err)
	}
	if outcome.Valid {
		v := outcome.Float64
		a.OutcomePnL = &v
	}
	return &a, nil
}

// SaveReentryAnalysis 插入一条分析记录（新快照），返回落库后的完整行
func (s *ReentryAIStore) SaveReentryAnalysis(a *ReentryAIAnalysis) (*ReentryAIAnalysis, error) {
	if a == nil {
		return nil, fmt.Errorf("nil reentry analysis")
	}
	res, err := s.db.Exec(`INSERT INTO reentry_ai_analyses
		(signal_id, candidate_id, trader_id, cycle_id, symbol, side, attempt_no, decision_generation, call_status, call_error, data_hash, system_prompt, user_prompt, datapack_json,
		 market_data_available, missing_fields, prompt_version, snapshot_price, snapshot_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		a.SignalID, a.CandidateID, a.TraderID, a.CycleID, a.Symbol, a.Side, a.AttemptNo, a.DecisionGeneration, defaultCallStatus(a.CallStatus), a.CallError, a.DataHash, a.SystemPrompt, a.UserPrompt, a.DatapackJSON,
		a.MarketDataAvailable, a.MissingFields, a.PromptVersion, a.SnapshotPrice)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetReentryAnalysis(id)
}

func defaultCallStatus(status string) string {
	if status == "" {
		return "PENDING"
	}
	return status
}

// GetReentryAnalysis 按 ID 读取
func (s *ReentryAIStore) GetReentryAnalysis(id int64) (*ReentryAIAnalysis, error) {
	row := s.db.QueryRow(`SELECT `+reentryAnalysisColumns+` FROM reentry_ai_analyses WHERE id=?`, id)
	return scanReentryAnalysis(row)
}

// ListReentryAnalysesBySignal 某信号的分析记录，按快照时间倒序（最新在前）
func (s *ReentryAIStore) ListReentryAnalysesBySignal(signalID int64, limit int) ([]*ReentryAIAnalysis, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := s.db.Query(`SELECT `+reentryAnalysisColumns+` FROM reentry_ai_analyses WHERE signal_id=? ORDER BY id DESC LIMIT ?`, signalID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReentryAIAnalysis{}
	for rows.Next() {
		a, err := scanReentryAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListReentryAnalysesByCycle returns the complete AI decision history needed
// to reconstruct a Copy Guard cycle export. The hard cap prevents an invalid
// request from turning the audit endpoint into an unbounded query.
func (s *ReentryAIStore) ListReentryAnalysesByCycle(cycleID int64, limit int) ([]*ReentryAIAnalysis, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT `+reentryAnalysisColumns+` FROM reentry_ai_analyses WHERE cycle_id=? ORDER BY id LIMIT ?`, cycleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ReentryAIAnalysis{}
	for rows.Next() {
		a, err := scanReentryAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListReentryAnalysesByTraders 按归属交易员列出分析历史（跨信号，最新在前），
// A1 分析历史列表用。traderIDs 为空返回空列表。
func (s *ReentryAIStore) ListReentryAnalysesByTraders(traderIDs []string, limit int) ([]*ReentryAIAnalysis, error) {
	out := []*ReentryAIAnalysis{}
	if len(traderIDs) == 0 {
		return out, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	args := make([]interface{}, 0, len(traderIDs)+1)
	for _, id := range traderIDs {
		args = append(args, id)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT `+reentryAnalysisColumns+` FROM reentry_ai_analyses
		WHERE trader_id IN (`+sqlMarks(len(traderIDs))+`) ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanReentryAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LatestReentryAnalysisBySignal 某信号最新一条分析（无记录返回 sql.ErrNoRows）
func (s *ReentryAIStore) LatestReentryAnalysisBySignal(signalID int64) (*ReentryAIAnalysis, error) {
	row := s.db.QueryRow(`SELECT `+reentryAnalysisColumns+` FROM reentry_ai_analyses WHERE signal_id=? ORDER BY id DESC LIMIT 1`, signalID)
	return scanReentryAnalysis(row)
}

// HasReentryAnalysisForSignal 该信号是否已有分析记录（插件轮询幂等判断）
func (s *ReentryAIStore) HasReentryAnalysisForSignal(signalID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM reentry_ai_analyses WHERE signal_id=?`, signalID).Scan(&n)
	return n > 0, err
}

// UpdateReentryExternal 保存/更新外部 AI 粘贴结果（永久可编辑）。
// externalVerdict 只接受空串或 ENTER/WAIT/SKIP。
func (s *ReentryAIStore) UpdateReentryExternal(id int64, externalResponse, externalVerdict string) error {
	switch externalVerdict {
	case "", ReentryVerdictEnter, ReentryVerdictWait, ReentryVerdictSkip:
	default:
		return fmt.Errorf("无效的外部结论标签: %s", externalVerdict)
	}
	res, err := s.db.Exec(`UPDATE reentry_ai_analyses SET external_response=?, external_verdict=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		externalResponse, externalVerdict, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("分析记录不存在: %d", id)
	}
	return nil
}

// UpdateReentryInternalResult 写入内置 AI 分析结果（Phase 2）。
// verdict 允许空串（表示原始回复解析失败，仅存 raw 供人工查看）。
func (s *ReentryAIStore) UpdateReentryInternalResult(id int64, rawResponse, verdict string, confidence float64, reasons string) error {
	return s.CompleteReentryInternalResult(id, rawResponse, verdict, confidence, reasons, 30)
}

// CompleteReentryInternalResult completes a model call and starts the decision
// TTL at model completion, rather than consuming it while the model is running.
func (s *ReentryAIStore) CompleteReentryInternalResult(id int64, rawResponse, verdict string, confidence float64, reasons string, ttlSeconds int) error {
	switch verdict {
	case "", ReentryVerdictEnter, ReentryVerdictWait, ReentryVerdictSkip, ReentryVerdictAbandon:
	default:
		return fmt.Errorf("无效的内部结论标签: %s", verdict)
	}
	callStatus := "COMPLETED"
	callError := ""
	if verdict == "" {
		callStatus = "INVALID"
		callError = "model response did not match the strict schema"
	}
	if ttlSeconds < 15 || ttlSeconds > 60 {
		ttlSeconds = 30
	}
	res, err := s.db.Exec(`UPDATE reentry_ai_analyses SET raw_response=?, verdict=?, confidence=?, reasons=?,call_status=?,call_error=?,
		model_completed_at=CURRENT_TIMESTAMP, decision_expires_at=datetime(CURRENT_TIMESTAMP, ?), updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		rawResponse, verdict, confidence, reasons, callStatus, callError, fmt.Sprintf("+%d seconds", ttlSeconds), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("分析记录不存在: %d", id)
	}
	return nil
}

func (s *ReentryAIStore) MarkReentryAnalysisFailed(id int64, message string) error {
	res, err := s.db.Exec(`UPDATE reentry_ai_analyses SET call_status='FAILED',call_error=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, message, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("分析记录不存在: %d", id)
	}
	return nil
}

func (s *ReentryAIStore) MarkReentryAnalysisRunning(id int64) error {
	res, err := s.db.Exec(`UPDATE reentry_ai_analyses SET call_status='RUNNING',call_error='',model_started_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=? AND candidate_id>0 AND call_status='PENDING'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("candidate analysis is no longer pending: %d", id)
	}
	return nil
}

func (s *ReentryAIStore) HasCompletedCandidateDataHash(candidateID, beforeID int64, dataHash string) (bool, error) {
	if candidateID <= 0 || beforeID <= 0 || dataHash == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM reentry_ai_analyses WHERE candidate_id=? AND id<? AND data_hash=? AND call_status='COMPLETED')`, candidateID, beforeID, dataHash).Scan(&exists)
	return exists == 1, err
}

func (s *ReentryAIStore) ListCandidateAnalysesPendingOutcome(limit int) ([]*ReentryAIAnalysis, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+reentryAnalysisColumns+` FROM reentry_ai_analyses a
		WHERE a.candidate_id>0 AND a.verdict=? AND a.call_status='COMPLETED' AND a.outcome_pnl IS NULL
		AND EXISTS (
			SELECT 1 FROM copy_trade_execution_intents i
			WHERE i.analysis_id=a.id AND i.status IN ('FILLED','PROTECTED')
		)
		ORDER BY a.id LIMIT ?`, ReentryVerdictEnter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReentryAIAnalysis
	for rows.Next() {
		a, err := scanReentryAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *ReentryAIStore) SetReentryOutcomeForAnalysis(analysisID int64, pnl float64) error {
	_, err := s.db.Exec(`UPDATE reentry_ai_analyses SET outcome_pnl=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND outcome_pnl IS NULL`, pnl, analysisID)
	return err
}

// ListExecutedSignalIDsPendingOutcome 已执行（EXECUTED）但分析记录尚未回填
// 结局盈亏的信号 ID 列表（Phase 2 准确率回填轮询用）。
func (s *ReentryAIStore) ListExecutedSignalIDsPendingOutcome(limit int) ([]int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT DISTINCT a.signal_id FROM reentry_ai_analyses a
		JOIN copy_guard_manual_reentry_signals s ON s.id = a.signal_id
		WHERE a.outcome_pnl IS NULL AND s.status = ? ORDER BY a.signal_id LIMIT ?`,
		ManualReentryStatusExecuted, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetReentryOutcomeForSignal 将该信号全部分析记录的结局盈亏回填为同一值
// （同信号的多个快照对应同一次真实重入，结局一致）。
func (s *ReentryAIStore) SetReentryOutcomeForSignal(signalID int64, pnl float64) error {
	_, err := s.db.Exec(`UPDATE reentry_ai_analyses SET outcome_pnl=?, updated_at=CURRENT_TIMESTAMP WHERE signal_id=? AND outcome_pnl IS NULL`, pnl, signalID)
	return err
}

// ReentryAIStats 内外部 AI 结论分布与准确率统计。
//
// 口径（按信号去重）：同一信号"重新生成"会产生多个快照，为避免重复快照
// 放大样本，内部/外部各取"该信号最新一条带相应结论的快照"作为唯一样本。
// 准确率：仅统计有结局盈亏的样本；ENTER 且盈利 / SKIP 且亏损 记为正确；
// WAIT 不计入（无对错基准）。结局盈亏直接使用已对账 attempt.PnL；
// OKX 已实现盈亏已包含手续费，不得再减 attempt.Fee。
type ReentryAIStats struct {
	TotalAnalyses  int `json:"total_analyses"`
	SignalsCovered int `json:"signals_covered"`
	ScoredCount    int `json:"scored_count"` // 已回填结局的信号数

	InternalVerdicts map[string]int `json:"internal_verdicts"` // ENTER/WAIT/SKIP → 信号数（按信号去重）
	ExternalVerdicts map[string]int `json:"external_verdicts"`

	InternalScored  int `json:"internal_scored"` // 内部结论中可评分（ENTER/SKIP 且有结局）
	InternalCorrect int `json:"internal_correct"`
	ExternalScored  int `json:"external_scored"`
	ExternalCorrect int `json:"external_correct"`

	// Candidate calls are never deduplicated away: WAIT, ABANDON, invalid
	// schema and transport/model failures all remain visible. Accuracy is only
	// computed where an ENTER analysis maps to a reconciled attempt.
	CandidateAnalyses           int            `json:"candidate_analyses"`
	CandidateDecisions          map[string]int `json:"candidate_decisions"`
	CandidateCallStatuses       map[string]int `json:"candidate_call_statuses"`
	CandidateScored             int            `json:"candidate_scored"`
	CandidateProfitable         int            `json:"candidate_profitable"`
	CandidateEvaluated          int            `json:"candidate_evaluated"`
	CandidateUnscorable         int            `json:"candidate_unscorable"`
	CandidateExecutionRequested int            `json:"candidate_execution_requested"`
	CandidateExecutionSubmitted int            `json:"candidate_execution_submitted"`
	CandidateExecutionFilled    int            `json:"candidate_execution_filled"`
	CandidateExecutionProtected int            `json:"candidate_execution_protected"`
	CandidateEvaluationOutcomes map[string]int `json:"candidate_evaluation_outcomes"`
	CandidateMarketOutcomes     map[string]int `json:"candidate_market_outcomes"`
}

// sqlMarks 生成 "?,?,...,?" 占位符
func sqlMarks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// GetReentryAIStats 汇总统计，按 traderIDs 归属过滤（表量级小，直接聚合查询）。
// traderIDs 为空返回零值统计（当前用户名下无交易员）。
func (s *ReentryAIStore) GetReentryAIStats(traderIDs []string) (*ReentryAIStats, error) {
	st := &ReentryAIStats{
		InternalVerdicts:            map[string]int{},
		ExternalVerdicts:            map[string]int{},
		CandidateDecisions:          map[string]int{},
		CandidateCallStatuses:       map[string]int{},
		CandidateEvaluationOutcomes: map[string]int{},
		CandidateMarketOutcomes:     map[string]int{},
	}
	if len(traderIDs) == 0 {
		return st, nil
	}
	marks := sqlMarks(len(traderIDs))
	args := make([]interface{}, len(traderIDs))
	for i, id := range traderIDs {
		args[i] = id
	}
	err := s.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT CASE WHEN candidate_id>0 THEN 'c:'||candidate_id ELSE 's:'||signal_id END),
		COUNT(DISTINCT CASE WHEN outcome_pnl IS NOT NULL THEN CASE WHEN candidate_id>0 THEN 'c:'||candidate_id ELSE 's:'||signal_id END END)
		FROM reentry_ai_analyses WHERE trader_id IN (`+marks+`)`, args...).
		Scan(&st.TotalAnalyses, &st.SignalsCovered, &st.ScoredCount)
	if err != nil {
		return nil, err
	}
	// 每信号取最新一条带结论的快照为样本（MAX(id) 子查询去重）
	fill := func(col string, verdicts map[string]int, scored, correct *int) error {
		rows, err := s.db.Query(`SELECT `+col+`, COUNT(*),
			COALESCE(SUM(CASE WHEN outcome_pnl IS NOT NULL AND `+col+` IN ('ENTER','SKIP') THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN outcome_pnl IS NOT NULL AND ((`+col+`='ENTER' AND outcome_pnl>0) OR (`+col+`='SKIP' AND outcome_pnl<=0)) THEN 1 ELSE 0 END),0)
			FROM reentry_ai_analyses a
			WHERE a.trader_id IN (`+marks+`) AND a.candidate_id=0 AND a.`+col+` != ''
			  AND a.id = (SELECT MAX(b.id) FROM reentry_ai_analyses b WHERE b.candidate_id=0 AND b.signal_id = a.signal_id AND b.`+col+` != '')
			GROUP BY `+col, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var verdict string
			var count, sc, co int
			if err := rows.Scan(&verdict, &count, &sc, &co); err != nil {
				return err
			}
			verdicts[verdict] = count
			*scored += sc
			*correct += co
		}
		return rows.Err()
	}
	if err := fill("verdict", st.InternalVerdicts, &st.InternalScored, &st.InternalCorrect); err != nil {
		return nil, err
	}
	if err := fill("external_verdict", st.ExternalVerdicts, &st.ExternalScored, &st.ExternalCorrect); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT verdict,call_status,COUNT(*) FROM reentry_ai_analyses WHERE trader_id IN (`+marks+`) AND candidate_id>0 GROUP BY verdict,call_status`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var verdict, status string
		var count int
		if err := rows.Scan(&verdict, &status, &count); err != nil {
			return nil, err
		}
		st.CandidateAnalyses += count
		st.CandidateCallStatuses[status] += count
		if verdict != "" {
			st.CandidateDecisions[verdict] += count
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	evalArgs := append(append([]interface{}{}, args...), ReentryDecisionEvaluationVersion)
	evalRows, err := s.db.Query(`SELECT decision_outcome,market_outcome,data_quality,COUNT(*) FROM reentry_ai_decision_evaluations WHERE trader_id IN (`+marks+`) AND evaluation_version=? AND horizon='LEADER_FINAL' GROUP BY decision_outcome,market_outcome,data_quality`, evalArgs...)
	if err != nil {
		return nil, err
	}
	defer evalRows.Close()
	for evalRows.Next() {
		var decisionOutcome, marketOutcome, dataQuality string
		var count int
		if err := evalRows.Scan(&decisionOutcome, &marketOutcome, &dataQuality, &count); err != nil {
			return nil, err
		}
		st.CandidateEvaluated += count
		st.CandidateEvaluationOutcomes[decisionOutcome] += count
		st.CandidateMarketOutcomes[marketOutcome] += count
		if dataQuality != "VERIFIED" {
			st.CandidateUnscorable += count
		}
	}
	if err := evalRows.Err(); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN actual_pnl IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN actual_pnl>0 THEN 1 ELSE 0 END),0)
		FROM reentry_ai_decision_evaluations
		WHERE trader_id IN (`+marks+`) AND evaluation_version=? AND horizon='LEADER_FINAL'`,
		evalArgs...).Scan(
		&st.CandidateScored, &st.CandidateProfitable,
	); err != nil {
		return nil, err
	}
	// Operational conversion is live lifecycle state, not a terminal evaluation
	// artifact. Active positions therefore appear immediately.
	if err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.analysis_id=a.id) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.analysis_id=a.id AND
			(i.status IN ('SUBMITTED','FILLED','PROTECTED') OR EXISTS(SELECT 1 FROM copy_trade_execution_order_attempts oa WHERE oa.intent_id=i.id AND oa.status IN ('SUBMITTED','FILLED')))) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.analysis_id=a.id AND i.status IN ('FILLED','PROTECTED')) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN EXISTS(SELECT 1 FROM copy_trade_execution_intents i WHERE i.analysis_id=a.id AND i.status='PROTECTED') THEN 1 ELSE 0 END),0)
		FROM reentry_ai_analyses a
		WHERE a.trader_id IN (`+marks+`) AND a.candidate_id>0 AND a.call_status='COMPLETED' AND a.verdict='ENTER'`,
		args...).Scan(&st.CandidateExecutionRequested, &st.CandidateExecutionSubmitted, &st.CandidateExecutionFilled, &st.CandidateExecutionProtected); err != nil {
		return nil, err
	}
	return st, nil
}

// GetReentryAIConfig 读全局配置；无行时返回默认值（enabled=true）
func (s *ReentryAIStore) GetReentryAIConfig() (*ReentryAIConfig, error) {
	cfg := &ReentryAIConfig{
		Enabled:             true,
		Provider:            "deepseek",
		ConfidenceThreshold: 0.7,
		TimeoutSeconds:      60,
	}
	err := s.db.QueryRow(`SELECT enabled, ai_enabled, auto_entry_enabled, provider, model, prompt_template, analysis_focus, confidence_threshold, timeout_seconds FROM reentry_ai_config WHERE id=1`).
		Scan(&cfg.Enabled, &cfg.AIEnabled, &cfg.AutoEntryEnabled, &cfg.Provider, &cfg.Model, &cfg.PromptTemplate, &cfg.AnalysisFocus, &cfg.ConfidenceThreshold, &cfg.TimeoutSeconds)
	if err == sql.ErrNoRows {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// SaveReentryAIConfig 覆盖式保存全局配置（单行 upsert）
func (s *ReentryAIStore) SaveReentryAIConfig(cfg *ReentryAIConfig) error {
	if cfg == nil {
		return fmt.Errorf("nil reentry ai config")
	}
	_, err := s.db.Exec(`INSERT INTO reentry_ai_config (id, enabled, ai_enabled, auto_entry_enabled, provider, model, prompt_template, analysis_focus, confidence_threshold, timeout_seconds, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, ai_enabled=excluded.ai_enabled, auto_entry_enabled=excluded.auto_entry_enabled,
			provider=excluded.provider, model=excluded.model,
			prompt_template=excluded.prompt_template, analysis_focus=excluded.analysis_focus, confidence_threshold=excluded.confidence_threshold,
			timeout_seconds=excluded.timeout_seconds, updated_at=CURRENT_TIMESTAMP`,
		cfg.Enabled, cfg.AIEnabled, cfg.AutoEntryEnabled, cfg.Provider, cfg.Model, cfg.PromptTemplate, cfg.AnalysisFocus, cfg.ConfidenceThreshold, cfg.TimeoutSeconds)
	return err
}

// ListManualReentrySignalsByStatus 按状态列出人工重入信号（系统级，跨全部
// 交易员）。插件轮询发现新 PENDING 信号用——只读既有表，不影响信号写路径。
func (s *ReentryAIStore) ListManualReentrySignalsByStatus(status string, limit int) ([]*CopyGuardManualReentrySignal, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT `+manualReentrySignalColumns+` FROM copy_guard_manual_reentry_signals WHERE status=? ORDER BY id DESC LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*CopyGuardManualReentrySignal{}
	for rows.Next() {
		sig, err := scanManualReentrySignal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, rows.Err()
}
