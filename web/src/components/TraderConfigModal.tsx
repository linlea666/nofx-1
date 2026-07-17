import { useState, useEffect } from 'react'
import type {
  AIModel,
  Exchange,
  CreateTraderRequest,
  Strategy,
  DecisionMode,
  CopyTradeProvider,
  CopyTradeConfig,
  BinanceCredentialsView,
} from '../types'
import { useLanguage } from '../contexts/LanguageContext'
import { t } from '../i18n/translations'
import { toast } from 'sonner'
import {
  Pencil,
  Plus,
  X as IconX,
  Sparkles,
  ExternalLink,
  UserPlus,
  Bot,
  Users,
  KeyRound,
  CheckCircle2,
  AlertCircle,
} from 'lucide-react'
import { httpClient } from '../lib/httpClient'
import { api } from '../lib/api'
import { BinanceGlobalCredsModal } from './traders/BinanceGlobalCredsModal'

// 提取下划线后面的名称部分
function getShortName(fullName: string): string {
  const parts = fullName.split('_')
  return parts.length > 1 ? parts[parts.length - 1] : fullName
}

// Copy Guard 账户保护支持的领航员数据源（与后端 copytrade.SupportsCopyGuard 对齐）。
// Hyperliquid 无稳定的仓位级 posId，不进入 Copy Guard 状态机。
function copyGuardCapableProvider(provider: CopyTradeProvider): boolean {
  return provider === 'okx' || provider === 'binance'
}

// 交易所注册链接配置
const EXCHANGE_REGISTRATION_LINKS: Record<
  string,
  { url: string; hasReferral?: boolean }
> = {
  binance: {
    url: 'https://www.binance.com/join?ref=NOFXENG',
    hasReferral: true,
  },
  okx: { url: 'https://www.okx.com/join/1865360', hasReferral: true },
  bybit: { url: 'https://partner.bybit.com/b/83856', hasReferral: true },
  hyperliquid: {
    url: 'https://app.hyperliquid.xyz/join/AITRADING',
    hasReferral: true,
  },
  aster: {
    url: 'https://www.asterdex.com/en/referral/fdfc0e',
    hasReferral: true,
  },
  lighter: {
    url: 'https://app.lighter.xyz/?referral=68151432',
    hasReferral: true,
  },
}

import type { TraderConfigData } from '../types'

// 表单内部状态类型
interface FormState {
  trader_id?: string
  trader_name: string
  ai_model: string
  exchange_id: string
  strategy_id: string
  is_cross_margin: boolean
  show_in_competition: boolean
  scan_interval_minutes: number
  initial_balance?: number
  // 跟单相关
  decision_mode: DecisionMode
  copy_provider_type: CopyTradeProvider
  copy_leader_id: string
  copy_ratio: number
  copy_sync_leverage: boolean
  copy_sync_margin_mode: boolean // 同步保证金模式（OKX 区分全仓/逐仓）
  // Binance Web 私有接口凭证（仅 copy_provider_type=binance 时使用）
  copy_binance_p20t: string
  copy_binance_csrf_token: string
  // ============================================================
  // 账户保护 / 止损兜底（Copy Guard v5）—— OKX / Binance 数据源生效
  // 所有字段都有合理默认值，用户可在 UI 调整。
  // v3 遗留字段（atr_enabled / reentry_tolerance / 反加仓铁律 /
  // stop_noise_floor / cycle_max_loss）已随 v5 下线。
  // ============================================================
  risk_stop_loss_enabled: boolean // 默认 true：启用账户保护硬止损
  risk_account_pct: number // 单次尝试风险（前端百分比）
  risk_cycle_loss_budget_pct: number
  risk_portfolio_loss_budget_pct: number
  risk_round_trip_fee_bps: number
  risk_atr_multiplier: number // 默认 2.0
  risk_atr_timeframe: string // 默认 "1h"
  risk_leverage_fallback: boolean // 默认 false（margin_cap 默认关）
  risk_leverage_max_loss: number // 默认 20%（前端用百分比展示，提交时转 0.2）：保证金硬 cap
  risk_reentry_enabled: boolean // 默认 true：AI guarded 持续观察重入
  risk_reentry_ratio: number // 默认 50%（前端用百分比，提交时转 0.5）
  risk_reentry_decision_mode: 'ai_guarded' | 'legacy_rule' | 'disabled'
  risk_ai_confidence_threshold: number // 前端百分比
  risk_ai_min_review_seconds: number
  risk_ai_daily_call_limit: number
  risk_ai_lifecycle_call_limit: number
  risk_notification_level: 'important' | 'critical' | 'verbose'
  risk_manual_reentry_enabled: boolean // 废弃兼容字段，始终 false
  risk_policy_version: number
  // v5：不再影响止损计算（账户线在任何模式下都是硬 cap），仅作兼容透传，无 UI
  risk_stop_mode: 'volatility_priority' | 'account_hard_limit'
  risk_atr_period: number
  risk_atr_cache_max_age_minutes: number
  risk_atr_fallback_pct: number
  risk_trigger_price_type: 'mark' | 'last' | 'index'
  risk_slippage_buffer_bps: number
  risk_liquidation_buffer_atr: number
  risk_max_reentries: number
  risk_reentry_band_atr: number
  risk_reentry_cooldown_seconds: number
  risk_reentry_max_chase_atr: number
  risk_reentry_max_atr_expansion: number
  risk_watch_timeout_minutes: number
  risk_migration_confirmed: boolean
  risk_addon_budget_pct: number // 默认 15%：超限自动缩量，低于最小量拒绝
  // v4.1 重入加严
  risk_reentry_min_recovery_atr: number // 默认 0.5：重入最小恢复幅度（ATR 倍数）
  risk_reentry_cooldown_escalation: number // 默认 3：第 N 次重入冷却倍率
  risk_reentry_recovery_escalation: number // 默认 1.5：第 N 次重入恢复幅度倍率
  // v5 可保护性状态机 / 噪音档重入
  risk_unprotectable_action: 'close' | 'follow' // 默认 close：确认不可保护时立即离场
  risk_reentry_noise_override: boolean // 默认 false：噪音档（止损距离/ATR<0.3）仍允许重入
}

interface TraderConfigModalProps {
  isOpen: boolean
  onClose: () => void
  traderData?: TraderConfigData | null
  isEditMode?: boolean
  availableModels?: AIModel[]
  availableExchanges?: Exchange[]
  onSave?: (data: CreateTraderRequest) => Promise<void>
}

export function TraderConfigModal({
  isOpen,
  onClose,
  traderData,
  isEditMode = false,
  availableModels = [],
  availableExchanges = [],
  onSave,
}: TraderConfigModalProps) {
  const { language } = useLanguage()
  const [formData, setFormData] = useState<FormState>({
    trader_name: '',
    ai_model: '',
    exchange_id: '',
    strategy_id: '',
    is_cross_margin: true,
    show_in_competition: true,
    scan_interval_minutes: 3,
    decision_mode: 'copy_trade', // 默认跟单模式
    copy_provider_type: 'hyperliquid',
    copy_leader_id: '',
    copy_ratio: 1.0,
    copy_sync_leverage: true,
    copy_sync_margin_mode: true, // 默认同步保证金模式
    copy_binance_p20t: '',
    copy_binance_csrf_token: '',
    // 账户保护 v5 风控默认值（与后端 store.FillRiskDefaults 保持一致）
    risk_stop_loss_enabled: true,
    risk_account_pct: 2,
    risk_cycle_loss_budget_pct: 5,
    risk_portfolio_loss_budget_pct: 8,
    risk_round_trip_fee_bps: 12,
    risk_atr_multiplier: 2.0, // v5.2 抗噪：默认 2.0×ATR
    risk_atr_timeframe: '1h',
    risk_leverage_fallback: false, // v5.2：margin_cap 默认关
    risk_leverage_max_loss: 20, // %（提交时 /100 转 0.2）—— v5 默认 20% 硬 cap
    risk_reentry_enabled: true,
    risk_reentry_ratio: 50, // %（提交时 /100 转 0.5）
    risk_reentry_decision_mode: 'ai_guarded',
    risk_ai_confidence_threshold: 80,
    risk_ai_min_review_seconds: 300,
    risk_ai_daily_call_limit: 12,
    risk_ai_lifecycle_call_limit: 30,
    risk_notification_level: 'important',
    risk_manual_reentry_enabled: false,
    risk_policy_version: 4,
    risk_stop_mode: 'volatility_priority',
    risk_atr_period: 14,
    risk_atr_cache_max_age_minutes: 120,
    risk_atr_fallback_pct: 2,
    risk_trigger_price_type: 'mark',
    risk_slippage_buffer_bps: 10,
    risk_liquidation_buffer_atr: 0.5,
    risk_max_reentries: 2, // v7 AI guarded 最多 2 次
    risk_reentry_band_atr: 0.5,
    risk_reentry_cooldown_seconds: 300, // v4.1：默认冷却 300s
    risk_reentry_max_chase_atr: 0.5,
    risk_reentry_max_atr_expansion: 2,
    risk_watch_timeout_minutes: 4320,
    risk_migration_confirmed: true,
    risk_addon_budget_pct: 15,
    // v4.1 重入加严
    risk_reentry_min_recovery_atr: 0.5,
    risk_reentry_cooldown_escalation: 3,
    risk_reentry_recovery_escalation: 1.5,
    // v5 可保护性状态机 / 噪音档重入
    risk_unprotectable_action: 'close',
    risk_reentry_noise_override: false,
  })
  const [isSaving, setIsSaving] = useState(false)
  const [loadedLegacyReentry, setLoadedLegacyReentry] = useState(false)
  const [strategies, setStrategies] = useState<Strategy[]>([])
  const [isFetchingBalance, setIsFetchingBalance] = useState(false)
  const [balanceFetchError, setBalanceFetchError] = useState<string>('')

  // Binance 全局凭证状态（仅 provider=binance 时拉取，作为状态展示）
  const [binanceGlobalCreds, setBinanceGlobalCreds] =
    useState<BinanceCredentialsView | null>(null)
  const [showBinanceCredsModal, setShowBinanceCredsModal] = useState(false)
  const [refreshGlobalCredsTick, setRefreshGlobalCredsTick] = useState(0)

  // 拉取 Binance 全局凭证状态（用于顶部状态卡展示）
  // 仅在弹窗打开 + provider=binance 时拉取，避免无谓的请求
  useEffect(() => {
    if (!isOpen || formData.copy_provider_type !== 'binance') return
    let cancelled = false
    api
      .listBinanceCredentials()
      .then((list) => {
        if (cancelled) return
        const def = list.find((c) => c.label === 'default') ?? null
        setBinanceGlobalCreds(def)
      })
      .catch(() => {
        /* 静默：状态卡仅展示用，失败时显示"未配置"即可 */
      })
    return () => {
      cancelled = true
    }
  }, [isOpen, formData.copy_provider_type, refreshGlobalCredsTick])

  // 获取用户的策略列表
  useEffect(() => {
    const fetchStrategies = async () => {
      try {
        const result = await httpClient.get<{ strategies: Strategy[] }>(
          '/api/strategies'
        )
        if (result.success && result.data?.strategies) {
          const strategyList = result.data.strategies
          setStrategies(strategyList)
          // 如果没有选择策略，默认选中激活的策略
          if (!formData.strategy_id && !isEditMode) {
            const activeStrategy = strategyList.find((s) => s.is_active)
            if (activeStrategy) {
              setFormData((prev) => ({
                ...prev,
                strategy_id: activeStrategy.id,
              }))
            } else if (strategyList.length > 0) {
              setFormData((prev) => ({
                ...prev,
                strategy_id: strategyList[0].id,
              }))
            }
          }
        }
      } catch (error) {
        console.error('Failed to fetch strategies:', error)
      }
    }
    if (isOpen) {
      fetchStrategies()
    }
  }, [isOpen])

  // 加载跟单配置（仅加载跟单参数，不覆盖 decision_mode）
  useEffect(() => {
    const fetchCopyTradeConfig = async () => {
      if (!isEditMode || !traderData?.trader_id) return
      setLoadedLegacyReentry(false)
      try {
        const result = await httpClient.get<{ config: CopyTradeConfig }>(
          `/api/copytrade/config/${traderData.trader_id}`
        )
        if (result.success && result.data?.config) {
          const cfg = result.data.config
          setLoadedLegacyReentry(
            cfg.risk_reentry_decision_mode === 'legacy_rule'
          )
          // 只加载跟单参数，decision_mode 由 traderData 决定
          // 风控字段：后端用比例（如 0.02=2%）存，前端用百分比展示，× 100 转换
          setFormData((prev) => ({
            ...prev,
            copy_provider_type: cfg.provider_type as CopyTradeProvider,
            copy_leader_id: cfg.leader_id,
            copy_ratio: cfg.copy_ratio,
            copy_sync_leverage: cfg.sync_leverage,
            copy_sync_margin_mode: cfg.sync_margin_mode ?? true, // 默认 true
            copy_binance_p20t: cfg.binance_p20t ?? '',
            copy_binance_csrf_token: cfg.binance_csrf_token ?? '',
            // 风控字段回填（× 100 转百分比展示，与 store.FillRiskDefaults 保持一致）
            risk_stop_loss_enabled: cfg.risk_stop_loss_enabled ?? true,
            risk_account_pct:
              cfg.risk_account_pct != null ? cfg.risk_account_pct * 100 : 2,
            risk_cycle_loss_budget_pct:
              (cfg.risk_cycle_loss_budget_pct ?? 0.05) * 100,
            risk_portfolio_loss_budget_pct:
              (cfg.risk_portfolio_loss_budget_pct ?? 0.08) * 100,
            risk_round_trip_fee_bps: cfg.risk_round_trip_fee_bps ?? 12,
            risk_atr_multiplier: cfg.risk_atr_multiplier ?? 2.0,
            risk_atr_timeframe: cfg.risk_atr_timeframe ?? '1h',
            risk_leverage_fallback: cfg.risk_leverage_fallback ?? false,
            risk_leverage_max_loss:
              cfg.risk_leverage_max_loss != null
                ? cfg.risk_leverage_max_loss * 100
                : 20,
            risk_reentry_enabled: cfg.risk_reentry_enabled ?? true,
            risk_reentry_ratio:
              cfg.risk_reentry_ratio != null
                ? cfg.risk_reentry_ratio * 100
                : 50,
            risk_reentry_decision_mode:
              cfg.risk_reentry_decision_mode ?? 'ai_guarded',
            risk_ai_confidence_threshold:
              (cfg.risk_ai_confidence_threshold ?? 0.8) * 100,
            risk_ai_min_review_seconds: cfg.risk_ai_min_review_seconds ?? 300,
            risk_ai_daily_call_limit: cfg.risk_ai_daily_call_limit ?? 12,
            risk_ai_lifecycle_call_limit:
              cfg.risk_ai_lifecycle_call_limit ?? 30,
            risk_notification_level: cfg.risk_notification_level ?? 'important',
            risk_manual_reentry_enabled: false,
            // `|| 4` 而非 `?? 4`：0 是旧版 API 对非 OKX 数据源剥离
            // risk_policy_version 留下的哨兵值，不是用户选择。本表单只能
            // 编辑 v4+（v5）策略，加载时把 0 归一为 4，否则存量币安配置
            // 会陷入"面板可见但参数区隐藏、保存永续 0、止损永不激活"的
            // 自锁状态（激活仍需用户显式保存，opt-in 语义不变）。
            risk_policy_version: cfg.risk_policy_version || 4,
            risk_stop_mode: cfg.risk_stop_mode ?? 'volatility_priority',
            risk_atr_period: cfg.risk_atr_period ?? 14,
            risk_atr_cache_max_age_minutes:
              cfg.risk_atr_cache_max_age_minutes ?? 120,
            risk_atr_fallback_pct: (cfg.risk_atr_fallback_pct ?? 0.02) * 100,
            risk_trigger_price_type: cfg.risk_trigger_price_type ?? 'mark',
            risk_slippage_buffer_bps: cfg.risk_slippage_buffer_bps ?? 10,
            risk_liquidation_buffer_atr: cfg.risk_liquidation_buffer_atr ?? 0.5,
            risk_max_reentries: cfg.risk_max_reentries ?? 2,
            risk_reentry_band_atr: cfg.risk_reentry_band_atr ?? 0.5,
            risk_reentry_cooldown_seconds:
              cfg.risk_reentry_cooldown_seconds ?? 300,
            risk_reentry_max_chase_atr: cfg.risk_reentry_max_chase_atr ?? 0.5,
            risk_reentry_max_atr_expansion:
              cfg.risk_reentry_max_atr_expansion ?? 2,
            risk_watch_timeout_minutes: cfg.risk_watch_timeout_minutes ?? 4320,
            risk_migration_confirmed: cfg.risk_migration_confirmed ?? false,
            risk_addon_budget_pct:
              cfg.risk_addon_budget_pct != null && cfg.risk_addon_budget_pct > 0
                ? cfg.risk_addon_budget_pct * 100
                : 15,
            // v4.1 重入加严
            risk_reentry_min_recovery_atr:
              cfg.risk_reentry_min_recovery_atr ?? 0.5,
            risk_reentry_cooldown_escalation:
              cfg.risk_reentry_cooldown_escalation ?? 3,
            risk_reentry_recovery_escalation:
              cfg.risk_reentry_recovery_escalation ?? 1.5,
            // v5 可保护性状态机 / 噪音档重入
            risk_unprotectable_action:
              cfg.risk_unprotectable_action === 'follow' ? 'follow' : 'close',
            risk_reentry_noise_override:
              cfg.risk_reentry_noise_override ?? false,
          }))
        }
      } catch (error) {
        // 没有跟单配置，保持当前状态
        setLoadedLegacyReentry(false)
        console.log('No copy trade config found')
      }
    }
    if (isOpen && isEditMode) {
      fetchCopyTradeConfig()
    }
  }, [isOpen, isEditMode, traderData?.trader_id])

  // 新建交易员始终读取后端统一默认值；前端常量只负责接口不可达时的安全占位。
  useEffect(() => {
    if (!isOpen || isEditMode) return
    let cancelled = false
    void httpClient
      .get<{
        defaults_version: number
        defaults: CopyTradeConfig
      }>('/api/copytrade/risk/defaults')
      .then((result) => {
        const cfg = result.data?.defaults
        if (!result.success || !cfg || cancelled) return
        setFormData((prev) => ({
          ...prev,
          risk_stop_loss_enabled: cfg.risk_stop_loss_enabled ?? true,
          risk_account_pct: (cfg.risk_account_pct ?? 0.02) * 100,
          risk_cycle_loss_budget_pct:
            (cfg.risk_cycle_loss_budget_pct ?? 0.05) * 100,
          risk_portfolio_loss_budget_pct:
            (cfg.risk_portfolio_loss_budget_pct ?? 0.08) * 100,
          risk_round_trip_fee_bps: cfg.risk_round_trip_fee_bps ?? 12,
          risk_atr_multiplier: cfg.risk_atr_multiplier ?? 2,
          risk_atr_timeframe: cfg.risk_atr_timeframe ?? '1h',
          risk_reentry_enabled: cfg.risk_reentry_enabled ?? true,
          risk_reentry_ratio: (cfg.risk_reentry_ratio ?? 0.5) * 100,
          risk_reentry_decision_mode:
            cfg.risk_reentry_decision_mode ?? 'ai_guarded',
          risk_ai_confidence_threshold:
            (cfg.risk_ai_confidence_threshold ?? 0.8) * 100,
          risk_ai_min_review_seconds: cfg.risk_ai_min_review_seconds ?? 300,
          risk_ai_daily_call_limit: cfg.risk_ai_daily_call_limit ?? 12,
          risk_ai_lifecycle_call_limit: cfg.risk_ai_lifecycle_call_limit ?? 30,
          risk_notification_level: cfg.risk_notification_level ?? 'important',
          risk_manual_reentry_enabled: false,
          risk_policy_version: cfg.risk_policy_version ?? 4,
          risk_atr_period: cfg.risk_atr_period ?? 14,
          risk_trigger_price_type: cfg.risk_trigger_price_type ?? 'mark',
          risk_slippage_buffer_bps: cfg.risk_slippage_buffer_bps ?? 10,
          risk_max_reentries: cfg.risk_max_reentries ?? 2,
          risk_reentry_cooldown_seconds:
            cfg.risk_reentry_cooldown_seconds ?? 300,
          risk_watch_timeout_minutes: cfg.risk_watch_timeout_minutes ?? 4320,
          risk_addon_budget_pct: (cfg.risk_addon_budget_pct ?? 0.15) * 100,
          risk_unprotectable_action:
            cfg.risk_unprotectable_action === 'follow' ? 'follow' : 'close',
        }))
      })
    return () => {
      cancelled = true
    }
  }, [isOpen, isEditMode])

  useEffect(() => {
    if (traderData) {
      setFormData((prev) => ({
        ...prev,
        ...traderData,
        strategy_id: traderData.strategy_id || '',
        // Keep decision_mode from traderData if exists, otherwise keep prev value
        decision_mode:
          traderData.decision_mode || prev.decision_mode || 'copy_trade',
      }))
    } else if (!isEditMode) {
      setLoadedLegacyReentry(false)
      setFormData({
        trader_name: '',
        ai_model: availableModels[0]?.id || '',
        exchange_id: availableExchanges[0]?.id || '',
        strategy_id: '',
        is_cross_margin: true,
        show_in_competition: true,
        scan_interval_minutes: 3,
        decision_mode: 'copy_trade', // 默认跟单模式
        copy_provider_type: 'hyperliquid',
        copy_leader_id: '',
        copy_ratio: 1.0,
        copy_sync_leverage: true,
        copy_sync_margin_mode: true, // 默认同步保证金模式
        copy_binance_p20t: '',
        copy_binance_csrf_token: '',
        // 风控默认值（与 useState 初始值保持一致）
        risk_stop_loss_enabled: true,
        risk_account_pct: 2,
        risk_cycle_loss_budget_pct: 5,
        risk_portfolio_loss_budget_pct: 8,
        risk_round_trip_fee_bps: 12,
        risk_atr_multiplier: 2.0,
        risk_atr_timeframe: '1h',
        risk_leverage_fallback: false,
        risk_leverage_max_loss: 20,
        risk_reentry_enabled: true,
        risk_reentry_ratio: 50,
        risk_reentry_decision_mode: 'ai_guarded',
        risk_ai_confidence_threshold: 80,
        risk_ai_min_review_seconds: 300,
        risk_ai_daily_call_limit: 12,
        risk_ai_lifecycle_call_limit: 30,
        risk_notification_level: 'important',
        risk_manual_reentry_enabled: false,
        risk_policy_version: 4,
        risk_stop_mode: 'volatility_priority',
        risk_atr_period: 14,
        risk_atr_cache_max_age_minutes: 120,
        risk_atr_fallback_pct: 2,
        risk_trigger_price_type: 'mark',
        risk_slippage_buffer_bps: 10,
        risk_liquidation_buffer_atr: 0.5,
        risk_max_reentries: 2,
        risk_reentry_band_atr: 0.5,
        risk_reentry_cooldown_seconds: 300,
        risk_reentry_max_chase_atr: 0.5,
        risk_reentry_max_atr_expansion: 2,
        risk_watch_timeout_minutes: 4320,
        risk_migration_confirmed: true,
        risk_addon_budget_pct: 15,
        risk_reentry_min_recovery_atr: 0.5,
        risk_reentry_cooldown_escalation: 3,
        risk_reentry_recovery_escalation: 1.5,
        risk_unprotectable_action: 'close',
        risk_reentry_noise_override: false,
      })
    }
  }, [traderData, isEditMode, availableModels, availableExchanges])

  if (!isOpen) return null

  const handleInputChange = (field: keyof FormState, value: any) => {
    setFormData((prev) => ({ ...prev, [field]: value }))
  }

  const handleFetchCurrentBalance = async () => {
    if (!isEditMode || !traderData?.trader_id) {
      setBalanceFetchError('只有在编辑模式下才能获取当前余额')
      return
    }

    setIsFetchingBalance(true)
    setBalanceFetchError('')

    try {
      const result = await httpClient.get<{
        total_equity?: number
        balance?: number
      }>(`/api/account?trader_id=${traderData.trader_id}`)

      if (result.success && result.data) {
        const currentBalance =
          result.data.total_equity || result.data.balance || 0
        setFormData((prev) => ({ ...prev, initial_balance: currentBalance }))
        toast.success('已获取当前余额')
      } else {
        throw new Error(result.message || '获取余额失败')
      }
    } catch (error) {
      console.error('获取余额失败:', error)
      setBalanceFetchError('获取余额失败，请检查网络连接')
    } finally {
      setIsFetchingBalance(false)
    }
  }

  const handleSave = async () => {
    if (!onSave) return

    // v7：单次/周期/组合预算逐级放宽；高风险值必须显式确认。
    // Copy Guard 风控对 OKX 与 Binance 领航员数据源生效
    const isCopyGuardEnabled =
      formData.decision_mode === 'copy_trade' &&
      copyGuardCapableProvider(formData.copy_provider_type) &&
      formData.risk_policy_version >= 4
    let highRiskConfirmed = false
    let extremeRiskConfirmValue: number | undefined
    if (
      isCopyGuardEnabled &&
      !(
        formData.risk_account_pct >= 0.1 &&
        formData.risk_account_pct <= formData.risk_cycle_loss_budget_pct &&
        formData.risk_cycle_loss_budget_pct <=
          formData.risk_portfolio_loss_budget_pct &&
        formData.risk_portfolio_loss_budget_pct <= 20
      )
    ) {
      window.alert(
        '风险预算必须满足：0.1% ≤ 单次风险 ≤ 周期风险 ≤ 组合风险 ≤ 20%'
      )
      return
    }
    if (isCopyGuardEnabled && formData.risk_account_pct > 8) {
      const typed = window.prompt(
        `单次风险 ${formData.risk_account_pct.toFixed(2)}% 属于极高风险。请输入该百分比数值确认：`
      )
      if (typed === null || Number(typed) !== formData.risk_account_pct) {
        return
      }
      extremeRiskConfirmValue = Number(typed)
    }
    if (
      isCopyGuardEnabled &&
      formData.risk_account_pct > 4 &&
      formData.risk_account_pct <= 8 &&
      !window.confirm(
        `单次尝试风险为 ${formData.risk_account_pct.toFixed(2)}%，高于推荐上限。确认仍要保存吗？`
      )
    ) {
      return
    }
    if (isCopyGuardEnabled && formData.risk_account_pct > 4) {
      highRiskConfirmed = true
    }

    // 跟单模式领航员 ID 必填：旧逻辑在 leader_id 为空时静默不带 copy_config，
    // 交易员被保存为"跟单模式但无跟单配置"的半成品状态
    if (
      formData.decision_mode === 'copy_trade' &&
      !formData.copy_leader_id.trim()
    ) {
      window.alert('跟单模式必须填写领航员 ID / 地址')
      return
    }

    setIsSaving(true)
    try {
      // Debug: log decision_mode before save
      console.log(
        '🔧 [DEBUG] Saving trader with decision_mode:',
        formData.decision_mode
      )

      const saveData: CreateTraderRequest = {
        name: formData.trader_name,
        ai_model_id: formData.ai_model,
        exchange_id: formData.exchange_id,
        strategy_id: formData.strategy_id,
        is_cross_margin: formData.is_cross_margin,
        show_in_competition: formData.show_in_competition,
        scan_interval_minutes: formData.scan_interval_minutes,
        decision_mode: formData.decision_mode || 'copy_trade', // 默认跟单模式
      }

      // 只在编辑模式时包含initial_balance
      if (isEditMode && formData.initial_balance !== undefined) {
        saveData.initial_balance = formData.initial_balance
      }

      // 如果是跟单模式，包含跟单配置
      if (formData.decision_mode === 'copy_trade' && formData.copy_leader_id) {
        saveData.copy_config = {
          provider_type: formData.copy_provider_type,
          leader_id: formData.copy_leader_id,
          copy_ratio: formData.copy_ratio,
          sync_leverage: formData.copy_sync_leverage,
          sync_margin_mode: formData.copy_sync_margin_mode, // 同步保证金模式（仅 OKX 生效）
        }
        // 账户保护 v5 风控（Copy Guard）支持 OKX 与 Binance 领航员数据源：
        // 不支持的数据源（Hyperliquid）不携带任何 risk_* 字段（含
        // risk_policy_version），否则后端 "v4 only for OKX/Binance" 校验会拒绝保存。
        if (copyGuardCapableProvider(formData.copy_provider_type)) {
          Object.assign(saveData.copy_config, {
            // 前端展示百分比 → 后端存比例，× 0.01 转换
            risk_stop_loss_enabled: formData.risk_stop_loss_enabled,
            risk_account_pct: formData.risk_account_pct / 100,
            risk_cycle_loss_budget_pct:
              formData.risk_cycle_loss_budget_pct / 100,
            risk_portfolio_loss_budget_pct:
              formData.risk_portfolio_loss_budget_pct / 100,
            risk_round_trip_fee_bps: formData.risk_round_trip_fee_bps,
            risk_atr_multiplier: formData.risk_atr_multiplier,
            risk_atr_timeframe: formData.risk_atr_timeframe,
            risk_leverage_fallback: formData.risk_leverage_fallback,
            risk_leverage_max_loss: formData.risk_leverage_max_loss / 100,
            risk_reentry_enabled: formData.risk_reentry_enabled,
            risk_reentry_ratio: formData.risk_reentry_ratio / 100,
            risk_reentry_decision_mode: formData.risk_reentry_decision_mode,
            risk_ai_confidence_threshold:
              formData.risk_ai_confidence_threshold / 100,
            risk_ai_min_review_seconds: formData.risk_ai_min_review_seconds,
            risk_ai_daily_call_limit: formData.risk_ai_daily_call_limit,
            risk_ai_lifecycle_call_limit: formData.risk_ai_lifecycle_call_limit,
            risk_notification_level: formData.risk_notification_level,
            risk_manual_reentry_enabled: false,
            risk_policy_version: formData.risk_policy_version,
            risk_stop_mode: formData.risk_stop_mode,
            risk_atr_period: formData.risk_atr_period,
            risk_atr_cache_max_age_minutes:
              formData.risk_atr_cache_max_age_minutes,
            risk_atr_fallback_pct: formData.risk_atr_fallback_pct / 100,
            risk_trigger_price_type: formData.risk_trigger_price_type,
            risk_slippage_buffer_bps: formData.risk_slippage_buffer_bps,
            risk_liquidation_buffer_atr: formData.risk_liquidation_buffer_atr,
            risk_max_reentries: formData.risk_max_reentries,
            risk_reentry_band_atr: formData.risk_reentry_band_atr,
            risk_reentry_cooldown_seconds:
              formData.risk_reentry_cooldown_seconds,
            risk_reentry_max_chase_atr: formData.risk_reentry_max_chase_atr,
            risk_reentry_max_atr_expansion:
              formData.risk_reentry_max_atr_expansion,
            risk_watch_timeout_minutes: formData.risk_watch_timeout_minutes,
            risk_migration_confirmed: formData.risk_migration_confirmed,
            risk_addon_budget_pct: formData.risk_addon_budget_pct / 100,
            risk_high_risk_confirmed: highRiskConfirmed,
            risk_extreme_risk_confirm_value: extremeRiskConfirmValue,
            // v4.1 重入加严
            risk_reentry_min_recovery_atr:
              formData.risk_reentry_min_recovery_atr,
            risk_reentry_cooldown_escalation:
              formData.risk_reentry_cooldown_escalation,
            risk_reentry_recovery_escalation:
              formData.risk_reentry_recovery_escalation,
            // v5 可保护性状态机 / 噪音档重入
            risk_unprotectable_action: formData.risk_unprotectable_action,
            risk_reentry_noise_override: formData.risk_reentry_noise_override,
          })
        }
        // Binance 数据源额外携带 Web 私有接口凭证
        if (formData.copy_provider_type === 'binance') {
          saveData.copy_config.binance_p20t = formData.copy_binance_p20t.trim()
          saveData.copy_config.binance_csrf_token =
            formData.copy_binance_csrf_token.trim()
        }
      }

      await toast.promise(onSave(saveData), {
        loading: '正在保存…',
        success: '保存成功',
        error: '保存失败',
      })

      onClose()
    } catch (error) {
      console.error('保存失败:', error)
    } finally {
      setIsSaving(false)
    }
  }

  const selectedStrategy = strategies.find((s) => s.id === formData.strategy_id)

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black bg-opacity-50 backdrop-blur-sm p-4 overflow-y-auto">
      <div
        className="bg-[#1E2329] border border-[#2B3139] rounded-xl shadow-2xl max-w-2xl w-full my-8"
        style={{ maxHeight: 'calc(100vh - 4rem)' }}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between p-6 border-b border-[#2B3139] bg-gradient-to-r from-[#1E2329] to-[#252B35] sticky top-0 z-10 rounded-t-xl">
          <div className="flex items-center gap-3">
            <div className="w-10 h-10 rounded-lg bg-gradient-to-br from-[#F0B90B] to-[#E1A706] flex items-center justify-center text-black">
              {isEditMode ? (
                <Pencil className="w-5 h-5" />
              ) : (
                <Plus className="w-5 h-5" />
              )}
            </div>
            <div>
              <h2 className="text-xl font-bold text-[#EAECEF]">
                {isEditMode ? '修改交易员' : '创建交易员'}
              </h2>
              <p className="text-sm text-[#848E9C] mt-1">
                {isEditMode ? '修改交易员配置' : '选择策略并配置基础参数'}
              </p>
            </div>
          </div>
          <button
            onClick={onClose}
            className="w-8 h-8 rounded-lg text-[#848E9C] hover:text-[#EAECEF] hover:bg-[#2B3139] transition-colors flex items-center justify-center"
          >
            <IconX className="w-4 h-4" />
          </button>
        </div>

        {/* Content */}
        <div
          className="p-6 space-y-6 overflow-y-auto"
          style={{ maxHeight: 'calc(100vh - 16rem)' }}
        >
          {/* Basic Info */}
          <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-5">
            <h3 className="text-lg font-semibold text-[#EAECEF] mb-5 flex items-center gap-2">
              <span className="text-[#F0B90B]">1</span> 基础配置
            </h3>
            <div className="space-y-4">
              <div>
                <label className="text-sm text-[#EAECEF] block mb-2">
                  交易员名称 <span className="text-red-500">*</span>
                </label>
                <input
                  type="text"
                  value={formData.trader_name}
                  onChange={(e) =>
                    handleInputChange('trader_name', e.target.value)
                  }
                  className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none"
                  placeholder="请输入交易员名称"
                />
              </div>
              <div className="grid grid-cols-2 gap-4">
                <div>
                  <label className="text-sm text-[#EAECEF] block mb-2">
                    AI模型 <span className="text-red-500">*</span>
                  </label>
                  <select
                    value={formData.ai_model}
                    onChange={(e) =>
                      handleInputChange('ai_model', e.target.value)
                    }
                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none"
                  >
                    {availableModels.map((model) => (
                      <option key={model.id} value={model.id}>
                        {getShortName(model.name || model.id).toUpperCase()}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label className="text-sm text-[#EAECEF] block mb-2">
                    交易所 <span className="text-red-500">*</span>
                  </label>
                  <select
                    value={formData.exchange_id}
                    onChange={(e) =>
                      handleInputChange('exchange_id', e.target.value)
                    }
                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none"
                  >
                    {availableExchanges.map((exchange) => (
                      <option key={exchange.id} value={exchange.id}>
                        {getShortName(
                          exchange.name || exchange.exchange_type || exchange.id
                        ).toUpperCase()}
                        {exchange.account_name
                          ? ` - ${exchange.account_name}`
                          : ''}
                      </option>
                    ))}
                  </select>
                  {/* Exchange Registration Link */}
                  {formData.exchange_id &&
                    (() => {
                      // Find the selected exchange to get its type
                      const selectedExchange = availableExchanges.find(
                        (e) => e.id === formData.exchange_id
                      )
                      const exchangeType =
                        selectedExchange?.exchange_type?.toLowerCase() || ''
                      const regLink = EXCHANGE_REGISTRATION_LINKS[exchangeType]
                      if (!regLink) return null
                      return (
                        <a
                          href={regLink.url}
                          target="_blank"
                          rel="noopener noreferrer"
                          className="mt-2 inline-flex items-center gap-1.5 text-xs text-[#848E9C] hover:text-[#F0B90B] transition-colors"
                        >
                          <UserPlus className="w-3.5 h-3.5" />
                          <span>还没有交易所账号？点击注册</span>
                          {regLink.hasReferral && (
                            <span className="px-1.5 py-0.5 bg-[#F0B90B]/10 text-[#F0B90B] rounded text-[10px]">
                              折扣优惠
                            </span>
                          )}
                          <ExternalLink className="w-3 h-3" />
                        </a>
                      )
                    })()}
                </div>
              </div>
            </div>
          </div>

          {/* Strategy Selection (required for both AI and copy_trade mode) */}
          <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-5">
            <h3 className="text-lg font-semibold text-[#EAECEF] mb-5 flex items-center gap-2">
              <span className="text-[#F0B90B]">2</span> 选择交易策略
              <Sparkles className="w-4 h-4 text-[#F0B90B]" />
            </h3>
            <div className="space-y-4">
              <div>
                <label className="text-sm text-[#EAECEF] block mb-2">
                  使用策略 <span className="text-red-500">*</span>
                </label>
                <select
                  value={formData.strategy_id}
                  onChange={(e) =>
                    handleInputChange('strategy_id', e.target.value)
                  }
                  className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none"
                >
                  <option value="">-- 请选择策略 --</option>
                  {strategies.map((strategy) => (
                    <option key={strategy.id} value={strategy.id}>
                      {strategy.name}
                      {strategy.is_active ? ' (当前激活)' : ''}
                      {strategy.is_default ? ' [默认]' : ''}
                    </option>
                  ))}
                </select>
                {strategies.length === 0 && (
                  <p className="text-xs text-[#848E9C] mt-2">
                    暂无策略，请先在策略工作室创建策略
                  </p>
                )}
                {formData.decision_mode === 'copy_trade' && (
                  <p className="text-xs text-[#848E9C] mt-2">
                    💡 跟单模式下策略不会执行，但需要配置以便随时切换回 AI 模式
                  </p>
                )}
              </div>

              {/* Strategy Preview */}
              {selectedStrategy && (
                <div className="mt-3 p-4 bg-[#1E2329] border border-[#2B3139] rounded-lg">
                  <div className="flex items-center gap-2 mb-2">
                    <span className="text-[#F0B90B] text-sm font-medium">
                      策略详情
                    </span>
                    {selectedStrategy.is_active && (
                      <span className="px-2 py-0.5 bg-green-500/20 text-green-400 text-xs rounded">
                        激活中
                      </span>
                    )}
                  </div>
                  <p className="text-sm text-[#848E9C] mb-2">
                    {selectedStrategy.description || '无描述'}
                  </p>
                  <div className="grid grid-cols-2 gap-2 text-xs text-[#848E9C]">
                    <div>
                      币种来源:{' '}
                      {selectedStrategy.config.coin_source.source_type ===
                      'static'
                        ? '固定币种'
                        : selectedStrategy.config.coin_source.source_type ===
                            'coinpool'
                          ? 'Coin Pool'
                          : selectedStrategy.config.coin_source.source_type ===
                              'oi_top'
                            ? 'OI Top'
                            : '混合'}
                    </div>
                    <div>
                      保证金上限:{' '}
                      {(
                        (selectedStrategy.config.risk_control
                          ?.max_margin_usage || 0.9) * 100
                      ).toFixed(0)}
                      %
                    </div>
                  </div>
                </div>
              )}
            </div>
          </div>

          {/* Decision Mode Selection */}
          <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-5">
            <h3 className="text-lg font-semibold text-[#EAECEF] mb-5 flex items-center gap-2">
              <span className="text-[#F0B90B]">3</span> 决策模式
            </h3>
            <div className="space-y-4">
              <div className="grid grid-cols-2 gap-4">
                {/* 跟单交易 - 默认选项，放在前面 */}
                <button
                  type="button"
                  onClick={() =>
                    handleInputChange('decision_mode', 'copy_trade')
                  }
                  className={`p-4 rounded-lg border-2 transition-all ${
                    formData.decision_mode === 'copy_trade'
                      ? 'border-[#F0B90B] bg-[#F0B90B]/10'
                      : 'border-[#2B3139] hover:border-[#404750]'
                  }`}
                >
                  <div className="flex items-center gap-3 mb-2">
                    <Users
                      className={`w-6 h-6 ${formData.decision_mode === 'copy_trade' ? 'text-[#F0B90B]' : 'text-[#848E9C]'}`}
                    />
                    <span
                      className={`font-medium ${formData.decision_mode === 'copy_trade' ? 'text-[#EAECEF]' : 'text-[#848E9C]'}`}
                    >
                      跟单交易
                    </span>
                  </div>
                  <p className="text-xs text-[#848E9C] text-left">
                    跟随真人领航员的交易操作，按比例同步开仓/平仓
                  </p>
                </button>
                {/* AI 决策 - 可选 */}
                <button
                  type="button"
                  onClick={() => handleInputChange('decision_mode', 'ai')}
                  className={`p-4 rounded-lg border-2 transition-all ${
                    formData.decision_mode === 'ai'
                      ? 'border-[#F0B90B] bg-[#F0B90B]/10'
                      : 'border-[#2B3139] hover:border-[#404750]'
                  }`}
                >
                  <div className="flex items-center gap-3 mb-2">
                    <Bot
                      className={`w-6 h-6 ${formData.decision_mode === 'ai' ? 'text-[#F0B90B]' : 'text-[#848E9C]'}`}
                    />
                    <span
                      className={`font-medium ${formData.decision_mode === 'ai' ? 'text-[#EAECEF]' : 'text-[#848E9C]'}`}
                    >
                      AI 决策
                    </span>
                  </div>
                  <p className="text-xs text-[#848E9C] text-left">
                    由 AI 模型根据策略自主分析市场并做出交易决策
                  </p>
                </button>
              </div>

              {/* Copy Trade Configuration */}
              {formData.decision_mode === 'copy_trade' && (
                <div className="mt-4 p-4 bg-[#1E2329] border border-[#2B3139] rounded-lg space-y-4">
                  <div className="flex items-center gap-2 mb-2">
                    <Users className="w-4 h-4 text-[#F0B90B]" />
                    <span className="text-[#F0B90B] text-sm font-medium">
                      跟单配置
                    </span>
                  </div>

                  {/* Provider Type */}
                  <div>
                    <label className="text-sm text-[#EAECEF] block mb-2">
                      数据源 <span className="text-red-500">*</span>
                    </label>
                    <div className="flex gap-2">
                      <button
                        type="button"
                        onClick={() =>
                          handleInputChange('copy_provider_type', 'hyperliquid')
                        }
                        className={`flex-1 px-3 py-2 rounded text-sm ${
                          formData.copy_provider_type === 'hyperliquid'
                            ? 'bg-[#F0B90B] text-black'
                            : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                        }`}
                      >
                        Hyperliquid
                      </button>
                      <button
                        type="button"
                        onClick={() =>
                          handleInputChange('copy_provider_type', 'okx')
                        }
                        className={`flex-1 px-3 py-2 rounded text-sm ${
                          formData.copy_provider_type === 'okx'
                            ? 'bg-[#F0B90B] text-black'
                            : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                        }`}
                      >
                        OKX
                      </button>
                      <button
                        type="button"
                        onClick={() =>
                          handleInputChange('copy_provider_type', 'binance')
                        }
                        className={`flex-1 px-3 py-2 rounded text-sm ${
                          formData.copy_provider_type === 'binance'
                            ? 'bg-[#F0B90B] text-black'
                            : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                        }`}
                      >
                        Binance
                      </button>
                    </div>
                  </div>

                  {/* Leader ID */}
                  <div>
                    <label className="text-sm text-[#EAECEF] block mb-2">
                      {formData.copy_provider_type === 'binance'
                        ? '领航员 PortfolioId'
                        : '领航员地址'}
                      <span className="text-red-500"> *</span>
                    </label>
                    <input
                      type="text"
                      value={formData.copy_leader_id}
                      onChange={(e) =>
                        handleInputChange('copy_leader_id', e.target.value)
                      }
                      className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-sm"
                      placeholder={
                        formData.copy_provider_type === 'hyperliquid'
                          ? '0x...'
                          : formData.copy_provider_type === 'binance'
                            ? '领航员主页 portfolioId (如 5008318166959365632)'
                            : 'UniqueName (如 F2BCA22ABBB69F57)'
                      }
                    />
                    <p className="text-xs text-[#848E9C] mt-1">
                      {formData.copy_provider_type === 'hyperliquid'
                        ? 'Hyperliquid 钱包地址 (0x开头)'
                        : formData.copy_provider_type === 'binance'
                          ? '填领航员主页 URL 末尾的 portfolioId（不是跟单订单里的关系 ID）。前提：你的账户已经在 Binance 上跟单了该领航员'
                          : 'OKX 交易员 uniqueName (交易员页面 URL 中的参数)'}
                    </p>
                  </div>

                  {/* Binance 全局共享凭证状态卡（v2 凭证全局化） */}
                  {/* 旧字段 copy_binance_p20t/copy_binance_csrf_token 仍保留在 formData 中
                      作为向后兼容的降级凭证；新用户使用全局凭证即可，无需在交易员级别配置 */}
                  {formData.copy_provider_type === 'binance' && (
                    <div className="mt-4 p-4 bg-[#0B0E11] border border-[#F0B90B33] rounded-lg space-y-3">
                      <div className="flex items-center justify-between">
                        <div className="flex items-center gap-2">
                          <KeyRound className="w-4 h-4 text-[#F0B90B]" />
                          <span className="text-[#F0B90B] text-sm font-medium">
                            Binance 共享凭证
                          </span>
                          <span className="px-2 py-0.5 text-[10px] bg-[#F0B90B22] text-[#F0B90B] rounded">
                            全局共享
                          </span>
                        </div>
                        <button
                          type="button"
                          onClick={() => setShowBinanceCredsModal(true)}
                          className="text-xs text-[#F0B90B] hover:underline"
                        >
                          {binanceGlobalCreds ? '管理凭证 →' : '立即配置 →'}
                        </button>
                      </div>

                      {binanceGlobalCreds ? (
                        <div className="flex items-center gap-3 text-xs">
                          {binanceGlobalCreds.last_status === 'valid' ? (
                            <span className="flex items-center gap-1.5 text-[#0ECB81]">
                              <CheckCircle2 className="w-3.5 h-3.5" />
                              凭证有效
                            </span>
                          ) : binanceGlobalCreds.last_status === 'expired' ? (
                            <span className="flex items-center gap-1.5 text-[#F6465D]">
                              <AlertCircle className="w-3.5 h-3.5" />
                              凭证已过期
                            </span>
                          ) : (
                            <span className="flex items-center gap-1.5 text-[#848E9C]">
                              <AlertCircle className="w-3.5 h-3.5" />
                              未校验
                            </span>
                          )}
                          {binanceGlobalCreds.binance_user_id && (
                            <span className="text-[#848E9C]">
                              账号 ID:{' '}
                              <span className="font-mono text-[#EAECEF]">
                                {binanceGlobalCreds.binance_user_id}
                              </span>
                            </span>
                          )}
                        </div>
                      ) : (
                        <div className="text-xs text-[#F6465D] flex items-center gap-1.5">
                          <AlertCircle className="w-3.5 h-3.5" />
                          全局凭证尚未配置，本交易员无法读取领航员数据
                        </div>
                      )}

                      <p className="text-[11px] text-[#848E9C] leading-relaxed">
                        所有 Binance 跟单交易员共用同一份凭证（p20t /
                        csrftoken）。 在此处或顶部"Binance
                        凭证"按钮配置一次后，所有 Binance
                        交易员立即生效，无需重启。
                      </p>
                    </div>
                  )}

                  {/* Copy Ratio */}
                  <div>
                    <label className="text-sm text-[#EAECEF] block mb-2">
                      跟单系数
                    </label>
                    <div className="flex items-center gap-3">
                      <input
                        type="range"
                        min="0.1"
                        max="3"
                        step="0.1"
                        value={formData.copy_ratio}
                        onChange={(e) =>
                          handleInputChange(
                            'copy_ratio',
                            parseFloat(e.target.value)
                          )
                        }
                        className="flex-1 accent-[#F0B90B]"
                      />
                      <div className="w-20 text-center">
                        <span className="text-[#F0B90B] font-bold text-lg">
                          {(formData.copy_ratio * 100).toFixed(0)}%
                        </span>
                      </div>
                    </div>
                    <p className="text-xs text-[#848E9C] mt-1">
                      100% = 等比例跟单 | 200% = 双倍仓位 | 50% = 半仓跟单
                    </p>
                  </div>

                  {/* Sync Leverage */}
                  <div className="flex items-center justify-between">
                    <div>
                      <label className="text-sm text-[#EAECEF]">同步杠杆</label>
                      <p className="text-xs text-[#848E9C]">
                        使用与领航员相同的杠杆倍数；关闭后固定使用 10 倍杠杆
                      </p>
                    </div>
                    <button
                      type="button"
                      onClick={() =>
                        handleInputChange(
                          'copy_sync_leverage',
                          !formData.copy_sync_leverage
                        )
                      }
                      className={`w-12 h-6 rounded-full transition-colors ${
                        formData.copy_sync_leverage
                          ? 'bg-[#F0B90B]'
                          : 'bg-[#2B3139]'
                      }`}
                    >
                      <div
                        className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${
                          formData.copy_sync_leverage
                            ? 'translate-x-6'
                            : 'translate-x-0.5'
                        }`}
                      />
                    </button>
                  </div>

                  {/* Sync Margin Mode（仅 OKX：HL/Binance 数据源无仓位级全仓/逐仓语义） */}
                  {formData.copy_provider_type === 'okx' && (
                    <div className="flex items-center justify-between">
                      <div>
                        <label className="text-sm text-[#EAECEF]">
                          同步保证金模式
                        </label>
                        <p className="text-xs text-[#848E9C]">
                          跟随领航员的全仓/逐仓模式；关闭后新开仓使用交易员自身的保证金模式配置
                        </p>
                      </div>
                      <button
                        type="button"
                        onClick={() =>
                          handleInputChange(
                            'copy_sync_margin_mode',
                            !formData.copy_sync_margin_mode
                          )
                        }
                        className={`w-12 h-6 rounded-full transition-colors ${
                          formData.copy_sync_margin_mode
                            ? 'bg-[#F0B90B]'
                            : 'bg-[#2B3139]'
                        }`}
                      >
                        <div
                          className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${
                            formData.copy_sync_margin_mode
                              ? 'translate-x-6'
                              : 'translate-x-0.5'
                          }`}
                        />
                      </button>
                    </div>
                  )}

                  {/* Info Box */}
                  <div className="p-3 bg-[#0B0E11] border border-[#2B3139] rounded flex items-start gap-2">
                    <svg
                      xmlns="http://www.w3.org/2000/svg"
                      className="w-4 h-4 text-[#F0B90B] mt-0.5 flex-shrink-0"
                      viewBox="0 0 24 24"
                      fill="none"
                      stroke="currentColor"
                      strokeWidth="2"
                    >
                      <circle cx="12" cy="12" r="10" />
                      <line x1="12" x2="12" y1="8" y2="12" />
                      <line x1="12" x2="12.01" y1="16" y2="16" />
                    </svg>
                    <span className="text-xs text-[#848E9C]">
                      跟单模式将监听领航员的交易操作，只跟随新开仓（不跟历史仓位）。
                      跟单金额 = 跟单系数 × (领航员交易金额÷领航员账户余额) ×
                      你的账户余额
                    </span>
                  </div>

                  {/* ============================================================
                      账户保护 / 止损兜底（Copy Guard v5）—— OKX / Binance 数据源生效
                      显示规则：copy_provider_type ∈ {okx, binance} 时显示
                      所有数值用百分比展示，提交时由 handleSave 转为比例
                      保护单挂在跟随者执行交易所（须支持交易所托管条件单）
                      ============================================================ */}
                  {copyGuardCapableProvider(formData.copy_provider_type) && (
                    <div className="mt-4 p-4 bg-[#0B0E11] border border-[#F0B90B33] rounded-lg space-y-4">
                      <div className="flex items-center gap-2">
                        <svg
                          xmlns="http://www.w3.org/2000/svg"
                          className="w-4 h-4 text-[#F0B90B]"
                          viewBox="0 0 24 24"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth="2"
                        >
                          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
                        </svg>
                        <span className="text-[#F0B90B] text-sm font-medium">
                          账户保护（止损兜底）
                        </span>
                        <span className="px-2 py-0.5 text-[10px] bg-[#F0B90B22] text-[#F0B90B] rounded">
                          OKX / 币安
                        </span>
                      </div>

                      {/* 主开关：账户保护止损 */}
                      <div className="flex items-center justify-between">
                        <div>
                          <label className="text-sm text-[#EAECEF]">
                            启用账户保护止损
                          </label>
                          <p className="text-xs text-[#848E9C]">
                            开仓时自动挂交易所托管的硬止损单，防止极端反向走势打穿账户
                          </p>
                        </div>
                        <button
                          type="button"
                          onClick={() =>
                            handleInputChange(
                              'risk_stop_loss_enabled',
                              !formData.risk_stop_loss_enabled
                            )
                          }
                          className={`w-12 h-6 rounded-full transition-colors ${formData.risk_stop_loss_enabled ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                        >
                          <div
                            className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_stop_loss_enabled ? 'translate-x-6' : 'translate-x-0.5'}`}
                          />
                        </button>
                      </div>

                      {/* 风控参数（开关打开时显示） */}
                      {formData.risk_stop_loss_enabled && (
                        <>
                          {formData.risk_policy_version >= 4 && (
                            <div className="grid grid-cols-2 gap-3">
                              <div className="col-span-2 flex items-center justify-between rounded border border-[#F0B90B44] bg-[#F0B90B0D] p-3 text-xs">
                                <span className="text-[#848E9C]">
                                  推荐（v7）：单次/周期/组合风险 2% / 5% /
                                  8%，ATR14 / 1小时 / 2.0倍，AI
                                  持续观察，最多重入 2 次，不可保护时立即离场
                                </span>
                                <button
                                  type="button"
                                  className="text-[#F0B90B] underline"
                                  onClick={() =>
                                    setFormData((v) => ({
                                      ...v,
                                      risk_stop_mode: 'volatility_priority',
                                      risk_trigger_price_type: 'mark',
                                      risk_atr_period: 14,
                                      risk_atr_cache_max_age_minutes: 120,
                                      risk_atr_multiplier: 2.0,
                                      risk_atr_timeframe: '1h',
                                      risk_atr_fallback_pct: 2,
                                      risk_account_pct: 2,
                                      risk_cycle_loss_budget_pct: 5,
                                      risk_portfolio_loss_budget_pct: 8,
                                      risk_round_trip_fee_bps: 12,
                                      risk_leverage_fallback: false,
                                      risk_leverage_max_loss: 20,
                                      risk_slippage_buffer_bps: 10,
                                      risk_liquidation_buffer_atr: 0.5,
                                      risk_reentry_enabled: true,
                                      risk_reentry_ratio: 50,
                                      risk_reentry_decision_mode: 'ai_guarded',
                                      risk_ai_confidence_threshold: 80,
                                      risk_ai_min_review_seconds: 300,
                                      risk_ai_daily_call_limit: 12,
                                      risk_ai_lifecycle_call_limit: 30,
                                      risk_notification_level: 'important',
                                      risk_manual_reentry_enabled: false,
                                      risk_max_reentries: 2,
                                      risk_reentry_band_atr: 0.5,
                                      risk_reentry_cooldown_seconds: 300,
                                      risk_reentry_max_chase_atr: 0.5,
                                      risk_reentry_max_atr_expansion: 2,
                                      risk_watch_timeout_minutes: 4320,
                                      risk_reentry_min_recovery_atr: 0.5,
                                      risk_reentry_cooldown_escalation: 3,
                                      risk_reentry_recovery_escalation: 1.5,
                                      risk_unprotectable_action: 'close',
                                      risk_reentry_noise_override: false,
                                    }))
                                  }
                                >
                                  应用推荐值
                                </button>
                              </div>
                              {/* v5：止损模式（risk_stop_mode）不再影响计算——
                                  账户线在任何模式下都是硬 cap，字段仅作兼容保留，
                                  故不再提供选择器（避免 UI 暗示不存在的行为差异） */}
                              <label className="text-xs text-[#848E9C]">
                                触发价格
                                <select
                                  value={formData.risk_trigger_price_type}
                                  onChange={(e) =>
                                    handleInputChange(
                                      'risk_trigger_price_type',
                                      e.target.value
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                >
                                  <option value="mark">标记价格</option>
                                  <option value="last">最新价格</option>
                                  <option value="index">指数价格</option>
                                </select>
                              </label>
                              <label className="text-xs text-[#848E9C]">
                                ATR 周期
                                <input
                                  type="number"
                                  min="5"
                                  max="100"
                                  value={formData.risk_atr_period}
                                  onChange={(e) =>
                                    handleInputChange(
                                      'risk_atr_period',
                                      Number(e.target.value)
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                              </label>
                              <label className="text-xs text-[#848E9C]">
                                ATR旧快照有效期（分钟）
                                <input
                                  type="number"
                                  min="1"
                                  max="1440"
                                  value={
                                    formData.risk_atr_cache_max_age_minutes
                                  }
                                  onChange={(e) =>
                                    handleInputChange(
                                      'risk_atr_cache_max_age_minutes',
                                      Number(e.target.value)
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                              </label>
                              <label className="text-xs text-[#848E9C]">
                                ATR失败降级（%）
                                <input
                                  type="number"
                                  min="0.1"
                                  max="20"
                                  step="0.1"
                                  value={formData.risk_atr_fallback_pct}
                                  onChange={(e) =>
                                    handleInputChange(
                                      'risk_atr_fallback_pct',
                                      Number(e.target.value)
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                              </label>
                              <label className="text-xs text-[#848E9C]">
                                滑点缓冲（bps）
                                <input
                                  type="number"
                                  min="0"
                                  max="1000"
                                  value={formData.risk_slippage_buffer_bps}
                                  onChange={(e) =>
                                    handleInputChange(
                                      'risk_slippage_buffer_bps',
                                      Number(e.target.value)
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                              </label>
                              <label className="text-xs text-[#848E9C]">
                                强平缓冲（ATR）
                                <input
                                  type="number"
                                  min="0"
                                  max="5"
                                  step="0.1"
                                  value={formData.risk_liquidation_buffer_atr}
                                  onChange={(e) =>
                                    handleInputChange(
                                      'risk_liquidation_buffer_atr',
                                      Number(e.target.value)
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                              </label>
                              <div className="col-span-2">
                                <label className="text-xs text-[#848E9C] block">
                                  加仓风险预算（% 账户权益）{' '}
                                  <span className="text-[#F0B90B]">
                                    {formData.risk_addon_budget_pct.toFixed(0)}%
                                  </span>
                                </label>
                                <input
                                  type="number"
                                  min="1"
                                  max="100"
                                  step="1"
                                  value={formData.risk_addon_budget_pct}
                                  onChange={(e) => {
                                    const v = Number(e.target.value)
                                    handleInputChange(
                                      'risk_addon_budget_pct',
                                      isFinite(v) && v > 0
                                        ? Math.min(v, 100)
                                        : 15
                                    )
                                  }}
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                                <p className="text-xs text-[#848E9C] mt-1">
                                  超限时自动缩量；缩量后低于交易所最小下单量则拒绝加仓。
                                </p>
                              </div>
                            </div>
                          )}
                          <div className="grid grid-cols-2 gap-3 border-t border-[#2B3139] pt-3">
                            <label className="text-xs text-[#848E9C] col-span-2">
                              重入决策模式
                              <select
                                value={formData.risk_reentry_decision_mode}
                                onChange={(e) => {
                                  const mode = e.target
                                    .value as FormState['risk_reentry_decision_mode']
                                  setFormData((prev) => ({
                                    ...prev,
                                    risk_reentry_decision_mode: mode,
                                    ...(mode === 'ai_guarded'
                                      ? {
                                          risk_unprotectable_action: 'close',
                                          risk_reentry_ratio: Math.min(
                                            prev.risk_reentry_ratio,
                                            50
                                          ),
                                          risk_max_reentries: Math.min(
                                            prev.risk_max_reentries,
                                            2
                                          ),
                                        }
                                      : {}),
                                  }))
                                }}
                                className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                              >
                                <option value="ai_guarded">
                                  AI 判断 + 代码风控（推荐）
                                </option>
                                {loadedLegacyReentry && (
                                  <option value="legacy_rule">
                                    旧规则（已废弃，仅本存量配置可保留）
                                  </option>
                                )}
                                <option value="disabled">不重入</option>
                              </select>
                              {loadedLegacyReentry &&
                                formData.risk_reentry_decision_mode ===
                                  'legacy_rule' && (
                                  <div className="mt-2 rounded border border-[#F0B90B] bg-[#F0B90B]/10 p-2 text-xs text-[#F0B90B]">
                                    此交易员仍在使用已废弃旧规则。AI
                                    不会失败回退旧规则；建议迁移到
                                    ai_guarded，或选择“不重入”。
                                    <button
                                      type="button"
                                      onClick={() =>
                                        setFormData((prev) => ({
                                          ...prev,
                                          risk_reentry_decision_mode:
                                            'ai_guarded',
                                          risk_reentry_ratio: Math.min(
                                            prev.risk_reentry_ratio,
                                            50
                                          ),
                                          risk_max_reentries: Math.min(
                                            prev.risk_max_reentries,
                                            2
                                          ),
                                          risk_unprotectable_action: 'close',
                                        }))
                                      }
                                      className="ml-2 underline"
                                    >
                                      迁移到 AI 模式
                                    </button>
                                  </div>
                                )}
                            </label>
                            {[
                              ['risk_account_pct', '单次尝试风险 %'],
                              [
                                'risk_cycle_loss_budget_pct',
                                '单周期累计风险 %',
                              ],
                              [
                                'risk_portfolio_loss_budget_pct',
                                '全账户组合风险 %',
                              ],
                              ['risk_round_trip_fee_bps', '往返手续费预算 bps'],
                            ].map(([field, label]) => (
                              <label
                                key={field}
                                className="text-xs text-[#848E9C]"
                              >
                                {label}
                                <input
                                  type="number"
                                  min={
                                    field === 'risk_round_trip_fee_bps'
                                      ? 0
                                      : 0.1
                                  }
                                  max={
                                    field === 'risk_round_trip_fee_bps'
                                      ? 100
                                      : 20
                                  }
                                  step="0.1"
                                  value={
                                    formData[field as keyof FormState] as number
                                  }
                                  onChange={(e) =>
                                    handleInputChange(
                                      field as keyof FormState,
                                      Number(e.target.value)
                                    )
                                  }
                                  className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                />
                              </label>
                            ))}
                          </div>
                          {formData.risk_account_pct > 4 && (
                            <div className="p-2 bg-[#F6465D11] border border-[#F6465D44] rounded text-xs text-[#F6465D]">
                              高风险：单次尝试风险超过 4%；超过 8%
                              保存时必须输入数值二次确认。
                            </div>
                          )}

                          {formData.risk_reentry_decision_mode ===
                            'ai_guarded' && (
                            <div className="grid grid-cols-2 gap-3 border border-[#2B3139] rounded p-3">
                              {[
                                [
                                  'risk_ai_confidence_threshold',
                                  'AI 入场置信度 %',
                                  70,
                                  95,
                                ],
                                [
                                  'risk_ai_min_review_seconds',
                                  '最短复查间隔（秒）',
                                  300,
                                  7200,
                                ],
                                [
                                  'risk_ai_daily_call_limit',
                                  '24 小时调用上限',
                                  1,
                                  12,
                                ],
                                [
                                  'risk_ai_lifecycle_call_limit',
                                  '生命周期调用上限',
                                  1,
                                  30,
                                ],
                              ].map(([field, label, min, max]) => (
                                <label
                                  key={String(field)}
                                  className="text-xs text-[#848E9C]"
                                >
                                  {label}
                                  <input
                                    type="number"
                                    min={Number(min)}
                                    max={Number(max)}
                                    value={
                                      formData[
                                        field as keyof FormState
                                      ] as number
                                    }
                                    onChange={(e) =>
                                      handleInputChange(
                                        field as keyof FormState,
                                        Number(e.target.value)
                                      )
                                    }
                                    className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                  />
                                </label>
                              ))}
                              <p className="col-span-2 text-xs text-[#848E9C]">
                                WAIT
                                不发邮件；事件、心跳退避和关注区间会持续触发复查，AI
                                不可绕过仓位、预算和保护预检。
                              </p>
                            </div>
                          )}

                          {/* ATR 波动参数 */}
                          <div className="border-t border-[#2B3139] pt-3">
                            <div className="flex items-center justify-between mb-2">
                              <div>
                                <label className="text-sm text-[#EAECEF]">
                                  ATR 波动参数
                                </label>
                                <p className="text-xs text-[#848E9C]">
                                  止损距离基线 =
                                  倍数×ATR，自动适应不同币种波动率（默认
                                  2.0，抗噪主力线）
                                </p>
                              </div>
                            </div>
                            {formData.risk_policy_version >= 4 && (
                              <div className="grid grid-cols-2 gap-3">
                                <div>
                                  <label className="text-xs text-[#848E9C] block mb-1">
                                    ATR 倍数
                                  </label>
                                  <input
                                    type="number"
                                    min="1.0"
                                    max="3.0"
                                    step="0.1"
                                    value={formData.risk_atr_multiplier}
                                    onChange={(e) =>
                                      handleInputChange(
                                        'risk_atr_multiplier',
                                        parseFloat(e.target.value) || 2.0
                                      )
                                    }
                                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none text-sm"
                                  />
                                </div>
                                <div>
                                  <label className="text-xs text-[#848E9C] block mb-1">
                                    时间周期
                                  </label>
                                  <select
                                    value={formData.risk_atr_timeframe}
                                    onChange={(e) =>
                                      handleInputChange(
                                        'risk_atr_timeframe',
                                        e.target.value
                                      )
                                    }
                                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none text-sm"
                                  >
                                    <option value="15m">15 分钟</option>
                                    <option value="1h">1 小时</option>
                                    <option value="4h">4 小时</option>
                                  </select>
                                </div>
                              </div>
                            )}
                          </div>

                          {/* 杠杆兜底 / 保证金止损上限（v3 与 v4 共用 risk_leverage_max_loss） */}
                          {
                            <div className="border-t border-[#2B3139] pt-3">
                              <div className="flex items-center justify-between mb-2">
                                <div>
                                  <label className="text-sm text-[#EAECEF]">
                                    {formData.risk_policy_version >= 4
                                      ? '仓位保证金止损上限'
                                      : '杠杆兜底封顶'}
                                  </label>
                                  <p className="text-xs text-[#848E9C]">
                                    {formData.risk_policy_version >= 4
                                      ? '可选的更严保证金封顶，默认关闭。高杠杆下会把止损压进噪音区（如 100x×20%≈0.2% 就止损），易被正常波动扫出；仅低杠杆或想要更小单次亏损时开启，开启后与 ATR 距离取更紧者'
                                      : '保证金亏损达此比例时强制平仓（高杠杆兜底）'}
                                  </p>
                                </div>
                                <button
                                  type="button"
                                  onClick={() =>
                                    handleInputChange(
                                      'risk_leverage_fallback',
                                      !formData.risk_leverage_fallback
                                    )
                                  }
                                  className={`w-12 h-6 rounded-full transition-colors ${formData.risk_leverage_fallback ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                                >
                                  <div
                                    className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_leverage_fallback ? 'translate-x-6' : 'translate-x-0.5'}`}
                                  />
                                </button>
                              </div>
                              {formData.risk_leverage_fallback && (
                                <div>
                                  <label className="text-xs text-[#848E9C] block mb-1">
                                    最大保证金亏损{' '}
                                    <span className="text-[#F0B90B]">
                                      {formData.risk_leverage_max_loss.toFixed(
                                        0
                                      )}
                                      %
                                    </span>
                                  </label>
                                  <input
                                    type="range"
                                    min="10"
                                    max="100"
                                    step="5"
                                    value={formData.risk_leverage_max_loss}
                                    onChange={(e) =>
                                      handleInputChange(
                                        'risk_leverage_max_loss',
                                        parseFloat(e.target.value)
                                      )
                                    }
                                    className="w-full accent-[#F0B90B]"
                                  />
                                  {formData.risk_policy_version >= 4 && (
                                    <p className="text-xs text-[#848E9C] mt-1">
                                      默认
                                      20%（硬上限，任何情况不放宽）。示例：20x
                                      杠杆、上限 20% → 价格反向 1% 即止损（OKX
                                      收益率约
                                      -20%）。高杠杆下止损距离小于市场噪音时不再放宽止损，而是禁止该档重入（可在下方覆盖）
                                    </p>
                                  )}
                                </div>
                              )}
                            </div>
                          }

                          {/* v5 不可保护处置 */}
                          {formData.risk_policy_version >= 4 && (
                            <div className="border-t border-[#2B3139] pt-3">
                              <label className="text-sm text-[#EAECEF] block mb-1">
                                无法建立保护单时的处置
                              </label>
                              <select
                                value={formData.risk_unprotectable_action}
                                onChange={(e) =>
                                  handleInputChange(
                                    'risk_unprotectable_action',
                                    e.target.value
                                  )
                                }
                                className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none text-sm"
                              >
                                <option value="close">
                                  立即市价离场（推荐）
                                </option>
                                {formData.risk_reentry_decision_mode ===
                                  'legacy_rule' && (
                                  <option value="follow">
                                    继续裸跟并标红告警（旧规则兼容）
                                  </option>
                                )}
                              </select>
                              <p className="text-xs text-[#848E9C] mt-1">
                                AI 模式强制立即离场；只有存量 legacy_rule
                                才能保留裸跟兼容选项。
                              </p>
                            </div>
                          )}

                          {/* 二次进场（高级） */}
                          <div className="border-t border-[#2B3139] pt-3">
                            <div className="flex items-center justify-between mb-2">
                              <div>
                                <label className="text-sm text-[#EAECEF]">
                                  二次进场（高级）
                                </label>
                                <p className="text-xs text-[#848E9C]">
                                  {formData.risk_reentry_decision_mode ===
                                  'ai_guarded'
                                    ? '完全平仓后持续观察，AI 判断反转，代码负责预算、幂等与保护'
                                    : '存量硬规则恢复带（仅兼容旧配置）'}
                                </p>
                              </div>
                              <button
                                type="button"
                                onClick={() =>
                                  handleInputChange(
                                    'risk_reentry_enabled',
                                    !formData.risk_reentry_enabled
                                  )
                                }
                                className={`w-12 h-6 rounded-full transition-colors ${formData.risk_reentry_enabled ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                              >
                                <div
                                  className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_reentry_enabled ? 'translate-x-6' : 'translate-x-0.5'}`}
                                />
                              </button>
                            </div>
                            {formData.risk_reentry_enabled && (
                              <>
                                <div className="grid grid-cols-2 gap-3">
                                  <div>
                                    <label className="text-xs text-[#848E9C] block mb-1">
                                      重入最大名义上限（× 上次止损尝试）{' '}
                                      <span className="text-[#F0B90B]">
                                        {formData.risk_reentry_ratio.toFixed(0)}
                                        %
                                      </span>
                                    </label>
                                    <input
                                      type="range"
                                      min="10"
                                      max={
                                        formData.risk_reentry_decision_mode ===
                                        'ai_guarded'
                                          ? 50
                                          : 100
                                      }
                                      step="10"
                                      value={formData.risk_reentry_ratio}
                                      onChange={(e) =>
                                        handleInputChange(
                                          'risk_reentry_ratio',
                                          parseFloat(e.target.value)
                                        )
                                      }
                                      className="w-full accent-[#F0B90B]"
                                    />
                                  </div>
                                </div>
                                {formData.risk_reentry_decision_mode ===
                                  'ai_guarded' && (
                                  <div className="mt-3 grid grid-cols-2 gap-3">
                                    <label className="text-xs text-[#848E9C]">
                                      最大重入次数
                                      <input
                                        type="number"
                                        min="0"
                                        max="2"
                                        value={formData.risk_max_reentries}
                                        onChange={(e) =>
                                          handleInputChange(
                                            'risk_max_reentries',
                                            Number(e.target.value)
                                          )
                                        }
                                        className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                      />
                                    </label>
                                    <label className="text-xs text-[#848E9C]">
                                      AI 观察期（分钟）
                                      <input
                                        type="number"
                                        min="5"
                                        max="10080"
                                        value={
                                          formData.risk_watch_timeout_minutes
                                        }
                                        onChange={(e) =>
                                          handleInputChange(
                                            'risk_watch_timeout_minutes',
                                            Number(e.target.value)
                                          )
                                        }
                                        className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                      />
                                    </label>
                                    <label className="col-span-2 text-xs text-[#848E9C]">
                                      邮件级别
                                      <select
                                        value={formData.risk_notification_level}
                                        onChange={(e) =>
                                          handleInputChange(
                                            'risk_notification_level',
                                            e.target.value
                                          )
                                        }
                                        className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                      >
                                        <option value="important">
                                          重要（推荐，WAIT 不发送）
                                        </option>
                                        <option value="critical">
                                          仅安全故障
                                        </option>
                                        <option value="verbose">
                                          详细（包含决策变化）
                                        </option>
                                      </select>
                                    </label>
                                  </div>
                                )}
                                {formData.risk_policy_version >= 4 &&
                                  formData.risk_reentry_decision_mode ===
                                    'legacy_rule' && (
                                    <div className="grid grid-cols-2 gap-3 mt-3">
                                      <label className="text-xs text-[#848E9C]">
                                        最大重入次数
                                        <input
                                          type="number"
                                          min="0"
                                          max="10"
                                          value={formData.risk_max_reentries}
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_max_reentries',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        恢复带（ATR）
                                        <input
                                          type="number"
                                          min="0"
                                          max="3"
                                          step="0.1"
                                          value={formData.risk_reentry_band_atr}
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_band_atr',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        冷却时间（秒）
                                        <input
                                          type="number"
                                          min="0"
                                          max="86400"
                                          value={
                                            formData.risk_reentry_cooldown_seconds
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_cooldown_seconds',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        最大追价（ATR）
                                        <input
                                          type="number"
                                          min="0"
                                          max="2"
                                          step="0.1"
                                          value={
                                            formData.risk_reentry_max_chase_atr
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_max_chase_atr',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        最大波动扩张倍数
                                        <input
                                          type="number"
                                          min="1"
                                          max="10"
                                          step="0.1"
                                          value={
                                            formData.risk_reentry_max_atr_expansion
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_max_atr_expansion',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        观察超时（分钟，0=直到平仓）
                                        <input
                                          type="number"
                                          min="0"
                                          value={
                                            formData.risk_watch_timeout_minutes
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_watch_timeout_minutes',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        最小恢复幅度（ATR，0=关闭）
                                        <input
                                          type="number"
                                          min="0"
                                          max="3"
                                          step="0.1"
                                          value={
                                            formData.risk_reentry_min_recovery_atr
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_min_recovery_atr',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        冷却递增倍率（第N次×倍率^N）
                                        <input
                                          type="number"
                                          min="1"
                                          max="10"
                                          step="0.5"
                                          value={
                                            formData.risk_reentry_cooldown_escalation
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_cooldown_escalation',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C]">
                                        恢复幅度递增倍率
                                        <input
                                          type="number"
                                          min="1"
                                          max="10"
                                          step="0.1"
                                          value={
                                            formData.risk_reentry_recovery_escalation
                                          }
                                          onChange={(e) =>
                                            handleInputChange(
                                              'risk_reentry_recovery_escalation',
                                              Number(e.target.value)
                                            )
                                          }
                                          className="mt-1 w-full px-2 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF]"
                                        />
                                      </label>
                                      <label className="text-xs text-[#848E9C] flex items-center justify-between col-span-2 pt-1">
                                        <span>
                                          噪音档仍允许重入
                                          <span className="block text-[#5E6673]">
                                            止损距离/ATR &lt; 0.3
                                            的高杠杆窄止损档默认禁止重入（易被噪音反复扫损，0.3~0.5
                                            为谨慎档自动加严确认）；打开表示接受该风险、按谨慎档放行
                                          </span>
                                        </span>
                                        <button
                                          type="button"
                                          onClick={() =>
                                            handleInputChange(
                                              'risk_reentry_noise_override',
                                              !formData.risk_reentry_noise_override
                                            )
                                          }
                                          className={`w-12 h-6 rounded-full transition-colors flex-shrink-0 ml-3 ${formData.risk_reentry_noise_override ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                                        >
                                          <div
                                            className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_reentry_noise_override ? 'translate-x-6' : 'translate-x-0.5'}`}
                                          />
                                        </button>
                                      </label>
                                    </div>
                                  )}
                                {formData.risk_policy_version >= 4 &&
                                  formData.risk_reentry_decision_mode ===
                                    'legacy_rule' && (
                                    <p className="text-xs text-[#848E9C] mt-2 leading-relaxed">
                                      <span className="text-[#F0B90B]">
                                        ⚙ v5 确认式重入：
                                      </span>
                                      价格须从止损价恢复至少 N×ATR
                                      并越过保守锚点（止损时刻与当前领航员均价的较优者），且连续多次采样均满足条件才重入（防单
                                      tick 假突破）；
                                      第二次及之后的重入冷却时间与恢复幅度按倍率递增；
                                      重入前会预检新仓位可保护性，无法建立有效止损时放弃重入。
                                    </p>
                                  )}
                              </>
                            )}
                          </div>

                          {/* 风控说明 */}
                          <div className="p-3 bg-[#1E2329] border border-[#F0B90B33] rounded flex items-start gap-2">
                            <svg
                              xmlns="http://www.w3.org/2000/svg"
                              className="w-4 h-4 text-[#F0B90B] mt-0.5 flex-shrink-0"
                              viewBox="0 0 24 24"
                              fill="none"
                              stroke="currentColor"
                              strokeWidth="2"
                            >
                              <circle cx="12" cy="12" r="10" />
                              <line x1="12" x2="12" y1="8" y2="12" />
                              <line x1="12" x2="12.01" y1="16" y2="16" />
                            </svg>
                            <span className="text-xs text-[#848E9C]">
                              Copy Guard v7：止损距离取 2.0×ATR、结构失效位
                              +0.25×ATR 与执行最小距离中的较宽者，超过 4×ATR
                              则拒绝入场；风险预算通过缩小仓位控制，绝不为了下足仓位把止损压进噪音区。
                              保护单由执行交易所托管并验证完整覆盖，无法建立或止损价已被行情穿越时立即受控退出；加仓超预算自动缩量，低于最小下单量则拒绝。
                            </span>
                          </div>
                        </>
                      )}
                    </div>
                  )}
                </div>
              )}
            </div>
          </div>

          {/* Trading Parameters */}
          <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-5">
            <h3 className="text-lg font-semibold text-[#EAECEF] mb-5 flex items-center gap-2">
              <span className="text-[#F0B90B]">4</span> 交易参数
            </h3>
            <div className="space-y-4">
              <div className="grid grid-cols-2 gap-4">
                <div>
                  <label className="text-sm text-[#EAECEF] block mb-2">
                    保证金模式
                  </label>
                  <div className="flex gap-2">
                    <button
                      type="button"
                      onClick={() => handleInputChange('is_cross_margin', true)}
                      className={`flex-1 px-3 py-2 rounded text-sm ${
                        formData.is_cross_margin
                          ? 'bg-[#F0B90B] text-black'
                          : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                      }`}
                    >
                      全仓
                    </button>
                    <button
                      type="button"
                      onClick={() =>
                        handleInputChange('is_cross_margin', false)
                      }
                      className={`flex-1 px-3 py-2 rounded text-sm ${
                        !formData.is_cross_margin
                          ? 'bg-[#F0B90B] text-black'
                          : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                      }`}
                    >
                      逐仓
                    </button>
                  </div>
                </div>
                <div>
                  <label className="text-sm text-[#EAECEF] block mb-2">
                    {t('aiScanInterval', language)}
                  </label>
                  <input
                    type="number"
                    value={formData.scan_interval_minutes}
                    onChange={(e) => {
                      const parsedValue = Number(e.target.value)
                      const safeValue = Number.isFinite(parsedValue)
                        ? Math.max(3, parsedValue)
                        : 3
                      handleInputChange('scan_interval_minutes', safeValue)
                    }}
                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none"
                    min="3"
                    max="60"
                    step="1"
                  />
                  <p className="text-xs text-gray-500 mt-1">
                    {t('scanIntervalRecommend', language)}
                  </p>
                </div>
              </div>

              {/* Competition visibility */}
              <div>
                <label className="text-sm text-[#EAECEF] block mb-2">
                  竞技场显示
                </label>
                <div className="flex gap-2">
                  <button
                    type="button"
                    onClick={() =>
                      handleInputChange('show_in_competition', true)
                    }
                    className={`flex-1 px-3 py-2 rounded text-sm ${
                      formData.show_in_competition
                        ? 'bg-[#F0B90B] text-black'
                        : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                    }`}
                  >
                    显示
                  </button>
                  <button
                    type="button"
                    onClick={() =>
                      handleInputChange('show_in_competition', false)
                    }
                    className={`flex-1 px-3 py-2 rounded text-sm ${
                      !formData.show_in_competition
                        ? 'bg-[#F0B90B] text-black'
                        : 'bg-[#0B0E11] text-[#848E9C] border border-[#2B3139]'
                    }`}
                  >
                    隐藏
                  </button>
                </div>
                <p className="text-xs text-[#848E9C] mt-1">
                  隐藏后将不在竞技场页面显示此交易员
                </p>
              </div>

              {/* Initial Balance (Edit mode only) */}
              {isEditMode && (
                <div>
                  <div className="flex items-center justify-between mb-2">
                    <label className="text-sm text-[#EAECEF]">
                      初始余额 ($)
                    </label>
                    <button
                      type="button"
                      onClick={handleFetchCurrentBalance}
                      disabled={isFetchingBalance}
                      className="px-3 py-1 text-xs bg-[#F0B90B] text-black rounded hover:bg-[#E1A706] transition-colors disabled:bg-[#848E9C] disabled:cursor-not-allowed"
                    >
                      {isFetchingBalance ? '获取中...' : '获取当前余额'}
                    </button>
                  </div>
                  <input
                    type="number"
                    value={formData.initial_balance || 0}
                    onChange={(e) =>
                      handleInputChange(
                        'initial_balance',
                        Number(e.target.value)
                      )
                    }
                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none"
                    min="100"
                    step="0.01"
                  />
                  <p className="text-xs text-[#848E9C] mt-1">
                    用于手动更新初始余额基准（例如充值/提现后）
                  </p>
                  {balanceFetchError && (
                    <p className="text-xs text-red-500 mt-1">
                      {balanceFetchError}
                    </p>
                  )}
                </div>
              )}

              {/* Create mode info */}
              {!isEditMode && (
                <div className="p-3 bg-[#1E2329] border border-[#2B3139] rounded flex items-center gap-2">
                  <svg
                    xmlns="http://www.w3.org/2000/svg"
                    className="w-4 h-4 text-[#F0B90B]"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  >
                    <circle cx="12" cy="12" r="10" />
                    <line x1="12" x2="12" y1="8" y2="12" />
                    <line x1="12" x2="12.01" y1="16" y2="16" />
                  </svg>
                  <span className="text-sm text-[#848E9C]">
                    系统将自动获取您的账户净值作为初始余额
                  </span>
                </div>
              )}
            </div>
          </div>
        </div>

        {/* Footer */}
        <div className="flex justify-end gap-3 p-6 border-t border-[#2B3139] bg-gradient-to-r from-[#1E2329] to-[#252B35] sticky bottom-0 z-10 rounded-b-xl">
          <button
            onClick={onClose}
            className="px-6 py-3 bg-[#2B3139] text-[#EAECEF] rounded-lg hover:bg-[#404750] transition-all duration-200 border border-[#404750]"
          >
            取消
          </button>
          {onSave && (
            <button
              onClick={handleSave}
              disabled={
                isSaving ||
                !formData.trader_name ||
                !formData.ai_model ||
                !formData.exchange_id ||
                !formData.strategy_id
              }
              className="px-8 py-3 bg-gradient-to-r from-[#F0B90B] to-[#E1A706] text-black rounded-lg hover:from-[#E1A706] hover:to-[#D4951E] transition-all duration-200 disabled:bg-[#848E9C] disabled:cursor-not-allowed font-medium shadow-lg"
            >
              {isSaving ? '保存中...' : isEditMode ? '保存修改' : '创建交易员'}
            </button>
          )}
        </div>
      </div>

      {/* Binance 全局共享凭证 Modal（嵌套在 TraderConfigModal 内，方便用户在配置 Binance 跟单时直接配置凭证） */}
      <BinanceGlobalCredsModal
        isOpen={showBinanceCredsModal}
        onClose={() => {
          setShowBinanceCredsModal(false)
          // 关闭后刷新一次凭证状态，立即反映新值
          setRefreshGlobalCredsTick((n) => n + 1)
        }}
      />
    </div>
  )
}
