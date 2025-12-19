import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import {
  TrendingUp,
  Activity,
  Users,
  BarChart3,
  Zap,
  Target,
  Shield,
  Cpu,
  Globe,
  Database,
  Terminal as TerminalIcon,
  Maximize2,
  Crosshair,
} from 'lucide-react'
import { PunkAvatar, getTraderAvatar } from '../components/PunkAvatar'

// --- Types ---
type TimeRange = 'today' | 'week' | 'month' | 'all'

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

// --- API Helpers ---
const fetchDashboardTraders = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/dashboard/traders`)
  if (!res.ok) throw new Error('Network failure: dashboard traders')
  return res.json()
}

const fetchDashboardSummary = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/dashboard/summary`)
  if (!res.ok) throw new Error('Network failure: dashboard summary')
  return res.json()
}

// --- UI Utilities ---
function formatMoney(value: number, showSign = true): string {
  const sign = value >= 0 ? '+' : ''
  const formatted = Math.abs(value).toLocaleString('en-US', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })
  return showSign ? `${sign}$${formatted}` : `$${formatted}`
}

// --- HUD Components ---

/**
 * Cyber HUD Card Component
 */
function CyberCard({
  children,
  className = '',
  title,
  icon: Icon,
  color = 'cyan',
}: {
  children: React.ReactNode
  className?: string
  title?: string
  icon?: React.ElementType
  color?: 'cyan' | 'purple' | 'green' | 'red' | 'yellow'
}) {
  const hexMap = {
    cyan: '#00f2ff',
    purple: '#bc00ff',
    green: '#00ff9d',
    red: '#ff0055',
    yellow: '#ffe600',
  }

  return (
    <div
      className={`
        relative p-1 transition-all duration-300 group
        ${className}
      `}
    >
      {/* HUD Background with slight transparency */}
      <div className="absolute inset-0 bg-slate-950/80 backdrop-blur-md overflow-hidden rounded-sm border border-white/5" />

      {/* Cyber Corners */}
      <div className="absolute top-0 left-0 w-4 h-4 border-t-2 border-l-2 opacity-80 pointer-events-none transition-all group-hover:scale-110" style={{ borderColor: hexMap[color] }} />
      <div className="absolute top-0 right-0 w-4 h-4 border-t-2 border-r-2 opacity-80 pointer-events-none transition-all group-hover:scale-110" style={{ borderColor: hexMap[color] }} />
      <div className="absolute bottom-0 left-0 w-4 h-4 border-b-2 border-l-2 opacity-80 pointer-events-none transition-all group-hover:scale-110" style={{ borderColor: hexMap[color] }} />
      <div className="absolute bottom-0 right-0 w-4 h-4 border-b-2 border-r-2 opacity-80 pointer-events-none transition-all group-hover:scale-110" style={{ borderColor: hexMap[color] }} />

      {/* Internal Content */}
      <div className="relative z-10 p-4">
        {title && (
          <div className="flex items-center gap-2 mb-4 border-b border-white/5 pb-2">
            {Icon && <Icon size={16} className={`opacity-80`} style={{ color: hexMap[color] }} />}
            <span className="text-[10px] uppercase tracking-[0.2em] font-black text-white/70">
              {title}
            </span>
            <div className="flex-1" />
            <div className="w-1.5 h-1.5 rounded-full animate-pulse" style={{ backgroundColor: hexMap[color] }} />
          </div>
        )}
        {children}
      </div>

      {/* Scanning Line Animation */}
      <div className="absolute inset-0 pointer-events-none overflow-hidden opacity-10">
        <div className="absolute top-0 left-0 w-full h-px bg-white animate-cyber-scan" />
      </div>
    </div>
  )
}

/**
 * HUD Matrix Number
 */
function MatrixNumber({
  value,
  prefix = '',
  suffix = '',
  decimals = 2,
  className = '',
  color = 'white',
}: {
  value: number
  prefix?: string
  suffix?: string
  decimals?: number
  className?: string
  color?: string
}) {
  const [displayValue, setDisplayValue] = useState(0)

  useEffect(() => {
    let start = 0
    const end = value
    const duration = 800
    const startTime = performance.now()

    const animate = (now: number) => {
      const elapsed = now - startTime
      const progress = Math.min(elapsed / duration, 1)
      const current = start + progress * (end - start)
      setDisplayValue(current)
      if (progress < 1) requestAnimationFrame(animate)
    }
    requestAnimationFrame(animate)
  }, [value])

  return (
    <span className={`font-mono ${className}`} style={{ color }}>
      {prefix}
      {displayValue.toLocaleString('en-US', {
        minimumFractionDigits: decimals,
        maximumFractionDigits: decimals,
      })}
      {suffix}
    </span>
  )
}

/**
 * Cyber Sparkline
 */
function CyberSparkline({ data, color, height = 40 }: { data: number[]; color: string; height?: number }) {
  if (!data || data.length < 2) return null
  const min = Math.min(...data)
  const max = Math.max(...data)
  const range = max - min || 1
  const points = data
    .map((v, i) => {
      const x = (i / (data.length - 1)) * 100
      const y = height - ((v - min) / range) * (height - 4) - 2
      return `${x},${y}`
    })
    .join(' ')

  return (
    <svg viewBox={`0 0 100 ${height}`} className="w-full h-full overflow-visible">
      <polyline
        fill="none"
        stroke={color}
        strokeWidth="1.5"
        points={points}
        className="drop-shadow-[0_0_2px_rgba(255,255,255,0.5)]"
      />
    </svg>
  )
}

/**
 * Grid Background Component
 */
function CyberGrid() {
  return (
    <div className="fixed inset-0 pointer-events-none z-0 opacity-20">
      <div
        className="absolute inset-0"
        style={{
          backgroundImage: `
            linear-gradient(to right, #ffffff05 1px, transparent 1px),
            linear-gradient(to bottom, #ffffff05 1px, transparent 1px)
          `,
          backgroundSize: '40px 40px',
        }}
      />
      <div className="absolute inset-0 bg-gradient-to-t from-slate-950 via-transparent to-slate-950" />
    </div>
  )
}

// --- Page Content ---

export function DashboardPage() {
  const [timeRange, setTimeRange] = useState<TimeRange>('month')
  const [selectedTrader, setSelectedTrader] = useState<TraderStats | null>(null)
  const [currentTime, setCurrentTime] = useState(new Date())

  // --- Data Fetching ---
  const { data: dashboardTraders, isLoading } = useSWR('dashboard-traders-cyber', fetchDashboardTraders, {
    refreshInterval: 30000,
  })

  const { data: summaryData } = useSWR('dashboard-summary-cyber', fetchDashboardSummary, {
    refreshInterval: 30000,
  })

  useEffect(() => {
    const timer = setInterval(() => setCurrentTime(new Date()), 10)
    return () => clearInterval(timer)
  }, [])

  const traderStats: TraderStats[] = useMemo(() => {
    if (!dashboardTraders || !Array.isArray(dashboardTraders)) return []
    return dashboardTraders.map((t: any) => ({
      trader_id: t.trader_id || '',
      trader_name: t.trader_name || t.trader_id?.substring(0, 8) || 'UNIT-01',
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

  const globalStats: GlobalStats = useMemo(() => {
    if (summaryData) return summaryData
    return {
      total_pnl: traderStats.reduce((sum, t) => sum + t.total_pnl, 0),
      total_trades: traderStats.reduce((sum, t) => sum + t.total_trades, 0),
      avg_win_rate: traderStats.length > 0 ? traderStats.reduce((sum, t) => sum + t.win_rate, 0) / traderStats.length : 0,
      active_traders: traderStats.filter(t => t.position_count > 0).length,
      total_equity: traderStats.reduce((sum, t) => sum + t.current_equity, 0),
      today_pnl: traderStats.reduce((sum, t) => sum + t.today_pnl, 0),
      week_pnl: traderStats.reduce((sum, t) => sum + t.week_pnl, 0),
      month_pnl: traderStats.reduce((sum, t) => sum + t.month_pnl, 0),
    }
  }, [summaryData, traderStats])

  const sortedTraders = useMemo(() => {
    return [...traderStats].sort((a, b) => b.total_pnl - a.total_pnl)
  }, [traderStats])

  useEffect(() => {
    if (sortedTraders.length > 0 && !selectedTrader) {
      setSelectedTrader(sortedTraders[0])
    }
  }, [sortedTraders])

  if (isLoading && !dashboardTraders) {
    return (
      <div className="min-h-screen bg-slate-950 flex flex-col items-center justify-center font-mono">
        <div className="w-48 h-1 bg-white/10 relative overflow-hidden mb-4">
          <div className="absolute top-0 left-0 h-full bg-[#00f2ff] animate-cyber-progress" />
        </div>
        <div className="text-[#00f2ff] text-[10px] tracking-[0.4em] uppercase animate-pulse">
          Initializing Command Center...
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen bg-slate-950 text-white font-sans selection:bg-[#00f2ff]/30 cursor-crosshair overflow-hidden flex flex-col">
      <CyberGrid />

      {/* --- HUD HEADER --- */}
      <header className="relative z-20 border-b border-white/10 bg-slate-950/40 backdrop-blur-xl px-6 py-2 flex items-center justify-between">
        <div className="flex items-center gap-6">
          <div className="flex items-center gap-3">
            <div className="w-8 h-8 rounded-sm bg-gradient-to-br from-[#00f2ff] to-[#bc00ff] flex items-center justify-center rotate-45 group">
              <Zap size={18} className="text-white -rotate-45 group-hover:scale-125 transition-transform" />
            </div>
            <div className="flex flex-col">
              <span className="text-xs font-black tracking-widest text-white/90">NOFX COMMAND</span>
              <span className="text-[8px] text-[#00f2ff] tracking-[0.2em] -mt-1 uppercase opacity-60">Operations Unit</span>
            </div>
          </div>

          <nav className="flex items-center gap-1 bg-white/5 rounded-sm p-0.5 border border-white/5">
            {(['today', 'week', 'month', 'all'] as TimeRange[]).map((r) => (
              <button
                key={r}
                onClick={() => setTimeRange(r)}
                className={`
                  px-4 py-1 text-[9px] uppercase tracking-wider font-bold transition-all
                  ${timeRange === r ? 'bg-[#00f2ff] text-slate-950 shadow-[0_0_15px_rgba(0,242,255,0.4)]' : 'text-white/40 hover:text-white/70'}
                `}
              >
                {r}
              </button>
            ))}
          </nav>
        </div>

        <div className="flex items-center gap-10">
          <div className="flex items-center gap-4 text-white/40">
            <div className="flex flex-col items-end">
              <span className="text-[9px] uppercase tracking-tighter">System Health</span>
              <div className="flex gap-0.5 mt-0.5">
                {[...Array(6)].map((_, i) => (
                  <div key={i} className={`w-2 h-0.5 rounded-full ${i < 5 ? 'bg-[#00ff9d]' : 'bg-white/10'}`} />
                ))}
              </div>
            </div>
            <div className="flex flex-col items-end">
              <span className="text-[9px] uppercase tracking-tighter">API Latency</span>
              <span className="text-[10px] text-[#00ff9d] font-mono">24ms</span>
            </div>
          </div>

          <div className="flex flex-col items-end bg-white/5 px-4 py-1 border-l border-r border-white/10">
            <span className="text-[14px] font-mono font-black text-[#00f2ff] tracking-tight">
              {currentTime.getHours().toString().padStart(2, '0')}:
              {currentTime.getMinutes().toString().padStart(2, '0')}:
              {currentTime.getSeconds().toString().padStart(2, '0')}
              <span className="text-[10px] opacity-40">:{currentTime.getMilliseconds().toString().padStart(3, '0')}</span>
            </span>
            <span className="text-[8px] uppercase tracking-widest text-white/30 font-bold">
              {currentTime.toLocaleDateString('en-US', { day: '2-digit', month: 'short', year: 'numeric' })}
            </span>
          </div>
        </div>
      </header>

      {/* --- HUD MAIN CONTENT --- */}
      <main className="relative z-10 flex-1 p-6 flex gap-6 overflow-hidden">
        
        {/* LEFT COLUMN: Leaderboard HUD */}
        <section className="w-1/4 flex flex-col gap-6">
          <CyberCard title="Personnel Roster" icon={Users} color="cyan" className="flex-1 overflow-hidden flex flex-col">
            <div className="flex-1 overflow-y-auto custom-cyber-scrollbar pr-2 space-y-2">
              {sortedTraders.map((trader, i) => (
                <div
                  key={trader.trader_id}
                  onClick={() => setSelectedTrader(trader)}
                  className={`
                    relative flex items-center gap-3 p-3 transition-all cursor-pointer border
                    ${selectedTrader?.trader_id === trader.trader_id 
                      ? 'bg-[#00f2ff]/10 border-[#00f2ff]/40 shadow-[inset_0_0_10px_rgba(0,242,255,0.05)]' 
                      : 'border-white/5 bg-white/2 hover:bg-white/5 hover:border-white/10'}
                  `}
                >
                  <div className={`text-[10px] font-mono ${i < 3 ? 'text-[#ffe600]' : 'text-white/20'}`}>
                    {(i + 1).toString().padStart(2, '0')}
                  </div>
                  <div className="relative">
                    <PunkAvatar
                      seed={getTraderAvatar(trader.trader_id, trader.trader_name)}
                      size={32}
                      className="rounded-sm grayscale group-hover:grayscale-0 transition-all"
                    />
                    {trader.position_count > 0 && (
                      <div className="absolute -bottom-1 -right-1 w-2 h-2 bg-[#00ff9d] rounded-full animate-pulse shadow-[0_0_8px_#00ff9d]" />
                    )}
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="text-[11px] font-black truncate text-white/90 tracking-tight uppercase">
                      {trader.trader_name}
                    </div>
                    <div className="flex items-center gap-2">
                      <div className="w-16 h-1 bg-white/5 rounded-full overflow-hidden mt-1">
                        <div className="h-full bg-[#bc00ff]/60" style={{ width: `${trader.win_rate}%` }} />
                      </div>
                      <span className="text-[8px] text-white/30 font-mono">{trader.win_rate.toFixed(0)}%</span>
                    </div>
                  </div>
                  <div className="text-right">
                    <div className={`text-[12px] font-mono font-bold ${trader.total_pnl >= 0 ? 'text-[#00ff9d]' : 'text-[#ff0055]'}`}>
                      {trader.total_pnl >= 0 ? '+' : '-'}${Math.abs(trader.total_pnl).toFixed(1)}
                    </div>
                    <div className="text-[8px] text-white/20 font-bold tracking-tighter">CUMULATIVE</div>
                  </div>
                </div>
              ))}
            </div>
          </CyberCard>

          <CyberCard title="Network Integrity" icon={Globe} color="green" className="h-40">
             <div className="flex flex-col gap-3">
                <div className="flex justify-between items-center">
                  <span className="text-[9px] text-white/40 uppercase font-black">Global Efficiency</span>
                  <span className="text-[12px] text-[#00ff9d] font-mono font-bold">98.2%</span>
                </div>
                <div className="grid grid-cols-5 gap-1 h-12">
                   {[...Array(15)].map((_, i) => (
                      <div key={i} className={`w-full rounded-sm transition-all duration-500 ${Math.random() > 0.2 ? 'bg-[#00ff9d]/20 h-full' : 'bg-white/5 h-1/2'}`} />
                   ))}
                </div>
                <p className="text-[8px] text-white/30 leading-tight">SYSTEM UPTIME: 994H 12M 33S. ALL NODES REPORTING OPTIMAL STATE.</p>
             </div>
          </CyberCard>
        </section>

        {/* CENTER COLUMN: Central Core Stats */}
        <section className="flex-1 flex flex-col gap-6">
          <div className="grid grid-cols-4 gap-4">
             <CyberCard color="cyan" className="text-center group">
                <div className="text-[9px] text-white/40 uppercase mb-1 tracking-[0.2em]">Total Realized PnL</div>
                <div className={`text-2xl font-black font-mono tracking-tighter ${globalStats.total_pnl >= 0 ? 'text-[#00ff9d]' : 'text-[#ff0055]'}`}>
                  <MatrixNumber value={globalStats.total_pnl} prefix="$" />
                </div>
                <div className="mt-2 h-1 bg-white/5 overflow-hidden">
                   <div className="h-full bg-[#00f2ff]/40 animate-cyber-scan" style={{ width: '40%' }} />
                </div>
             </CyberCard>
             <CyberCard color="purple" className="text-center">
                <div className="text-[9px] text-white/40 uppercase mb-1 tracking-[0.2em]">Active Operations</div>
                <div className="text-2xl font-black font-mono tracking-tighter text-[#bc00ff]">
                  <MatrixNumber value={globalStats.active_traders} decimals={0} />
                  <span className="text-xs opacity-40 ml-1">UNITS</span>
                </div>
                <div className="mt-2 text-[8px] text-white/20 uppercase">Tactical Deployment</div>
             </CyberCard>
             <CyberCard color="green" className="text-center">
                <div className="text-[9px] text-white/40 uppercase mb-1 tracking-[0.2em]">Avg Win Rate</div>
                <div className="text-2xl font-black font-mono tracking-tighter text-[#00ff9d]">
                   <MatrixNumber value={globalStats.avg_win_rate} suffix="%" />
                </div>
                <div className="mt-2 flex justify-center gap-1">
                   {[...Array(5)].map((_, i) => <div key={i} className="w-1.5 h-1.5 bg-[#00ff9d]/40 rounded-full animate-pulse" style={{ animationDelay: `${i * 0.2}s` }} />)}
                </div>
             </CyberCard>
             <CyberCard color="yellow" className="text-center">
                <div className="text-[9px] text-white/40 uppercase mb-1 tracking-[0.2em]">Operational Equity</div>
                <div className="text-2xl font-black font-mono tracking-tighter text-[#ffe600]">
                  <MatrixNumber value={globalStats.total_equity} prefix="$" />
                </div>
                <div className="mt-2 text-[8px] text-white/20 uppercase">Liquidity Index</div>
             </CyberCard>
          </div>

          <CyberCard title="Operational Performance Matrix" icon={BarChart3} className="flex-1 relative overflow-hidden">
             {selectedTrader ? (
               <div className="h-full flex flex-col">
                  <div className="flex items-center justify-between mb-8">
                     <div className="flex items-center gap-6">
                        <div className="relative">
                          <PunkAvatar
                            seed={getTraderAvatar(selectedTrader.trader_id, selectedTrader.trader_name)}
                            size={64}
                            className="rounded-sm border-2 border-white/10 p-0.5 shadow-[0_0_20px_rgba(255,255,255,0.05)]"
                          />
                          <div className="absolute -top-3 -left-3 bg-[#00f2ff] text-slate-950 px-2 py-0.5 text-[8px] font-black uppercase">Selected</div>
                        </div>
                        <div>
                           <h2 className="text-3xl font-black tracking-tight uppercase leading-none">{selectedTrader.trader_name}</h2>
                           <div className="flex items-center gap-4 mt-2">
                              <span className="text-[10px] text-white/40 flex items-center gap-1 uppercase font-black">
                                <Shield size={10} className="text-[#bc00ff]" /> {selectedTrader.mode} protocol
                              </span>
                              <span className="text-[10px] text-white/40 flex items-center gap-1 uppercase font-black">
                                <Database size={10} className="text-[#00f2ff]" /> ID: {selectedTrader.trader_id.substring(0, 12)}...
                              </span>
                           </div>
                        </div>
                     </div>
                     <div className="text-right">
                        <div className={`text-4xl font-mono font-black ${selectedTrader.total_pnl >= 0 ? 'text-[#00ff9d]' : 'text-[#ff0055]'}`}>
                          {formatMoney(selectedTrader.total_pnl)}
                        </div>
                        <div className="text-[10px] text-white/30 uppercase tracking-[0.3em] font-black">Cumulative Return</div>
                     </div>
                  </div>

                  <div className="grid grid-cols-4 gap-6 mb-10">
                     {[
                       { label: 'Return ROI', value: `${selectedTrader.return_rate.toFixed(2)}%`, icon: TrendingUp, color: '#00ff9d' },
                       { label: 'Profit Factor', value: selectedTrader.profit_factor.toFixed(2), icon: Target, color: '#00f2ff' },
                       { label: 'Efficiency', value: `${selectedTrader.win_rate.toFixed(1)}%`, icon: Activity, color: '#ffe600' },
                       { label: 'Operations', value: selectedTrader.total_trades, icon: Cpu, color: '#bc00ff' },
                     ].map((s, i) => (
                       <div key={i} className="bg-white/2 border border-white/5 p-4 relative group hover:bg-white/5 transition-colors">
                          <div className="absolute top-0 left-0 w-1 h-full opacity-60" style={{ backgroundColor: s.color }} />
                          <div className="text-[9px] text-white/40 uppercase mb-1 font-black tracking-widest">{s.label}</div>
                          <div className="text-xl font-mono font-black text-white/90">{s.value}</div>
                          <s.icon className="absolute bottom-2 right-2 opacity-10" size={24} style={{ color: s.color }} />
                       </div>
                     ))}
                  </div>

                  <div className="flex-1 bg-slate-900/50 border border-white/5 rounded-sm p-6 relative">
                     <div className="absolute top-4 left-6 flex items-center gap-2">
                        <div className="w-2 h-2 rounded-full bg-[#00f2ff] animate-pulse" />
                        <span className="text-[10px] text-white/50 uppercase font-black tracking-widest">Growth Analytics Signal</span>
                     </div>
                     <div className="h-full w-full pt-8">
                        {/* Fake but cool looking chart structure */}
                        <div className="absolute inset-0 opacity-10 pointer-events-none p-10">
                           <div className="w-full h-full border-b border-l border-white/40 relative">
                              {[...Array(5)].map((_, i) => (
                                <div key={i} className="absolute w-full border-t border-white/10" style={{ top: `${i * 25}%` }} />
                              ))}
                           </div>
                        </div>
                        <div className="relative h-full w-full">
                           <CyberSparkline 
                             data={[10, 15, 12, 25, 22, 35, 40, 38, 50, 65, 60, 75, 82]} 
                             color={selectedTrader.total_pnl >= 0 ? '#00ff9d' : '#ff0055'} 
                             height={150} 
                           />
                        </div>
                     </div>
                  </div>
               </div>
             ) : (
               <div className="h-full flex flex-col items-center justify-center opacity-20">
                  <Maximize2 size={64} />
                  <span className="mt-4 text-[10px] tracking-[0.5em] uppercase">Awaiting Protocol Selection</span>
               </div>
             )}
          </CyberCard>
        </section>

        {/* RIGHT COLUMN: Realtime TerminalHUD */}
        <section className="w-1/4 flex flex-col gap-6">
          <CyberCard title="Tactical Log" icon={TerminalIcon} color="purple" className="flex-1 overflow-hidden flex flex-col">
             <div className="flex-1 font-mono text-[9px] text-[#bc00ff] space-y-1.5 overflow-y-auto custom-cyber-scrollbar">
                <div className="opacity-50">[SYSTEM] Initialization complete.</div>
                <div className="opacity-50">[AUTH] User verified. Commencing data stream...</div>
                {[...Array(20)].map((_, i) => {
                  const trader = traderStats[i % traderStats.length]
                  if (!trader) return null
                  return (
                    <div key={i} className="flex gap-2 group hover:bg-white/5 p-0.5">
                      <span className="text-white/20 whitespace-nowrap">[{new Date(Date.now() - i * 600000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}]</span>
                      <span className="text-[#00f2ff] font-black">{trader.trader_name}</span>
                      <span className="text-white/40">EXECUTED</span>
                      <span className={Math.random() > 0.3 ? 'text-[#00ff9d]' : 'text-[#ff0055]'}>
                        {Math.random() > 0.5 ? 'BUY' : 'SELL'}
                      </span>
                    </div>
                  )
                })}
             </div>
             <div className="mt-4 pt-2 border-t border-white/5 flex items-center gap-2">
                <div className="w-1 h-4 bg-[#bc00ff] animate-pulse" />
                <span className="text-[8px] text-white/30 uppercase animate-cyber-type">Kernel Monitoring active...</span>
             </div>
          </CyberCard>

          <CyberCard title="Asset Allocation" icon={Crosshair} color="yellow" className="h-56">
             <div className="space-y-4">
                <div className="flex items-center justify-between text-[10px]">
                   <span className="text-white/40 uppercase font-bold">Risk Exposure</span>
                   <span className="text-[#ffe600] font-mono">0.05%</span>
                </div>
                <div className="w-full h-2 bg-white/5 rounded-full overflow-hidden flex">
                   <div className="h-full bg-[#00f2ff]/60" style={{ width: '45%' }} />
                   <div className="h-full bg-[#bc00ff]/60" style={{ width: '25%' }} />
                   <div className="h-full bg-[#00ff9d]/60" style={{ width: '20%' }} />
                </div>
                <div className="grid grid-cols-2 gap-2 mt-4">
                   <div className="bg-white/2 p-2 border border-white/5">
                      <div className="text-[8px] text-white/30 uppercase">Node Sync</div>
                      <div className="text-[12px] font-mono font-black text-[#00ff9d]">ACTIVE</div>
                   </div>
                   <div className="bg-white/2 p-2 border border-white/5">
                      <div className="text-[8px] text-white/30 uppercase">Protocol</div>
                      <div className="text-[12px] font-mono font-black text-[#00f2ff]">V-2.0.4</div>
                   </div>
                </div>
             </div>
          </CyberCard>
        </section>
      </main>

      {/* --- HUD FOOTER --- */}
      <footer className="relative z-20 border-t border-white/10 bg-slate-950/60 backdrop-blur-md px-6 py-2 flex items-center justify-between text-[10px] text-white/30">
        <div className="flex items-center gap-8 uppercase font-black tracking-widest">
           <div className="flex items-center gap-2">
              <div className="w-1.5 h-1.5 rounded-full bg-[#00ff9d]" />
              <span>Network: Encrypted</span>
           </div>
           <div className="flex items-center gap-2">
              <div className="w-1.5 h-1.5 rounded-full bg-[#00f2ff] animate-pulse" />
              <span>Realtime Signal Sync</span>
           </div>
        </div>
        <div className="flex items-center gap-6">
           <div className="flex items-center gap-2 border-l border-white/10 pl-6 uppercase tracking-tighter">
              <Shield size={12} className="text-[#ffe600]" />
              <span>Access Level: Administrator</span>
           </div>
           <span className="font-mono opacity-50 uppercase">NOFX-TERMINAL-00X-SYS</span>
        </div>
      </footer>

      {/* Global Style Inject for HUD animations */}
      <style>{`
        @keyframes cyber-scan {
          0% { transform: translateY(-100%); opacity: 0; }
          50% { opacity: 1; }
          100% { transform: translateY(1000%); opacity: 0; }
        }
        .animate-cyber-scan {
          animation: cyber-scan 5s linear infinite;
        }
        @keyframes cyber-progress {
          0% { left: -100%; }
          100% { left: 100%; }
        }
        .animate-cyber-progress {
          animation: cyber-progress 1.5s ease-in-out infinite;
        }
        @keyframes cyber-type {
          from { opacity: 0.3; }
          to { opacity: 1; }
        }
        .animate-cyber-type {
          animation: cyber-type 0.1s infinite alternate;
        }
        .custom-cyber-scrollbar::-webkit-scrollbar {
          width: 2px;
        }
        .custom-cyber-scrollbar::-webkit-scrollbar-track {
          background: rgba(255, 255, 255, 0.02);
        }
        .custom-cyber-scrollbar::-webkit-scrollbar-thumb {
          background: rgba(0, 242, 255, 0.2);
          border-radius: 1px;
        }
        .custom-cyber-scrollbar::-webkit-scrollbar-thumb:hover {
          background: rgba(0, 242, 255, 0.4);
        }
      `}</style>
    </div>
  )
}
