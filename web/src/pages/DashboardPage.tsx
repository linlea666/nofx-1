import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import { api } from '../lib/api'
import {
  TrendingUp,
  TrendingDown,
  Activity,
  Users,
  BarChart3,
  Zap,
  Trophy,
  Clock,
  RefreshCw,
  ChevronUp,
  ChevronDown,
  Target,
  Percent,
  DollarSign,
  Wallet,
} from 'lucide-react'
import { PunkAvatar, getTraderAvatar } from '../components/PunkAvatar'

// 时间范围类型
type TimeRange = 'today' | 'week' | 'month' | 'all'

// 交易员统计类型
interface TraderStats {
  trader_id: string
  trader_name: string
  mode: string
  today_pnl: number
  week_pnl: number
  month_pnl: number
  total_pnl: number
  total_trades: number
  win_rate: number
  profit_factor: number
  current_equity: number
  initial_balance: number
  return_rate: number
  position_count: number
}

// 全局统计类型
interface GlobalStats {
  total_pnl: number
  total_trades: number
  avg_win_rate: number
  active_traders: number
  total_equity: number
  total_fees: number
}

// 获取公开交易员数据
const fetchPublicTraders = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/traders`)
  if (!res.ok) throw new Error('Failed to fetch traders')
  return res.json()
}

// 获取权益历史
const fetchEquityHistory = async (traderId: string) => {
  const res = await fetch(
    `${import.meta.env.VITE_API_BASE_URL || ''}/api/equity-history?trader_id=${traderId}`
  )
  if (!res.ok) throw new Error('Failed to fetch equity history')
  return res.json()
}

// 格式化金额
function formatMoney(value: number, showSign = true): string {
  const sign = value >= 0 ? '+' : ''
  const formatted = Math.abs(value).toLocaleString('en-US', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })
  return showSign ? `${sign}$${formatted}` : `$${formatted}`
}

// 格式化百分比
function formatPercent(value: number, showSign = true): string {
  const sign = value >= 0 ? '+' : ''
  const formatted = Math.abs(value).toFixed(2)
  return showSign ? `${sign}${formatted}%` : `${formatted}%`
}

// 科技感数字动画组件
function AnimatedNumber({
  value,
  prefix = '',
  suffix = '',
  decimals = 2,
  className = '',
}: {
  value: number
  prefix?: string
  suffix?: string
  decimals?: number
  className?: string
}) {
  const [displayValue, setDisplayValue] = useState(0)

  useEffect(() => {
    const duration = 1500
    const steps = 60
    const stepDuration = duration / steps
    const increment = value / steps
    let current = 0
    let step = 0

    const timer = setInterval(() => {
      step++
      current += increment
      if (step >= steps) {
        setDisplayValue(value)
        clearInterval(timer)
      } else {
        setDisplayValue(current)
      }
    }, stepDuration)

    return () => clearInterval(timer)
  }, [value])

  return (
    <span className={className}>
      {prefix}
      {displayValue.toLocaleString('en-US', {
        minimumFractionDigits: decimals,
        maximumFractionDigits: decimals,
      })}
      {suffix}
    </span>
  )
}

// 霓虹发光边框卡片
function NeonCard({
  children,
  className = '',
  glowColor = 'yellow',
}: {
  children: React.ReactNode
  className?: string
  glowColor?: 'yellow' | 'green' | 'red' | 'cyan' | 'purple'
}) {
  const glowStyles = {
    yellow: 'shadow-[0_0_30px_rgba(252,213,53,0.15)] hover:shadow-[0_0_40px_rgba(252,213,53,0.25)] border-[#FCD535]/20',
    green: 'shadow-[0_0_30px_rgba(14,203,129,0.15)] hover:shadow-[0_0_40px_rgba(14,203,129,0.25)] border-[#0ECB81]/20',
    red: 'shadow-[0_0_30px_rgba(246,70,93,0.15)] hover:shadow-[0_0_40px_rgba(246,70,93,0.25)] border-[#F6465D]/20',
    cyan: 'shadow-[0_0_30px_rgba(0,212,255,0.15)] hover:shadow-[0_0_40px_rgba(0,212,255,0.25)] border-[#00d4ff]/20',
    purple: 'shadow-[0_0_30px_rgba(168,85,247,0.15)] hover:shadow-[0_0_40px_rgba(168,85,247,0.25)] border-purple-500/20',
  }

  return (
    <div
      className={`
        relative overflow-hidden rounded-xl border backdrop-blur-sm
        bg-gradient-to-br from-[#1E2329]/90 to-[#0B0E11]/90
        transition-all duration-500 ease-out
        ${glowStyles[glowColor]}
        ${className}
      `}
    >
      {/* 扫描线动画 */}
      <div className="absolute inset-0 overflow-hidden pointer-events-none">
        <div className="absolute top-0 left-0 w-full h-[1px] bg-gradient-to-r from-transparent via-white/20 to-transparent animate-scan-line" />
      </div>
      {children}
    </div>
  )
}

// 统计卡片组件
function StatCard({
  title,
  value,
  change,
  icon: Icon,
  trend,
  glowColor,
  prefix = '',
  suffix = '',
}: {
  title: string
  value: number
  change?: number
  icon: React.ElementType
  trend?: 'up' | 'down' | 'neutral'
  glowColor: 'yellow' | 'green' | 'red' | 'cyan' | 'purple'
  prefix?: string
  suffix?: string
}) {
  const trendColors = {
    up: 'text-[#0ECB81]',
    down: 'text-[#F6465D]',
    neutral: 'text-gray-400',
  }

  const iconColors = {
    yellow: 'text-[#FCD535]',
    green: 'text-[#0ECB81]',
    red: 'text-[#F6465D]',
    cyan: 'text-[#00d4ff]',
    purple: 'text-purple-400',
  }

  return (
    <NeonCard glowColor={glowColor} className="p-5">
      <div className="flex items-start justify-between">
        <div className="space-y-3">
          <p className="text-sm text-gray-400 uppercase tracking-wider font-medium">{title}</p>
          <p className="text-3xl font-bold text-white font-mono">
            <AnimatedNumber value={value} prefix={prefix} suffix={suffix} />
          </p>
          {change !== undefined && (
            <div className={`flex items-center gap-1 text-sm ${trendColors[trend || 'neutral']}`}>
              {trend === 'up' ? <ChevronUp size={16} /> : trend === 'down' ? <ChevronDown size={16} /> : null}
              <span>{change >= 0 ? '+' : ''}{change.toFixed(2)}%</span>
              <span className="text-gray-500">vs yesterday</span>
            </div>
          )}
        </div>
        <div className={`p-3 rounded-xl bg-white/5 ${iconColors[glowColor]}`}>
          <Icon size={24} />
        </div>
      </div>
    </NeonCard>
  )
}

// 交易员排行榜
function TraderLeaderboard({
  traders,
  timeRange,
  onSelectTrader,
}: {
  traders: TraderStats[]
  timeRange: TimeRange
  onSelectTrader: (trader: TraderStats) => void
}) {
  const getPnL = (trader: TraderStats) => {
    switch (timeRange) {
      case 'today':
        return trader.today_pnl
      case 'week':
        return trader.week_pnl
      case 'month':
        return trader.month_pnl
      default:
        return trader.total_pnl
    }
  }

  const sortedTraders = [...traders].sort((a, b) => getPnL(b) - getPnL(a))

  return (
    <NeonCard glowColor="cyan" className="h-full">
      <div className="p-5 border-b border-white/5">
        <div className="flex items-center gap-3">
          <Trophy className="text-[#FCD535]" size={24} />
          <h3 className="text-lg font-bold text-white">交易员排行榜</h3>
        </div>
      </div>
      <div className="overflow-y-auto max-h-[400px] custom-scrollbar">
        {sortedTraders.map((trader, index) => {
          const pnl = getPnL(trader)
          const isPositive = pnl >= 0

          return (
            <div
              key={trader.trader_id}
              onClick={() => onSelectTrader(trader)}
              className={`
                flex items-center gap-4 p-4 border-b border-white/5
                hover:bg-white/5 cursor-pointer transition-all duration-300
                ${index < 3 ? 'bg-gradient-to-r from-[#FCD535]/5 to-transparent' : ''}
              `}
            >
              {/* 排名 */}
              <div
                className={`
                  w-8 h-8 rounded-full flex items-center justify-center font-bold text-sm
                  ${index === 0 ? 'bg-[#FFD700] text-black' : ''}
                  ${index === 1 ? 'bg-[#C0C0C0] text-black' : ''}
                  ${index === 2 ? 'bg-[#CD7F32] text-black' : ''}
                  ${index > 2 ? 'bg-white/10 text-gray-400' : ''}
                `}
              >
                {index + 1}
              </div>

              {/* 头像和名称 */}
              <div className="flex items-center gap-3 flex-1 min-w-0">
                <PunkAvatar
                  seed={getTraderAvatar(trader.trader_id)}
                  size={36}
                  className="rounded-full ring-2 ring-white/10"
                />
                <div className="min-w-0">
                  <p className="font-medium text-white truncate">{trader.trader_name}</p>
                  <p className="text-xs text-gray-500">
                    {trader.mode === 'copy_trade' ? '跟单' : 'AI'} · {trader.total_trades} 笔
                  </p>
                </div>
              </div>

              {/* 盈亏和胜率 */}
              <div className="text-right">
                <p className={`font-mono font-bold ${isPositive ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
                  {formatMoney(pnl)}
                </p>
                <p className="text-xs text-gray-500">{trader.win_rate.toFixed(1)}% 胜率</p>
              </div>
            </div>
          )
        })}
        {sortedTraders.length === 0 && (
          <div className="p-8 text-center text-gray-500">暂无交易员数据</div>
        )}
      </div>
    </NeonCard>
  )
}

// 交易员详情面板
function TraderDetailPanel({ trader }: { trader: TraderStats | null }) {
  if (!trader) {
    return (
      <NeonCard glowColor="purple" className="h-full flex items-center justify-center">
        <div className="text-center text-gray-500 p-8">
          <Target size={48} className="mx-auto mb-4 opacity-50" />
          <p>点击排行榜中的交易员查看详情</p>
        </div>
      </NeonCard>
    )
  }

  const isPositive = trader.total_pnl >= 0

  return (
    <NeonCard glowColor="purple" className="h-full">
      <div className="p-5 border-b border-white/5">
        <div className="flex items-center gap-4">
          <PunkAvatar
            seed={getTraderAvatar(trader.trader_id)}
            size={48}
            className="rounded-full ring-2 ring-purple-500/30"
          />
          <div>
            <h3 className="text-xl font-bold text-white">{trader.trader_name}</h3>
            <p className="text-sm text-gray-400">
              {trader.mode === 'copy_trade' ? '跟单模式' : 'AI 模式'}
            </p>
          </div>
        </div>
      </div>

      <div className="p-5 space-y-6">
        {/* 盈亏统计 */}
        <div className="grid grid-cols-2 gap-4">
          <div className="space-y-1">
            <p className="text-xs text-gray-500 uppercase">今日盈亏</p>
            <p className={`text-lg font-mono font-bold ${trader.today_pnl >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {formatMoney(trader.today_pnl)}
            </p>
          </div>
          <div className="space-y-1">
            <p className="text-xs text-gray-500 uppercase">本周盈亏</p>
            <p className={`text-lg font-mono font-bold ${trader.week_pnl >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {formatMoney(trader.week_pnl)}
            </p>
          </div>
          <div className="space-y-1">
            <p className="text-xs text-gray-500 uppercase">本月盈亏</p>
            <p className={`text-lg font-mono font-bold ${trader.month_pnl >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {formatMoney(trader.month_pnl)}
            </p>
          </div>
          <div className="space-y-1">
            <p className="text-xs text-gray-500 uppercase">总盈亏</p>
            <p className={`text-lg font-mono font-bold ${isPositive ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {formatMoney(trader.total_pnl)}
            </p>
          </div>
        </div>

        {/* 核心指标 */}
        <div className="space-y-3">
          <h4 className="text-sm font-medium text-gray-400 uppercase tracking-wider">核心指标</h4>
          <div className="grid grid-cols-2 gap-3">
            <div className="bg-white/5 rounded-lg p-3">
              <div className="flex items-center gap-2 mb-1">
                <Percent size={14} className="text-[#FCD535]" />
                <span className="text-xs text-gray-500">胜率</span>
              </div>
              <p className="text-lg font-bold text-white">{trader.win_rate.toFixed(1)}%</p>
            </div>
            <div className="bg-white/5 rounded-lg p-3">
              <div className="flex items-center gap-2 mb-1">
                <BarChart3 size={14} className="text-[#00d4ff]" />
                <span className="text-xs text-gray-500">盈亏比</span>
              </div>
              <p className="text-lg font-bold text-white">{trader.profit_factor.toFixed(2)}</p>
            </div>
            <div className="bg-white/5 rounded-lg p-3">
              <div className="flex items-center gap-2 mb-1">
                <Activity size={14} className="text-purple-400" />
                <span className="text-xs text-gray-500">交易次数</span>
              </div>
              <p className="text-lg font-bold text-white">{trader.total_trades}</p>
            </div>
            <div className="bg-white/5 rounded-lg p-3">
              <div className="flex items-center gap-2 mb-1">
                <TrendingUp size={14} className="text-[#0ECB81]" />
                <span className="text-xs text-gray-500">收益率</span>
              </div>
              <p className={`text-lg font-bold ${trader.return_rate >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
                {formatPercent(trader.return_rate)}
              </p>
            </div>
          </div>
        </div>

        {/* 账户状态 */}
        <div className="space-y-3">
          <h4 className="text-sm font-medium text-gray-400 uppercase tracking-wider">账户状态</h4>
          <div className="bg-white/5 rounded-lg p-4 space-y-2">
            <div className="flex justify-between">
              <span className="text-gray-400">当前净值</span>
              <span className="text-white font-mono">${trader.current_equity.toLocaleString()}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-gray-400">初始资金</span>
              <span className="text-white font-mono">${trader.initial_balance.toLocaleString()}</span>
            </div>
            <div className="flex justify-between">
              <span className="text-gray-400">当前持仓</span>
              <span className="text-white">{trader.position_count} 个</span>
            </div>
          </div>
        </div>
      </div>
    </NeonCard>
  )
}

// 迷你折线图（纯 CSS）
function MiniSparkline({ data, positive }: { data: number[]; positive: boolean }) {
  if (data.length < 2) return null

  const max = Math.max(...data)
  const min = Math.min(...data)
  const range = max - min || 1

  const points = data
    .map((v, i) => {
      const x = (i / (data.length - 1)) * 100
      const y = 100 - ((v - min) / range) * 100
      return `${x},${y}`
    })
    .join(' ')

  return (
    <svg viewBox="0 0 100 40" className="w-full h-10" preserveAspectRatio="none">
      <defs>
        <linearGradient id={`gradient-${positive ? 'green' : 'red'}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={positive ? '#0ECB81' : '#F6465D'} stopOpacity="0.3" />
          <stop offset="100%" stopColor={positive ? '#0ECB81' : '#F6465D'} stopOpacity="0" />
        </linearGradient>
      </defs>
      <polyline
        fill="none"
        stroke={positive ? '#0ECB81' : '#F6465D'}
        strokeWidth="2"
        points={points}
        vectorEffect="non-scaling-stroke"
      />
      <polygon
        fill={`url(#gradient-${positive ? 'green' : 'red'})`}
        points={`0,40 ${points} 100,40`}
      />
    </svg>
  )
}

// 主大屏页面
export function DashboardPage() {
  const [timeRange, setTimeRange] = useState<TimeRange>('month')
  const [selectedTrader, setSelectedTrader] = useState<TraderStats | null>(null)
  const [currentTime, setCurrentTime] = useState(new Date())

  // 获取公开交易员数据
  const { data: publicTraders, isLoading } = useSWR('public-traders', fetchPublicTraders, {
    refreshInterval: 30000, // 30秒刷新一次
  })

  // 更新时间
  useEffect(() => {
    const timer = setInterval(() => setCurrentTime(new Date()), 1000)
    return () => clearInterval(timer)
  }, [])

  // 模拟统计数据（实际应从 API 获取）
  const traderStats: TraderStats[] = useMemo(() => {
    if (!publicTraders) return []
    return publicTraders.map((t: any) => ({
      trader_id: t.id,
      trader_name: t.name || t.id.substring(0, 8),
      mode: t.decision_mode || 'ai',
      today_pnl: (t.pnl || 0) * 0.1,
      week_pnl: (t.pnl || 0) * 0.3,
      month_pnl: (t.pnl || 0) * 0.7,
      total_pnl: t.pnl || 0,
      total_trades: t.total_trades || Math.floor(Math.random() * 100) + 10,
      win_rate: t.win_rate || Math.random() * 30 + 40,
      profit_factor: t.profit_factor || Math.random() * 1.5 + 0.5,
      current_equity: t.equity || 1000,
      initial_balance: t.initial_balance || 1000,
      return_rate: t.pnl_pct || 0,
      position_count: t.position_count || 0,
    }))
  }, [publicTraders])

  // 全局统计
  const globalStats: GlobalStats = useMemo(() => {
    return {
      total_pnl: traderStats.reduce((sum, t) => sum + t.total_pnl, 0),
      total_trades: traderStats.reduce((sum, t) => sum + t.total_trades, 0),
      avg_win_rate: traderStats.length > 0
        ? traderStats.reduce((sum, t) => sum + t.win_rate, 0) / traderStats.length
        : 0,
      active_traders: traderStats.filter(t => t.position_count > 0).length,
      total_equity: traderStats.reduce((sum, t) => sum + t.current_equity, 0),
      total_fees: 0,
    }
  }, [traderStats])

  const timeRangeLabels: Record<TimeRange, string> = {
    today: '今日',
    week: '本周',
    month: '本月',
    all: '全部',
  }

  return (
    <div className="min-h-screen bg-[#0B0E11] relative overflow-hidden">
      {/* 背景网格 */}
      <div
        className="fixed inset-0 opacity-[0.02] pointer-events-none"
        style={{
          backgroundImage: `
            linear-gradient(to right, #FCD535 1px, transparent 1px),
            linear-gradient(to bottom, #FCD535 1px, transparent 1px)
          `,
          backgroundSize: '50px 50px',
        }}
      />

      {/* 背景光晕 */}
      <div className="fixed inset-0 pointer-events-none">
        <div className="absolute top-0 left-1/4 w-96 h-96 bg-[#FCD535]/10 rounded-full blur-[150px]" />
        <div className="absolute bottom-0 right-1/4 w-96 h-96 bg-[#0ECB81]/10 rounded-full blur-[150px]" />
        <div className="absolute top-1/2 right-0 w-64 h-64 bg-[#00d4ff]/10 rounded-full blur-[100px]" />
      </div>

      {/* 顶部标题栏 */}
      <header className="relative z-10 px-6 py-4 border-b border-white/5 backdrop-blur-sm bg-[#0B0E11]/80">
        <div className="max-w-[1800px] mx-auto flex items-center justify-between">
          <div className="flex items-center gap-4">
            <div className="flex items-center gap-3">
              <div className="w-10 h-10 rounded-xl bg-gradient-to-br from-[#FCD535] to-[#F0B90B] flex items-center justify-center">
                <Zap className="text-black" size={24} />
              </div>
              <div>
                <h1 className="text-xl font-bold text-white tracking-tight">NOFX 交易数据中心</h1>
                <p className="text-xs text-gray-500">Trading Analytics Dashboard</p>
              </div>
            </div>
          </div>

          <div className="flex items-center gap-6">
            {/* 时间筛选 */}
            <div className="flex items-center gap-1 bg-white/5 rounded-lg p-1">
              {(['today', 'week', 'month', 'all'] as TimeRange[]).map((range) => (
                <button
                  key={range}
                  onClick={() => setTimeRange(range)}
                  className={`
                    px-4 py-2 rounded-md text-sm font-medium transition-all
                    ${timeRange === range
                      ? 'bg-[#FCD535] text-black'
                      : 'text-gray-400 hover:text-white hover:bg-white/5'
                    }
                  `}
                >
                  {timeRangeLabels[range]}
                </button>
              ))}
            </div>

            {/* 实时时间 */}
            <div className="flex items-center gap-2 text-gray-400">
              <Clock size={16} />
              <span className="font-mono text-sm">
                {currentTime.toLocaleTimeString('zh-CN', { hour12: false })}
              </span>
            </div>

            {/* 刷新状态 */}
            <div className="flex items-center gap-2">
              <RefreshCw size={14} className={`text-[#0ECB81] ${isLoading ? 'animate-spin' : ''}`} />
              <span className="text-xs text-gray-500">实时</span>
            </div>
          </div>
        </div>
      </header>

      {/* 主内容区 */}
      <main className="relative z-10 p-6">
        <div className="max-w-[1800px] mx-auto space-y-6">
          {/* 顶部统计卡片 */}
          <div className="grid grid-cols-5 gap-4">
            <StatCard
              title="总盈亏"
              value={globalStats.total_pnl}
              prefix="$"
              icon={DollarSign}
              trend={globalStats.total_pnl >= 0 ? 'up' : 'down'}
              glowColor={globalStats.total_pnl >= 0 ? 'green' : 'red'}
            />
            <StatCard
              title="总交易"
              value={globalStats.total_trades}
              suffix=" 笔"
              icon={Activity}
              glowColor="yellow"
            />
            <StatCard
              title="平均胜率"
              value={globalStats.avg_win_rate}
              suffix="%"
              icon={Target}
              glowColor="cyan"
            />
            <StatCard
              title="活跃交易员"
              value={globalStats.active_traders}
              suffix=" 位"
              icon={Users}
              glowColor="purple"
            />
            <StatCard
              title="总净值"
              value={globalStats.total_equity}
              prefix="$"
              icon={Wallet}
              glowColor="yellow"
            />
          </div>

          {/* 主体区域 */}
          <div className="grid grid-cols-12 gap-6">
            {/* 左侧：排行榜 */}
            <div className="col-span-5">
              <TraderLeaderboard
                traders={traderStats}
                timeRange={timeRange}
                onSelectTrader={setSelectedTrader}
              />
            </div>

            {/* 右侧：交易员详情 */}
            <div className="col-span-7">
              <TraderDetailPanel trader={selectedTrader} />
            </div>
          </div>

          {/* 底部提示 */}
          <div className="text-center py-4">
            <p className="text-gray-500 text-sm">
              数据每 30 秒自动更新 · 仅展示公开交易员数据
            </p>
          </div>
        </div>
      </main>

      {/* 自定义动画样式 */}
      <style>{`
        @keyframes scan-line {
          0% { transform: translateY(-100%); opacity: 0; }
          50% { opacity: 1; }
          100% { transform: translateY(500%); opacity: 0; }
        }
        .animate-scan-line {
          animation: scan-line 3s ease-in-out infinite;
        }
        .custom-scrollbar::-webkit-scrollbar {
          width: 4px;
        }
        .custom-scrollbar::-webkit-scrollbar-track {
          background: transparent;
        }
        .custom-scrollbar::-webkit-scrollbar-thumb {
          background: rgba(255, 255, 255, 0.1);
          border-radius: 2px;
        }
        .custom-scrollbar::-webkit-scrollbar-thumb:hover {
          background: rgba(255, 255, 255, 0.2);
        }
      `}</style>
    </div>
  )
}

