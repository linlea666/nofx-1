// Decision mode types (defined early for use throughout the file)
export type DecisionMode = 'ai' | 'copy_trade'
export type CopyTradeProvider = 'hyperliquid' | 'okx' | 'binance'

export interface SystemStatus {
  trader_id: string
  trader_name: string
  ai_model: string
  is_running: boolean
  start_time: string
  runtime_minutes: number
  call_count: number
  initial_balance: number
  scan_interval: string
  stop_until: string
  last_reset_time: string
  ai_provider: string
}

export interface AccountInfo {
  total_equity: number
  wallet_balance: number
  unrealized_profit: number // 未实现盈亏（交易所API官方值）
  available_balance: number
  total_pnl: number
  total_pnl_pct: number
  initial_balance: number
  daily_pnl: number
  position_count: number
  margin_used: number
  margin_used_pct: number
}

export interface Position {
  symbol: string
  side: string
  entry_price: number
  mark_price: number
  quantity: number
  leverage: number
  unrealized_pnl: number
  unrealized_pnl_pct: number
  liquidation_price: number
  margin_used: number
}

export interface DecisionAction {
  action: string
  symbol: string
  quantity: number
  leverage: number
  price: number
  stop_loss?: number // Stop loss price
  take_profit?: number // Take profit price
  confidence?: number // AI confidence (0-100)
  reasoning?: string // Brief reasoning
  order_id: number
  timestamp: string
  success: boolean
  error?: string
  execution_intent_id?: number
  source_fill_id?: string
  leader_pos_id?: string
  source_revision?: number
  requested_quantity?: number
  quantized_quantity?: number
  filled_quantity?: number
  quantity_step?: number
  exchange_min_quantity?: number
  exchange_min_notional?: number
  minimum_executable_quantity?: number
  exchange_order_id?: string
  exchange_order_state?: string
  execution_status?:
    | 'RESERVED'
    | 'SUBMITTED'
    | 'FILLED'
    | 'PROTECTED'
    | 'SKIPPED'
    | 'FAILED'
    | 'RECONCILING'
  execution_reason_code?: string
  protection_status?: string
  protection_coverage?: number
  copy_guard_cycle_status?: string
}

export interface AccountSnapshot {
  total_balance: number
  available_balance: number
  total_unrealized_profit: number
  position_count: number
  margin_used_pct: number
}

export interface DecisionRecord {
  timestamp: string
  cycle_number: number
  input_prompt: string
  cot_trace: string
  decision_json: string
  account_state: AccountSnapshot
  positions: any[]
  candidate_coins: string[]
  decisions: DecisionAction[]
  execution_log: string[]
  success: boolean
  error_message?: string
}

export interface Statistics {
  total_cycles: number
  successful_cycles: number
  failed_cycles: number
  total_open_positions: number
  total_close_positions: number
}

// AI Trading相关类型
export interface TraderInfo {
  trader_id: string
  trader_name: string
  ai_model: string
  exchange_id?: string
  is_running?: boolean
  show_in_competition?: boolean
  strategy_id?: string
  strategy_name?: string
  custom_prompt?: string
  use_coin_pool?: boolean
  use_oi_top?: boolean
  system_prompt_template?: string
  decision_mode?: DecisionMode // "ai" or "copy_trade"
}

export interface AIModel {
  id: string
  name: string
  provider: string
  enabled: boolean
  apiKey?: string
  customApiUrl?: string
  customModelName?: string
}

export interface Exchange {
  id: string // UUID (empty for supported exchange templates)
  exchange_type: string // "binance", "bybit", "okx", "hyperliquid", "aster", "lighter"
  account_name: string // User-defined account name
  name: string // Display name
  type: 'cex' | 'dex'
  enabled: boolean
  apiKey?: string
  secretKey?: string
  passphrase?: string // OKX specific
  testnet?: boolean
  // Hyperliquid specific
  hyperliquidWalletAddr?: string
  // Aster specific
  asterUser?: string
  asterSigner?: string
  asterPrivateKey?: string
  // LIGHTER specific
  lighterWalletAddr?: string
  lighterPrivateKey?: string
  lighterApiKeyPrivateKey?: string
  lighterApiKeyIndex?: number
}

export interface CopyGuardAccountRiskPolicy {
  exchange_id: string
  max_position_loss_pct: number
  source?: string
  updated_at?: string
}

export interface CopyGuardAccountRiskPolicyResponse {
  policy: CopyGuardAccountRiskPolicy
  open_protected_positions: number
  aggregate_worst_case_risk_usd: number
  aggregate_is_warning_only: boolean
  aggregate_risk_quality: 'ESTIMATED_CONFIG_CAP'
}

export interface CreateExchangeRequest {
  exchange_type: string // "binance", "bybit", "okx", "hyperliquid", "aster", "lighter"
  account_name: string // User-defined account name
  enabled: boolean
  api_key?: string
  secret_key?: string
  passphrase?: string
  testnet?: boolean
  hyperliquid_wallet_addr?: string
  aster_user?: string
  aster_signer?: string
  aster_private_key?: string
  lighter_wallet_addr?: string
  lighter_private_key?: string
  lighter_api_key_private_key?: string
  lighter_api_key_index?: number
}

export interface CreateTraderRequest {
  name: string
  ai_model_id: string
  exchange_id: string
  strategy_id?: string // 策略ID（新版，使用保存的策略配置）
  initial_balance?: number // 可选：创建时由后端自动获取，编辑时可手动更新
  scan_interval_minutes?: number
  is_cross_margin?: boolean
  show_in_competition?: boolean // 是否在竞技场显示
  // 以下字段为向后兼容保留，新版使用策略配置
  btc_eth_leverage?: number
  altcoin_leverage?: number
  trading_symbols?: string
  custom_prompt?: string
  override_base_prompt?: boolean
  system_prompt_template?: string
  use_coin_pool?: boolean
  use_oi_top?: boolean
  // Copy trading configuration
  decision_mode?: DecisionMode // "ai" or "copy_trade"
  copy_config?: CopyConfigRequest // Copy trade config (required when decision_mode is "copy_trade")
}

export type BinanceSourceMode = 'copy_management' | 'smart_money'

export type CopyTradeSourceHealthStatus =
  | 'HEALTHY'
  | 'PRIVATE'
  | 'DISABLED'
  | 'AUTH_FAILED'
  | 'DEGRADED'
  | 'STALE'

export interface CopyTradeSourceHealth {
  trader_id: string
  leader_id: string
  source_mode: string
  source_generation: number
  status: CopyTradeSourceHealthStatus
  previous_status?: CopyTradeSourceHealthStatus
  trader_name?: string
  last_checked_at?: string
  last_complete_snapshot_at?: string
  last_transition_at?: string
  consecutive_failures: number
  last_error?: string
  unsupported_contracts?: Array<{
    leader_pos_id: string
    source_symbol: string
    execution_venue?: string
    reason: string
    last_seen_at: string
  }>
}

// Copy config request (simplified version for creating traders)
export interface CopyConfigRequest {
  provider_type: CopyTradeProvider
  leader_id: string
  copy_ratio: number
  sync_leverage: boolean
  sync_margin_mode?: boolean // 同步保证金模式（OKX 区分全仓/逐仓）
  min_trade_warn?: number // 纯运营预警阈值；不参与交易所最小量或订单量化
  max_trade_warn?: number // 最大跟单金额预警阈值（USDT），0=不预警；未传保留存量值
  // Binance Web 凭证（仅 provider_type=binance 时使用）
  binance_p20t?: string // 登录 cookie p20t
  binance_csrf_token?: string // CSRF header csrftoken
  binance_source_mode?: BinanceSourceMode
  binance_top_trader_id?: string

  // ============================================================
  // Copy Guard v7：可靠止损、风险预算与 AI guarded 重入
  // 所有字段可选，未传走后端默认值。
  // v3 遗留字段（risk_atr_enabled / risk_reentry_tolerance / 反加仓铁律 /
  // risk_stop_noise_floor_atr / risk_cycle_max_loss_pct）已随 v5 下线。
  // ============================================================
  risk_stop_loss_enabled?: boolean // 默认 true：启用账户保护硬止损
  risk_stop_max_account_loss_pct?: number // 0=继承执行账户默认值（默认10%）
  risk_account_pct?: number // 默认 0.02：单次尝试风险预算
  risk_cycle_loss_budget_pct?: number
  risk_portfolio_loss_budget_pct?: number
  risk_round_trip_fee_bps?: number
  risk_atr_multiplier?: number // 默认 2.0：SL 距离基线 = k×ATR（1.0-3.0，抗噪主力线）
  risk_atr_timeframe?: string // 默认 "1h"：ATR 时间周期（"15m" / "1h" / "4h"）
  risk_leverage_fallback?: boolean // 默认 false：margin_cap 默认关（高杠杆下会压进噪音区）
  risk_leverage_max_loss?: number // 默认 0.2：仅 risk_leverage_fallback 开启时的保证金封顶
  risk_reentry_enabled?: boolean // 默认 true：止损完全平仓后进入 AI 持续观察
  risk_reentry_ratio?: number // 默认 0.5：重入仓位系数（× 被止损仓位名义）
  risk_reentry_decision_mode?: 'ai_guarded' | 'legacy_rule' | 'disabled'
  risk_reentry_min_notional?: number // 0=使用交易所合约最小名义，不继承预警阈值
  risk_ai_confidence_threshold?: number
  risk_ai_min_review_seconds?: number
  risk_ai_daily_call_limit?: number
  risk_ai_lifecycle_call_limit?: number
  risk_notification_level?: 'important' | 'critical' | 'verbose'
  // 历史兼容字段；v7 固定 false，不再逐笔人工确认
  risk_manual_reentry_enabled?: boolean

  risk_policy_version?: number
  risk_stop_mode?: 'volatility_priority' | 'account_hard_limit'
  risk_atr_period?: number
  risk_atr_cache_max_age_minutes?: number
  risk_atr_fallback_pct?: number
  risk_trigger_price_type?: 'mark' | 'last' | 'index'
  risk_slippage_buffer_bps?: number
  risk_liquidation_buffer_atr?: number
  risk_max_reentries?: number
  risk_reentry_band_atr?: number
  risk_reentry_cooldown_seconds?: number
  risk_reentry_max_chase_atr?: number
  risk_reentry_max_atr_expansion?: number
  risk_watch_timeout_minutes?: number
  risk_migration_confirmed?: boolean
  risk_addon_budget_pct?: number // deprecated：仅协议兼容，普通跟单加仓不读取
  risk_high_risk_confirmed?: boolean
  risk_extreme_risk_confirm_value?: number
  risk_stop_extreme_confirm_value?: number

  // v4.1 重入加严
  risk_reentry_min_recovery_atr?: number // 默认 0.5：重入前价格须从止损价恢复的最小幅度（ATR 倍数）
  risk_reentry_cooldown_escalation?: number // 默认 3：第 N 次重入冷却时间倍率（cooldown × 倍率^N）
  risk_reentry_recovery_escalation?: number // 默认 1.5：第 N 次重入恢复幅度倍率

  // v5 可保护性状态机 / 噪音档重入
  risk_unprotectable_action?: 'close' | 'follow' // follow 仅旧协议兼容；所有新保存与运行时均强制 close
  risk_reentry_noise_override?: boolean // 默认 false：距离/ATR<0.3 的噪音档仍允许重入（默认该档禁止重入）
}

export interface UpdateModelConfigRequest {
  models: {
    [key: string]: {
      enabled: boolean
      api_key: string
      custom_api_url?: string
      custom_model_name?: string
    }
  }
}

export interface UpdateExchangeConfigRequest {
  exchanges: {
    [key: string]: {
      enabled: boolean
      api_key: string
      secret_key: string
      passphrase?: string
      testnet?: boolean
      // Hyperliquid 特定字段
      hyperliquid_wallet_addr?: string
      // Aster 特定字段
      aster_user?: string
      aster_signer?: string
      aster_private_key?: string
      // LIGHTER 特定字段
      lighter_wallet_addr?: string
      lighter_private_key?: string
      lighter_api_key_private_key?: string
      lighter_api_key_index?: number
    }
  }
}

// Competition related types
export interface CompetitionTraderData {
  trader_id: string
  trader_name: string
  ai_model: string
  exchange: string
  total_equity: number
  total_pnl: number
  total_pnl_pct: number
  position_count: number
  margin_used_pct: number
  is_running: boolean
}

export interface CompetitionData {
  traders: CompetitionTraderData[]
  count: number
}

// Trader Configuration Data for View Modal
export interface TraderConfigData {
  trader_id?: string
  trader_name: string
  ai_model: string
  exchange_id: string
  strategy_id?: string // 策略ID
  strategy_name?: string // 策略名称
  is_cross_margin: boolean
  show_in_competition: boolean // 是否在竞技场显示
  scan_interval_minutes: number
  initial_balance: number
  is_running: boolean
  decision_mode?: DecisionMode // "ai" or "copy_trade"
  // 以下为旧版字段（向后兼容）
  btc_eth_leverage?: number
  altcoin_leverage?: number
  trading_symbols?: string
  custom_prompt?: string
  override_base_prompt?: boolean
  system_prompt_template?: string
  use_coin_pool?: boolean
  use_oi_top?: boolean
}

// Backtest types
export interface BacktestRunSummary {
  symbol_count: number
  decision_tf: string
  processed_bars: number
  progress_pct: number
  equity_last: number
  max_drawdown_pct: number
  liquidated: boolean
  liquidation_note?: string
}

export interface BacktestRunMetadata {
  run_id: string
  label?: string
  user_id?: string
  last_error?: string
  version: number
  state: string
  created_at: string
  updated_at: string
  summary: BacktestRunSummary
}

export interface BacktestRunsResponse {
  total: number
  items: BacktestRunMetadata[]
}

export interface BacktestStatusPayload {
  run_id: string
  state: string
  progress_pct: number
  processed_bars: number
  current_time: number
  decision_cycle: number
  equity: number
  unrealized_pnl: number
  realized_pnl: number
  note?: string
  last_error?: string
  last_updated_iso: string
}

export interface BacktestEquityPoint {
  ts: number
  equity: number
  available: number
  pnl: number
  pnl_pct: number
  dd_pct: number
  cycle: number
}

export interface BacktestTradeEvent {
  ts: number
  symbol: string
  action: string
  side?: string
  qty: number
  price: number
  fee: number
  slippage: number
  order_value: number
  realized_pnl: number
  leverage?: number
  cycle: number
  position_after: number
  liquidation: boolean
  note?: string
}

export interface BacktestMetrics {
  total_return_pct: number
  max_drawdown_pct: number
  sharpe_ratio: number
  profit_factor: number
  win_rate: number
  trades: number
  avg_win: number
  avg_loss: number
  best_symbol: string
  worst_symbol: string
  liquidated: boolean
  symbol_stats?: Record<
    string,
    {
      total_trades: number
      winning_trades: number
      losing_trades: number
      total_pnl: number
      avg_pnl: number
      win_rate: number
    }
  >
}

export interface BacktestStartConfig {
  run_id?: string
  ai_model_id?: string
  symbols: string[]
  timeframes: string[]
  decision_timeframe: string
  decision_cadence_nbars: number
  start_ts: number
  end_ts: number
  initial_balance: number
  fee_bps: number
  slippage_bps: number
  fill_policy: string
  prompt_variant?: string
  prompt_template?: string
  custom_prompt?: string
  override_prompt?: boolean
  cache_ai?: boolean
  replay_only?: boolean
  checkpoint_interval_bars?: number
  checkpoint_interval_seconds?: number
  replay_decision_dir?: string
  shared_ai_cache_path?: string
  ai?: {
    provider?: string
    model?: string
    key?: string
    secret_key?: string
    base_url?: string
  }
  leverage?: {
    btc_eth_leverage?: number
    altcoin_leverage?: number
  }
}

// Strategy Studio Types
export interface Strategy {
  id: string
  name: string
  description: string
  is_active: boolean
  is_default: boolean
  config: StrategyConfig
  created_at: string
  updated_at: string
}

export interface PromptSectionsConfig {
  role_definition?: string
  trading_frequency?: string
  entry_standards?: string
  decision_process?: string
}

export interface StrategyConfig {
  coin_source: CoinSourceConfig
  indicators: IndicatorConfig
  custom_prompt?: string
  risk_control: RiskControlConfig
  prompt_sections?: PromptSectionsConfig
}

export interface CoinSourceConfig {
  source_type: 'static' | 'coinpool' | 'oi_top' | 'mixed'
  static_coins?: string[]
  use_coin_pool: boolean
  coin_pool_limit?: number
  coin_pool_api_url?: string // AI500 币种池 API URL
  use_oi_top: boolean
  oi_top_limit?: number
  oi_top_api_url?: string // OI Top API URL
}

export interface IndicatorConfig {
  klines: KlineConfig
  // Raw OHLCV kline data - required for AI analysis
  enable_raw_klines: boolean
  // Technical indicators (optional)
  enable_ema: boolean
  enable_macd: boolean
  enable_rsi: boolean
  enable_atr: boolean
  enable_volume: boolean
  enable_oi: boolean
  enable_funding_rate: boolean
  ema_periods?: number[]
  rsi_periods?: number[]
  atr_periods?: number[]
  external_data_sources?: ExternalDataSource[]
  // 量化数据源（资金流向、持仓变化、价格变化）
  enable_quant_data?: boolean
  quant_data_api_url?: string
  enable_quant_oi?: boolean
  enable_quant_netflow?: boolean
  // OI 排行数据（市场持仓量增减排行）
  enable_oi_ranking?: boolean
  oi_ranking_api_url?: string
  oi_ranking_duration?: string // "1h", "4h", "24h"
  oi_ranking_limit?: number
}

export interface KlineConfig {
  primary_timeframe: string
  primary_count: number
  longer_timeframe?: string
  longer_count?: number
  enable_multi_timeframe: boolean
  // 新增：支持选择多个时间周期
  selected_timeframes?: string[]
}

export interface ExternalDataSource {
  name: string
  type: 'api' | 'webhook'
  url: string
  method: string
  headers?: Record<string, string>
  data_path?: string
  refresh_secs?: number
}

export interface RiskControlConfig {
  // Max number of coins held simultaneously (CODE ENFORCED)
  max_positions: number

  // Trading Leverage - exchange leverage for opening positions (AI guided)
  btc_eth_max_leverage: number // BTC/ETH max exchange leverage
  altcoin_max_leverage: number // Altcoin max exchange leverage

  // Position Value Ratio - single position notional value / account equity (CODE ENFORCED)
  // Max position value = equity × this ratio
  btc_eth_max_position_value_ratio?: number // default: 5 (BTC/ETH max position = 5x equity)
  altcoin_max_position_value_ratio?: number // default: 1 (Altcoin max position = 1x equity)

  // Risk Parameters
  max_margin_usage: number // Max margin utilization, e.g. 0.9 = 90% (CODE ENFORCED)
  min_position_size: number // Min position size in USDT (CODE ENFORCED)
  min_risk_reward_ratio: number // Min take_profit / stop_loss ratio (AI guided)
  min_confidence: number // Min AI confidence to open position (AI guided)
}

// Debate Arena Types
export type DebateStatus =
  | 'pending'
  | 'running'
  | 'voting'
  | 'completed'
  | 'cancelled'
export type DebatePersonality =
  | 'bull'
  | 'bear'
  | 'analyst'
  | 'contrarian'
  | 'risk_manager'

export interface DebateDecision {
  action: string
  symbol: string
  confidence: number
  leverage?: number
  position_pct?: number
  position_size_usd?: number
  stop_loss?: number
  take_profit?: number
  reasoning: string
  // Execution tracking
  executed?: boolean
  executed_at?: string
  order_id?: string
  error?: string
}

export interface DebateSession {
  id: string
  user_id: string
  name: string
  strategy_id: string
  status: DebateStatus
  symbol: string
  interval_minutes: number
  prompt_variant: string
  trader_id?: string
  max_rounds: number
  current_round: number
  final_decision?: DebateDecision
  final_decisions?: DebateDecision[] // Multi-coin decisions
  auto_execute: boolean
  created_at: string
  updated_at: string
}

export interface DebateParticipant {
  id: string
  session_id: string
  ai_model_id: string
  ai_model_name: string
  provider: string
  personality: DebatePersonality
  color: string
  speak_order: number
  created_at: string
}

export interface DebateMessage {
  id: string
  session_id: string
  round: number
  ai_model_id: string
  ai_model_name: string
  provider: string
  personality: DebatePersonality
  message_type: string
  content: string
  decision?: DebateDecision
  decisions?: DebateDecision[] // Multi-coin decisions
  confidence: number
  created_at: string
}

export interface DebateVote {
  id: string
  session_id: string
  ai_model_id: string
  ai_model_name: string
  action: string
  symbol: string
  confidence: number
  leverage?: number
  position_pct?: number
  stop_loss_pct?: number
  take_profit_pct?: number
  reasoning: string
  created_at: string
}

export interface DebateSessionWithDetails extends DebateSession {
  participants: DebateParticipant[]
  messages: DebateMessage[]
  votes: DebateVote[]
}

export interface CreateDebateRequest {
  name: string
  strategy_id: string
  symbol: string
  max_rounds?: number
  interval_minutes?: number // 5, 15, 30, 60 minutes
  prompt_variant?: string // balanced, aggressive, conservative, scalping
  auto_execute?: boolean
  trader_id?: string // Trader to use for auto-execute
  // OI Ranking data options
  enable_oi_ranking?: boolean // Whether to include OI ranking data
  oi_ranking_limit?: number // Number of OI ranking entries (default 10)
  oi_duration?: string // Duration for OI data (1h, 4h, 24h, etc.)
  participants: {
    ai_model_id: string
    personality: DebatePersonality
  }[]
}

export interface DebatePersonalityInfo {
  id: DebatePersonality
  name: string
  emoji: string
  color: string
  description: string
}

// Copy Trading Types
export interface CopyTradeConfig {
  trader_id: string
  provider_type: CopyTradeProvider
  leader_id: string
  copy_ratio: number
  sync_leverage: boolean
  sync_margin_mode: boolean
  min_trade_warn: number
  max_trade_warn: number
  enabled: boolean
  // Binance Web 凭证（仅 provider_type=binance 时使用，明文返回，用于编辑表单回填）
  binance_p20t?: string
  binance_csrf_token?: string
  binance_source_mode?: BinanceSourceMode
  binance_top_trader_id?: string
  source_generation?: number
  // Copy Guard v7，详见 CopyConfigRequest
  risk_stop_loss_enabled?: boolean
  risk_stop_max_account_loss_pct?: number
  risk_account_pct?: number
  risk_cycle_loss_budget_pct?: number
  risk_portfolio_loss_budget_pct?: number
  risk_round_trip_fee_bps?: number
  risk_atr_multiplier?: number
  risk_atr_timeframe?: string
  risk_leverage_fallback?: boolean
  risk_leverage_max_loss?: number
  risk_reentry_enabled?: boolean
  risk_reentry_ratio?: number
  risk_reentry_decision_mode?: 'ai_guarded' | 'legacy_rule' | 'disabled'
  risk_reentry_min_notional?: number
  risk_ai_confidence_threshold?: number
  risk_ai_min_review_seconds?: number
  risk_ai_daily_call_limit?: number
  risk_ai_lifecycle_call_limit?: number
  risk_notification_level?: 'important' | 'critical' | 'verbose'
  risk_manual_reentry_enabled?: boolean // 历史兼容字段；v7 固定 false
  risk_policy_version?: number
  risk_stop_mode?: 'volatility_priority' | 'account_hard_limit'
  risk_atr_period?: number
  risk_atr_cache_max_age_minutes?: number
  risk_atr_fallback_pct?: number
  risk_trigger_price_type?: 'mark' | 'last' | 'index'
  risk_slippage_buffer_bps?: number
  risk_liquidation_buffer_atr?: number
  risk_max_reentries?: number
  risk_reentry_band_atr?: number
  risk_reentry_cooldown_seconds?: number
  risk_reentry_max_chase_atr?: number
  risk_reentry_max_atr_expansion?: number
  risk_watch_timeout_minutes?: number
  risk_migration_confirmed?: boolean
  risk_addon_budget_pct?: number
  risk_high_risk_confirmed?: boolean
  risk_extreme_risk_confirm_value?: number
  // v4.1 重入加严
  risk_reentry_min_recovery_atr?: number
  risk_reentry_cooldown_escalation?: number
  risk_reentry_recovery_escalation?: number
  // v5 可保护性状态机 / 噪音档重入
  risk_unprotectable_action?: 'close' | 'follow'
  risk_reentry_noise_override?: boolean
  created_at?: string
  updated_at?: string
}

export interface CopyTradeConfigResponse {
  config: CopyTradeConfig
  status: boolean
  source_health?: CopyTradeSourceHealth | null
  effective_stop_policy?: {
    exchange_id: string
    effective_risk_stop_max_account_loss_pct: number
    risk_stop_pct_source: 'account_default' | 'trader_override'
  } | null
}

export interface CopyGuardSummary {
  follower_count: number
  cycle_count: number
  stop_count: number
  reentry_count: number
  actual_pnl: number
  baseline_pnl: number
  avoided_loss: number
  opportunity_cost: number
  net_guard_effect: number
  stop_only_pnl: number
  reentry_contribution: number
  fees: number
  funding_fee: number
  liquidation_penalty: number
  slippage: number
  protected_count: number
  pending_protection_count: number
  unknown_count: number
  degraded_count: number
  clamped_count: number // v5：活跃仓位中止损被强平价钳紧的数量
  unprotectable_count: number // v5：无法保护，正在强制退出或等待交易所终态的周期数
  accounting_pending_count: number
  accounting_delayed_count: number
  accounting_unrecoverable_count: number
  legacy_unverified_count: number
  average_coverage: number
  ignored_count: number
  reentry_first: number
  reentry_second: number
  reentry_third_plus: number
  max_avoided_loss: number
  max_opportunity_cost: number
  protection_missing_seconds: number
  reentry_success_rate: number
  false_kill_rate: number
  // v5 统计口径修正：比率带样本数；估算基线的净效果单独列示
  reentry_sample_count: number // 重入成功率分母（已结束的重入 attempt 数）
  stopped_cycle_count: number // 误杀率分母（已对账且发生过止损的周期数）
  false_kill_count: number // 误杀次数（分子）
  estimated_baseline_cycles: number // 基线仍为"最后观测价估算"的已对账周期数
  estimated_net_guard_effect: number // 上述周期贡献的净效果（含在 net_guard_effect 内）
  unscorable_baseline_cycles: number // 缺少有效领航员离场价，不进入保护效果统计
  verified_baseline_cycles: number
  reentry_success_estimate: RateEstimate
  false_kill_estimate: RateEstimate
  mean_net_guard_effect_estimate: MeanEstimate
  max_realized_drawdown_usd: number // 已对账止损周期按结束顺序形成的真实盈亏路径最大回撤
  worst_cycle_loss_usd: number // 已对账止损周期的最大单周期亏损绝对值
  tail_loss_cvar_95_usd: number // 样本 95% 尾部平均损失；小样本时等于最差观测
  trend: Array<{
    date: string
    actual: number
    baseline: number
    net_effect: number
  }>
}
export interface RateEstimate {
  numerator: number
  denominator: number
  rate: number
  ci95_low: number
  ci95_high: number
  method: string
  status: string
}
export interface MeanEstimate {
  sample_count: number
  mean: number
  ci95_low: number
  ci95_high: number
  method: string
  status: string
}
export interface CopyGuardCycle {
  id: number
  trader_id: string
  trader_name?: string
  leader_id: string
  leader_pos_id: string
  symbol: string
  side: string
  margin_mode: string
  status: string
  stop_count: number
  reentry_count: number
  actual_pnl: number
  baseline_pnl: number
  net_guard_effect: number
  tracking_difference: number
  accounting_status:
    | 'OPEN'
    | 'PENDING'
    | 'RECONCILED'
    | 'DELAYED'
    | 'UNRECOVERABLE'
    | 'LEGACY_UNVERIFIED'
  accounting_error: string
  baseline_source: '' | 'last_observed' | 'leader_history'
  fees: number
  funding_fee: number
  liquidation_penalty: number
  slippage: number
  protection_status:
    | 'PENDING'
    | 'VERIFIED'
    | 'UNKNOWN'
    | 'DEGRADED'
    | 'TRIGGERED'
    | 'CANCELED'
    | 'CLAMPED' // v5：止损价被强平缓冲夹紧（比目标更紧），保护单有效
    | 'UNPROTECTABLE' // v5：确认无法建立有效保护（终态，按 unprotectable_action 处理）
  protection_coverage: number
  protection_retries: number
  protection_error: string
  protection_last_retry_at?: string
  follower_pos_id?: string
  policy_snapshot: string
  opened_at: string
  closed_at?: string
  reconciled_at?: string
}

// 统一跟单事件日志（开仓/加仓/减仓/平仓 + 止损/二次入场/接手/保护/对账）
export type CopyEventCategory =
  | 'action'
  | 'stoploss'
  | 'reentry'
  | 'takeover'
  | 'protection'
  | 'reconcile'
  | 'error'
export type CopyEventSeverity = 'info' | 'warn' | 'error'

export interface CopyTradeEvent {
  id: number
  trader_id: string
  trader_name?: string
  leader_id: string
  provider_type: string // okx | hyperliquid | binance
  category: CopyEventCategory
  event_type: string
  severity: CopyEventSeverity
  symbol: string
  side: string
  margin_mode: string
  leader_pos_id: string
  follower_pos_id: string
  cycle_id: number // 关联 Copy Guard 周期（OKX/Binance 有值，Hyperliquid 为 0）
  signal_id: string
  status: string // success | failed | skipped | ''
  price: number
  quantity: number
  notional: number
  pnl: number
  operator: string // 人工 user_id | ai:auto | ai
  summary: string
  detail?: Record<string, unknown>
  created_at: string
}

// v5.1 人工重入信号（自动重入次数用尽后，合格信号等待用户确认）
export interface CopyGuardManualSignal {
  id: number
  cycle_id: number
  trader_id: string
  trader_name?: string
  leader_pos_id: string
  symbol: string
  side: string
  margin_mode: string
  status:
    | 'PENDING'
    | 'EXECUTING'
    | 'EXECUTED'
    | 'FAILED'
    | 'DISMISSED'
    | 'INVALIDATED'
  trigger_price: number // 信号触发时标记价
  atr: number
  distance_atr_ratio: number // 止损距离/ATR（噪音档参考，0=数据缺失）
  reentry_boundary: number
  recommended_notional: number // 建议重入金额（USDT）
  stop_count: number
  reentry_count: number
  leader_size: number
  leader_entry_price: number
  protectable: boolean // 可保护性预检（仅提示，不拦截确认）
  reason: string
  operator: string
  confirm_price: number
  error: string
  created_at: string
  last_alert_at?: string
  confirmed_at?: string
  executed_at?: string
}

export interface CopyGuardAICandidate {
  id: number
  cycle_id: number
  trader_id: string
  leader_pos_id: string
  symbol: string
  side: string
  margin_mode: string
  status:
    | 'WATCHING'
    | 'REVIEWING'
    | 'WAITING'
    | 'ENTRY_PENDING'
    | 'REENTERED'
    | 'ABANDONED'
    | 'EXPIRED'
    | 'BUDGET_SUSPENDED'
    | 'INVALIDATED'
    | 'PAUSED'
  trigger_price: number
  atr: number
  max_notional: number
  stop_count: number
  reentry_count: number
  leader_size: number
  leader_entry_price: number
  last_stop_price: number
  distance_atr_ratio: number
  protectable: boolean
  feature_hash: string
  pending_trigger: string
  decision_generation: number
  review_count: number
  failure_count: number
  last_decision: string
  regime: string
  confidence: number
  size_factor: number
  entry_price_low: number
  entry_price_high: number
  attention_price_low: number
  attention_price_high: number
  last_analysis_id: number
  last_error: string
  ai_confidence_threshold: number
  ai_min_review_seconds: number
  ai_daily_call_limit: number
  ai_daily_calls_used: number
  ai_lifecycle_call_limit: number
  snapshot_at: string
  last_review_at?: string
  next_review_at: string
  created_at: string
  closed_at?: string
}

// 重入 AI 助手：一次数据包快照 + 双 AI 结果载体（同信号可多条，重新生成产生新快照）
export interface ReentryAIAnalysis {
  id: number
  signal_id: number
  candidate_id: number
  trader_id: string
  cycle_id: number
  symbol: string
  side: string
  attempt_no: number
  decision_generation: number
  call_status:
    | 'PENDING'
    | 'RUNNING'
    | 'COMPLETED'
    | 'INVALID'
    | 'FAILED'
    | 'PREPARE_FAILED'
    | 'UNACTIONABLE'
    | 'SKIPPED'
  call_error?: string
  data_hash: string
  system_prompt: string
  user_prompt: string
  datapack_json: string // 纯数据 JSON（喂外部 AI 用）
  market_data_available: boolean // false = 该币种无 Binance 市场数据（仓位层仍完整）
  missing_fields: string // 逗号分隔的降级字段列表
  raw_response: string // 内部 AI 返回（Phase 2 起）
  verdict: string
  confidence: number
  reasons: string
  external_response: string // 用户粘贴的外部 AI 结论（永久可编辑）
  external_verdict: '' | 'ENTER' | 'WAIT' | 'SKIP'
  prompt_version: string
  snapshot_at: string
  model_started_at?: string
  model_completed_at?: string
  decision_expires_at?: string
  outcome_pnl?: number
  created_at: string
  updated_at?: string
}

// AI 决策的确定性后验评价。仅使用已落库行情、事件和真实对账结果，
// 不由模型给自己打分；evaluation_version 固定评价口径，便于跨版本比较。
export interface ReentryAIDecisionEvaluation {
  id: number
  analysis_id: number
  candidate_id: number
  trader_id: string
  trader_name_snapshot: string
  cycle_id: number
  attempt_no: number
  decision_generation: number
  decision: string
  horizon: '30_MINUTES' | '2_HOURS' | 'LEADER_FINAL'
  evaluation_version: number
  evaluation_status: string
  data_quality: 'VERIFIED' | 'ESTIMATED' | 'UNSCORABLE'
  execution_data_quality:
    | 'VERIFIED'
    | 'ESTIMATED'
    | 'UNSCORABLE'
    | 'NOT_APPLICABLE'
  market_outcome:
    | 'REVERSAL_CONFIRMED'
    | 'CONTINUED_AGAINST'
    | 'CHOP_INCONCLUSIVE'
    | 'INSUFFICIENT_DATA'
  decision_outcome: string
  actionability: string
  reason: string
  reference_price: number
  reference_atr: number
  mfe_atr: number
  mae_atr: number
  first_reversal_at?: string
  window_start_at: string
  window_end_at: string
  sample_count: number
  coverage_ratio: number
  max_gap_seconds: number
  actual_executed: boolean
  execution_requested: boolean
  execution_submitted: boolean
  execution_filled: boolean
  execution_protected: boolean
  actual_pnl?: number
  evaluation_latency_seconds: number
  created_at: string
  updated_at: string
}

export interface CopyGuardAIEffectSummary {
  cycle_id: number
  evaluation_version: number
  total_decisions: number
  scorable_decisions: number
  unscorable_decisions: number
  decision_counts: Record<string, number>
  decision_outcome_counts: Record<string, number>
  market_outcome_counts: Record<string, number>
  missed_reversals: number
  correct_abandons: number
  risk_gate_saved_losses: number
  enter_decisions: number
  execution_requested: number
  execution_submitted: number
  execution_filled: number
  execution_protected: number
  actual_reentry_pnl: number
  final_decision: string
  final_decision_outcome: string
}

// 重入 AI 助手全局配置
export interface ReentryAIConfig {
  enabled: boolean // 插件总开关（数据包自动生成）
  ai_enabled: boolean // 新信号自动触发内置 AI 分析
  auto_entry_enabled: boolean // ai_guarded 候选真实执行的全局安全开关（依赖 ai_enabled）
  provider: string
  model: string // ai_models 表的模型 ID（空=自动选默认）
  prompt_template: string // 仅历史人工信号分析兼容；不影响 ai_guarded
  analysis_focus: string // 追加到不可覆盖的生产 Prompt 后的分析关注点
  confidence_threshold: number // 仅历史人工信号分析兼容
  timeout_seconds: number
}

export interface ReentryAIDiagnostic {
  id: number
  provider: string
  model: string
  prompt_version: string
  success: boolean
  latency_ms: number
  raw_response: string
  parsed_json: string
  error?: string
  created_at: string
}

// 重入 AI 结论分布与准确率统计（Phase 2）
export interface ReentryAIStats {
  total_analyses: number
  signals_covered: number
  scored_count: number
  internal_verdicts: Record<string, number>
  external_verdicts: Record<string, number>
  internal_scored: number
  internal_correct: number
  external_scored: number
  external_correct: number
  candidate_analyses: number
  candidate_decisions: Record<string, number>
  candidate_call_statuses: Record<string, number>
  candidate_scored: number
  candidate_profitable: number
  candidate_evaluated: number
  candidate_unscorable: number
  candidate_execution_requested: number
  candidate_execution_submitted: number
  candidate_execution_filled: number
  candidate_execution_protected: number
  candidate_evaluation_outcomes: Record<string, number>
  candidate_market_outcomes: Record<string, number>
}

// 市场指标实时预览（A2，与信号无关；market 结构与数据包 market 段一致，
// 此处只声明卡片展示用到的字段，完整内容以原始 JSON 折叠段展示）
export interface ReentryMarketPreview {
  symbol: string
  generated_at: string
  futures_available: boolean
  spot_available: boolean
  atr_okx_1h: number
  market: {
    current_price: number
    current_price_source: string
    klines?: Record<
      string,
      { pct_change_window: number; volume_ratio_5_20: number }
    >
    contract_cvd?: Record<
      string,
      { slope_recent: string; divergence_note?: string }
    >
    spot_cvd?: Record<
      string,
      { slope_recent: string; divergence_note?: string }
    >
    open_interest?: {
      latest_usd: number
      change_pct: Record<string, number>
      price_oi_read_4h: string
    }
    funding?: {
      current_rate: number
      state: string
      current_percentile_10d: number
      next_funding_minutes: number
    }
    long_short_ratio?: {
      global_accounts_ratio: number
      top_positions_ratio: number
      global_trend_24h: string
    }
    basis?: { basis_pct: number }
    support_resistance?: Record<
      string,
      {
        nearest_support: number
        support_touches: number
        support_distance_atr: number
        nearest_resistance: number
        resistance_touches: number
        resistance_distance_atr: number
      }
    >
    spot_to_contract_volume_ratio_24h?: number
  } | null
  missing_fields?: string[]
}

export interface CopyTradeStats {
  signals_received: number
  signals_followed: number
  signals_skipped: number
  decisions_generated: number
  warnings_count: number
  last_signal_time: string
  start_time: string
}

export interface CopyTradeSignalLog {
  id: number
  trader_id: string
  leader_id: string
  provider_type: string
  signal_id: string
  symbol: string
  action: string
  position_side: string
  leader_price: number
  leader_value: number
  copy_size: number
  followed: boolean
  follow_reason: string
  warnings_json: string
  status: string
  error_message: string
  created_at: string
}

// ============================================================================
// Binance 全局共享凭证（v2 凭证全局化）
// ============================================================================
// 所有 Binance 跟单 trader 共享同一份凭证，避免逐个交易员维护

export type BinanceCredsStatus = 'valid' | 'expired' | 'unknown' | 'error'

// 凭证视图（脱敏后；对应后端 BinanceCredentialsView）
export interface BinanceCredentialsView {
  label: string
  binance_user_id: string
  masked_p20t: string
  masked_csrf_token: string
  last_validated_at: string
  last_status: BinanceCredsStatus
  last_error: string
  created_at: string
  updated_at: string
}

export interface BinanceCredentialsListResponse {
  credentials: BinanceCredentialsView[]
  count: number
}

export interface BinanceCredentialsSetRequest {
  label?: string // 默认 'default'
  p20t?: string
  csrftoken?: string
  curl?: string // 任选其一：直接填字段，或粘贴整段 cURL
}

export interface BinanceCredentialsTestResponse {
  label: string
  status: BinanceCredsStatus
  binance_user_id: string
  error: string
}

export interface BinanceCredentialsAffectedResponse {
  trader_ids: string[]
  count: number
}
