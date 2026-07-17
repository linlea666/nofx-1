import { useCallback, useEffect, useMemo, useState } from 'react'
import useSWR from 'swr'
import { api } from '../lib/api'
import type {
  CopyEventCategory,
  CopyEventSeverity,
  CopyTradeEvent,
} from '../types'

const PAGE_SIZE = 50

// 导出/查询时间窗（喂 AI 分析）
const WINDOWS: { value: string; label: string }[] = [
  { value: '1h', label: '最近 1 小时' },
  { value: '3h', label: '最近 3 小时' },
  { value: '5h', label: '最近 5 小时' },
  { value: '24h', label: '最近 24 小时' },
  { value: '7d', label: '最近 7 天' },
  { value: '15d', label: '最近 15 天' },
]

const categoryLabels: Record<CopyEventCategory, string> = {
  action: '动作',
  stoploss: '止损',
  reentry: '二次入场',
  takeover: '接手',
  protection: '保护',
  reconcile: '对账',
  error: '异常',
}

const eventTypeLabels: Record<string, string> = {
  OPEN: '开仓',
  ADD: '加仓',
  REDUCE: '减仓',
  CLOSE: '平仓',
  STOP_TRIGGERED: '止损触发',
  STOP_CONFIRMED: '止损确认',
  REENTRY_REQUESTED: '二次入场触发',
  REENTRY_FILLED: '二次入场成交',
  REENTRY_RECOVERED_AFTER_RESTART: '二次入场恢复',
  REENTRY_FAILED: '二次入场失败',
  REENTRY_WINDOW_COLLAPSED: '旧规则窗口关闭',
  GUARD_MANUAL_REENTRY_SIGNAL: '人工重入信号',
  GUARD_MANUAL_REENTRY_CONFIRMED: '重入已确认',
  GUARD_MANUAL_REENTRY_DISMISSED: '重入已忽略',
  AI_ANALYSIS: 'AI 分析',
  PROTECTION_PENDING: '保护建立中',
  PROTECTION_ACTIVE: '保护生效',
  PROTECTIVE_STOP_ACTIVE: '保护生效',
  PROTECTION_RECOVERED: '保护恢复',
  PROTECTION_DEGRADED: '保护异常',
  PROTECTION_CLAMPED: '止损钳紧',
  PROTECTION_COVERAGE_LOW: '保护覆盖不足',
  PROTECTIVE_STOP_GONE: '保护单丢失',
  PROTECTION_VERIFY_UNKNOWN: '保护状态未知',
  PROTECTION_CREATE_FAILED: '保护创建失败',
  GUARD_UNPROTECTABLE: '无法保护',
  GUARD_FORCED_EXIT: '强平兜底',
  GUARD_FORCED_EXIT_FAILED: '强平兜底失败',
  ADDON_RISK_WARNING: '加仓风险预警',
  ACCOUNTING_RECONCILED: '对账完成',
  ACCOUNTING_DELAYED: '对账延迟',
  ACCOUNTING_UNRECOVERABLE: '对账不可恢复',
  BASELINE_CALIBRATED: '基线校准',
  LEADER_CLOSED: '领航员平仓',
  LEADER_REVERSED: '领航员反手',
  ENGINE_START_FAILED: '引擎启动失败',
  BINANCE_CREDENTIALS_EXPIRED: '凭证失效',
}

const statusLabels: Record<string, string> = {
  success: '成功',
  failed: '失败',
  skipped: '跳过',
}

const severityStyle: Record<CopyEventSeverity, { color: string; bg: string }> =
  {
    info: { color: '#848E9C', bg: 'rgba(132,142,156,0.12)' },
    warn: { color: '#F0B90B', bg: 'rgba(240,185,11,0.12)' },
    error: { color: '#F6465D', bg: 'rgba(246,70,93,0.12)' },
  }

const categoryColor: Record<CopyEventCategory, string> = {
  action: '#0ECB81',
  stoploss: '#F6465D',
  reentry: '#F0B90B',
  takeover: '#4A9EFF',
  protection: '#9B8AFB',
  reconcile: '#848E9C',
  error: '#F6465D',
}

const providerLabels: Record<string, string> = {
  okx: 'OKX',
  hyperliquid: 'Hyperliquid',
  binance: 'Binance',
}

const fmtNum = (v: number) =>
  v ? v.toLocaleString(undefined, { maximumFractionDigits: 6 }) : '-'
const fmtPnl = (v: number) => (v ? `${v > 0 ? '+' : ''}${v.toFixed(2)}` : '-')

export function CopyEventLogPage() {
  const [win, setWin] = useState('24h')
  const [provider, setProvider] = useState('')
  const [category, setCategory] = useState('')
  const [severity, setSeverity] = useState('')
  const [symbol, setSymbol] = useState('')
  const [traderID, setTraderID] = useState('')
  const [page, setPage] = useState(0)
  const [expanded, setExpanded] = useState<number | null>(null)
  const [exportWin, setExportWin] = useState('24h')

  const buildParams = useCallback(
    (windowValue: string) => {
      const q = new URLSearchParams({ window: windowValue })
      if (provider) q.set('provider', provider)
      if (category) q.set('category', category)
      if (severity) q.set('severity', severity)
      if (symbol) q.set('symbol', symbol.trim().toUpperCase())
      if (traderID) q.set('trader_id', traderID.trim())
      return `?${q.toString()}`
    },
    [provider, category, severity, symbol, traderID]
  )

  const params = useMemo(() => buildParams(win), [buildParams, win])
  const listParams = `${params}&limit=${PAGE_SIZE}&offset=${page * PAGE_SIZE}`

  useEffect(
    () => setPage(0),
    [win, provider, category, severity, symbol, traderID]
  )

  const { data, isLoading } = useSWR(
    `copy-events-${listParams}`,
    () => api.getCopyEvents(listParams),
    { refreshInterval: 10000 }
  )
  const events = data?.events ?? []
  const total = data?.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  // 全部交易员 id->名称（后端随查询返回，跨用户全局），用于筛选下拉
  const traders = data?.traders ?? {}
  const traderOptions = useMemo(
    () =>
      Object.entries(traders).sort((a, b) =>
        (a[1] || a[0]).localeCompare(b[1] || b[0])
      ),
    [traders]
  )

  const exportEvents = async (format: 'csv' | 'jsonl') => {
    const token = localStorage.getItem('auth_token')
    const res = await fetch(
      `/api/copytrade/events/export${buildParams(exportWin)}&format=${format}`,
      { headers: token ? { Authorization: `Bearer ${token}` } : {} }
    )
    if (!res.ok) return
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `copy-events-${exportWin}.${format}`
    a.click()
    URL.revokeObjectURL(url)
  }

  const selectClass =
    'px-3 py-2 rounded-lg text-sm bg-[#1E2329] border border-[#2B3139] text-[#EAECEF] focus:outline-none focus:border-[#F0B90B]'

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold">跟单事件日志</h1>
          <p className="text-sm text-[#848E9C] mt-1">
            全链条动作与风控事件时间线，用于监控异常、定位问题、追踪排查（保留最近
            30 天）
          </p>
        </div>
        <div className="flex items-center gap-2">
          <select
            value={exportWin}
            onChange={(e) => setExportWin(e.target.value)}
            className={selectClass}
            title="导出时间窗"
          >
            {WINDOWS.map((w) => (
              <option key={w.value} value={w.value}>
                导出{w.label}
              </option>
            ))}
          </select>
          <button
            onClick={() => exportEvents('jsonl')}
            className="px-3 py-2 rounded-lg text-sm bg-[#1E2329] border border-[#2B3139] hover:border-[#F0B90B] text-[#EAECEF]"
          >
            导出 JSONL
          </button>
          <button
            onClick={() => exportEvents('csv')}
            className="px-3 py-2 rounded-lg text-sm bg-[#1E2329] border border-[#2B3139] hover:border-[#F0B90B] text-[#EAECEF]"
          >
            导出 CSV
          </button>
        </div>
      </div>

      {/* 过滤器 */}
      <div className="grid md:grid-cols-6 gap-3">
        <select
          value={win}
          onChange={(e) => setWin(e.target.value)}
          className={selectClass}
        >
          {WINDOWS.map((w) => (
            <option key={w.value} value={w.value}>
              {w.label}
            </option>
          ))}
        </select>
        <select
          value={provider}
          onChange={(e) => setProvider(e.target.value)}
          className={selectClass}
        >
          <option value="">全部数据源</option>
          <option value="okx">OKX</option>
          <option value="hyperliquid">Hyperliquid</option>
          <option value="binance">Binance</option>
        </select>
        <select
          value={category}
          onChange={(e) => setCategory(e.target.value)}
          className={selectClass}
        >
          <option value="">全部分类</option>
          {(Object.keys(categoryLabels) as CopyEventCategory[]).map((k) => (
            <option key={k} value={k}>
              {categoryLabels[k]}
            </option>
          ))}
        </select>
        <select
          value={severity}
          onChange={(e) => setSeverity(e.target.value)}
          className={selectClass}
        >
          <option value="">全部严重度</option>
          <option value="info">info 常态</option>
          <option value="warn">warn 关注</option>
          <option value="error">error 异常</option>
        </select>
        <input
          value={symbol}
          onChange={(e) => setSymbol(e.target.value)}
          placeholder="交易对，如 BTCUSDT"
          className={selectClass}
        />
        <select
          value={traderID}
          onChange={(e) => setTraderID(e.target.value)}
          className={selectClass}
        >
          <option value="">全部交易员</option>
          {traderOptions.map(([id, name]) => (
            <option key={id} value={id}>
              {name || id}
            </option>
          ))}
        </select>
      </div>

      <p className="text-xs text-[#5E6673]">
        提示：止损介入 / 二次入场 / 人工接手 / AI 接收 / 保护等风控事件仅 OKX
        Copy Guard 产生； Hyperliquid / Binance
        仅有开仓/加仓/减仓/平仓动作与执行错误事件。单个仓位的细致全程记录请见
        Copy Guard 页面。
      </p>

      {/* 事件表格 */}
      <div className="binance-card overflow-x-auto">
        <table className="w-full text-xs">
          <thead className="text-left border-b border-[#2B3139] text-[#848E9C]">
            <tr>
              <th className="py-2 px-2">时间</th>
              <th className="py-2 px-2">交易员</th>
              <th className="py-2 px-2">交易对</th>
              <th className="py-2 px-2">数据源</th>
              <th className="py-2 px-2">分类</th>
              <th className="py-2 px-2">事件</th>
              <th className="py-2 px-2">方向</th>
              <th className="py-2 px-2">状态</th>
              <th className="py-2 px-2 text-right">价格</th>
              <th className="py-2 px-2 text-right">名义(USDT)</th>
              <th className="py-2 px-2 text-right">盈亏</th>
              <th className="py-2 px-2">操作者</th>
              <th className="py-2 px-2">摘要</th>
              <th className="py-2 px-2">周期</th>
            </tr>
          </thead>
          <tbody>
            {events.map((e: CopyTradeEvent) => {
              const sev = severityStyle[e.severity] ?? severityStyle.info
              const isOpen = expanded === e.id
              return (
                <tr
                  key={e.id}
                  onClick={() => setExpanded(isOpen ? null : e.id)}
                  className="border-b border-[#1E2329] hover:bg-[#1E2329] cursor-pointer align-top"
                  style={{ borderLeft: `3px solid ${sev.color}` }}
                >
                  <td className="py-2 px-2 whitespace-nowrap text-[#B7BDC6]">
                    {new Date(e.created_at).toLocaleString()}
                  </td>
                  <td
                    className="py-2 px-2 max-w-[140px] truncate text-[#EAECEF]"
                    title={e.trader_id}
                  >
                    {e.trader_name || e.trader_id || '-'}
                  </td>
                  <td className="py-2 px-2 font-medium">{e.symbol || '-'}</td>
                  <td className="py-2 px-2 text-[#848E9C]">
                    {providerLabels[e.provider_type] || e.provider_type || '-'}
                  </td>
                  <td className="py-2 px-2">
                    <span
                      className="px-2 py-0.5 rounded text-[11px]"
                      style={{
                        color: categoryColor[e.category] ?? '#848E9C',
                        background: 'rgba(255,255,255,0.04)',
                      }}
                    >
                      {categoryLabels[e.category] ?? e.category}
                    </span>
                  </td>
                  <td className="py-2 px-2 whitespace-nowrap">
                    {eventTypeLabels[e.event_type] ?? e.event_type}
                  </td>
                  <td className="py-2 px-2">
                    {e.side === 'long' ? (
                      <span style={{ color: '#0ECB81' }}>多</span>
                    ) : e.side === 'short' ? (
                      <span style={{ color: '#F6465D' }}>空</span>
                    ) : (
                      '-'
                    )}
                  </td>
                  <td className="py-2 px-2">
                    {e.status ? (
                      <span
                        style={{ color: sev.color, background: sev.bg }}
                        className="px-2 py-0.5 rounded text-[11px]"
                      >
                        {statusLabels[e.status] ?? e.status}
                      </span>
                    ) : (
                      '-'
                    )}
                  </td>
                  <td className="py-2 px-2 text-right text-[#B7BDC6]">
                    {fmtNum(e.price)}
                  </td>
                  <td className="py-2 px-2 text-right text-[#B7BDC6]">
                    {fmtNum(e.notional)}
                  </td>
                  <td
                    className="py-2 px-2 text-right"
                    style={{
                      color:
                        e.pnl > 0
                          ? '#0ECB81'
                          : e.pnl < 0
                            ? '#F6465D'
                            : '#848E9C',
                    }}
                  >
                    {fmtPnl(e.pnl)}
                  </td>
                  <td className="py-2 px-2 text-[#848E9C]">
                    {e.operator
                      ? e.operator.startsWith('ai')
                        ? `AI(${e.operator})`
                        : `人工(${e.operator})`
                      : '-'}
                  </td>
                  <td className="py-2 px-2 max-w-[320px]">
                    <div className={isOpen ? '' : 'truncate'}>
                      {e.summary || '-'}
                    </div>
                    {isOpen && e.detail && (
                      <pre className="mt-2 p-2 rounded bg-[#0B0E11] text-[11px] text-[#848E9C] overflow-x-auto whitespace-pre-wrap">
                        {JSON.stringify(e.detail, null, 2)}
                      </pre>
                    )}
                  </td>
                  <td className="py-2 px-2">
                    {e.cycle_id > 0 ? (
                      <button
                        onClick={(ev) => {
                          ev.stopPropagation()
                          window.location.href = '/copy-guard'
                        }}
                        className="text-[#F0B90B] hover:underline"
                        title="查看该周期细致记录"
                      >
                        #{e.cycle_id}
                      </button>
                    ) : (
                      '-'
                    )}
                  </td>
                </tr>
              )
            })}
            {events.length === 0 && (
              <tr>
                <td colSpan={14} className="py-10 text-center text-[#5E6673]">
                  {isLoading ? '加载中…' : '当前时间窗与过滤条件下暂无事件'}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {/* 分页 */}
      <div className="flex items-center justify-between text-sm text-[#848E9C]">
        <span>共 {total} 条</span>
        <div className="flex items-center gap-3">
          <button
            disabled={page === 0}
            onClick={() => setPage((p) => Math.max(0, p - 1))}
            className="px-3 py-1.5 rounded-lg bg-[#1E2329] border border-[#2B3139] disabled:opacity-40"
          >
            上一页
          </button>
          <span>
            第 {page + 1} / {totalPages} 页
          </span>
          <button
            disabled={page + 1 >= totalPages}
            onClick={() => setPage((p) => p + 1)}
            className="px-3 py-1.5 rounded-lg bg-[#1E2329] border border-[#2B3139] disabled:opacity-40"
          >
            下一页
          </button>
        </div>
      </div>
    </div>
  )
}
