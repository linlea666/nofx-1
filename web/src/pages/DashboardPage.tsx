import { useState, useEffect, useMemo, useRef } from 'react'
import useSWR from 'swr'
import {
  TrendingUp,
  Activity,
  Users,
  BarChart3,
  Zap,
  Trophy,
  Clock,
  RefreshCw,
  Target,
  DollarSign,
  Wallet,
  ArrowUpRight,
  ArrowDownRight,
  Sparkles,
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
  today_pnl: number
  week_pnl: number
  month_pnl: number
}

// 获取大屏交易员统计数据
const fetchDashboardTraders = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/dashboard/traders`)
  if (!res.ok) throw new Error('Failed to fetch dashboard traders')
  return res.json()
}

// 获取全局汇总数据
const fetchDashboardSummary = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/dashboard/summary`)
  if (!res.ok) throw new Error('Failed to fetch dashboard summary')
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

// 粒子背景动画
function ParticleBackground() {
  const canvasRef = useRef<HTMLCanvasElement>(null)

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return

    const ctx = canvas.getContext('2d')
    if (!ctx) return

    let animationId: number
    const particles: Array<{
      x: number
      y: number
      vx: number
      vy: number
      size: number
      opacity: number
    }> = []

    const resize = () => {
      canvas.width = window.innerWidth
      canvas.height = window.innerHeight
    }
    resize()
    window.addEventListener('resize', resize)

    // 创建粒子
    for (let i = 0; i < 80; i++) {
      particles.push({
        x: Math.random() * canvas.width,
        y: Math.random() * canvas.height,
        vx: (Math.random() - 0.5) * 0.3,
        vy: (Math.random() - 0.5) * 0.3,
        size: Math.random() * 2 + 0.5,
        opacity: Math.random() * 0.5 + 0.1,
      })
    }

    const animate = () => {
      ctx.clearRect(0, 0, canvas.width, canvas.height)

      particles.forEach((p, i) => {
        p.x += p.vx
        p.y += p.vy

        if (p.x < 0 || p.x > canvas.width) p.vx *= -1
        if (p.y < 0 || p.y > canvas.height) p.vy *= -1

        // 绘制粒子
        ctx.beginPath()
        ctx.arc(p.x, p.y, p.size, 0, Math.PI * 2)
        ctx.fillStyle = `rgba(0, 212, 255, ${p.opacity})`
        ctx.fill()

        // 连接线
        particles.slice(i + 1).forEach((p2) => {
          const dx = p.x - p2.x
          const dy = p.y - p2.y
          const dist = Math.sqrt(dx * dx + dy * dy)
          if (dist < 120) {
            ctx.beginPath()
            ctx.moveTo(p.x, p.y)
            ctx.lineTo(p2.x, p2.y)
            ctx.strokeStyle = `rgba(0, 212, 255, ${0.1 * (1 - dist / 120)})`
            ctx.stroke()
          }
        })
      })

      animationId = requestAnimationFrame(animate)
    }
    animate()

    return () => {
      cancelAnimationFrame(animationId)
      window.removeEventListener('resize', resize)
    }
  }, [])

  return (
    <canvas
      ref={canvasRef}
      className="fixed inset-0 pointer-events-none z-0"
      style={{ opacity: 0.6 }}
    />
  )
}

// 霓虹发光边框卡片
function NeonCard({
  children,
  className = '',
  glowColor = 'cyan',
  title,
  icon: Icon,
}: {
  children: React.ReactNode
  className?: string
  glowColor?: 'yellow' | 'green' | 'red' | 'cyan' | 'purple' | 'blue'
  title?: string
  icon?: React.ElementType
}) {
  const glowStyles = {
    yellow: 'border-[#FCD535]/30 shadow-[0_0_20px_rgba(252,213,53,0.2),inset_0_0_30px_rgba(252,213,53,0.05)]',
    green: 'border-[#0ECB81]/30 shadow-[0_0_20px_rgba(14,203,129,0.2),inset_0_0_30px_rgba(14,203,129,0.05)]',
    red: 'border-[#F6465D]/30 shadow-[0_0_20px_rgba(246,70,93,0.2),inset_0_0_30px_rgba(246,70,93,0.05)]',
    cyan: 'border-[#00d4ff]/30 shadow-[0_0_20px_rgba(0,212,255,0.2),inset_0_0_30px_rgba(0,212,255,0.05)]',
    purple: 'border-purple-500/30 shadow-[0_0_20px_rgba(168,85,247,0.2),inset_0_0_30px_rgba(168,85,247,0.05)]',
    blue: 'border-blue-500/30 shadow-[0_0_20px_rgba(59,130,246,0.2),inset_0_0_30px_rgba(59,130,246,0.05)]',
  }

  const iconColors = {
    yellow: 'text-[#FCD535]',
    green: 'text-[#0ECB81]',
    red: 'text-[#F6465D]',
    cyan: 'text-[#00d4ff]',
    purple: 'text-purple-400',
    blue: 'text-blue-400',
  }

  return (
    <div
      className={`
        relative overflow-hidden rounded-2xl border backdrop-blur-xl
        bg-gradient-to-br from-[#0d1421]/95 via-[#0a1628]/95 to-[#061220]/95
        transition-all duration-500 ease-out hover:scale-[1.01]
        ${glowStyles[glowColor]}
        ${className}
      `}
    >
      {/* 顶部光条 */}
      <div className="absolute top-0 left-0 right-0 h-[2px] bg-gradient-to-r from-transparent via-current to-transparent opacity-50"
        style={{ color: glowColor === 'yellow' ? '#FCD535' : glowColor === 'green' ? '#0ECB81' : glowColor === 'red' ? '#F6465D' : glowColor === 'cyan' ? '#00d4ff' : glowColor === 'purple' ? '#a855f7' : '#3b82f6' }}
      />
      
      {/* 角落装饰 */}
      <div className="absolute top-0 left-0 w-6 h-6 border-l-2 border-t-2 rounded-tl-2xl opacity-40"
        style={{ borderColor: glowColor === 'yellow' ? '#FCD535' : glowColor === 'green' ? '#0ECB81' : glowColor === 'red' ? '#F6465D' : glowColor === 'cyan' ? '#00d4ff' : glowColor === 'purple' ? '#a855f7' : '#3b82f6' }}
      />
      <div className="absolute top-0 right-0 w-6 h-6 border-r-2 border-t-2 rounded-tr-2xl opacity-40"
        style={{ borderColor: glowColor === 'yellow' ? '#FCD535' : glowColor === 'green' ? '#0ECB81' : glowColor === 'red' ? '#F6465D' : glowColor === 'cyan' ? '#00d4ff' : glowColor === 'purple' ? '#a855f7' : '#3b82f6' }}
      />
      <div className="absolute bottom-0 left-0 w-6 h-6 border-l-2 border-b-2 rounded-bl-2xl opacity-40"
        style={{ borderColor: glowColor === 'yellow' ? '#FCD535' : glowColor === 'green' ? '#0ECB81' : glowColor === 'red' ? '#F6465D' : glowColor === 'cyan' ? '#00d4ff' : glowColor === 'purple' ? '#a855f7' : '#3b82f6' }}
      />
      <div className="absolute bottom-0 right-0 w-6 h-6 border-r-2 border-b-2 rounded-br-2xl opacity-40"
        style={{ borderColor: glowColor === 'yellow' ? '#FCD535' : glowColor === 'green' ? '#0ECB81' : glowColor === 'red' ? '#F6465D' : glowColor === 'cyan' ? '#00d4ff' : glowColor === 'purple' ? '#a855f7' : '#3b82f6' }}
      />

      {/* 扫描线动画 */}
      <div className="absolute inset-0 overflow-hidden pointer-events-none">
        <div className="absolute top-0 left-0 w-full h-[1px] bg-gradient-to-r from-transparent via-white/30 to-transparent animate-scan-line" />
      </div>

      {/* 标题栏 */}
      {title && (
        <div className="px-5 py-4 border-b border-white/5 flex items-center gap-3">
          {Icon && <Icon className={iconColors[glowColor]} size={20} />}
          <h3 className="text-base font-bold text-white tracking-wide uppercase">{title}</h3>
          <div className="flex-1" />
          <div className="w-2 h-2 rounded-full bg-current animate-pulse" style={{ color: glowColor === 'yellow' ? '#FCD535' : glowColor === 'green' ? '#0ECB81' : glowColor === 'red' ? '#F6465D' : glowColor === 'cyan' ? '#00d4ff' : glowColor === 'purple' ? '#a855f7' : '#3b82f6' }} />
        </div>
      )}
      {children}
    </div>
  )
}

// 圆环进度图
function CircleProgress({
  value,
  max = 100,
  size = 120,
  strokeWidth = 8,
  color = '#00d4ff',
  label,
  sublabel,
}: {
  value: number
  max?: number
  size?: number
  strokeWidth?: number
  color?: string
  label?: string
  sublabel?: string
}) {
  const radius = (size - strokeWidth) / 2
  const circumference = radius * 2 * Math.PI
  const progress = Math.min(value / max, 1)
  const offset = circumference - progress * circumference

  return (
    <div className="relative inline-flex items-center justify-center">
      <svg width={size} height={size} className="-rotate-90">
        {/* 背景圆环 */}
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="rgba(255,255,255,0.1)"
          strokeWidth={strokeWidth}
        />
        {/* 进度圆环 */}
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke={color}
          strokeWidth={strokeWidth}
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={offset}
          className="transition-all duration-1000 ease-out"
          style={{
            filter: `drop-shadow(0 0 8px ${color})`,
          }}
        />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center">
        <span className="text-2xl font-bold text-white font-mono">
          {value.toFixed(1)}%
        </span>
        {label && <span className="text-xs text-gray-400 mt-1">{label}</span>}
        {sublabel && <span className="text-[10px] text-gray-500">{sublabel}</span>}
      </div>
    </div>
  )
}

// 迷你柱状图
function MiniBarChart({
  data,
  color = '#00d4ff',
  height = 60,
}: {
  data: number[]
  color?: string
  height?: number
}) {
  const max = Math.max(...data.map(Math.abs), 1)

  return (
    <div className="flex items-end gap-1" style={{ height }}>
      {data.map((value, i) => {
        const isPositive = value >= 0
        const barHeight = (Math.abs(value) / max) * height * 0.8
        return (
          <div
            key={i}
            className="flex-1 flex flex-col justify-end items-center relative group"
          >
            <div
              className="w-full rounded-t transition-all duration-300 group-hover:opacity-80"
              style={{
                height: barHeight,
                background: isPositive
                  ? `linear-gradient(180deg, ${color} 0%, ${color}50 100%)`
                  : `linear-gradient(180deg, #F6465D 0%, #F6465D50 100%)`,
                boxShadow: `0 0 10px ${isPositive ? color : '#F6465D'}40`,
              }}
            />
            <div className="absolute -top-6 left-1/2 -translate-x-1/2 opacity-0 group-hover:opacity-100 transition-opacity bg-black/90 px-2 py-1 rounded text-xs text-white whitespace-nowrap z-10">
              {value >= 0 ? '+' : ''}{value.toFixed(2)}
            </div>
          </div>
        )
      })}
    </div>
  )
}

// 迷你折线图
function MiniLineChart({
  data,
  color = '#00d4ff',
  height = 60,
  showArea = true,
}: {
  data: number[]
  color?: string
  height?: number
  showArea?: boolean
}) {
  if (data.length < 2) return null

  const max = Math.max(...data)
  const min = Math.min(...data)
  const range = max - min || 1
  const width = 100

  const points = data
    .map((v, i) => {
      const x = (i / (data.length - 1)) * width
      const y = height - ((v - min) / range) * (height - 10) - 5
      return `${x},${y}`
    })
    .join(' ')

  const areaPoints = `0,${height} ${points} ${width},${height}`

  return (
    <svg viewBox={`0 0 ${width} ${height}`} className="w-full" style={{ height }}>
      <defs>
        <linearGradient id={`lineGradient-${color.replace('#', '')}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity="0.5" />
          <stop offset="100%" stopColor={color} stopOpacity="0" />
        </linearGradient>
      </defs>
      {showArea && (
        <polygon
          fill={`url(#lineGradient-${color.replace('#', '')})`}
          points={areaPoints}
        />
      )}
      <polyline
        fill="none"
        stroke={color}
        strokeWidth="2"
        points={points}
        style={{ filter: `drop-shadow(0 0 4px ${color})` }}
      />
      {/* 最后一个点发光 */}
      <circle
        cx={(data.length - 1) / (data.length - 1) * width}
        cy={height - ((data[data.length - 1] - min) / range) * (height - 10) - 5}
        r="4"
        fill={color}
        className="animate-pulse"
        style={{ filter: `drop-shadow(0 0 6px ${color})` }}
      />
    </svg>
  )
}

// 大数字统计卡片
function BigStatCard({
  title,
  value,
  change,
  trend,
  icon: Icon,
  color,
  prefix = '',
  suffix = '',
}: {
  title: string
  value: number
  change?: number
  trend?: 'up' | 'down' | 'neutral'
  icon: React.ElementType
  color: string
  prefix?: string
  suffix?: string
}) {
  return (
    <div className="relative group">
      <div
        className="absolute inset-0 rounded-2xl opacity-20 blur-xl transition-opacity group-hover:opacity-30"
        style={{ background: color }}
      />
      <div className="relative bg-gradient-to-br from-[#0d1421]/90 to-[#061220]/90 rounded-2xl p-5 border border-white/10 backdrop-blur-xl">
        <div className="flex items-start justify-between mb-3">
          <div
            className="w-10 h-10 rounded-xl flex items-center justify-center"
            style={{ background: `${color}20`, border: `1px solid ${color}40` }}
          >
            <Icon size={20} style={{ color }} />
          </div>
          {change !== undefined && (
            <div
              className={`flex items-center gap-1 text-sm font-medium ${
                trend === 'up' ? 'text-[#0ECB81]' : trend === 'down' ? 'text-[#F6465D]' : 'text-gray-400'
              }`}
            >
              {trend === 'up' ? <ArrowUpRight size={16} /> : trend === 'down' ? <ArrowDownRight size={16} /> : null}
              {change >= 0 ? '+' : ''}{change.toFixed(2)}%
            </div>
          )}
        </div>
        <p className="text-gray-400 text-sm mb-2 uppercase tracking-wider">{title}</p>
        <p className="text-3xl font-bold text-white font-mono" style={{ textShadow: `0 0 20px ${color}40` }}>
          <AnimatedNumber value={value} prefix={prefix} suffix={suffix} />
        </p>
      </div>
    </div>
  )
}

// 交易员排行榜
function TraderLeaderboard({
  traders,
  timeRange,
  onSelectTrader,
  selectedTraderId,
}: {
  traders: TraderStats[]
  timeRange: TimeRange
  onSelectTrader: (trader: TraderStats) => void
  selectedTraderId?: string
}) {
  const getPnL = (trader: TraderStats) => {
    switch (timeRange) {
      case 'today': return trader.today_pnl
      case 'week': return trader.week_pnl
      case 'month': return trader.month_pnl
      default: return trader.total_pnl
    }
  }

  const sortedTraders = [...traders].sort((a, b) => getPnL(b) - getPnL(a))

  const getRankStyle = (index: number) => {
    if (index === 0) return { bg: 'linear-gradient(135deg, #FFD700 0%, #FFA500 100%)', text: 'text-black', glow: '#FFD700' }
    if (index === 1) return { bg: 'linear-gradient(135deg, #C0C0C0 0%, #A0A0A0 100%)', text: 'text-black', glow: '#C0C0C0' }
    if (index === 2) return { bg: 'linear-gradient(135deg, #CD7F32 0%, #8B4513 100%)', text: 'text-white', glow: '#CD7F32' }
    return { bg: 'rgba(255,255,255,0.1)', text: 'text-gray-400', glow: 'transparent' }
  }

  return (
    <NeonCard glowColor="cyan" title="交易员排行榜" icon={Trophy} className="h-full">
      <div className="overflow-y-auto max-h-[450px] custom-scrollbar">
        {sortedTraders.map((trader, index) => {
          const pnl = getPnL(trader)
          const isPositive = pnl >= 0
          const rankStyle = getRankStyle(index)
          const isSelected = trader.trader_id === selectedTraderId

          return (
            <div
              key={trader.trader_id}
              onClick={() => onSelectTrader(trader)}
              className={`
                flex items-center gap-4 px-5 py-4 border-b border-white/5
                cursor-pointer transition-all duration-300
                ${isSelected ? 'bg-[#00d4ff]/10' : 'hover:bg-white/5'}
                ${index < 3 ? 'relative' : ''}
              `}
            >
              {/* 前三名发光效果 */}
              {index < 3 && (
                <div
                  className="absolute inset-0 opacity-10"
                  style={{
                    background: `linear-gradient(90deg, ${rankStyle.glow}40 0%, transparent 50%)`,
                  }}
                />
              )}

              {/* 排名 */}
              <div
                className={`relative w-9 h-9 rounded-lg flex items-center justify-center font-bold text-sm ${rankStyle.text}`}
                style={{
                  background: rankStyle.bg,
                  boxShadow: index < 3 ? `0 0 15px ${rankStyle.glow}60` : 'none',
                }}
              >
                {index + 1}
              </div>

              {/* 头像和名称 */}
              <div className="flex items-center gap-3 flex-1 min-w-0">
                <div className="relative">
                  <PunkAvatar
                    seed={getTraderAvatar(trader.trader_id, trader.trader_name)}
                    size={40}
                    className="rounded-xl ring-2 ring-white/10"
                  />
                  {trader.position_count > 0 && (
                    <div className="absolute -bottom-1 -right-1 w-4 h-4 bg-[#0ECB81] rounded-full flex items-center justify-center">
                      <Activity size={10} className="text-white" />
                    </div>
                  )}
                </div>
                <div className="min-w-0">
                  <p className="font-medium text-white truncate">{trader.trader_name}</p>
                  <div className="flex items-center gap-2 text-xs text-gray-500">
                    <span className={`px-1.5 py-0.5 rounded text-[10px] ${trader.mode === 'copy_trade' ? 'bg-blue-500/20 text-blue-400' : 'bg-purple-500/20 text-purple-400'}`}>
                      {trader.mode === 'copy_trade' ? '跟单' : 'AI'}
                    </span>
                    <span>{trader.total_trades} 笔</span>
                  </div>
                </div>
              </div>

              {/* 盈亏 */}
              <div className="text-right">
                <p className={`font-mono font-bold text-lg ${isPositive ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}
                  style={{ textShadow: `0 0 10px ${isPositive ? '#0ECB8140' : '#F6465D40'}` }}>
                  {formatMoney(pnl)}
                </p>
                <div className="flex items-center justify-end gap-2 text-xs">
                  <span className={`${trader.win_rate >= 50 ? 'text-[#0ECB81]' : 'text-gray-500'}`}>
                    {trader.win_rate.toFixed(1)}% 胜率
                  </span>
                </div>
              </div>
            </div>
          )
        })}
        {sortedTraders.length === 0 && (
          <div className="p-8 text-center text-gray-500">
            <Sparkles size={32} className="mx-auto mb-3 opacity-30" />
            <p>暂无交易员数据</p>
          </div>
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
          <Target size={64} className="mx-auto mb-4 opacity-20" />
          <p className="text-lg">点击左侧排行榜</p>
          <p className="text-sm mt-1">查看交易员详细数据</p>
        </div>
      </NeonCard>
    )
  }

  const isPositive = trader.total_pnl >= 0

  // 模拟7天盈亏数据
  const weekData = [
    trader.total_pnl * 0.1,
    trader.total_pnl * 0.15,
    trader.total_pnl * -0.05,
    trader.total_pnl * 0.2,
    trader.total_pnl * 0.08,
    trader.total_pnl * 0.25,
    trader.total_pnl * 0.3,
  ]

  return (
    <NeonCard glowColor="purple" className="h-full">
      {/* 头部 */}
      <div className="p-5 border-b border-white/5">
        <div className="flex items-center gap-4">
          <div className="relative">
            <PunkAvatar
              seed={getTraderAvatar(trader.trader_id, trader.trader_name)}
              size={56}
              className="rounded-2xl ring-2 ring-purple-500/30"
            />
            <div className="absolute -bottom-1 -right-1 w-5 h-5 bg-gradient-to-br from-purple-500 to-pink-500 rounded-lg flex items-center justify-center">
              <Zap size={12} className="text-white" />
            </div>
          </div>
          <div className="flex-1">
            <h3 className="text-xl font-bold text-white">{trader.trader_name}</h3>
            <div className="flex items-center gap-2 mt-1">
              <span className={`px-2 py-0.5 rounded text-xs ${trader.mode === 'copy_trade' ? 'bg-blue-500/20 text-blue-400 border border-blue-500/30' : 'bg-purple-500/20 text-purple-400 border border-purple-500/30'}`}>
                {trader.mode === 'copy_trade' ? '跟单模式' : 'AI 模式'}
              </span>
              {trader.position_count > 0 && (
                <span className="px-2 py-0.5 rounded text-xs bg-[#0ECB81]/20 text-[#0ECB81] border border-[#0ECB81]/30">
                  交易中
                </span>
              )}
            </div>
          </div>
          <div className="text-right">
            <p className={`text-2xl font-bold font-mono ${isPositive ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}
              style={{ textShadow: `0 0 15px ${isPositive ? '#0ECB8140' : '#F6465D40'}` }}>
              {formatMoney(trader.total_pnl)}
            </p>
            <p className="text-xs text-gray-500 mt-1">总盈亏</p>
          </div>
        </div>
      </div>

      <div className="p-5 space-y-6">
        {/* 盈亏统计 */}
        <div className="grid grid-cols-4 gap-3">
          {[
            { label: '今日', value: trader.today_pnl },
            { label: '本周', value: trader.week_pnl },
            { label: '本月', value: trader.month_pnl },
            { label: '全部', value: trader.total_pnl },
          ].map(({ label, value }) => (
            <div key={label} className="text-center p-3 rounded-xl bg-white/5">
              <p className="text-xs text-gray-500 uppercase mb-1">{label}</p>
              <p className={`text-sm font-mono font-bold ${value >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
                {formatMoney(value)}
              </p>
            </div>
          ))}
        </div>

        {/* 7日盈亏趋势 */}
        <div className="bg-white/5 rounded-xl p-4">
          <div className="flex items-center justify-between mb-3">
            <h4 className="text-sm font-medium text-gray-400 uppercase tracking-wider">7日盈亏趋势</h4>
            <span className={`text-xs ${isPositive ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {isPositive ? '↗ 上升' : '↘ 下降'}
            </span>
          </div>
          <MiniLineChart data={weekData} color={isPositive ? '#0ECB81' : '#F6465D'} height={80} />
        </div>

        {/* 核心指标 */}
        <div className="grid grid-cols-2 gap-4">
          <div className="flex flex-col items-center">
            <CircleProgress
              value={trader.win_rate}
              color={trader.win_rate >= 50 ? '#0ECB81' : '#F6465D'}
              size={100}
              label="胜率"
            />
          </div>
          <div className="space-y-3">
            {[
              { label: '盈亏比', value: trader.profit_factor.toFixed(2), icon: BarChart3, color: '#00d4ff' },
              { label: '交易次数', value: trader.total_trades.toString(), icon: Activity, color: '#a855f7' },
              { label: '收益率', value: formatPercent(trader.return_rate), icon: TrendingUp, color: trader.return_rate >= 0 ? '#0ECB81' : '#F6465D' },
            ].map(({ label, value, icon: ItemIcon, color }) => (
              <div key={label} className="flex items-center gap-3 bg-white/5 rounded-lg p-3">
                <div className="w-8 h-8 rounded-lg flex items-center justify-center" style={{ background: `${color}20` }}>
                  <ItemIcon size={16} style={{ color }} />
                </div>
                <div className="flex-1">
                  <p className="text-xs text-gray-500">{label}</p>
                  <p className="text-sm font-bold text-white">{value}</p>
                </div>
              </div>
            ))}
          </div>
        </div>

        {/* 账户状态 */}
        <div className="bg-gradient-to-r from-purple-500/10 to-blue-500/10 rounded-xl p-4 border border-purple-500/20">
          <div className="flex items-center gap-2 mb-3">
            <Wallet size={16} className="text-purple-400" />
            <h4 className="text-sm font-medium text-gray-300">账户概览</h4>
          </div>
          <div className="grid grid-cols-3 gap-4">
            <div>
              <p className="text-xs text-gray-500 mb-1">当前净值</p>
              <p className="text-lg font-bold text-white font-mono">${trader.current_equity.toLocaleString()}</p>
            </div>
            <div>
              <p className="text-xs text-gray-500 mb-1">初始资金</p>
              <p className="text-lg font-bold text-gray-300 font-mono">${trader.initial_balance.toLocaleString()}</p>
            </div>
            <div>
              <p className="text-xs text-gray-500 mb-1">持仓数</p>
              <p className="text-lg font-bold text-white">{trader.position_count} 个</p>
            </div>
          </div>
        </div>
      </div>
    </NeonCard>
  )
}

// 全局盈亏分布图
function GlobalPnLChart({ traders, timeRange }: { traders: TraderStats[]; timeRange: TimeRange }) {
  const getPnL = (trader: TraderStats) => {
    switch (timeRange) {
      case 'today': return trader.today_pnl
      case 'week': return trader.week_pnl
      case 'month': return trader.month_pnl
      default: return trader.total_pnl
    }
  }

  const pnlData = traders.map(t => getPnL(t)).slice(0, 12)

  return (
    <NeonCard glowColor="blue" title="交易员盈亏分布" icon={BarChart3}>
      <div className="p-5">
        <MiniBarChart data={pnlData} color="#3b82f6" height={100} />
        <div className="flex justify-between mt-3 text-xs text-gray-500">
          <span>交易员排名 1-{Math.min(traders.length, 12)}</span>
          <div className="flex items-center gap-3">
            <span className="flex items-center gap-1">
              <div className="w-2 h-2 rounded-full bg-[#3b82f6]" />
              盈利
            </span>
            <span className="flex items-center gap-1">
              <div className="w-2 h-2 rounded-full bg-[#F6465D]" />
              亏损
            </span>
          </div>
        </div>
      </div>
    </NeonCard>
  )
}

// 实时数据面板
function RealtimePanel({ stats }: { stats: GlobalStats }) {
  return (
    <NeonCard glowColor="green" title="实时数据" icon={Activity}>
      <div className="p-5 grid grid-cols-2 gap-4">
        <div className="text-center">
          <p className="text-3xl font-bold text-[#0ECB81] font-mono">
            {stats.active_traders}
          </p>
          <p className="text-xs text-gray-500 mt-1">活跃交易员</p>
        </div>
        <div className="text-center">
          <p className="text-3xl font-bold text-[#00d4ff] font-mono">
            {stats.total_trades}
          </p>
          <p className="text-xs text-gray-500 mt-1">总交易笔数</p>
        </div>
      </div>
      <div className="px-5 pb-5">
        <div className="bg-white/5 rounded-lg p-3">
          <div className="flex items-center justify-between mb-2">
            <span className="text-xs text-gray-500">今日盈亏</span>
            <span className={`text-sm font-mono font-bold ${stats.today_pnl >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {formatMoney(stats.today_pnl)}
            </span>
          </div>
          <div className="flex items-center justify-between">
            <span className="text-xs text-gray-500">本周盈亏</span>
            <span className={`text-sm font-mono font-bold ${stats.week_pnl >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {formatMoney(stats.week_pnl)}
            </span>
          </div>
        </div>
      </div>
    </NeonCard>
  )
}

// 主大屏页面
export function DashboardPage() {
  const [timeRange, setTimeRange] = useState<TimeRange>('month')
  const [selectedTrader, setSelectedTrader] = useState<TraderStats | null>(null)
  const [currentTime, setCurrentTime] = useState(new Date())

  // 获取大屏交易员数据 (使用新的 Dashboard API)
  const { data: dashboardTraders, isLoading, error } = useSWR('dashboard-traders', fetchDashboardTraders, {
    refreshInterval: 30000,
    onError: (err) => console.error('Dashboard API error:', err),
  })

  // 获取全局汇总数据
  const { data: summaryData } = useSWR('dashboard-summary', fetchDashboardSummary, {
    refreshInterval: 30000,
  })

  // 更新时间
  useEffect(() => {
    const timer = setInterval(() => setCurrentTime(new Date()), 1000)
    return () => clearInterval(timer)
  }, [])

  // 交易员统计数据 (直接使用后端返回的数据)
  const traderStats: TraderStats[] = useMemo(() => {
    if (!dashboardTraders || !Array.isArray(dashboardTraders)) return []
    return dashboardTraders.map((t: any) => ({
      trader_id: t.trader_id || '',
      trader_name: t.trader_name || t.trader_id?.substring(0, 8) || '未知',
      mode: t.mode || 'ai',
      today_pnl: t.today_pnl || 0,
      week_pnl: t.week_pnl || 0,
      month_pnl: t.month_pnl || 0,
      total_pnl: t.total_pnl || 0,
      total_trades: t.total_trades || 0,
      win_rate: t.win_rate || 0,
      profit_factor: t.profit_factor || 0,
      current_equity: t.current_equity || 0,
      initial_balance: t.initial_balance || 0,
      return_rate: t.return_rate || 0,
      position_count: t.position_count || 0,
    }))
  }, [dashboardTraders])

  // 全局统计 (优先使用后端汇总数据)
  const globalStats: GlobalStats = useMemo(() => {
    if (summaryData) {
      return {
        total_pnl: summaryData.total_pnl || 0,
        total_trades: summaryData.total_trades || 0,
        avg_win_rate: summaryData.avg_win_rate || 0,
        active_traders: summaryData.active_traders || 0,
        total_equity: summaryData.total_equity || 0,
        today_pnl: summaryData.today_pnl || 0,
        week_pnl: summaryData.week_pnl || 0,
        month_pnl: summaryData.month_pnl || 0,
      }
    }
    // 回退：从交易员数据计算
    return {
      total_pnl: traderStats.reduce((sum, t) => sum + t.total_pnl, 0),
      total_trades: traderStats.reduce((sum, t) => sum + t.total_trades, 0),
      avg_win_rate: traderStats.length > 0
        ? traderStats.reduce((sum, t) => sum + t.win_rate, 0) / traderStats.length
        : 0,
      active_traders: traderStats.filter(t => t.position_count > 0).length,
      total_equity: traderStats.reduce((sum, t) => sum + t.current_equity, 0),
      today_pnl: traderStats.reduce((sum, t) => sum + t.today_pnl, 0),
      week_pnl: traderStats.reduce((sum, t) => sum + t.week_pnl, 0),
      month_pnl: traderStats.reduce((sum, t) => sum + t.month_pnl, 0),
    }
  }, [summaryData, traderStats])

  const timeRangeLabels: Record<TimeRange, string> = {
    today: '今日',
    week: '本周',
    month: '本月',
    all: '全部',
  }

  // 加载状态
  if (isLoading && !dashboardTraders) {
    return (
      <div className="min-h-screen bg-[#030712] flex items-center justify-center">
        <div className="text-center">
          <div className="w-16 h-16 border-4 border-[#00d4ff]/30 border-t-[#00d4ff] rounded-full animate-spin mx-auto mb-4" />
          <p className="text-gray-400">正在加载数据...</p>
        </div>
      </div>
    )
  }

  // 错误状态
  if (error) {
    return (
      <div className="min-h-screen bg-[#030712] flex items-center justify-center">
        <div className="text-center">
          <div className="w-16 h-16 rounded-full bg-[#F6465D]/20 flex items-center justify-center mx-auto mb-4">
            <span className="text-3xl">⚠️</span>
          </div>
          <p className="text-gray-400 mb-2">加载数据失败</p>
          <p className="text-gray-600 text-sm">{error.message}</p>
          <button 
            onClick={() => window.location.reload()}
            className="mt-4 px-4 py-2 bg-[#00d4ff]/20 text-[#00d4ff] rounded-lg hover:bg-[#00d4ff]/30 transition-colors"
          >
            重新加载
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-[#030712] relative overflow-hidden">
      {/* 粒子背景 */}
      <ParticleBackground />

      {/* 渐变背景 */}
      <div className="fixed inset-0 pointer-events-none">
        <div className="absolute top-0 left-0 w-full h-1/2 bg-gradient-to-b from-[#0a1628]/80 to-transparent" />
        <div className="absolute bottom-0 left-0 w-full h-1/2 bg-gradient-to-t from-[#030712] to-transparent" />
        <div className="absolute top-1/4 left-1/4 w-[600px] h-[600px] bg-blue-500/5 rounded-full blur-[150px]" />
        <div className="absolute bottom-1/4 right-1/4 w-[500px] h-[500px] bg-purple-500/5 rounded-full blur-[150px]" />
        <div className="absolute top-1/2 left-0 w-[400px] h-[400px] bg-cyan-500/5 rounded-full blur-[100px]" />
      </div>

      {/* 顶部标题栏 */}
      <header className="relative z-10 px-6 py-4 border-b border-white/5 backdrop-blur-xl bg-[#030712]/80">
        <div className="max-w-[1920px] mx-auto flex items-center justify-between">
          <div className="flex items-center gap-4">
            <div className="relative">
              <div className="w-12 h-12 rounded-2xl bg-gradient-to-br from-[#00d4ff] via-[#0066ff] to-[#9333ea] flex items-center justify-center shadow-[0_0_30px_rgba(0,212,255,0.4)]">
                <Zap className="text-white" size={28} />
              </div>
              <div className="absolute -bottom-1 -right-1 w-4 h-4 bg-[#0ECB81] rounded-full border-2 border-[#030712] animate-pulse" />
            </div>
            <div>
              <h1 className="text-2xl font-bold text-white tracking-tight bg-gradient-to-r from-white via-white to-gray-400 bg-clip-text">
                NOFX 交易数据中心
              </h1>
              <p className="text-xs text-gray-500 tracking-widest uppercase">Trading Analytics Dashboard v2.0</p>
            </div>
          </div>

          <div className="flex items-center gap-6">
            {/* 时间筛选 */}
            <div className="flex items-center gap-1 bg-white/5 rounded-xl p-1 backdrop-blur-sm border border-white/10">
              {(['today', 'week', 'month', 'all'] as TimeRange[]).map((range) => (
                <button
                  key={range}
                  onClick={() => setTimeRange(range)}
                  className={`
                    px-5 py-2.5 rounded-lg text-sm font-medium transition-all duration-300
                    ${timeRange === range
                      ? 'bg-gradient-to-r from-[#00d4ff] to-[#0066ff] text-white shadow-[0_0_20px_rgba(0,212,255,0.3)]'
                      : 'text-gray-400 hover:text-white hover:bg-white/5'
                    }
                  `}
                >
                  {timeRangeLabels[range]}
                </button>
              ))}
            </div>

            {/* 实时时间 */}
            <div className="flex items-center gap-3 bg-white/5 rounded-xl px-4 py-2.5 border border-white/10">
              <Clock size={18} className="text-[#00d4ff]" />
              <div className="text-right">
                <p className="font-mono text-lg text-white font-bold">
                  {currentTime.toLocaleTimeString('zh-CN', { hour12: false })}
                </p>
                <p className="text-[10px] text-gray-500 uppercase tracking-wider">
                  {currentTime.toLocaleDateString('zh-CN', { month: 'short', day: 'numeric', weekday: 'short' })}
                </p>
              </div>
            </div>

            {/* 刷新状态 */}
            <div className="flex items-center gap-2 bg-[#0ECB81]/10 rounded-xl px-4 py-2.5 border border-[#0ECB81]/20">
              <RefreshCw size={16} className={`text-[#0ECB81] ${isLoading ? 'animate-spin' : ''}`} />
              <span className="text-sm text-[#0ECB81] font-medium">实时同步</span>
            </div>
          </div>
        </div>
      </header>

      {/* 主内容区 */}
      <main className="relative z-10 p-6">
        <div className="max-w-[1920px] mx-auto space-y-6">
          {/* 顶部统计卡片 */}
          <div className="grid grid-cols-5 gap-4">
            <BigStatCard
              title="总盈亏"
              value={globalStats.total_pnl}
              prefix="$"
              icon={DollarSign}
              trend={globalStats.total_pnl >= 0 ? 'up' : 'down'}
              change={12.5}
              color={globalStats.total_pnl >= 0 ? '#0ECB81' : '#F6465D'}
            />
            <BigStatCard
              title="总交易"
              value={globalStats.total_trades}
              suffix=" 笔"
              icon={Activity}
              color="#00d4ff"
            />
            <BigStatCard
              title="平均胜率"
              value={globalStats.avg_win_rate}
              suffix="%"
              icon={Target}
              color="#a855f7"
            />
            <BigStatCard
              title="活跃交易员"
              value={globalStats.active_traders}
              suffix=" 位"
              icon={Users}
              color="#3b82f6"
            />
            <BigStatCard
              title="总净值"
              value={globalStats.total_equity}
              prefix="$"
              icon={Wallet}
              change={5.2}
              trend="up"
              color="#FCD535"
            />
          </div>

          {/* 主体区域 - 三栏布局 */}
          <div className="grid grid-cols-12 gap-6">
            {/* 左侧：排行榜 */}
            <div className="col-span-4">
              <TraderLeaderboard
                traders={traderStats}
                timeRange={timeRange}
                onSelectTrader={setSelectedTrader}
                selectedTraderId={selectedTrader?.trader_id}
              />
            </div>

            {/* 中间：交易员详情 */}
            <div className="col-span-5">
              <TraderDetailPanel trader={selectedTrader} />
            </div>

            {/* 右侧：实时数据和图表 */}
            <div className="col-span-3 space-y-6">
              <RealtimePanel stats={globalStats} />
              <GlobalPnLChart traders={traderStats} timeRange={timeRange} />
            </div>
          </div>

          {/* 底部提示 */}
          <div className="text-center py-4">
            <div className="inline-flex items-center gap-3 bg-white/5 rounded-xl px-6 py-3 border border-white/10">
              <div className="w-2 h-2 rounded-full bg-[#0ECB81] animate-pulse" />
              <p className="text-gray-400 text-sm">
                数据每 30 秒自动更新 · 仅展示公开交易员数据 · Powered by NOFX Trading System
              </p>
            </div>
          </div>
        </div>
      </main>

      {/* 自定义动画样式 */}
      <style>{`
        @keyframes scan-line {
          0% { transform: translateY(-100%); opacity: 0; }
          50% { opacity: 1; }
          100% { transform: translateY(1000%); opacity: 0; }
        }
        .animate-scan-line {
          animation: scan-line 4s ease-in-out infinite;
        }
        .custom-scrollbar::-webkit-scrollbar {
          width: 4px;
        }
        .custom-scrollbar::-webkit-scrollbar-track {
          background: transparent;
        }
        .custom-scrollbar::-webkit-scrollbar-thumb {
          background: rgba(0, 212, 255, 0.3);
          border-radius: 2px;
        }
        .custom-scrollbar::-webkit-scrollbar-thumb:hover {
          background: rgba(0, 212, 255, 0.5);
        }
        @keyframes float {
          0%, 100% { transform: translateY(0); }
          50% { transform: translateY(-10px); }
        }
        .animate-float {
          animation: float 3s ease-in-out infinite;
        }
      `}</style>
    </div>
  )
}
