package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ============================================================================
// 重入 AI 助手（Reentry Advisor）存储层
//
// 定位：人工重入信号（copy_guard_manual_reentry_signals）产生后，插件为其
// 生成"决策数据包 + Prompt"并落库，供前端可复制透明化展示与后续 AI 分析。
//
// 两张表：
//   reentry_ai_analyses  每条信号的分析记录（同信号可多条：手动"重新生成"
//                        会产生新快照，内/外部 AI 结果各自绑定所属快照）
//   reentry_ai_config    单行全局配置（Phase 1 仅 enabled 生效；provider/
//                        model/prompt_template/confidence_threshold 等列为
//                        Phase 2 内置 AI 分析预留，避免后续 ALTER）
//
// 设计约束（插件化铁律）：本文件只新增表与读取既有表，不修改任何 Copy Guard
// 既有表结构与写路径；插件关闭时这些表静默闲置，对跟单零影响。
// ============================================================================

// 外部/内部 AI 结论标签（准确率对比用；空串=未标注）
const (
	ReentryVerdictEnter = "ENTER"
	ReentryVerdictWait  = "WAIT"
	ReentryVerdictSkip  = "SKIP"
)

// ReentryAIAnalysis 一次数据包快照 + 双 AI 结果载体
type ReentryAIAnalysis struct {
	ID       int64  `json:"id"`
	SignalID int64  `json:"signal_id"`
	TraderID string `json:"trader_id"`
	CycleID  int64  `json:"cycle_id"`
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`

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

	PromptVersion string     `json:"prompt_version"`
	SnapshotAt    time.Time  `json:"snapshot_at"`
	OutcomePnL    *float64   `json:"outcome_pnl,omitempty"` // 周期闭合后回填（Phase 2）
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
}

// ReentryAIConfig 全局配置（单行，id 恒为 1）
type ReentryAIConfig struct {
	Enabled             bool    `json:"enabled"`              // 插件总开关（控制数据包自动生成）
	AIEnabled           bool    `json:"ai_enabled"`           // Phase 2：新信号自动触发内置 AI 分析（默认关；手动"AI 分析"按钮不受此开关限制）
	AutoEntryEnabled    bool    `json:"auto_entry_enabled"`   // Phase 3：AI 结论 ENTER 且置信度达标时自动确认重入（默认关；依赖 ai_enabled）
	Provider            string  `json:"provider"`             // 展示用 provider（实际密钥由 model 指向的 ai_models 行决定）
	Model               string  `json:"model"`                // ai_models 表的模型 ID（空=自动选用已启用的默认模型）
	PromptTemplate      string  `json:"prompt_template"`      // 自定义 System Prompt 模板（空=内置默认；在数据包生成时固化进快照）
	ConfidenceThreshold float64 `json:"confidence_threshold"` // Phase 3：自动入场置信度门槛
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
			trader_id TEXT NOT NULL,
			cycle_id INTEGER NOT NULL,
			symbol TEXT NOT NULL DEFAULT '',
			side TEXT NOT NULL DEFAULT '',
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
			snapshot_at DATETIME DEFAULT CURRENT_TIMESTAMP,
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
			confidence_threshold REAL DEFAULT 0.7,
			timeout_seconds INTEGER DEFAULT 60,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`)
	if err != nil {
		return err
	}
	// 早期建表缺列时老库补列（重复执行报 duplicate column 忽略）
	s.db.Exec(`ALTER TABLE reentry_ai_config ADD COLUMN ai_enabled BOOLEAN DEFAULT 0`)
	s.db.Exec(`ALTER TABLE reentry_ai_config ADD COLUMN auto_entry_enabled BOOLEAN DEFAULT 0`)
	return nil
}

const reentryAnalysisColumns = `id, signal_id, trader_id, cycle_id, symbol, side,
	system_prompt, user_prompt, datapack_json, market_data_available, missing_fields,
	raw_response, verdict, confidence, reasons, external_response, external_verdict,
	prompt_version, snapshot_at, outcome_pnl, created_at, updated_at`

func scanReentryAnalysis(row rowScanner) (*ReentryAIAnalysis, error) {
	var a ReentryAIAnalysis
	var snapshot, created string
	var updated sql.NullString
	var outcome sql.NullFloat64
	if err := row.Scan(&a.ID, &a.SignalID, &a.TraderID, &a.CycleID, &a.Symbol, &a.Side,
		&a.SystemPrompt, &a.UserPrompt, &a.DatapackJSON, &a.MarketDataAvailable, &a.MissingFields,
		&a.RawResponse, &a.Verdict, &a.Confidence, &a.Reasons, &a.ExternalResponse, &a.ExternalVerdict,
		&a.PromptVersion, &snapshot, &outcome, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	if a.SnapshotAt, err = parseDBTime(snapshot); err != nil {
		return nil, fmt.Errorf("reentry analysis %d snapshot_at: %w", a.ID, err)
	}
	if a.CreatedAt, err = parseDBTime(created); err != nil {
		return nil, fmt.Errorf("reentry analysis %d created_at: %w", a.ID, err)
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
		(signal_id, trader_id, cycle_id, symbol, side, system_prompt, user_prompt, datapack_json,
		 market_data_available, missing_fields, prompt_version, snapshot_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		a.SignalID, a.TraderID, a.CycleID, a.Symbol, a.Side, a.SystemPrompt, a.UserPrompt, a.DatapackJSON,
		a.MarketDataAvailable, a.MissingFields, a.PromptVersion)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetReentryAnalysis(id)
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
	switch verdict {
	case "", ReentryVerdictEnter, ReentryVerdictWait, ReentryVerdictSkip:
	default:
		return fmt.Errorf("无效的内部结论标签: %s", verdict)
	}
	res, err := s.db.Exec(`UPDATE reentry_ai_analyses SET raw_response=?, verdict=?, confidence=?, reasons=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`,
		rawResponse, verdict, confidence, reasons, id)
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
// 准确率口径：仅统计有结局盈亏的记录；ENTER 且盈利 / SKIP 且亏损 记为正确；
// WAIT 不计入（无对错基准）。结局盈亏 = 该信号重入尝试的已对账净额（pnl−fee）。
type ReentryAIStats struct {
	TotalAnalyses  int `json:"total_analyses"`
	SignalsCovered int `json:"signals_covered"`
	ScoredCount    int `json:"scored_count"` // 已回填结局的分析记录数

	InternalVerdicts map[string]int `json:"internal_verdicts"` // ENTER/WAIT/SKIP → 数量
	ExternalVerdicts map[string]int `json:"external_verdicts"`

	InternalScored  int `json:"internal_scored"` // 内部结论中可评分（ENTER/SKIP 且有结局）
	InternalCorrect int `json:"internal_correct"`
	ExternalScored  int `json:"external_scored"`
	ExternalCorrect int `json:"external_correct"`
}

// GetReentryAIStats 汇总统计（表量级小，直接聚合查询）
func (s *ReentryAIStore) GetReentryAIStats() (*ReentryAIStats, error) {
	st := &ReentryAIStats{
		InternalVerdicts: map[string]int{},
		ExternalVerdicts: map[string]int{},
	}
	err := s.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT signal_id),
		COALESCE(SUM(CASE WHEN outcome_pnl IS NOT NULL THEN 1 ELSE 0 END),0)
		FROM reentry_ai_analyses`).Scan(&st.TotalAnalyses, &st.SignalsCovered, &st.ScoredCount)
	if err != nil {
		return nil, err
	}
	fill := func(col string, verdicts map[string]int, scored, correct *int) error {
		rows, err := s.db.Query(`SELECT ` + col + `, COUNT(*),
			COALESCE(SUM(CASE WHEN outcome_pnl IS NOT NULL AND ` + col + ` IN ('ENTER','SKIP') THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN outcome_pnl IS NOT NULL AND ((` + col + `='ENTER' AND outcome_pnl>0) OR (` + col + `='SKIP' AND outcome_pnl<=0)) THEN 1 ELSE 0 END),0)
			FROM reentry_ai_analyses WHERE ` + col + ` != '' GROUP BY ` + col)
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
	err := s.db.QueryRow(`SELECT enabled, ai_enabled, auto_entry_enabled, provider, model, prompt_template, confidence_threshold, timeout_seconds FROM reentry_ai_config WHERE id=1`).
		Scan(&cfg.Enabled, &cfg.AIEnabled, &cfg.AutoEntryEnabled, &cfg.Provider, &cfg.Model, &cfg.PromptTemplate, &cfg.ConfidenceThreshold, &cfg.TimeoutSeconds)
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
	_, err := s.db.Exec(`INSERT INTO reentry_ai_config (id, enabled, ai_enabled, auto_entry_enabled, provider, model, prompt_template, confidence_threshold, timeout_seconds, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, ai_enabled=excluded.ai_enabled, auto_entry_enabled=excluded.auto_entry_enabled,
			provider=excluded.provider, model=excluded.model,
			prompt_template=excluded.prompt_template, confidence_threshold=excluded.confidence_threshold,
			timeout_seconds=excluded.timeout_seconds, updated_at=CURRENT_TIMESTAMP`,
		cfg.Enabled, cfg.AIEnabled, cfg.AutoEntryEnabled, cfg.Provider, cfg.Model, cfg.PromptTemplate, cfg.ConfidenceThreshold, cfg.TimeoutSeconds)
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
