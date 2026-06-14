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

// 交易所注册链接配置
const EXCHANGE_REGISTRATION_LINKS: Record<string, { url: string; hasReferral?: boolean }> = {
  binance: { url: 'https://www.binance.com/join?ref=NOFXENG', hasReferral: true },
  okx: { url: 'https://www.okx.com/join/1865360', hasReferral: true },
  bybit: { url: 'https://partner.bybit.com/b/83856', hasReferral: true },
  hyperliquid: { url: 'https://app.hyperliquid.xyz/join/AITRADING', hasReferral: true },
  aster: { url: 'https://www.asterdex.com/en/referral/fdfc0e', hasReferral: true },
  lighter: { url: 'https://app.lighter.xyz/?referral=68151432', hasReferral: true },
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
  copy_sync_margin_mode: boolean  // 同步保证金模式（OKX 区分全仓/逐仓）
  // Binance Web 私有接口凭证（仅 copy_provider_type=binance 时使用）
  copy_binance_p20t: string
  copy_binance_csrf_token: string
  // ============================================================
  // 账户保护 / 止损兜底（v3 风控）—— 仅 OKX 路径生效
  // 所有字段都有合理默认值，用户可在 UI 调整
  // ============================================================
  risk_stop_loss_enabled: boolean   // 默认 true：启用账户保护硬止损
  risk_account_pct: number          // 默认 0.5%（前端用百分比展示，提交时转 0.005）
  risk_atr_enabled: boolean         // 默认 true
  risk_atr_multiplier: number       // 默认 1.5
  risk_atr_timeframe: string        // 默认 "1h"
  risk_leverage_fallback: boolean   // 默认 true
  risk_leverage_max_loss: number    // 默认 50%（前端用百分比展示，提交时转 0.5）
  risk_reentry_enabled: boolean     // 默认 false（用户 opt-in）
  risk_reentry_ratio: number        // 默认 50%（前端用百分比，提交时转 0.5）
  risk_reentry_tolerance: number    // 默认 0.5%（前端用百分比，提交时转 0.005）
  // v3.2 反加仓铁律（仅 risk_reentry_enabled=on 时有效）
  risk_reentry_block_addback: boolean  // 默认 true：阻止领航员加仓后的重入
  risk_reentry_addback_tolerance: number // 前端用百分比展示：默认 20 (=20% 加仓容差)，提交时 1+x/100 转倍数
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
    copy_sync_margin_mode: true,  // 默认同步保证金模式
    copy_binance_p20t: '',
    copy_binance_csrf_token: '',
    // 账户保护 v3 风控默认值（与后端 store.FillRiskDefaults 保持一致）
    risk_stop_loss_enabled: true,
    risk_account_pct: 50,           // %（提交时 /100 转 0.5）—— v3.1 激进默认值
    risk_atr_enabled: true,
    risk_atr_multiplier: 1.5,
    risk_atr_timeframe: '1h',
    risk_leverage_fallback: true,
    risk_leverage_max_loss: 50,     // %（提交时 /100 转 0.5）
    risk_reentry_enabled: false,
    risk_reentry_ratio: 50,         // %（提交时 /100 转 0.5）
    risk_reentry_tolerance: 0.5,    // %（提交时 /100 转 0.005）
    risk_reentry_block_addback: true,  // 默认开启（保护账户）
    risk_reentry_addback_tolerance: 20, // %（提交时 1+x/100 转 1.20）
  })
  const [, setCopyTradeConfig] = useState<CopyTradeConfig | null>(null)
  const [isSaving, setIsSaving] = useState(false)
  const [strategies, setStrategies] = useState<Strategy[]>([])
  const [isFetchingBalance, setIsFetchingBalance] = useState(false)
  const [balanceFetchError, setBalanceFetchError] = useState<string>('')

  // Binance 全局凭证状态（仅 provider=binance 时拉取，作为状态展示）
  const [binanceGlobalCreds, setBinanceGlobalCreds] = useState<BinanceCredentialsView | null>(null)
  const [showBinanceCredsModal, setShowBinanceCredsModal] = useState(false)
  const [refreshGlobalCredsTick, setRefreshGlobalCredsTick] = useState(0)

  // 拉取 Binance 全局凭证状态（用于顶部状态卡展示）
  // 仅在弹窗打开 + provider=binance 时拉取，避免无谓的请求
  useEffect(() => {
    if (!isOpen || formData.copy_provider_type !== 'binance') return
    let cancelled = false
    api.listBinanceCredentials()
      .then((list) => {
        if (cancelled) return
        const def = list.find((c) => c.label === 'default') ?? null
        setBinanceGlobalCreds(def)
      })
      .catch(() => { /* 静默：状态卡仅展示用，失败时显示"未配置"即可 */ })
    return () => { cancelled = true }
  }, [isOpen, formData.copy_provider_type, refreshGlobalCredsTick])

  // 获取用户的策略列表
  useEffect(() => {
    const fetchStrategies = async () => {
      try {
        const result = await httpClient.get<{ strategies: Strategy[] }>('/api/strategies')
        if (result.success && result.data?.strategies) {
          const strategyList = result.data.strategies
          setStrategies(strategyList)
          // 如果没有选择策略，默认选中激活的策略
          if (!formData.strategy_id && !isEditMode) {
            const activeStrategy = strategyList.find(s => s.is_active)
            if (activeStrategy) {
              setFormData(prev => ({ ...prev, strategy_id: activeStrategy.id }))
            } else if (strategyList.length > 0) {
              setFormData(prev => ({ ...prev, strategy_id: strategyList[0].id }))
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
      try {
        const result = await httpClient.get<{ config: CopyTradeConfig }>(`/api/copytrade/config/${traderData.trader_id}`)
        if (result.success && result.data?.config) {
          const cfg = result.data.config
          setCopyTradeConfig(cfg)
          // 只加载跟单参数，decision_mode 由 traderData 决定
          // 风控字段：后端用比例（0.005=0.5%）存，前端用百分比展示，× 100 转换
          setFormData(prev => ({
            ...prev,
            copy_provider_type: cfg.provider_type as CopyTradeProvider,
            copy_leader_id: cfg.leader_id,
            copy_ratio: cfg.copy_ratio,
            copy_sync_leverage: cfg.sync_leverage,
            copy_sync_margin_mode: cfg.sync_margin_mode ?? true,  // 默认 true
            copy_binance_p20t: cfg.binance_p20t ?? '',
            copy_binance_csrf_token: cfg.binance_csrf_token ?? '',
            // 风控字段回填（× 100 转百分比展示）
            // 默认 50% 是 v3.1 用户明确选择的激进配置（与 store.FillRiskDefaults 保持一致）
            risk_stop_loss_enabled: cfg.risk_stop_loss_enabled ?? true,
            risk_account_pct: cfg.risk_account_pct != null ? cfg.risk_account_pct * 100 : 50,
            risk_atr_enabled: cfg.risk_atr_enabled ?? true,
            risk_atr_multiplier: cfg.risk_atr_multiplier ?? 1.5,
            risk_atr_timeframe: cfg.risk_atr_timeframe ?? '1h',
            risk_leverage_fallback: cfg.risk_leverage_fallback ?? true,
            risk_leverage_max_loss: cfg.risk_leverage_max_loss != null ? cfg.risk_leverage_max_loss * 100 : 50,
            risk_reentry_enabled: cfg.risk_reentry_enabled ?? false,
            risk_reentry_ratio: cfg.risk_reentry_ratio != null ? cfg.risk_reentry_ratio * 100 : 50,
            risk_reentry_tolerance: cfg.risk_reentry_tolerance != null ? cfg.risk_reentry_tolerance * 100 : 0.5,
            risk_reentry_block_addback: cfg.risk_reentry_block_addback ?? true,
            // 后端存倍数（如 1.20），前端展示百分比（如 20%）：(倍数 - 1) × 100
            // 防御：DB 异常值（< 1.0）clamp 到 0，避免 UI 显示负百分比
            risk_reentry_addback_tolerance: cfg.risk_reentry_addback_tolerance != null
              ? Math.max(0, (cfg.risk_reentry_addback_tolerance - 1) * 100)
              : 20,
          }))
        }
      } catch (error) {
        // 没有跟单配置，保持当前状态
        console.log('No copy trade config found')
      }
    }
    if (isOpen && isEditMode) {
      fetchCopyTradeConfig()
    }
  }, [isOpen, isEditMode, traderData?.trader_id])

  useEffect(() => {
    if (traderData) {
      setFormData(prev => ({
        ...prev,
        ...traderData,
        strategy_id: traderData.strategy_id || '',
        // Keep decision_mode from traderData if exists, otherwise keep prev value
        decision_mode: traderData.decision_mode || prev.decision_mode || 'copy_trade',
      }))
    } else if (!isEditMode) {
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
        copy_sync_margin_mode: true,  // 默认同步保证金模式
        copy_binance_p20t: '',
        copy_binance_csrf_token: '',
        // 风控默认值（与 useState 初始值保持一致）
        risk_stop_loss_enabled: true,
        risk_account_pct: 50,
        risk_atr_enabled: true,
        risk_atr_multiplier: 1.5,
        risk_atr_timeframe: '1h',
        risk_leverage_fallback: true,
        risk_leverage_max_loss: 50,
        risk_reentry_enabled: false,
        risk_reentry_ratio: 50,
        risk_reentry_tolerance: 0.5,
        risk_reentry_block_addback: true,
        risk_reentry_addback_tolerance: 20,
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

    setIsSaving(true)
    try {
      // Debug: log decision_mode before save
      console.log('🔧 [DEBUG] Saving trader with decision_mode:', formData.decision_mode)
      
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
          sync_margin_mode: formData.copy_sync_margin_mode,  // 同步保证金模式
          // 账户保护 v3 风控（前端展示百分比 → 后端存比例，× 0.01 转换）
          // 仅 OKX 路径生效；HL/Binance 后端会忽略这些字段（已通过 ProviderType 守卫）
          risk_stop_loss_enabled: formData.risk_stop_loss_enabled,
          risk_account_pct: formData.risk_account_pct / 100,
          risk_atr_enabled: formData.risk_atr_enabled,
          risk_atr_multiplier: formData.risk_atr_multiplier,
          risk_atr_timeframe: formData.risk_atr_timeframe,
          risk_leverage_fallback: formData.risk_leverage_fallback,
          risk_leverage_max_loss: formData.risk_leverage_max_loss / 100,
          risk_reentry_enabled: formData.risk_reentry_enabled,
          risk_reentry_ratio: formData.risk_reentry_ratio / 100,
          risk_reentry_tolerance: formData.risk_reentry_tolerance / 100,
          risk_reentry_block_addback: formData.risk_reentry_block_addback,
          // 前端百分比 → 后端倍数：20% → 1.20
          risk_reentry_addback_tolerance: 1 + formData.risk_reentry_addback_tolerance / 100,
        }
        // Binance 数据源额外携带 Web 私有接口凭证
        if (formData.copy_provider_type === 'binance') {
          saveData.copy_config.binance_p20t = formData.copy_binance_p20t.trim()
          saveData.copy_config.binance_csrf_token = formData.copy_binance_csrf_token.trim()
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

  const selectedStrategy = strategies.find(s => s.id === formData.strategy_id)

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
                        {getShortName(exchange.name || exchange.exchange_type || exchange.id).toUpperCase()}
                        {exchange.account_name ? ` - ${exchange.account_name}` : ''}
                      </option>
                    ))}
                  </select>
                  {/* Exchange Registration Link */}
                  {formData.exchange_id && (() => {
                    // Find the selected exchange to get its type
                    const selectedExchange = availableExchanges.find(e => e.id === formData.exchange_id)
                    const exchangeType = selectedExchange?.exchange_type?.toLowerCase() || ''
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
                      币种来源: {selectedStrategy.config.coin_source.source_type === 'static' ? '固定币种' :
                        selectedStrategy.config.coin_source.source_type === 'coinpool' ? 'Coin Pool' :
                        selectedStrategy.config.coin_source.source_type === 'oi_top' ? 'OI Top' : '混合'}
                    </div>
                    <div>
                      保证金上限: {((selectedStrategy.config.risk_control?.max_margin_usage || 0.9) * 100).toFixed(0)}%
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
                  onClick={() => handleInputChange('decision_mode', 'copy_trade')}
                  className={`p-4 rounded-lg border-2 transition-all ${
                    formData.decision_mode === 'copy_trade'
                      ? 'border-[#F0B90B] bg-[#F0B90B]/10'
                      : 'border-[#2B3139] hover:border-[#404750]'
                  }`}
                >
                  <div className="flex items-center gap-3 mb-2">
                    <Users className={`w-6 h-6 ${formData.decision_mode === 'copy_trade' ? 'text-[#F0B90B]' : 'text-[#848E9C]'}`} />
                    <span className={`font-medium ${formData.decision_mode === 'copy_trade' ? 'text-[#EAECEF]' : 'text-[#848E9C]'}`}>
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
                    <Bot className={`w-6 h-6 ${formData.decision_mode === 'ai' ? 'text-[#F0B90B]' : 'text-[#848E9C]'}`} />
                    <span className={`font-medium ${formData.decision_mode === 'ai' ? 'text-[#EAECEF]' : 'text-[#848E9C]'}`}>
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
                    <span className="text-[#F0B90B] text-sm font-medium">跟单配置</span>
                  </div>

                  {/* Provider Type */}
                  <div>
                    <label className="text-sm text-[#EAECEF] block mb-2">
                      数据源 <span className="text-red-500">*</span>
                    </label>
                    <div className="flex gap-2">
                      <button
                        type="button"
                        onClick={() => handleInputChange('copy_provider_type', 'hyperliquid')}
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
                        onClick={() => handleInputChange('copy_provider_type', 'okx')}
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
                        onClick={() => handleInputChange('copy_provider_type', 'binance')}
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
                      {formData.copy_provider_type === 'binance' ? '领航员 PortfolioId' : '领航员地址'}
                      <span className="text-red-500"> *</span>
                    </label>
                    <input
                      type="text"
                      value={formData.copy_leader_id}
                      onChange={(e) => handleInputChange('copy_leader_id', e.target.value)}
                      className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-sm"
                      placeholder={
                        formData.copy_provider_type === 'hyperliquid' ? '0x...' :
                        formData.copy_provider_type === 'binance' ? '领航员主页 portfolioId (如 5008318166959365632)' :
                        'UniqueName (如 F2BCA22ABBB69F57)'
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
                          <span className="text-[#F0B90B] text-sm font-medium">Binance 共享凭证</span>
                          <span className="px-2 py-0.5 text-[10px] bg-[#F0B90B22] text-[#F0B90B] rounded">全局共享</span>
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
                              账号 ID: <span className="font-mono text-[#EAECEF]">{binanceGlobalCreds.binance_user_id}</span>
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
                        所有 Binance 跟单交易员共用同一份凭证（p20t / csrftoken）。
                        在此处或顶部"Binance 凭证"按钮配置一次后，所有 Binance 交易员立即生效，无需重启。
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
                        onChange={(e) => handleInputChange('copy_ratio', parseFloat(e.target.value))}
                        className="flex-1 accent-[#F0B90B]"
                      />
                      <div className="w-20 text-center">
                        <span className="text-[#F0B90B] font-bold text-lg">{(formData.copy_ratio * 100).toFixed(0)}%</span>
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
                      <p className="text-xs text-[#848E9C]">使用与领航员相同的杠杆倍数</p>
                    </div>
                    <button
                      type="button"
                      onClick={() => handleInputChange('copy_sync_leverage', !formData.copy_sync_leverage)}
                      className={`w-12 h-6 rounded-full transition-colors ${
                        formData.copy_sync_leverage ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'
                      }`}
                    >
                      <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${
                        formData.copy_sync_leverage ? 'translate-x-6' : 'translate-x-0.5'
                      }`} />
                    </button>
                  </div>

                  {/* Sync Margin Mode (OKX) */}
                  <div className="flex items-center justify-between">
                    <div>
                      <label className="text-sm text-[#EAECEF]">同步保证金模式</label>
                      <p className="text-xs text-[#848E9C]">跟随领航员的全仓/逐仓模式（OKX 专用）</p>
                    </div>
                    <button
                      type="button"
                      onClick={() => handleInputChange('copy_sync_margin_mode', !formData.copy_sync_margin_mode)}
                      className={`w-12 h-6 rounded-full transition-colors ${
                        formData.copy_sync_margin_mode ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'
                      }`}
                    >
                      <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${
                        formData.copy_sync_margin_mode ? 'translate-x-6' : 'translate-x-0.5'
                      }`} />
                    </button>
                  </div>

                  {/* Info Box */}
                  <div className="p-3 bg-[#0B0E11] border border-[#2B3139] rounded flex items-start gap-2">
                    <svg xmlns="http://www.w3.org/2000/svg" className="w-4 h-4 text-[#F0B90B] mt-0.5 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                      <circle cx="12" cy="12" r="10" />
                      <line x1="12" x2="12" y1="8" y2="12" />
                      <line x1="12" x2="12.01" y1="16" y2="16" />
                    </svg>
                    <span className="text-xs text-[#848E9C]">
                      跟单模式将监听领航员的交易操作，只跟随新开仓（不跟历史仓位）。
                      跟单金额 = 跟单系数 × (领航员交易金额÷领航员账户余额) × 你的账户余额
                    </span>
                  </div>

                  {/* ============================================================
                      账户保护 / 止损兜底（v3 风控）—— 仅 OKX 路径生效
                      显示规则：仅 copy_provider_type=okx 时显示
                      所有数值用百分比展示，提交时由 handleSave 转为比例
                      ============================================================ */}
                  {formData.copy_provider_type === 'okx' && (
                    <div className="mt-4 p-4 bg-[#0B0E11] border border-[#F0B90B33] rounded-lg space-y-4">
                      <div className="flex items-center gap-2">
                        <svg xmlns="http://www.w3.org/2000/svg" className="w-4 h-4 text-[#F0B90B]" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
                        </svg>
                        <span className="text-[#F0B90B] text-sm font-medium">账户保护（止损兜底）</span>
                        <span className="px-2 py-0.5 text-[10px] bg-[#F0B90B22] text-[#F0B90B] rounded">仅 OKX</span>
                      </div>

                      {/* 主开关：账户保护止损 */}
                      <div className="flex items-center justify-between">
                        <div>
                          <label className="text-sm text-[#EAECEF]">启用账户保护止损</label>
                          <p className="text-xs text-[#848E9C]">开仓时自动挂交易所托管的硬止损单，防止极端反向走势打穿账户</p>
                        </div>
                        <button
                          type="button"
                          onClick={() => handleInputChange('risk_stop_loss_enabled', !formData.risk_stop_loss_enabled)}
                          className={`w-12 h-6 rounded-full transition-colors ${formData.risk_stop_loss_enabled ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                        >
                          <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_stop_loss_enabled ? 'translate-x-6' : 'translate-x-0.5'}`} />
                        </button>
                      </div>

                      {/* 风控参数（开关打开时显示） */}
                      {formData.risk_stop_loss_enabled && (
                        <>
                          {/* 单笔账户风险 % —— v3.1：无上限 + 默认 50%（激进） */}
                          <div>
                            <label className="text-sm text-[#EAECEF] block mb-2">
                              单笔最大账户风险 <span className="text-[#F0B90B] font-bold">{formData.risk_account_pct.toFixed(2)}%</span>
                            </label>
                            <input
                              type="number"
                              min="0.01"
                              step="0.1"
                              value={formData.risk_account_pct}
                              onChange={(e) => {
                                const v = parseFloat(e.target.value)
                                handleInputChange('risk_account_pct', isFinite(v) && v > 0 ? v : 0.5)
                              }}
                              className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none text-sm"
                            />
                            <p className="text-xs text-[#848E9C] mt-1">
                              单笔最多亏账户的百分比（账户 1000U + {formData.risk_account_pct.toFixed(2)}% = 单笔最多亏 <span className="text-[#F0B90B] font-mono">{(10 * formData.risk_account_pct).toFixed(2)} U</span>）。
                            </p>
                            {/* 激进配置警告：≥ 5% 时显示 */}
                            {formData.risk_account_pct >= 5 && (
                              <div className="mt-2 p-2 bg-[#F6465D11] border border-[#F6465D44] rounded flex items-start gap-2">
                                <svg xmlns="http://www.w3.org/2000/svg" className="w-4 h-4 text-[#F6465D] mt-0.5 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                                  <path d="M12 9v2m0 4h.01M5.07 19h13.86a2 2 0 0 0 1.74-3l-6.93-12a2 2 0 0 0-3.48 0l-6.93 12a2 2 0 0 0 1.74 3z" />
                                </svg>
                                <span className="text-xs text-[#F6465D]">
                                  <span className="font-bold">高风险设置：</span>
                                  单笔风险 ≥ 5% 时，{formData.risk_account_pct >= 50 ? '2-3 笔失败就可能造成账户严重亏损' : '建议仅在领航员经过严格筛选时使用'}。新手强烈建议调到 0.5-1%。
                                </span>
                              </div>
                            )}
                          </div>

                          {/* ATR 噪音防护 */}
                          <div className="border-t border-[#2B3139] pt-3">
                            <div className="flex items-center justify-between mb-2">
                              <div>
                                <label className="text-sm text-[#EAECEF]">ATR 噪音防护</label>
                                <p className="text-xs text-[#848E9C]">自动适应不同币种波动率，低波动币不被噪音扫损</p>
                              </div>
                              <button
                                type="button"
                                onClick={() => handleInputChange('risk_atr_enabled', !formData.risk_atr_enabled)}
                                className={`w-12 h-6 rounded-full transition-colors ${formData.risk_atr_enabled ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                              >
                                <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_atr_enabled ? 'translate-x-6' : 'translate-x-0.5'}`} />
                              </button>
                            </div>
                            {formData.risk_atr_enabled && (
                              <div className="grid grid-cols-2 gap-3">
                                <div>
                                  <label className="text-xs text-[#848E9C] block mb-1">ATR 倍数</label>
                                  <input
                                    type="number"
                                    min="1.0"
                                    max="3.0"
                                    step="0.1"
                                    value={formData.risk_atr_multiplier}
                                    onChange={(e) => handleInputChange('risk_atr_multiplier', parseFloat(e.target.value) || 1.5)}
                                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none text-sm"
                                  />
                                </div>
                                <div>
                                  <label className="text-xs text-[#848E9C] block mb-1">时间周期</label>
                                  <select
                                    value={formData.risk_atr_timeframe}
                                    onChange={(e) => handleInputChange('risk_atr_timeframe', e.target.value)}
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

                          {/* 杠杆兜底 */}
                          <div className="border-t border-[#2B3139] pt-3">
                            <div className="flex items-center justify-between mb-2">
                              <div>
                                <label className="text-sm text-[#EAECEF]">杠杆兜底封顶</label>
                                <p className="text-xs text-[#848E9C]">保证金亏损达此比例时强制平仓（高杠杆兜底）</p>
                              </div>
                              <button
                                type="button"
                                onClick={() => handleInputChange('risk_leverage_fallback', !formData.risk_leverage_fallback)}
                                className={`w-12 h-6 rounded-full transition-colors ${formData.risk_leverage_fallback ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                              >
                                <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_leverage_fallback ? 'translate-x-6' : 'translate-x-0.5'}`} />
                              </button>
                            </div>
                            {formData.risk_leverage_fallback && (
                              <div>
                                <label className="text-xs text-[#848E9C] block mb-1">
                                  最大保证金亏损 <span className="text-[#F0B90B]">{formData.risk_leverage_max_loss.toFixed(0)}%</span>
                                </label>
                                <input
                                  type="range"
                                  min="20"
                                  max="80"
                                  step="5"
                                  value={formData.risk_leverage_max_loss}
                                  onChange={(e) => handleInputChange('risk_leverage_max_loss', parseFloat(e.target.value))}
                                  className="w-full accent-[#F0B90B]"
                                />
                              </div>
                            )}
                          </div>

                          {/* 二次进场（高级） */}
                          <div className="border-t border-[#2B3139] pt-3">
                            <div className="flex items-center justify-between mb-2">
                              <div>
                                <label className="text-sm text-[#EAECEF]">二次进场（高级）</label>
                                <p className="text-xs text-[#848E9C]">止损触发后，若价格回到入场价 + 领航员浮亏明显收窄 + 未继续加仓，自动以小仓位重入</p>
                              </div>
                              <button
                                type="button"
                                onClick={() => handleInputChange('risk_reentry_enabled', !formData.risk_reentry_enabled)}
                                className={`w-12 h-6 rounded-full transition-colors ${formData.risk_reentry_enabled ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                              >
                                <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_reentry_enabled ? 'translate-x-6' : 'translate-x-0.5'}`} />
                              </button>
                            </div>
                            {formData.risk_reentry_enabled && (
                              <>
                                <div className="grid grid-cols-2 gap-3">
                                  <div>
                                    <label className="text-xs text-[#848E9C] block mb-1">
                                      重入仓位系数 <span className="text-[#F0B90B]">{formData.risk_reentry_ratio.toFixed(0)}%</span>
                                    </label>
                                    <input
                                      type="range"
                                      min="10"
                                      max="100"
                                      step="10"
                                      value={formData.risk_reentry_ratio}
                                      onChange={(e) => handleInputChange('risk_reentry_ratio', parseFloat(e.target.value))}
                                      className="w-full accent-[#F0B90B]"
                                    />
                                  </div>
                                  <div>
                                    <label className="text-xs text-[#848E9C] block mb-1">
                                      价格回归容差 <span className="text-[#F0B90B]">{formData.risk_reentry_tolerance.toFixed(2)}%</span>
                                    </label>
                                    <input
                                      type="number"
                                      min="0"
                                      max="2"
                                      step="0.1"
                                      value={formData.risk_reentry_tolerance}
                                      onChange={(e) => handleInputChange('risk_reentry_tolerance', parseFloat(e.target.value) || 0.5)}
                                      className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none text-sm"
                                    />
                                  </div>
                                </div>

                                {/* v3.2 反加仓铁律配置 */}
                                <div className="mt-3 pt-3 border-t border-[#2B3139]">
                                  <div className="flex items-center justify-between mb-2">
                                    <div>
                                      <label className="text-sm text-[#EAECEF]">阻止反加仓重入</label>
                                      <p className="text-xs text-[#848E9C]">领航员止损后又加仓（赌徒行为）时拒绝重入</p>
                                    </div>
                                    <button
                                      type="button"
                                      onClick={() => handleInputChange('risk_reentry_block_addback', !formData.risk_reentry_block_addback)}
                                      className={`w-12 h-6 rounded-full transition-colors ${formData.risk_reentry_block_addback ? 'bg-[#F0B90B]' : 'bg-[#2B3139]'}`}
                                    >
                                      <div className={`w-5 h-5 rounded-full bg-white shadow transition-transform ${formData.risk_reentry_block_addback ? 'translate-x-6' : 'translate-x-0.5'}`} />
                                    </button>
                                  </div>

                                  {formData.risk_reentry_block_addback && (
                                    <div>
                                      <label className="text-xs text-[#848E9C] block mb-1">
                                        允许加仓容差 <span className="text-[#F0B90B]">{formData.risk_reentry_addback_tolerance.toFixed(0)}%</span>
                                        <span className="text-[#5E6673] ml-2">(0% = 严格禁止, 20% = 推荐, 50%+ = 宽松)</span>
                                      </label>
                                      <input
                                        type="range"
                                        min="0"
                                        max="100"
                                        step="5"
                                        value={formData.risk_reentry_addback_tolerance}
                                        onChange={(e) => handleInputChange('risk_reentry_addback_tolerance', parseFloat(e.target.value))}
                                        className="w-full accent-[#F0B90B]"
                                      />
                                      <p className="text-xs text-[#848E9C] mt-1">
                                        领航员止损后加仓 ≤ <span className="text-[#F0B90B] font-mono">{formData.risk_reentry_addback_tolerance.toFixed(0)}%</span> 时仍允许重入；
                                        超出视为赌徒型补仓救单，拒绝重入保护账户。
                                      </p>
                                    </div>
                                  )}

                                  {!formData.risk_reentry_block_addback && (
                                    <div className="p-2 bg-[#F6465D11] border border-[#F6465D44] rounded">
                                      <p className="text-xs text-[#F6465D]">
                                        ⚠️ 已关闭反加仓拦截：领航员即使止损后疯狂加仓救单，系统也会重入跟随。
                                        请确认你信任的是非赌徒型领航员。
                                      </p>
                                    </div>
                                  )}
                                </div>
                              </>
                            )}
                          </div>

                          {/* 风控说明 */}
                          <div className="p-3 bg-[#1E2329] border border-[#F0B90B33] rounded flex items-start gap-2">
                            <svg xmlns="http://www.w3.org/2000/svg" className="w-4 h-4 text-[#F0B90B] mt-0.5 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                              <circle cx="12" cy="12" r="10" />
                              <line x1="12" x2="12" y1="8" y2="12" />
                              <line x1="12" x2="12.01" y1="16" y2="16" />
                            </svg>
                            <span className="text-xs text-[#848E9C]">
                              账户保护止损算法：账户风险线（硬上限）+ ATR 下界（防噪音）+ 杠杆兜底（最外层封顶）三层取严。
                              SL 由 OKX 交易所托管（algo 条件单），即使本系统离线也不影响触发。
                              加仓后会按新均价自动收紧 SL，保护账户最大亏损不变。
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
                    onClick={() => handleInputChange('show_in_competition', true)}
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
                    onClick={() => handleInputChange('show_in_competition', false)}
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
