import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import {
  TrendingUp,
  Users,
  Zap,
  Target,
  Shield,
  Cpu,
  Globe,
  Database,
  Terminal as TerminalIcon,
  Crosshair,
  Layers,
  AlertTriangle,
} from 'lucide-react'
import { PunkAvatar, getTraderAvatar } from '../components/PunkAvatar'

// --- Types ---
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

// --- UI Components ---

/**
 * 仪表盘仪表件 (Gauge)
 */
function CyberGauge({
  value,
  label,
  color = '#00f2ff',
  size = 120,
}: {
  value: number
  label: string
  color?: string
  size?: number
}) {
  const radius = size * 0.4
  const circumference = 2 * Math.PI * radius
  const offset = circumference - (value / 100) * circumference

  return (
    <div className="relative flex flex-col items-center justify-center">
      <svg width={size} height={size} className="-rotate-90">
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="rgba(255,255,255,0.05)"
          strokeWidth={size * 0.08}
        />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke={color}
          strokeWidth={size * 0.08}
          strokeDasharray={circumference}
          strokeDashoffset={offset}
          strokeLinecap="round"
          className="transition-all duration-1000 ease-out"
          style={{ filter: `drop-shadow(0 0 8px ${color}80)` }}
        />
        {/* 刻度线 */}
        {[...Array(12)].map((_, i) => (
          <line
            key={i}
            x1={size / 2 + (radius + 8) * Math.cos((i * 30 * Math.PI) / 180)}
            y1={size / 2 + (radius + 8) * Math.sin((i * 30 * Math.PI) / 180)}
            x2={size / 2 + (radius + 12) * Math.cos((i * 30 * Math.PI) / 180)}
            y2={size / 2 + (radius + 12) * Math.sin((i * 30 * Math.PI) / 180)}
            stroke="rgba(255,255,255,0.2)"
            strokeWidth="1"
          />
        ))}
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center mt-[-4px]">
        <span className="text-xl font-black font-mono leading-none">{value.toFixed(1)}%</span>
        <span className="text-[8px] uppercase tracking-widest text-white/40 mt-1">{label}</span>
      </div>
    </div>
  )
}

/**
 * 科技感边框容器 (Cockpit Panel)
 */
function CockpitPanel({
  children,
  title,
  icon: Icon,
  className = '',
  color = 'cyan',
}: {
  children: React.ReactNode
  title?: string
  icon?: any
  className?: string
  color?: 'cyan' | 'pink' | 'green' | 'yellow'
}) {
  const colors = {
    cyan: '#00f2ff',
    pink: '#ff00ff',
    green: '#00ff9d',
    yellow: '#ffe600',
  }
  const themeColor = colors[color]

  return (
    <div className={`relative flex flex-col ${className}`}>
      {/* 背景层 */}
      <div className="absolute inset-0 bg-slate-900/40 backdrop-blur-xl border border-white/5 rounded-sm" />
      
      {/* 装饰边角 */}
      <div className="absolute top-0 left-0 w-4 h-4 border-t-2 border-l-2" style={{ borderColor: themeColor }} />
      <div className="absolute top-0 right-0 w-4 h-4 border-t-2 border-r-2" style={{ borderColor: themeColor }} />
      <div className="absolute bottom-0 left-0 w-4 h-4 border-b-2 border-l-2" style={{ borderColor: themeColor }} />
      <div className="absolute bottom-0 right-0 w-4 h-4 border-b-2 border-r-2" style={{ borderColor: themeColor }} />

      {/* 顶部标题条 */}
      {title && (
        <div className="relative z-10 flex items-center gap-2 px-4 py-2 border-b border-white/10 bg-white/5">
          {Icon && <Icon size={14} style={{ color: themeColor }} />}
          <span className="text-[10px] font-black uppercase tracking-[0.2em] text-white/80">{title}</span>
          <div className="flex-1" />
          <div className="flex gap-1">
            <div className="w-1 h-3 bg-white/10" />
            <div className="w-1 h-3" style={{ backgroundColor: themeColor }} />
          </div>
        </div>
      )}

      {/* 内容区 */}
      <div className="relative z-10 p-4 flex-1">
        {children}
      </div>

      {/* 底部装饰线 */}
      <div className="absolute bottom-[-1px] left-1/2 -translate-x-1/2 w-1/2 h-px opacity-50" 
           style={{ background: `linear-gradient(90deg, transparent, ${themeColor}, transparent)` }} />
    </div>
  )
}

/**
 * 战术数字组件
 */
function TacticalNumber({
  value,
  prefix,
  suffix,
  className = '',
  color = 'white',
}: {
  value: number
  prefix?: string
  suffix?: string
  className?: string
  color?: string
}) {
  return (
    <div className={`font-mono font-black tracking-tighter ${className}`} style={{ color }}>
      {prefix && <span className="text-[0.6em] mr-1 opacity-60 font-sans">{prefix}</span>}
      {value.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
      {suffix && <span className="text-[0.6em] ml-1 opacity-60 font-sans">{suffix}</span>}
    </div>
  )
}

// --- Main Page ---

export function DashboardPage() {
  const [currentTime, setCurrentTime] = useState(new Date())
  const [selectedTrader, setSelectedTrader] = useState<TraderStats | null>(null)

  // 数据获取
  const { data: dashboardTraders, isLoading } = useSWR('dashboard-traders-cockpit', fetchDashboardTraders, {
    refreshInterval: 30000,
  })
  const { data: summaryData } = useSWR('dashboard-summary-cockpit', fetchDashboardSummary, {
    refreshInterval: 30000,
  })

  useEffect(() => {
    const timer = setInterval(() => setCurrentTime(new Date()), 10)
    return () => clearInterval(timer)
  }, [])

  const traderStats: TraderStats[] = useMemo(() => {
    if (!dashboardTraders) return []
    return (dashboardTraders as any[]).map((t: any) => ({
      ...t,
      trader_name: t.trader_name || t.trader_id?.substring(0, 8) || 'UNIT-01',
    }))
  }, [dashboardTraders])

  const globalStats: GlobalStats = useMemo(() => {
    return summaryData || {
      total_pnl: 0, 
      total_trades: 0, 
      avg_win_rate: 0, 
      active_traders: 0, 
      total_equity: 0,
      today_pnl: 0,
      week_pnl: 0,
      month_pnl: 0
    }
  }, [summaryData])

  const sortedTraders = [...traderStats].sort((a, b) => b.total_pnl - a.total_pnl)

  // 初始选中
  useEffect(() => {
    if (sortedTraders.length > 0 && !selectedTrader) {
      setSelectedTrader(sortedTraders[0])
    }
  }, [sortedTraders])

  if (isLoading && !dashboardTraders) {
    return (
      <div className="fixed inset-0 bg-black z-[9999] flex flex-col items-center justify-center font-mono">
        <div className="w-64 h-1 bg-white/5 relative overflow-hidden mb-4">
          <div className="absolute top-0 left-0 h-full bg-[#00f2ff] animate-[loading_2s_infinite]" />
        </div>
        <div className="text-[#00f2ff] text-xs tracking-[0.5em] uppercase animate-pulse">Establishing Neural Link...</div>
        <style>{`@keyframes loading { 0% { left: -100% } 100% { left: 100% } }`}</style>
      </div>
    )
  }

  return (
    <div className="fixed inset-0 bg-[#00050a] text-white z-[9999] overflow-hidden flex flex-col font-sans select-none">
      {/* 背景动效层 */}
      <div className="absolute inset-0 pointer-events-none opacity-20">
        <div className="absolute inset-0" style={{
          backgroundImage: `linear-gradient(to right, #ffffff05 1px, transparent 1px), linear-gradient(to bottom, #ffffff05 1px, transparent 1px)`,
          backgroundSize: '50px 50px',
          transform: 'perspective(1000px) rotateX(60deg) translateY(-100px)',
          transformOrigin: 'top'
        }} />
        <div className="absolute inset-0 bg-gradient-to-b from-[#00f2ff05] via-transparent to-transparent" />
      </div>

      {/* --- HEADER --- */}
      <header className="relative z-10 h-16 border-b border-white/10 bg-black/40 backdrop-blur-md px-8 flex items-center justify-between">
        <div className="flex items-center gap-8">
          <div className="flex items-center gap-3">
            <div className="w-10 h-10 bg-gradient-to-br from-[#00f2ff] to-[#ff00ff] rounded-sm rotate-45 flex items-center justify-center">
              <Zap size={24} className="text-white -rotate-45" />
            </div>
            <div className="flex flex-col">
              <span className="text-lg font-black tracking-tighter leading-none">NOFX COMMAND</span>
              <span className="text-[10px] text-[#00f2ff] font-bold tracking-[0.3em] uppercase opacity-60">Operations Unit v2.5</span>
            </div>
          </div>
          
          <div className="h-8 w-px bg-white/10 mx-2" />
          
          <div className="flex flex-col">
            <span className="text-[10px] text-white/40 uppercase font-black tracking-widest">System Status</span>
            <div className="flex items-center gap-2 mt-1">
              <div className="w-2 h-2 rounded-full bg-[#00ff9d] shadow-[0_0_8px_#00ff9d]" />
              <span className="text-xs font-mono text-[#00ff9d] uppercase">All Nodes Operational</span>
            </div>
          </div>
        </div>

        <div className="flex items-center gap-12">
          <div className="flex flex-col items-end">
            <span className="text-[10px] text-white/40 uppercase font-black tracking-widest">Network Latency</span>
            <span className="text-sm font-mono text-[#ffe600]">12.4ms <span className="text-[8px] opacity-40 uppercase ml-1">Optimized</span></span>
          </div>
          
          <div className="bg-white/5 border border-white/10 px-6 py-2 rounded-sm text-right min-w-[200px]">
            <div className="text-xl font-mono font-black text-[#00f2ff] leading-none">
              {currentTime.getHours().toString().padStart(2, '0')}:
              {currentTime.getMinutes().toString().padStart(2, '0')}:
              {currentTime.getSeconds().toString().padStart(2, '0')}
              <span className="text-xs opacity-40 ml-1">.{currentTime.getMilliseconds().toString().padStart(3, '0')}</span>
            </div>
            <div className="text-[9px] text-white/30 font-black uppercase tracking-[0.2em] mt-1">
              {currentTime.toLocaleDateString('en-US', { weekday: 'short', month: 'short', day: '2-digit', year: 'numeric' })}
            </div>
          </div>
        </div>
      </header>

      {/* --- MAIN GRID --- */}
      <main className="relative z-10 flex-1 p-6 grid grid-cols-12 gap-6 overflow-hidden">
        
        {/* LEFT: RANKINGS WING */}
        <div className="col-span-3 flex flex-col gap-6 overflow-hidden">
          <CockpitPanel title="Operations Roster" icon={Users} color="cyan" className="flex-1 overflow-hidden">
            <div className="h-full overflow-y-auto pr-2 custom-cyber-scrollbar space-y-3">
              {sortedTraders.map((t, i) => (
                <div 
                  key={t.trader_id}
                  onClick={() => setSelectedTrader(t)}
                  className={`group relative p-3 border cursor-pointer transition-all ${
                    selectedTrader?.trader_id === t.trader_id 
                    ? 'bg-[#00f2ff]/10 border-[#00f2ff]/40 shadow-[inset_0_0_15px_rgba(0,242,255,0.1)]' 
                    : 'bg-white/2 border-white/5 hover:border-white/20'
                  }`}
                >
                  <div className="flex items-center gap-3">
                    <div className={`text-lg font-mono font-black ${i < 3 ? 'text-[#ffe600]' : 'text-white/20'}`}>
                      {(i + 1).toString().padStart(2, '0')}
                    </div>
                    <div className="relative">
                      <PunkAvatar 
                        seed={getTraderAvatar(t.trader_id, t.trader_name)} 
                        size={36} 
                        className={`rounded-sm transition-all ${selectedTrader?.trader_id === t.trader_id ? 'grayscale-0' : 'grayscale group-hover:grayscale-0'}`} 
                      />
                      {t.position_count > 0 && (
                        <div className="absolute -bottom-1 -right-1 w-2 h-2 bg-[#00ff9d] rounded-full animate-pulse shadow-[0_0_8px_#00ff9d]" />
                      )}
                    </div>
                    <div className="flex-1 min-w-0">
                      <div className="text-[11px] font-black uppercase truncate text-white/90">{t.trader_name}</div>
                      <div className="flex items-center gap-2 mt-1">
                        <div className="flex-1 h-1 bg-white/5 rounded-full overflow-hidden">
                          <div className="h-full bg-[#ff00ff]/60" style={{ width: `${t.win_rate}%` }} />
                        </div>
                        <span className="text-[8px] font-mono text-white/40">{t.win_rate.toFixed(0)}%</span>
                      </div>
                    </div>
                    <div className="text-right">
                      <TacticalNumber 
                        value={t.total_pnl} 
                        className="text-xs" 
                        color={t.total_pnl >= 0 ? '#00ff9d' : '#ff0055'} 
                      />
                      <div className="text-[8px] font-black text-white/20 uppercase tracking-tighter">Yield</div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </CockpitPanel>

          <CockpitPanel title="System Alerts" icon={AlertTriangle} color="yellow" className="h-48">
            <div className="space-y-3">
              <div className="flex items-start gap-3 text-[10px] text-white/60">
                <div className="w-1 h-10 bg-[#ffe600] shrink-0" />
                <div>
                  <div className="font-black text-[#ffe600] uppercase mb-1">Volatilty Warning</div>
                  <p className="leading-tight opacity-80">BTCUSDT cross-exchange liquidity depth decreased by 14.2%. Adjusting risk parameters...</p>
                </div>
              </div>
              <div className="flex items-start gap-3 text-[10px] text-white/40">
                <div className="w-1 h-10 bg-white/10 shrink-0" />
                <div>
                  <div className="font-black uppercase mb-1">Maintenance Scheduled</div>
                  <p className="leading-tight opacity-80">Hyperliquid API node optimization in T-minus 144H. No downtime expected.</p>
                </div>
              </div>
            </div>
          </CockpitPanel>
        </div>

        {/* CENTER: CORE HUD */}
        <div className="col-span-6 flex flex-col gap-6">
          {/* Top Global Stats */}
          <div className="grid grid-cols-4 gap-4">
            <div className="relative group">
              <div className="absolute inset-0 bg-[#00f2ff0a] border border-[#00f2ff20] rounded-sm" />
              <div className="relative p-4 flex flex-col items-center">
                <span className="text-[9px] font-black uppercase text-white/40 tracking-[0.2em] mb-1">Total PnL</span>
                <TacticalNumber value={globalStats.total_pnl} className="text-2xl" color={globalStats.total_pnl >= 0 ? '#00ff9d' : '#ff0055'} prefix="$" />
              </div>
            </div>
            <div className="relative">
              <div className="absolute inset-0 bg-[#ff00ff0a] border border-[#ff00ff20] rounded-sm" />
              <div className="relative p-4 flex flex-col items-center">
                <span className="text-[9px] font-black uppercase text-white/40 tracking-[0.2em] mb-1">Active Units</span>
                <TacticalNumber value={globalStats.active_traders} className="text-2xl" color="#ff00ff" suffix="U" />
              </div>
            </div>
            <div className="relative">
              <div className="absolute inset-0 bg-[#00ff9d0a] border border-[#00ff9d20] rounded-sm" />
              <div className="relative p-4 flex flex-col items-center">
                <span className="text-[9px] font-black uppercase text-white/40 tracking-[0.2em] mb-1">Avg Efficiency</span>
                <TacticalNumber value={globalStats.avg_win_rate} className="text-2xl" color="#00ff9d" suffix="%" />
              </div>
            </div>
            <div className="relative">
              <div className="absolute inset-0 bg-[#ffe6000a] border border-[#ffe60020] rounded-sm" />
              <div className="relative p-4 flex flex-col items-center">
                <span className="text-[9px] font-black uppercase text-white/40 tracking-[0.2em] mb-1">Net Liquidity</span>
                <TacticalNumber value={globalStats.total_equity} className="text-2xl" color="#ffe600" prefix="$" />
              </div>
            </div>
          </div>

          {/* Central Visualization */}
          <CockpitPanel className="flex-1 relative overflow-hidden" title="Neural Performance Analytics" icon={Layers}>
            {selectedTrader ? (
              <div className="h-full flex flex-col">
                <div className="flex items-center justify-between mb-8">
                  <div className="flex items-center gap-6">
                    <div className="relative w-24 h-24 border-2 border-[#00f2ff]/20 p-1">
                      <div className="absolute -top-2 -left-2 w-4 h-4 border-t-2 border-l-2 border-[#00f2ff]" />
                      <div className="absolute -bottom-2 -right-2 w-4 h-4 border-b-2 border-r-2 border-[#00f2ff]" />
                      <PunkAvatar 
                        seed={getTraderAvatar(selectedTrader.trader_id, selectedTrader.trader_name)} 
                        size={88} 
                        className="rounded-none w-full h-full"
                      />
                    </div>
                    <div>
                      <h2 className="text-4xl font-black tracking-tighter uppercase leading-none">{selectedTrader.trader_name}</h2>
                      <div className="flex items-center gap-4 mt-3">
                        <span className="px-2 py-0.5 bg-[#ff00ff]/20 text-[#ff00ff] text-[10px] font-black uppercase border border-[#ff00ff]/30">
                          {selectedTrader.mode} Protocol
                        </span>
                        <span className="text-[10px] text-white/40 font-mono flex items-center gap-1 uppercase">
                          <Database size={10} className="text-[#00f2ff]" /> NODE_{selectedTrader.trader_id.substring(0, 8).toUpperCase()}
                        </span>
                      </div>
                    </div>
                  </div>
                  
                  <div className="flex items-center gap-10">
                    <CyberGauge value={selectedTrader.win_rate} label="Efficiency" color="#00ff9d" size={100} />
                    <div className="text-right">
                      <div className="text-[10px] font-black uppercase text-white/30 tracking-[0.3em] mb-1">Total Return</div>
                      <TacticalNumber 
                        value={selectedTrader.total_pnl} 
                        className="text-4xl" 
                        color={selectedTrader.total_pnl >= 0 ? '#00ff9d' : '#ff0055'} 
                        prefix="$" 
                      />
                    </div>
                  </div>
                </div>

                <div className="grid grid-cols-4 gap-6 flex-1 mt-4">
                  <div className="flex flex-col gap-4">
                    {[
                      { label: 'Return ROI', value: `${selectedTrader.return_rate.toFixed(2)}%`, icon: TrendingUp, color: '#00ff9d' },
                      { label: 'Profit Factor', value: selectedTrader.profit_factor.toFixed(2), icon: Target, color: '#00f2ff' },
                      { label: 'Total Cycles', value: selectedTrader.total_trades, icon: Cpu, color: '#ff00ff' },
                    ].map((item, i) => (
                      <div key={i} className="bg-white/2 border-l-2 border-white/10 p-3 hover:bg-white/5 transition-all">
                        <div className="text-[9px] font-black text-white/40 uppercase tracking-widest mb-1">{item.label}</div>
                        <div className="text-xl font-mono font-black" style={{ color: item.color }}>{item.value}</div>
                      </div>
                    ))}
                  </div>
                  
                  <div className="col-span-3 bg-black/40 border border-white/5 relative p-6">
                    <div className="absolute top-4 left-6 flex items-center gap-2">
                      <Crosshair size={12} className="text-[#00f2ff]" />
                      <span className="text-[9px] font-black text-white/40 uppercase tracking-[0.2em]">Neural Growth Signal Trace</span>
                    </div>
                    {/* Fake Chart Grid */}
                    <div className="h-full pt-8 relative">
                      <div className="absolute inset-0 grid grid-cols-6 pointer-events-none p-8">
                        {[...Array(6)].map((_, i) => <div key={i} className="border-r border-white/5 h-full" />)}
                      </div>
                      <div className="absolute inset-0 grid grid-rows-4 pointer-events-none p-8">
                        {[...Array(4)].map((_, i) => <div key={i} className="border-b border-white/5 w-full" />)}
                      </div>
                      {/* Line Chart Area would go here */}
                      <div className="relative h-full flex items-end px-4">
                        {[...Array(24)].map((_, i) => (
                          <div 
                            key={i} 
                            className="flex-1 bg-gradient-to-t from-[#00f2ff20] to-[#00f2ff80] mx-[1px] transition-all hover:to-[#ff00ff]" 
                            style={{ height: `${Math.random() * 80 + 10}%` }}
                          />
                        ))}
                      </div>
                    </div>
                  </div>
                </div>
              </div>
            ) : null}
          </CockpitPanel>
        </div>

        {/* RIGHT: DATA LOGS */}
        <div className="col-span-3 flex flex-col gap-6 overflow-hidden">
          <CockpitPanel title="Tactical Log [实时]" icon={TerminalIcon} color="pink" className="flex-[1.5] overflow-hidden flex flex-col">
            <div className="flex-1 font-mono text-[9px] space-y-2 overflow-y-auto custom-cyber-scrollbar">
              <div className="text-[#ff00ff] opacity-50">[SYSTEM] Secure session initialized.</div>
              <div className="text-[#ff00ff] opacity-50">[AUTH] Administrator level clearance granted.</div>
              <div className="text-[#00f2ff] opacity-50">[SYNC] Real-time signal stream established.</div>
              
              {[...Array(15)].map((_, i) => {
                const trader = traderStats[i % traderStats.length]
                if (!trader) return null
                const side = Math.random() > 0.5 ? 'BUY' : 'SELL'
                const isBuy = side === 'BUY'
                return (
                  <div key={i} className="flex gap-2 p-1 bg-white/2 border-l border-white/5 hover:bg-white/5">
                    <span className="text-white/20">[{new Date(Date.now() - i * 450000).toLocaleTimeString([], { hour12: false })}]</span>
                    <span className="text-[#00f2ff] font-black truncate max-w-[60px]">{trader.trader_name}</span>
                    <span className="text-white/40">EXEC</span>
                    <span className={isBuy ? 'text-[#00ff9d]' : 'text-[#ff0055]'}>{side}</span>
                    <span className="text-white/20 font-bold ml-auto">{Math.random().toFixed(2)} ETH</span>
                  </div>
                )
              })}
            </div>
            <div className="mt-4 pt-2 border-t border-white/5 flex items-center gap-2">
              <div className="w-1 h-4 bg-[#ff00ff] animate-pulse" />
              <div className="text-[8px] text-[#ff00ff] font-black uppercase animate-[flicker_0.1s_infinite]">Monitoring Signal Packets...</div>
            </div>
          </CockpitPanel>

          <CockpitPanel title="Node Distribution" icon={Globe} color="green" className="flex-1">
            <div className="h-full flex flex-col gap-4">
              <div className="grid grid-cols-2 gap-4">
                <div>
                  <div className="text-[9px] font-black text-white/40 uppercase mb-1">Hyperliquid</div>
                  <div className="h-1 w-full bg-white/5 overflow-hidden">
                    <div className="h-full bg-[#00f2ff]" style={{ width: '65%' }} />
                  </div>
                </div>
                <div>
                  <div className="text-[9px] font-black text-white/40 uppercase mb-1">OKX Proxy</div>
                  <div className="h-1 w-full bg-white/5 overflow-hidden">
                    <div className="h-full bg-[#ff00ff]" style={{ width: '35%' }} />
                  </div>
                </div>
              </div>
              
              <div className="flex-1 flex items-center justify-center p-4">
                <div className="relative">
                  <Globe size={80} className="text-white/5 animate-[spin_20s_linear_infinite]" />
                  <div className="absolute inset-0 flex items-center justify-center">
                    <Shield size={24} className="text-[#00ff9d]/40 animate-pulse" />
                  </div>
                </div>
              </div>
              <div className="text-[8px] font-mono text-white/20 text-center uppercase tracking-widest">
                Global Edge Computing Active
              </div>
            </div>
          </CockpitPanel>
        </div>
      </main>

      {/* --- FOOTER STATUS --- */}
      <footer className="relative z-10 h-10 border-t border-white/10 bg-black/60 px-8 flex items-center justify-between text-[10px] font-black tracking-[0.2em] uppercase">
        <div className="flex items-center gap-8">
          <div className="flex items-center gap-2">
            <div className="w-1.5 h-1.5 rounded-full bg-[#00ff9d]" />
            <span className="text-white/40">Secure Stream: <span className="text-white/80">AES-256</span></span>
          </div>
          <div className="flex items-center gap-2">
            <div className="w-1.5 h-1.5 rounded-full bg-[#00f2ff] animate-pulse" />
            <span className="text-white/40">Heartbeat: <span className="text-white/80">Active</span></span>
          </div>
        </div>
        
        <div className="flex items-center gap-6 text-white/20">
          <span>Encryption Protocol v4.0</span>
          <div className="h-4 w-px bg-white/10" />
          <span>NOFX Terminal ID: <span className="text-white/40">TX-999-BETA</span></span>
        </div>
      </footer>

      {/* GLOBAL CSS INJECT */}
      <style>{`
        @keyframes flicker { 0% { opacity: 0.5 } 100% { opacity: 1 } }
        .custom-cyber-scrollbar::-webkit-scrollbar { width: 2px; }
        .custom-cyber-scrollbar::-webkit-scrollbar-track { background: transparent; }
        .custom-cyber-scrollbar::-webkit-scrollbar-thumb { background: #ffffff10; }
        .custom-cyber-scrollbar::-webkit-scrollbar-thumb:hover { background: #ffffff20; }
      `}</style>
    </div>
  )
}
