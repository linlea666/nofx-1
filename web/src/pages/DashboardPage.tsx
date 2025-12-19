import { useState, useEffect, useMemo } from 'react'
import useSWR from 'swr'
import {
  TrendingUp,
  Users,
  Zap,
  Target,
  Shield,
  Globe,
  Database,
  Terminal as TerminalIcon,
  Crosshair,
  Layers,
  AlertTriangle,
  Activity,
  Radio,
  Bug,
} from 'lucide-react'
import { PunkAvatar, getTraderAvatar } from '../components/PunkAvatar'

// --- 类型定义 ---
interface TraderStats {
  trader_id: string
  trader_name: string
  mode: string
  today_pnl: number
  today_trades: number
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

// --- API 调用 ---
const fetchDashboardTraders = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/dashboard/traders`)
  if (!res.ok) throw new Error('数据加载失败')
  return res.json()
}

const fetchDashboardSummary = async () => {
  const res = await fetch(`${import.meta.env.VITE_API_BASE_URL || ''}/api/dashboard/summary`)
  if (!res.ok) throw new Error('汇总数据加载失败')
  return res.json()
}

// --- UI 组件库 ---

/**
 * 仪表盘组件 (仿图4样式)
 */
function DataGauge({ value, label, color = '#00f2ff' }: { value: number; label: string; color?: string }) {
  const radius = 40
  const circumference = 2 * Math.PI * radius
  const offset = circumference - (value / 100) * circumference

  return (
    <div className="relative flex flex-col items-center">
      <svg width="100" height="100" className="-rotate-90">
        <circle cx="50" cy="50" r={radius} fill="none" stroke="rgba(255,255,255,0.05)" strokeWidth="8" />
        <circle
          cx="50"
          cy="50"
          r={radius}
          fill="none"
          stroke={color}
          strokeWidth="8"
          strokeDasharray={circumference}
          strokeDashoffset={offset}
          strokeLinecap="round"
          className="transition-all duration-1000 ease-out"
          style={{ filter: `drop-shadow(0 0 10px ${color}80)` }}
        />
      </svg>
      <div className="absolute inset-0 flex flex-col items-center justify-center -mt-2">
        <span className="text-xl font-black font-mono leading-none">{value.toFixed(1)}%</span>
        <span className="text-[10px] text-white/40 mt-1">{label}</span>
      </div>
    </div>
  )
}

/**
 * 大屏模块面板 (可视化设计)
 */
function DashboardModule({
  children,
  title,
  subtitle,
  icon: Icon,
  className = '',
  color = 'cyan',
}: {
  children: React.ReactNode
  title: string
  subtitle?: string
  icon?: any
  className?: string
  color?: 'cyan' | 'pink' | 'green' | 'yellow'
}) {
  const colorMap = {
    cyan: '#00f2ff',
    pink: '#ff00ff',
    green: '#00ff9d',
    yellow: '#ffe600',
  }
  const themeColor = colorMap[color]

  return (
    <div className={`relative flex flex-col ${className}`}>
      {/* 玻璃拟态底色 */}
      <div className="absolute inset-0 bg-[#0a1628]/60 backdrop-blur-xl border border-white/5 rounded-sm shadow-2xl" />
      
      {/* 战术边角 */}
      <div className="absolute top-0 left-0 w-3 h-3 border-t-2 border-l-2" style={{ borderColor: themeColor }} />
      <div className="absolute top-0 right-0 w-3 h-3 border-t-2 border-r-2" style={{ borderColor: themeColor }} />
      <div className="absolute bottom-0 left-0 w-3 h-3 border-b-2 border-l-2" style={{ borderColor: themeColor }} />
      <div className="absolute bottom-0 right-0 w-3 h-3 border-b-2 border-r-2" style={{ borderColor: themeColor }} />

      {/* 标题栏 */}
      <div className="relative z-10 flex items-center justify-between px-4 py-2 border-b border-white/10 bg-white/5">
        <div className="flex items-center gap-2">
          {Icon && <Icon size={16} style={{ color: themeColor }} />}
          <span className="text-sm font-black tracking-widest text-white/90">{title}</span>
          {subtitle && <span className="text-[9px] text-white/30 font-bold uppercase tracking-tighter ml-1">/ {subtitle}</span>}
        </div>
        <div className="flex gap-0.5">
          <div className="w-1.5 h-1.5 rounded-full animate-pulse" style={{ backgroundColor: themeColor }} />
        </div>
      </div>

      {/* 内容区 */}
      <div className="relative z-10 p-4 flex-1 overflow-hidden">
        {children}
      </div>
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

/**
 * 战术发光数字
 */
function NeonNumber({ 
  value, 
  prefix = '', 
  suffix = '', 
  className = '', 
  color = 'white' 
}: { 
  value: number; 
  prefix?: string; 
  suffix?: string; 
  className?: string; 
  color?: string 
}) {
  return (
    <div className={`font-mono font-black tracking-tighter ${className}`} style={{ color, textShadow: `0 0 20px ${color}40` }}>
      {prefix && <span className="text-[0.5em] mr-1 opacity-60 font-sans">{prefix}</span>}
      {value.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}
      {suffix && <span className="text-[0.5em] ml-1 opacity-60 font-sans">{suffix}</span>}
    </div>
  )
}

// --- 页面主体 ---

export function DashboardPage() {
  const [currentTime, setCurrentTime] = useState(new Date())
  const [selectedTrader, setSelectedTrader] = useState<TraderStats | null>(null)

  // 数据获取
  const { data: tradersRaw, isLoading } = useSWR('dashboard-traders-v3', fetchDashboardTraders, { refreshInterval: 15000 })
  const { data: summaryRaw } = useSWR('dashboard-summary-v3', fetchDashboardSummary, { refreshInterval: 15000 })

  useEffect(() => {
    const timer = setInterval(() => setCurrentTime(new Date()), 10)
    return () => clearInterval(timer)
  }, [])

  const traderStats: TraderStats[] = useMemo(() => {
    if (!tradersRaw) return []
    return (tradersRaw as any[]).map(t => ({
      ...t,
      trader_name: t.trader_name || t.trader_id?.substring(0, 8) || '未知单位',
    }))
  }, [tradersRaw])

  const globalStats: GlobalStats = useMemo(() => {
    return summaryRaw || {
      total_pnl: 0, total_trades: 0, avg_win_rate: 0, active_traders: 0, total_equity: 0,
      today_pnl: 0, week_pnl: 0, month_pnl: 0
    }
  }, [summaryRaw])

  const sortedTraders = [...traderStats].sort((a, b) => b.total_pnl - a.total_pnl)

  useEffect(() => {
    if (sortedTraders.length > 0 && !selectedTrader) setSelectedTrader(sortedTraders[0])
  }, [sortedTraders])

  if (isLoading && !tradersRaw) {
    return (
      <div className="fixed inset-0 bg-[#020617] z-[9999] flex flex-col items-center justify-center font-mono">
        <div className="w-64 h-1 bg-white/5 relative overflow-hidden mb-4 rounded-full">
          <div className="absolute top-0 left-0 h-full bg-[#00f2ff] animate-[loading_2s_infinite]" />
        </div>
        <div className="text-[#00f2ff] text-xs tracking-[0.5em] animate-pulse">正在同步全球交易节点...</div>
        <style>{`@keyframes loading { 0% { left: -100% } 100% { left: 100% } }`}</style>
      </div>
    )
  }

  return (
    <div className="fixed inset-0 bg-[#020617] text-white z-[9999] overflow-hidden flex flex-col font-sans select-none tracking-tight">
      {/* 动态背景背景 */}
      <div className="absolute inset-0 pointer-events-none opacity-30">
        <div className="absolute inset-0" style={{
          backgroundImage: `radial-gradient(circle at 50% 50%, #1e293b 0%, transparent 70%)`,
        }} />
        <div className="absolute inset-0 opacity-10" style={{
          backgroundImage: `linear-gradient(to right, #ffffff05 1px, transparent 1px), linear-gradient(to bottom, #ffffff05 1px, transparent 1px)`,
          backgroundSize: '40px 40px',
        }} />
      </div>

      {/* --- 顶部导航条 --- */}
      <header className="relative z-10 h-20 border-b border-white/10 bg-black/40 backdrop-blur-xl px-10 flex items-center justify-between">
        <div className="flex items-center gap-10">
          <div className="flex items-center gap-4">
            <div className="w-12 h-12 bg-gradient-to-br from-[#00f2ff] to-[#ff00ff] rounded-lg rotate-45 flex items-center justify-center shadow-[0_0_20px_rgba(0,242,255,0.3)]">
              <Zap size={28} className="text-white -rotate-45" />
            </div>
            <div className="flex flex-col">
              <span className="text-2xl font-black tracking-tighter italic">NOFX 交易指挥中心</span>
              <span className="text-[10px] text-[#00f2ff] font-bold tracking-[0.4em] uppercase opacity-60">Operations Unit v2.8</span>
            </div>
          </div>
          
          <div className="flex flex-col border-l border-white/10 pl-10">
            <span className="text-[10px] text-white/40 uppercase font-black tracking-widest mb-1">系统状态</span>
            <div className="flex items-center gap-2">
              <div className="w-2 h-2 rounded-full bg-[#00ff9d] shadow-[0_0_10px_#00ff9d]" />
              <span className="text-xs font-mono text-[#00ff9d] uppercase font-bold">节点运行正常</span>
            </div>
          </div>
        </div>

        {/* 顶部中央核心指标 (仿图3) */}
        <div className="absolute left-1/2 -translate-x-1/2 flex items-center gap-16">
          <div className="flex flex-col items-center">
            <span className="text-[10px] text-white/40 font-black mb-1">今日总盈亏</span>
            <NeonNumber 
              value={globalStats.today_pnl} 
              prefix="$" 
              className="text-3xl" 
              color={globalStats.today_pnl >= 0 ? '#00ff9d' : '#ff0055'} 
            />
          </div>
          <div className="flex flex-col items-center border-l border-white/5 pl-16">
            <span className="text-[10px] text-white/40 font-black mb-1">今日成交笔数</span>
            <NeonNumber value={globalStats.total_trades} suffix="笔" className="text-3xl" color="#ffe600" />
          </div>
          <div className="flex flex-col items-center border-l border-white/5 pl-16">
            <span className="text-[10px] text-white/40 font-black mb-1">活跃交易员</span>
            <NeonNumber value={globalStats.active_traders} suffix="位" className="text-3xl" color="#ff00ff" />
          </div>
        </div>

        <div className="flex items-center gap-10">
          <div className="bg-white/5 border border-white/10 px-8 py-2 rounded-sm text-right min-w-[220px] shadow-inner">
            <div className="text-2xl font-mono font-black text-[#00f2ff] leading-none tracking-tighter">
              {currentTime.getHours().toString().padStart(2, '0')}:
              {currentTime.getMinutes().toString().padStart(2, '0')}:
              {currentTime.getSeconds().toString().padStart(2, '0')}
              <span className="text-sm opacity-40 ml-1">.{currentTime.getMilliseconds().toString().padStart(3, '0')}</span>
            </div>
            <div className="text-[10px] text-white/30 font-black uppercase tracking-[0.2em] mt-1">
              {currentTime.toLocaleDateString('zh-CN', { weekday: 'long', month: 'long', day: '2-digit', year: 'numeric' })}
            </div>
          </div>
        </div>
      </header>

      {/* --- 主视口布局 --- */}
      <main className="relative z-10 flex-1 p-6 grid grid-cols-12 gap-6 overflow-hidden">
        
        {/* 左翼：排行与资产 */}
        <div className="col-span-3 flex flex-col gap-6 overflow-hidden">
          <DashboardModule title="交易员排行榜" subtitle="Leaderboard" icon={Users} color="cyan" className="flex-[2] overflow-hidden">
            <div className="h-full overflow-y-auto pr-2 custom-cyber-scrollbar space-y-3">
              {sortedTraders.map((t, i) => (
                <div 
                  key={t.trader_id}
                  onClick={() => setSelectedTrader(t)}
                  className={`group relative p-3 border transition-all cursor-pointer ${
                    selectedTrader?.trader_id === t.trader_id 
                    ? 'bg-[#00f2ff]/10 border-[#00f2ff]/40 shadow-[inset_0_0_20px_rgba(0,242,255,0.1)]' 
                    : 'bg-white/2 border-white/5 hover:border-white/20'
                  }`}
                >
                  <div className="flex items-center gap-3">
                    <div className={`text-xl font-mono font-black ${i < 3 ? 'text-[#ffe600]' : 'text-white/20'}`}>
                      {(i + 1).toString().padStart(2, '0')}
                    </div>
                    <div className="relative">
                      <PunkAvatar 
                        seed={getTraderAvatar(t.trader_id, t.trader_name)} 
                        size={40} 
                        className={`rounded-sm shadow-lg transition-all ${selectedTrader?.trader_id === t.trader_id ? 'grayscale-0 scale-110' : 'grayscale group-hover:grayscale-0'}`} 
                      />
                      {t.position_count > 0 && (
                        <div className="absolute -bottom-1 -right-1 w-2.5 h-2.5 bg-[#00ff9d] rounded-full animate-pulse shadow-[0_0_10px_#00ff9d]" />
                      )}
                    </div>
                    <div className="flex-1 min-w-0">
                      <div className="text-xs font-black uppercase truncate text-white/90">{t.trader_name}</div>
                      <div className="flex items-center gap-2 mt-1.5">
                        <div className="flex-1 h-1.5 bg-white/5 rounded-full overflow-hidden">
                          <div className="h-full bg-gradient-to-r from-[#00f2ff] to-[#ff00ff] opacity-60" style={{ width: `${t.win_rate}%` }} />
                        </div>
                        <span className="text-[9px] font-mono text-white/40">{t.win_rate.toFixed(0)}%</span>
                      </div>
                    </div>
                    <div className="text-right">
                      <TacticalNumber value={t.total_pnl} className="text-sm" color={t.total_pnl >= 0 ? '#00ff9d' : '#ff0055'} />
                      <div className="text-[8px] font-black text-white/20 uppercase tracking-tighter">累计回报</div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          </DashboardModule>

          <DashboardModule title="资产分布情况" subtitle="Allocation" icon={Globe} color="green" className="flex-1">
            <div className="h-full flex flex-col justify-center gap-4">
              <div className="space-y-4">
                {[
                  { label: 'Hyperliquid 节点', val: 65, color: '#00f2ff' },
                  { label: 'OKX 代理节点', val: 35, color: '#ff00ff' },
                ].map(item => (
                  <div key={item.label}>
                    <div className="flex justify-between text-[10px] font-black mb-1.5">
                      <span className="text-white/40">{item.label}</span>
                      <span style={{ color: item.color }}>{item.val}%</span>
                    </div>
                    <div className="h-1.5 w-full bg-white/5 rounded-full overflow-hidden">
                      <div className="h-full transition-all duration-1000" style={{ width: `${item.val}%`, backgroundColor: item.color }} />
                    </div>
                  </div>
                ))}
              </div>
              <div className="mt-2 text-[10px] font-mono text-white/20 text-center uppercase tracking-[0.2em]">数据流全球同步中...</div>
            </div>
          </DashboardModule>
        </div>

        {/* 中心：核心详情与战报 */}
        <div className="col-span-6 flex flex-col gap-6">
          <DashboardModule title="交易员神经性能透视" subtitle="Neural Performance" icon={Layers} className="flex-1">
            {selectedTrader ? (
              <div className="h-full flex flex-col">
                <div className="flex items-start justify-between mb-8">
                  <div className="flex items-center gap-8">
                    <div className="relative w-28 h-28 border-2 border-[#00f2ff]/30 p-1.5 bg-[#00f2ff]/5">
                      <div className="absolute -top-2.5 -left-2.5 w-6 h-6 border-t-4 border-l-4 border-[#00f2ff]" />
                      <div className="absolute -bottom-2.5 -right-2.5 w-6 h-6 border-b-4 border-r-4 border-[#00f2ff]" />
                      <PunkAvatar seed={getTraderAvatar(selectedTrader.trader_id, selectedTrader.trader_name)} size={100} className="rounded-none w-full h-full object-cover" />
                    </div>
                    <div>
                      <div className="flex items-center gap-3">
                        <h2 className="text-5xl font-black tracking-tighter uppercase leading-none italic">{selectedTrader.trader_name}</h2>
                        <div className="px-3 py-1 bg-[#00ff9d]/20 border border-[#00ff9d]/40 text-[#00ff9d] text-[10px] font-black uppercase rounded-sm">正在交易</div>
                      </div>
                      <div className="flex items-center gap-6 mt-5">
                        <div className="flex flex-col">
                          <span className="text-[10px] text-white/30 font-black uppercase">跟单协议</span>
                          <span className="text-sm font-black text-[#ff00ff]">{selectedTrader.mode.toUpperCase()} 2.0</span>
                        </div>
                        <div className="w-px h-8 bg-white/10" />
                        <div className="flex flex-col">
                          <span className="text-[10px] text-white/30 font-black uppercase">资产净值</span>
                          <span className="text-sm font-mono font-black text-[#ffe600]">${selectedTrader.current_equity.toLocaleString()}</span>
                        </div>
                        <div className="w-px h-8 bg-white/10" />
                        <div className="flex flex-col">
                          <span className="text-[10px] text-white/30 font-black uppercase">节点标识</span>
                          <span className="text-sm font-mono font-black text-[#00f2ff]">#{selectedTrader.trader_id.substring(0, 12).toUpperCase()}</span>
                        </div>
                      </div>
                    </div>
                  </div>
                  
                  <div className="flex items-center gap-12 bg-white/5 p-6 border border-white/10 rounded-sm">
                    <DataGauge value={selectedTrader.win_rate} label="胜率均值" color="#00ff9d" />
                    <div className="text-right">
                      <div className="text-[10px] font-black uppercase text-white/30 tracking-[0.3em] mb-2">累计总盈亏</div>
                      <NeonNumber value={selectedTrader.total_pnl} className="text-5xl" color={selectedTrader.total_pnl >= 0 ? '#00ff9d' : '#ff0055'} prefix="$" />
                    </div>
                  </div>
                </div>

                <div className="grid grid-cols-4 gap-6 flex-1">
                  <div className="space-y-4">
                    {[
                      { label: '收益率 (ROI)', value: `${selectedTrader.return_rate.toFixed(2)}%`, icon: TrendingUp, color: '#00ff9d' },
                      { label: '盈亏比 (Factor)', value: selectedTrader.profit_factor.toFixed(2), icon: Target, color: '#00f2ff' },
                      { label: '执行周期 (Trades)', value: selectedTrader.total_trades, icon: Activity, color: '#ff00ff' },
                      { label: '当前持仓 (Units)', value: selectedTrader.position_count, icon: Database, color: '#ffe600' },
                    ].map((item, i) => (
                      <div key={i} className="bg-white/5 border border-white/10 p-4 relative group hover:bg-white/10 transition-all cursor-default">
                        <div className="absolute top-0 left-0 w-1 h-full" style={{ backgroundColor: item.color }} />
                        <div className="text-[10px] font-black text-white/40 uppercase mb-1 tracking-widest">{item.label}</div>
                        <div className="text-2xl font-mono font-black" style={{ color: item.color }}>{item.value}</div>
                      </div>
                    ))}
                  </div>
                  
                  <div className="col-span-3 bg-[#020617]/80 border border-white/10 relative p-8">
                    <div className="absolute top-4 left-6 flex items-center gap-2">
                      <Crosshair size={14} className="text-[#00f2ff] animate-pulse" />
                      <span className="text-xs font-black text-white/50 uppercase tracking-[0.3em]">实时收益增长信号追踪 (Trace)</span>
                    </div>
                    {/* 模拟图表网格 */}
                    <div className="h-full pt-10 relative">
                      <div className="absolute inset-0 grid grid-cols-8 pointer-events-none opacity-20">
                        {[...Array(8)].map((_, i) => <div key={i} className="border-r border-white/10 h-full" />)}
                      </div>
                      <div className="absolute inset-0 grid grid-rows-5 pointer-events-none opacity-20">
                        {[...Array(5)].map((_, i) => <div key={i} className="border-b border-white/10 w-full" />)}
                      </div>
                      {/* 动态波形模拟 */}
                      <div className="relative h-full flex items-end px-2 gap-1">
                        {[...Array(40)].map((_, i) => (
                          <div 
                            key={i} 
                            className="flex-1 bg-gradient-to-t from-[#00f2ff10] to-[#00f2ff80] transition-all hover:to-[#ff00ff] rounded-t-sm" 
                            style={{ height: `${Math.random() * 70 + 10}%`, animationDelay: `${i * 0.05}s` }}
                          />
                        ))}
                      </div>
                    </div>
                  </div>
                </div>
              </div>
            ) : null}
          </DashboardModule>
        </div>

        {/* 右翼：日志与异常 */}
        <div className="col-span-3 flex flex-col gap-6 overflow-hidden">
          <DashboardModule title="战术执行日志" subtitle="Tactical Log" icon={TerminalIcon} color="pink" className="flex-[1.5] overflow-hidden flex flex-col">
            <div className="flex-1 font-mono text-[10px] space-y-2.5 overflow-y-auto custom-cyber-scrollbar">
              <div className="text-[#ff00ff] opacity-60 font-bold tracking-widest">[系统] 安全会话已建立</div>
              <div className="text-[#00f2ff] opacity-60 font-bold tracking-widest">[同步] 实时跟单流已接入</div>
              
              {[...Array(12)].map((_, i) => {
                const trader = traderStats[i % traderStats.length]
                if (!trader) return null
                const side = Math.random() > 0.5 ? '买入' : '卖出'
                const isBuy = side === '买入'
                return (
                  <div key={i} className="flex gap-2 p-2 bg-white/5 border-l-2 border-white/10 hover:bg-white/10 transition-colors">
                    <span className="text-white/20">[{new Date(Date.now() - i * 300000).toLocaleTimeString('zh-CN', { hour12: false })}]</span>
                    <span className="text-[#00f2ff] font-black truncate w-16">{trader.trader_name}</span>
                    <span className="text-white/40">执行</span>
                    <span className={isBuy ? 'text-[#00ff9d]' : 'text-[#ff0055] font-bold'}>{side}</span>
                    <span className="text-white/40 ml-auto font-mono">{Math.random().toFixed(3)} ETH</span>
                  </div>
                )
              })}
            </div>
            <div className="mt-4 pt-3 border-t border-white/10 flex items-center gap-3">
              <div className="w-1.5 h-5 bg-[#ff00ff] animate-pulse" />
              <div className="text-[10px] text-[#ff00ff] font-black uppercase animate-[flicker_0.2s_infinite]">正在监听实时交易封包...</div>
            </div>
          </DashboardModule>

          <DashboardModule title="异常告警监控" subtitle="Alert Wall" icon={Bug} color="yellow" className="flex-1">
            <div className="flex-1 overflow-y-auto pr-2 custom-cyber-scrollbar space-y-3">
              <div className="flex items-start gap-3 p-3 bg-red-500/10 border border-red-500/30 rounded-sm">
                <AlertTriangle size={16} className="text-red-500 shrink-0 mt-0.5" />
                <div>
                  <div className="text-[10px] font-black text-red-500 uppercase">跟单引擎异常</div>
                  <p className="text-[9px] text-red-500/70 leading-tight mt-1">Hyperliquid API 返回 429 频率限制，尝试自动重连中...</p>
                </div>
              </div>
              <div className="flex items-start gap-3 p-3 bg-yellow-500/10 border border-yellow-500/30 rounded-sm opacity-60">
                <Radio size={16} className="text-yellow-500 shrink-0 mt-0.5" />
                <div>
                  <div className="text-[10px] font-black text-yellow-500 uppercase">网络延迟抖动</div>
                  <p className="text-[9px] text-yellow-500/70 leading-tight mt-1">亚太节点延迟超过 150ms，系统已切换至北美备用节点。</p>
                </div>
              </div>
            </div>
          </DashboardModule>
        </div>
      </main>

      {/* --- 底部状态条 --- */}
      <footer className="relative z-10 h-10 border-t border-white/10 bg-black/60 px-10 flex items-center justify-between text-[10px] font-black tracking-[0.3em] uppercase">
        <div className="flex items-center gap-12">
          <div className="flex items-center gap-2">
            <div className="w-2 h-2 rounded-full bg-[#00ff9d]" />
            <span className="text-white/40">安全协议: <span className="text-[#00ff9d]">AES-256-GCM 已激活</span></span>
          </div>
          <div className="flex items-center gap-2">
            <div className="w-2 h-2 rounded-full bg-[#00f2ff] animate-pulse" />
            <span className="text-white/40">数据同步: <span className="text-[#00f2ff]">实时双向流 (Full-Duplex)</span></span>
          </div>
        </div>
        
        <div className="flex items-center gap-8 text-white/20">
          <div className="flex items-center gap-2">
            <Shield size={14} className="text-[#ffe600] opacity-50" />
            <span>加密指挥终端 v4.0.2</span>
          </div>
          <div className="h-4 w-px bg-white/10" />
          <span>终端 ID: <span className="text-white/40 font-mono">TX-CORE-999-STABLE</span></span>
        </div>
      </footer>

      {/* 全局动效注入 */}
      <style>{`
        @keyframes flicker { 0% { opacity: 0.4 } 100% { opacity: 1 } }
        .custom-cyber-scrollbar::-webkit-scrollbar { width: 3px; }
        .custom-cyber-scrollbar::-webkit-scrollbar-track { background: rgba(255,255,255,0.02); }
        .custom-cyber-scrollbar::-webkit-scrollbar-thumb { background: rgba(255,255,255,0.1); border-radius: 10px; }
        .custom-cyber-scrollbar::-webkit-scrollbar-thumb:hover { background: rgba(0,242,255,0.3); }
      `}</style>
    </div>
  )
}
