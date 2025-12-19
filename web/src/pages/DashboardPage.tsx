import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import {
  TrendingUp,
  TrendingDown,
  Users,
  Zap,
  Target,
  Shield,
  Globe,
  Database,
  Terminal as TerminalIcon,
  Layers,
  AlertTriangle,
  Activity,
  Radio,
  Bug,
  CheckCircle,
  XCircle,
  Clock,
  BarChart3,
  Gauge,
  Heart,
} from 'lucide-react'
import { PunkAvatar, getTraderAvatar } from '../components/PunkAvatar'

// --- 类型定义 ---
interface TraderStats {
  trader_id: string
  trader_name: string
  mode: string
  exchange: string
  today_pnl: number
  today_trades: number
  week_pnl: number
  week_trades: number
  month_pnl: number
  month_trades: number
  total_pnl: number
  total_trades: number
  win_rate: number
  profit_factor: number
  current_equity: number
  initial_balance: number
  return_rate: number
  position_count: number
  is_running: boolean
}

interface GlobalStats {
  total_pnl: number
  total_trades: number
  avg_win_rate: number
  active_traders: number
  total_equity: number
  today_pnl: number
  week_pnl: number
  month_pnl: number
}

interface RiskAlert {
  level: 'critical' | 'warning' | 'info'
  type: string
  trader_id: string
  trader_name: string
  message: string
  value: number
  timestamp: string
}

interface SystemMonitor {
  today_signals: number
  today_executed: number
  today_skipped: number
  today_failed: number
  execution_rate: number
  rate_limit_errors: number
  network_errors: number
  auth_errors: number
  other_errors: number
  health_score: number
  alerts: RiskAlert[]
  updated_at: string
}

// --- API 调用 ---
const API_BASE = import.meta.env.VITE_API_BASE_URL || ''

const fetchDashboardTraders = async () => {
  const res = await fetch(`${API_BASE}/api/dashboard/traders`)
  if (!res.ok) throw new Error('数据加载失败')
  return res.json()
}

const fetchDashboardSummary = async () => {
  const res = await fetch(`${API_BASE}/api/dashboard/summary`)
  if (!res.ok) throw new Error('汇总数据加载失败')
  return res.json()
}

const fetchDashboardMonitor = async () => {
  const res = await fetch(`${API_BASE}/api/dashboard/monitor`)
  if (!res.ok) throw new Error('监控数据加载失败')
  return res.json()
}

// --- UI 组件库 ---

/** 仪表盘组件 */
function DataGauge({ value, label, color = '#00f2ff', size = 80 }: { value: number; label: string; color?: string; size?: number }) {
  const radius = size * 0.4
  const circumference = 2 * Math.PI * radius
  const offset = circumference - (Math.min(value, 100) / 100) * circumference

  return (
    <div className="relative flex flex-col items-center">
      <svg width={size} height={size} className="-rotate-90">
        <circle cx={size/2} cy={size/2} r={radius} fill="none" stroke="rgba(255,255,255,0.05)" strokeWidth="6" />
        <circle
          cx={size/2}
          cy={size/2}
          r={radius}
          fill="none"
          stroke={color}
          strokeWidth="6"
          strokeDasharray={circumference}
          strokeDashoffset={offset}
          strokeLinecap="round"
          className="transition-all duration-1000 ease-out"
          style={{ filter: `drop-shadow(0 0 8px ${color}80)` }}
        />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="text-lg font-black font-mono leading-none">{value.toFixed(1)}%</span>
        <span className="text-[9px] text-white/40 mt-0.5">{label}</span>
      </div>
    </div>
  )
}

/** 大屏模块面板 */
function DashboardModule({
  children,
  title,
  subtitle,
  icon: Icon,
  className = '',
  color = 'cyan',
  rightContent,
}: {
  children: React.ReactNode
  title: string
  subtitle?: string
  icon?: any
  className?: string
  color?: 'cyan' | 'pink' | 'green' | 'yellow' | 'red'
  rightContent?: React.ReactNode
}) {
  const colorMap = {
    cyan: '#00f2ff',
    pink: '#ff00ff',
    green: '#00ff9d',
    yellow: '#ffe600',
    red: '#ff0055',
  }
  const themeColor = colorMap[color]

  return (
    <div className={`relative flex flex-col ${className}`}>
      <div className="absolute inset-0 bg-[#0a1628]/70 backdrop-blur-xl border border-white/5 rounded-sm shadow-2xl" />
      
      {/* 战术边角 */}
      <div className="absolute top-0 left-0 w-2.5 h-2.5 border-t-2 border-l-2" style={{ borderColor: themeColor }} />
      <div className="absolute top-0 right-0 w-2.5 h-2.5 border-t-2 border-r-2" style={{ borderColor: themeColor }} />
      <div className="absolute bottom-0 left-0 w-2.5 h-2.5 border-b-2 border-l-2" style={{ borderColor: themeColor }} />
      <div className="absolute bottom-0 right-0 w-2.5 h-2.5 border-b-2 border-r-2" style={{ borderColor: themeColor }} />

      {/* 标题栏 */}
      <div className="relative z-10 flex items-center justify-between px-3 py-2 border-b border-white/10 bg-white/5">
        <div className="flex items-center gap-2">
          {Icon && <Icon size={14} style={{ color: themeColor }} />}
          <span className="text-xs font-black tracking-wider text-white/90">{title}</span>
          {subtitle && <span className="text-[8px] text-white/30 font-bold uppercase tracking-tight ml-1">/ {subtitle}</span>}
        </div>
        <div className="flex items-center gap-2">
          {rightContent}
          <div className="w-1.5 h-1.5 rounded-full animate-pulse" style={{ backgroundColor: themeColor }} />
        </div>
      </div>

      {/* 内容区 */}
      <div className="relative z-10 p-3 flex-1 overflow-hidden">
        {children}
      </div>
    </div>
  )
}

/** 核心统计卡片 */
function StatCard({ 
  label, 
  value, 
  suffix = '', 
  color = '#00f2ff',
  trend,
}: { 
  label: string
  value: number
  suffix?: string
  color?: string
  trend?: 'up' | 'down' | null
}) {
  return (
    <div className="bg-[#0a1628]/80 border border-white/10 px-4 py-3 rounded-sm text-center min-w-[140px]">
      <div className="text-[10px] text-white/40 font-black uppercase tracking-widest mb-1">{label}</div>
      <div className="flex items-center justify-center gap-1">
        <span 
          className="text-2xl font-mono font-black tracking-tight" 
          style={{ color, textShadow: `0 0 15px ${color}40` }}
        >
          {typeof value === 'number' ? value.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 }) : value}
        </span>
        {suffix && <span className="text-sm text-white/40 font-bold">{suffix}</span>}
        {trend === 'up' && <TrendingUp size={14} className="text-[#00ff9d] ml-1" />}
        {trend === 'down' && <TrendingDown size={14} className="text-[#ff0055] ml-1" />}
      </div>
    </div>
  )
}

/** 周期切换按钮 */
function PeriodTabs({ 
  value, 
  onChange 
}: { 
  value: 'today' | 'week' | 'month'
  onChange: (v: 'today' | 'week' | 'month') => void 
}) {
  const tabs = [
    { key: 'today', label: '今日' },
    { key: 'week', label: '本周' },
    { key: 'month', label: '本月' },
  ] as const

  return (
    <div className="flex bg-white/5 rounded-sm p-0.5">
      {tabs.map(tab => (
        <button
          key={tab.key}
          onClick={() => onChange(tab.key)}
          className={`px-3 py-1 text-[10px] font-black uppercase tracking-wider transition-all ${
            value === tab.key 
              ? 'bg-[#00f2ff]/20 text-[#00f2ff]' 
              : 'text-white/40 hover:text-white/60'
          }`}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}

// --- 页面主体 ---

export function DashboardPage() {
  const [currentTime, setCurrentTime] = useState(new Date())
  const [selectedTrader, setSelectedTrader] = useState<TraderStats | null>(null)
  const [period, setPeriod] = useState<'today' | 'week' | 'month'>('today')

  // 数据获取（添加 error 处理）
  const { data: tradersRaw, isLoading, error: tradersError, mutate: refreshTraders } = useSWR(
    'dashboard-traders-v4', 
    fetchDashboardTraders, 
    { refreshInterval: 15000, revalidateOnFocus: false }
  )
  const { data: summaryRaw, error: summaryError } = useSWR(
    'dashboard-summary-v4', 
    fetchDashboardSummary, 
    { refreshInterval: 15000, revalidateOnFocus: false }
  )
  const { data: monitorRaw, error: monitorError } = useSWR(
    'dashboard-monitor-v4', 
    fetchDashboardMonitor, 
    { refreshInterval: 30000, revalidateOnFocus: false }
  )
  
  // 综合错误状态
  const hasError = tradersError || summaryError || monitorError

  useEffect(() => {
    const timer = setInterval(() => setCurrentTime(new Date()), 100)
    return () => clearInterval(timer)
  }, [])

  const traderStats: TraderStats[] = useMemo(() => {
    if (!tradersRaw) return []
    return (tradersRaw as any[]).map(t => ({
      ...t,
      trader_name: t.trader_name || t.trader_id?.substring(0, 8) || '未知交易员',
    }))
  }, [tradersRaw])

  const globalStats: GlobalStats = useMemo(() => {
    return summaryRaw || {
      total_pnl: 0, total_trades: 0, avg_win_rate: 0, active_traders: 0, total_equity: 0,
      today_pnl: 0, week_pnl: 0, month_pnl: 0
    }
  }, [summaryRaw])

  const monitor: SystemMonitor = useMemo(() => {
    const defaults = {
      today_signals: 0, today_executed: 0, today_skipped: 0, today_failed: 0,
      execution_rate: 0, rate_limit_errors: 0, network_errors: 0, auth_errors: 0, other_errors: 0,
      health_score: 100, alerts: [] as RiskAlert[], updated_at: ''
    }
    if (!monitorRaw) return defaults
    // 深度合并，确保 alerts 不为 null
    return {
      ...defaults,
      ...monitorRaw,
      alerts: monitorRaw.alerts || []
    }
  }, [monitorRaw])

  const sortedTraders = [...traderStats].sort((a, b) => b.total_pnl - a.total_pnl)

  useEffect(() => {
    if (sortedTraders.length > 0 && !selectedTrader) setSelectedTrader(sortedTraders[0])
  }, [sortedTraders, selectedTrader])

  // 根据周期获取盈亏数据
  const getPeriodPnL = (stats: GlobalStats | TraderStats) => {
    switch (period) {
      case 'today': return stats.today_pnl
      case 'week': return stats.week_pnl
      case 'month': return stats.month_pnl
      default: return stats.today_pnl
    }
  }

  const getPeriodTrades = (stats: TraderStats) => {
    switch (period) {
      case 'today': return stats.today_trades
      case 'week': return stats.week_trades
      case 'month': return stats.month_trades
      default: return stats.today_trades
    }
  }

  const periodLabel = { today: '今日', week: '本周', month: '本月' }[period]

  // 加载中状态
  if (isLoading && !tradersRaw) {
    return (
      <div className="fixed inset-0 bg-[#020617] z-[9999] flex flex-col items-center justify-center font-mono">
        <div className="w-64 h-1 bg-white/5 relative overflow-hidden mb-4 rounded-full">
          <div className="absolute top-0 left-0 h-full bg-[#00f2ff] animate-[loading_2s_infinite]" />
        </div>
        <div className="text-[#00f2ff] text-xs tracking-[0.5em] animate-pulse">正在同步全球交易节点...</div>
        <style>{`@keyframes loading { 0% { left: -100%; width: 30% } 100% { left: 100%; width: 30% } }`}</style>
      </div>
    )
  }

  // 错误状态（API 请求失败）
  if (hasError && !tradersRaw) {
    return (
      <div className="fixed inset-0 bg-[#020617] z-[9999] flex flex-col items-center justify-center font-mono">
        <div className="text-[#ff0055] mb-4">
          <AlertTriangle size={48} />
        </div>
        <div className="text-[#ff0055] text-lg font-bold mb-2">数据加载失败</div>
        <div className="text-white/40 text-sm mb-6 max-w-md text-center">
          {tradersError?.message || summaryError?.message || monitorError?.message || '无法连接到服务器'}
        </div>
        <button
          onClick={() => refreshTraders()}
          className="px-6 py-2 bg-[#00f2ff]/20 border border-[#00f2ff]/40 text-[#00f2ff] hover:bg-[#00f2ff]/30 transition-colors rounded"
        >
          重新加载
        </button>
      </div>
    )
  }

  return (
    <div className="fixed inset-0 bg-[#020617] text-white z-[9999] overflow-hidden flex flex-col font-sans select-none">
      {/* 背景 */}
      <div className="absolute inset-0 pointer-events-none opacity-20">
        <div className="absolute inset-0" style={{
          backgroundImage: `radial-gradient(circle at 50% 50%, #1e3a5f 0%, transparent 60%)`,
        }} />
        <div className="absolute inset-0" style={{
          backgroundImage: `linear-gradient(to right, #ffffff08 1px, transparent 1px), linear-gradient(to bottom, #ffffff08 1px, transparent 1px)`,
          backgroundSize: '50px 50px',
        }} />
      </div>

      {/* --- 顶部导航条 --- */}
      <header className="relative z-10 h-16 border-b border-white/10 bg-[#0a1628]/80 backdrop-blur-xl px-6 flex items-center justify-between">
        {/* 左侧：Logo + 系统状态 */}
        <div className="flex items-center gap-6">
          <div className="flex items-center gap-3">
            <div className="w-10 h-10 bg-gradient-to-br from-[#00f2ff] to-[#ff00ff] rounded-lg rotate-45 flex items-center justify-center shadow-[0_0_15px_rgba(0,242,255,0.3)]">
              <Zap size={22} className="text-white -rotate-45" />
            </div>
            <div className="flex flex-col">
              <span className="text-lg font-black tracking-tight">NOFX 交易指挥中心</span>
              <span className="text-[9px] text-[#00f2ff] font-bold tracking-[0.3em] uppercase opacity-60">Operations v3.0</span>
            </div>
          </div>
          
          <div className="h-8 w-px bg-white/10" />
          
          <div className="flex items-center gap-2">
            <Heart size={14} className={monitor.health_score >= 80 ? 'text-[#00ff9d]' : monitor.health_score >= 50 ? 'text-[#ffe600]' : 'text-[#ff0055]'} />
            <span className="text-xs font-mono font-bold" style={{ color: monitor.health_score >= 80 ? '#00ff9d' : monitor.health_score >= 50 ? '#ffe600' : '#ff0055' }}>
              健康度 {monitor.health_score}%
            </span>
          </div>
        </div>

        {/* 中央：核心统计 + 周期切换 */}
        <div className="absolute left-1/2 -translate-x-1/2 flex items-center gap-4">
          <PeriodTabs value={period} onChange={setPeriod} />
          
          <div className="flex items-center gap-3">
            <StatCard 
              label={`${periodLabel}盈亏`}
              value={getPeriodPnL(globalStats)}
              color={getPeriodPnL(globalStats) >= 0 ? '#00ff9d' : '#ff0055'}
              trend={getPeriodPnL(globalStats) >= 0 ? 'up' : 'down'}
            />
            <StatCard 
              label="总成交笔数"
              value={globalStats.total_trades}
              suffix="笔"
              color="#ffe600"
            />
            <StatCard 
              label="活跃交易员"
              value={globalStats.active_traders}
              suffix="位"
              color="#ff00ff"
            />
          </div>
        </div>

        {/* 右侧：时钟 */}
        <div className="bg-[#0a1628]/80 border border-white/10 px-4 py-2 rounded-sm text-right min-w-[180px]">
          <div className="text-xl font-mono font-black text-[#00f2ff] leading-none tracking-tight">
            {currentTime.getHours().toString().padStart(2, '0')}:
            {currentTime.getMinutes().toString().padStart(2, '0')}:
            {currentTime.getSeconds().toString().padStart(2, '0')}
            <span className="text-xs opacity-40 ml-1">.{Math.floor(currentTime.getMilliseconds() / 100)}</span>
          </div>
          <div className="text-[9px] text-white/30 font-bold uppercase tracking-wider mt-1">
            {currentTime.toLocaleDateString('zh-CN', { month: 'long', day: '2-digit', weekday: 'short' })}
          </div>
        </div>
      </header>

      {/* --- 主视口布局 --- */}
      <main className="relative z-10 flex-1 p-4 grid grid-cols-12 gap-4 overflow-hidden">
        
        {/* 左翼：排行与资产 */}
        <div className="col-span-3 flex flex-col gap-4 overflow-hidden">
          <DashboardModule title="交易员排行榜" subtitle="Leaderboard" icon={Users} color="cyan" className="flex-[2] overflow-hidden">
            <div className="h-full overflow-y-auto pr-1 custom-scrollbar space-y-2">
              {sortedTraders.map((t, i) => (
                <div 
                  key={t.trader_id}
                  onClick={() => setSelectedTrader(t)}
                  className={`group relative p-2.5 border transition-all cursor-pointer ${
                    selectedTrader?.trader_id === t.trader_id 
                    ? 'bg-[#00f2ff]/10 border-[#00f2ff]/40' 
                    : 'bg-white/2 border-white/5 hover:border-white/20'
                  }`}
                >
                  <div className="flex items-center gap-2.5">
                    <div className={`text-lg font-mono font-black w-6 text-center ${i < 3 ? 'text-[#ffe600]' : 'text-white/20'}`}>
                      {i + 1}
                    </div>
                    <div className="relative">
                      <PunkAvatar 
                        seed={getTraderAvatar(t.trader_id, t.trader_name)} 
                        size={36} 
                        className={`rounded-sm shadow transition-all ${selectedTrader?.trader_id === t.trader_id ? 'grayscale-0' : 'grayscale-[50%] group-hover:grayscale-0'}`} 
                      />
                      {t.is_running && (
                        <div className="absolute -bottom-0.5 -right-0.5 w-2 h-2 bg-[#00ff9d] rounded-full animate-pulse shadow-[0_0_6px_#00ff9d]" />
                      )}
                    </div>
                    <div className="flex-1 min-w-0">
                      <div className="text-xs font-bold truncate text-white/90">{t.trader_name}</div>
                      <div className="flex items-center gap-1.5 mt-1">
                        <div className="flex-1 h-1 bg-white/5 rounded-full overflow-hidden">
                          <div className="h-full bg-gradient-to-r from-[#00f2ff] to-[#ff00ff]" style={{ width: `${t.win_rate}%` }} />
                        </div>
                        <span className="text-[8px] font-mono text-white/40">{t.win_rate.toFixed(0)}%</span>
                      </div>
                    </div>
                    <div className="text-right">
                      <div className="text-sm font-mono font-black" style={{ color: t.total_pnl >= 0 ? '#00ff9d' : '#ff0055' }}>
                        {t.total_pnl >= 0 ? '+' : ''}{t.total_pnl.toFixed(2)}
                      </div>
                      <div className="text-[8px] text-white/30 uppercase">累计</div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </DashboardModule>

          <DashboardModule title="交易所分布" subtitle="Exchange" icon={Globe} color="green" className="flex-1">
            <div className="h-full flex flex-col justify-center gap-3">
              {(() => {
                // 计算交易所分布
                const exchangeMap: Record<string, number> = {}
                traderStats.forEach(t => {
                  const ex = t.exchange || 'Unknown'
                  exchangeMap[ex] = (exchangeMap[ex] || 0) + 1
                })
                const total = traderStats.length || 1
                const exchanges = Object.entries(exchangeMap).map(([name, count]) => ({
                  label: name.toUpperCase(),
                  val: Math.round((count / total) * 100),
                  color: name.toLowerCase().includes('hyper') ? '#00f2ff' : '#ff00ff',
                }))
                
                return exchanges.length > 0 ? exchanges.map(item => (
                  <div key={item.label}>
                    <div className="flex justify-between text-[9px] font-bold mb-1">
                      <span className="text-white/40">{item.label}</span>
                      <span style={{ color: item.color }}>{item.val}%</span>
                    </div>
                    <div className="h-1.5 w-full bg-white/5 rounded-full overflow-hidden">
                      <div className="h-full transition-all duration-500" style={{ width: `${item.val}%`, backgroundColor: item.color }} />
                    </div>
                  </div>
                )) : (
                  <div className="text-xs text-white/30 text-center">暂无数据</div>
                )
              })()}
            </div>
          </DashboardModule>
        </div>

        {/* 中心：核心详情 */}
        <div className="col-span-6 flex flex-col gap-4">
          <DashboardModule 
            title="交易员详情" 
            subtitle="Detail" 
            icon={Layers} 
            className="flex-1"
            rightContent={selectedTrader && (
              <span className="text-[9px] font-mono text-white/30">ID: {selectedTrader.trader_id.substring(0, 12)}</span>
            )}
          >
            {selectedTrader ? (
              <div className="h-full flex flex-col">
                {/* 头部信息 */}
                <div className="flex items-start justify-between mb-4">
                  <div className="flex items-center gap-4">
                    <div className="relative w-20 h-20 border-2 border-[#00f2ff]/30 p-1 bg-[#00f2ff]/5">
                      <div className="absolute -top-1.5 -left-1.5 w-4 h-4 border-t-2 border-l-2 border-[#00f2ff]" />
                      <div className="absolute -bottom-1.5 -right-1.5 w-4 h-4 border-b-2 border-r-2 border-[#00f2ff]" />
                      <PunkAvatar seed={getTraderAvatar(selectedTrader.trader_id, selectedTrader.trader_name)} size={72} className="rounded-none w-full h-full" />
                    </div>
                    <div>
                      <div className="flex items-center gap-2">
                        <h2 className="text-3xl font-black tracking-tight uppercase">{selectedTrader.trader_name}</h2>
                        {selectedTrader.is_running && (
                          <div className="px-2 py-0.5 bg-[#00ff9d]/20 border border-[#00ff9d]/40 text-[#00ff9d] text-[9px] font-bold uppercase">运行中</div>
                        )}
                      </div>
                      <div className="flex items-center gap-4 mt-2 text-xs">
                        <div>
                          <span className="text-white/30">模式: </span>
                          <span className="text-[#ff00ff] font-bold uppercase">{selectedTrader.mode}</span>
                        </div>
                        <div>
                          <span className="text-white/30">交易所: </span>
                          <span className="text-[#00f2ff] font-bold uppercase">{selectedTrader.exchange || 'N/A'}</span>
                        </div>
                        <div>
                          <span className="text-white/30">净值: </span>
                          <span className="text-[#ffe600] font-mono font-bold">${selectedTrader.current_equity.toLocaleString()}</span>
                        </div>
                      </div>
                    </div>
                  </div>
                  
                  <div className="flex items-center gap-6 bg-white/5 p-4 border border-white/10 rounded-sm">
                    <DataGauge value={selectedTrader.win_rate} label="胜率" color="#00ff9d" size={70} />
                    <div className="text-right">
                      <div className="text-[9px] text-white/30 uppercase tracking-wider mb-1">{periodLabel}盈亏</div>
                      <div 
                        className="text-3xl font-mono font-black" 
                        style={{ color: getPeriodPnL(selectedTrader) >= 0 ? '#00ff9d' : '#ff0055' }}
                      >
                        {getPeriodPnL(selectedTrader) >= 0 ? '+' : ''}${Math.abs(getPeriodPnL(selectedTrader)).toFixed(2)}
                      </div>
                      <div className="text-[9px] text-white/30 mt-1">{getPeriodTrades(selectedTrader)} 笔交易</div>
                    </div>
                  </div>
                </div>

                {/* 核心指标网格 */}
                <div className="grid grid-cols-4 gap-3 mb-4">
                  {[
                    { label: '收益率', value: `${selectedTrader.return_rate.toFixed(2)}%`, icon: TrendingUp, color: '#00ff9d' },
                    { label: '盈亏比', value: selectedTrader.profit_factor.toFixed(2), icon: Target, color: '#00f2ff' },
                    { label: '总交易', value: selectedTrader.total_trades, icon: Activity, color: '#ff00ff' },
                    { label: '当前持仓', value: selectedTrader.position_count, icon: Database, color: '#ffe600' },
                  ].map((item, i) => (
                    <div key={i} className="bg-white/5 border border-white/10 p-3 relative">
                      <div className="absolute top-0 left-0 w-0.5 h-full" style={{ backgroundColor: item.color }} />
                      <div className="text-[9px] text-white/40 uppercase mb-1">{item.label}</div>
                      <div className="text-xl font-mono font-black" style={{ color: item.color }}>{item.value}</div>
                    </div>
                  ))}
                </div>

                {/* 图表区域 */}
                <div className="flex-1 bg-[#020617]/60 border border-white/10 relative p-4">
                  <div className="absolute top-3 left-4 flex items-center gap-2">
                    <BarChart3 size={12} className="text-[#00f2ff]" />
                    <span className="text-[10px] text-white/40 uppercase tracking-wider">收益趋势</span>
                  </div>
                  <div className="h-full pt-6 relative">
                    <div className="absolute inset-0 grid grid-cols-10 pointer-events-none opacity-10">
                      {[...Array(10)].map((_, i) => <div key={i} className="border-r border-white/20 h-full" />)}
                    </div>
                    <div className="relative h-full flex items-end px-1 gap-0.5">
                      {[...Array(30)].map((_, i) => {
                        const height = Math.sin(i * 0.3 + Date.now() / 5000) * 30 + 40 + Math.random() * 20
                        const isPositive = height > 50
                        return (
                          <div 
                            key={i} 
                            className="flex-1 transition-all rounded-t-sm" 
                            style={{ 
                              height: `${height}%`,
                              backgroundColor: isPositive ? '#00ff9d20' : '#ff005520',
                              borderTop: `2px solid ${isPositive ? '#00ff9d' : '#ff0055'}`,
                            }}
                          />
                        )
                      })}
                    </div>
                  </div>
                </div>
              </div>
            ) : (
              <div className="h-full flex items-center justify-center text-white/30">
                请选择一个交易员查看详情
              </div>
            )}
          </DashboardModule>
        </div>

        {/* 右翼：监控与预警 */}
        <div className="col-span-3 flex flex-col gap-4 overflow-hidden">
          {/* 系统监控 */}
          <DashboardModule title="系统监控" subtitle="Monitor" icon={Gauge} color="cyan" className="flex-1">
            <div className="h-full flex flex-col gap-3">
              {/* 跟单执行统计 */}
              <div className="grid grid-cols-2 gap-2">
                <div className="bg-white/5 p-2 border border-white/10 text-center">
                  <div className="text-[9px] text-white/40 uppercase">今日信号</div>
                  <div className="text-lg font-mono font-bold text-[#00f2ff]">{monitor.today_signals}</div>
                </div>
                <div className="bg-white/5 p-2 border border-white/10 text-center">
                  <div className="text-[9px] text-white/40 uppercase">执行率</div>
                  <div className="text-lg font-mono font-bold text-[#00ff9d]">{monitor.execution_rate.toFixed(1)}%</div>
                </div>
              </div>
              
              {/* 执行状态条 */}
              <div className="space-y-2">
                {[
                  { label: '执行成功', value: monitor.today_executed, color: '#00ff9d', icon: CheckCircle },
                  { label: '跳过', value: monitor.today_skipped, color: '#ffe600', icon: Clock },
                  { label: '失败', value: monitor.today_failed, color: '#ff0055', icon: XCircle },
                ].map(item => (
                  <div key={item.label} className="flex items-center gap-2">
                    <item.icon size={12} style={{ color: item.color }} />
                    <span className="text-[9px] text-white/40 w-12">{item.label}</span>
                    <div className="flex-1 h-1.5 bg-white/5 rounded-full overflow-hidden">
                      <div 
                        className="h-full transition-all" 
                        style={{ 
                          width: monitor.today_signals > 0 ? `${(item.value / monitor.today_signals) * 100}%` : '0%',
                          backgroundColor: item.color 
                        }} 
                      />
                    </div>
                    <span className="text-[9px] font-mono" style={{ color: item.color }}>{item.value}</span>
                  </div>
                ))}
              </div>

              {/* API 错误统计 */}
              <div className="border-t border-white/10 pt-2 mt-auto">
                <div className="text-[9px] text-white/40 uppercase mb-2">API 错误 (24h)</div>
                <div className="grid grid-cols-2 gap-1.5 text-[9px]">
                  {[
                    { label: '频率限制', value: monitor.rate_limit_errors },
                    { label: '网络错误', value: monitor.network_errors },
                    { label: '认证错误', value: monitor.auth_errors },
                    { label: '其他错误', value: monitor.other_errors },
                  ].map(item => (
                    <div key={item.label} className="flex justify-between bg-white/5 px-2 py-1">
                      <span className="text-white/40">{item.label}</span>
                      <span className={item.value > 0 ? 'text-[#ff0055] font-bold' : 'text-white/20'}>{item.value}</span>
                    </div>
                  ))}
                </div>
              </div>
            </div>
          </DashboardModule>

          {/* 风险预警 */}
          <DashboardModule 
            title="风险预警" 
            subtitle="Alerts" 
            icon={AlertTriangle} 
            color={monitor.alerts.some(a => a.level === 'critical') ? 'red' : 'yellow'} 
            className="flex-1 overflow-hidden"
          >
            <div className="h-full overflow-y-auto pr-1 custom-scrollbar space-y-2">
              {monitor.alerts.length > 0 ? monitor.alerts.map((alert, i) => (
                <div 
                  key={i} 
                  className={`flex items-start gap-2 p-2 border rounded-sm ${
                    alert.level === 'critical' 
                      ? 'bg-red-500/10 border-red-500/30' 
                      : alert.level === 'warning'
                      ? 'bg-yellow-500/10 border-yellow-500/30'
                      : 'bg-blue-500/10 border-blue-500/30'
                  }`}
                >
                  {alert.level === 'critical' ? (
                    <Bug size={14} className="text-red-500 shrink-0 mt-0.5" />
                  ) : (
                    <Radio size={14} className="text-yellow-500 shrink-0 mt-0.5" />
                  )}
                  <div className="flex-1 min-w-0">
                    <div className={`text-[9px] font-bold uppercase ${
                      alert.level === 'critical' ? 'text-red-500' : 'text-yellow-500'
                    }`}>
                      {alert.trader_name}
                    </div>
                    <p className={`text-[9px] leading-tight mt-0.5 ${
                      alert.level === 'critical' ? 'text-red-500/70' : 'text-yellow-500/70'
                    }`}>
                      {alert.message}
                    </p>
                  </div>
                </div>
              )) : (
                <div className="h-full flex flex-col items-center justify-center text-white/30">
                  <Shield size={24} className="mb-2 opacity-30" />
                  <span className="text-[10px]">系统运行正常</span>
                  <span className="text-[9px] text-white/20">暂无风险预警</span>
                </div>
              )}
            </div>
          </DashboardModule>

          {/* 实时日志 */}
          <DashboardModule title="实时日志" subtitle="Log" icon={TerminalIcon} color="pink" className="flex-1 overflow-hidden">
            <div className="h-full font-mono text-[9px] space-y-1.5 overflow-y-auto custom-scrollbar">
              <div className="text-[#ff00ff]/60">[SYS] 数据大屏已连接</div>
              <div className="text-[#00f2ff]/60">[API] 实时数据流已接入</div>
              
              {traderStats.slice(0, 8).map((trader, i) => {
                const actions = ['开多', '开空', '平多', '平空']
                const action = actions[i % 4]
                const isOpen = action.includes('开')
                return (
                  <div key={i} className="flex gap-1 p-1.5 bg-white/5 border-l border-white/10">
                    <span className="text-white/20">[{new Date(Date.now() - i * 180000).toLocaleTimeString('zh-CN', { hour12: false })}]</span>
                    <span className="text-[#00f2ff] truncate max-w-[60px]">{trader.trader_name}</span>
                    <span className={isOpen ? 'text-[#00ff9d]' : 'text-[#ff0055]'}>{action}</span>
                    <span className="text-white/30 ml-auto">{(Math.random() * 0.5).toFixed(3)} ETH</span>
                  </div>
                )
              })}
              
              <div className="flex items-center gap-2 pt-2 border-t border-white/10">
                <div className="w-1 h-3 bg-[#ff00ff] animate-pulse" />
                <span className="text-[#ff00ff]/60 animate-pulse">监听中...</span>
              </div>
            </div>
          </DashboardModule>
        </div>
      </main>

      {/* --- 底部状态条 --- */}
      <footer className="relative z-10 h-8 border-t border-white/10 bg-[#0a1628]/80 px-6 flex items-center justify-between text-[9px] font-bold tracking-wider uppercase">
        <div className="flex items-center gap-6">
          <div className="flex items-center gap-1.5">
            <div className="w-1.5 h-1.5 rounded-full bg-[#00ff9d]" />
            <span className="text-white/40">安全协议: <span className="text-[#00ff9d]">AES-256</span></span>
          </div>
          <div className="flex items-center gap-1.5">
            <div className="w-1.5 h-1.5 rounded-full bg-[#00f2ff] animate-pulse" />
            <span className="text-white/40">数据同步: <span className="text-[#00f2ff]">实时</span></span>
          </div>
          <div className="flex items-center gap-1.5">
            <span className="text-white/40">最后更新: </span>
            <span className="text-white/60 font-mono">{monitor.updated_at || currentTime.toLocaleTimeString('zh-CN')}</span>
          </div>
        </div>
        
        <div className="flex items-center gap-4 text-white/30">
          <span>NOFX Trading Terminal v3.0</span>
        </div>
      </footer>

      {/* 全局样式 */}
      <style>{`
        .custom-scrollbar::-webkit-scrollbar { width: 3px; }
        .custom-scrollbar::-webkit-scrollbar-track { background: rgba(255,255,255,0.02); }
        .custom-scrollbar::-webkit-scrollbar-thumb { background: rgba(255,255,255,0.1); border-radius: 10px; }
        .custom-scrollbar::-webkit-scrollbar-thumb:hover { background: rgba(0,242,255,0.3); }
      `}</style>
    </div>
  )
}
