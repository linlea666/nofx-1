import { useState, useEffect } from 'react'
import type { AIModel, Exchange, CreateTraderRequest, Strategy, DecisionMode, CopyTradeProvider, CopyTradeConfig } from '../types'
import { useLanguage } from '../contexts/LanguageContext'
import { t } from '../i18n/translations'
import { toast } from 'sonner'
import { Pencil, Plus, X as IconX, Sparkles, ExternalLink, UserPlus, Bot, Users } from 'lucide-react'
import { httpClient } from '../lib/httpClient'

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
    decision_mode: 'ai',
    copy_provider_type: 'hyperliquid',
    copy_leader_id: '',
    copy_ratio: 1.0,
    copy_sync_leverage: true,
  })
  const [, setCopyTradeConfig] = useState<CopyTradeConfig | null>(null)
  const [isSaving, setIsSaving] = useState(false)
  const [strategies, setStrategies] = useState<Strategy[]>([])
  const [isFetchingBalance, setIsFetchingBalance] = useState(false)
  const [balanceFetchError, setBalanceFetchError] = useState<string>('')

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

  // 加载跟单配置
  useEffect(() => {
    const fetchCopyTradeConfig = async () => {
      if (!isEditMode || !traderData?.trader_id) return
      try {
        const result = await httpClient.get<{ config: CopyTradeConfig }>(`/api/copytrade/config/${traderData.trader_id}`)
        if (result.success && result.data?.config) {
          const cfg = result.data.config
          setCopyTradeConfig(cfg)
          setFormData(prev => ({
            ...prev,
            decision_mode: cfg.enabled ? 'copy_trade' : 'ai',
            copy_provider_type: cfg.provider_type as CopyTradeProvider,
            copy_leader_id: cfg.leader_id,
            copy_ratio: cfg.copy_ratio,
            copy_sync_leverage: cfg.sync_leverage,
          }))
        }
      } catch (error) {
        // 没有跟单配置，保持默认 AI 模式
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
        decision_mode: traderData.decision_mode || prev.decision_mode || 'ai',
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
        decision_mode: 'ai',
        copy_provider_type: 'hyperliquid',
        copy_leader_id: '',
        copy_ratio: 1.0,
        copy_sync_leverage: true,
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
        decision_mode: formData.decision_mode || 'ai', // Ensure non-empty
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
                    </div>
                  </div>

                  {/* Leader ID */}
                  <div>
                    <label className="text-sm text-[#EAECEF] block mb-2">
                      领航员地址 <span className="text-red-500">*</span>
                    </label>
                    <input
                      type="text"
                      value={formData.copy_leader_id}
                      onChange={(e) => handleInputChange('copy_leader_id', e.target.value)}
                      className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-sm"
                      placeholder={formData.copy_provider_type === 'hyperliquid' ? '0x...' : 'UniqueName (如 F2BCA22ABBB69F57)'}
                    />
                    <p className="text-xs text-[#848E9C] mt-1">
                      {formData.copy_provider_type === 'hyperliquid'
                        ? 'Hyperliquid 钱包地址 (0x开头)'
                        : 'OKX 交易员 uniqueName (交易员页面 URL 中的参数)'}
                    </p>
                  </div>

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
    </div>
  )
}
