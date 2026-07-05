import { useEffect, useMemo, useState } from 'react'
import useSWR from 'swr'
import {
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { api } from '../lib/api'
import type { CopyGuardCycle } from '../types'

const money = (v: number) => `${v >= 0 ? '+' : ''}${v.toFixed(2)} USDT`

const statusLabels: Record<string, string> = {
  FOLLOWING: '正常跟随',
  STOP_TRIGGERED: '止损已触发',
  STOPPED_WATCHING: '止损后观察',
  REENTRY_PENDING: '等待重入成交',
  FOLLOWING_REENTRY: '重入后跟随',
  LEADER_CLOSED: '领航员已平仓',
  LEADER_REVERSED: '领航员已反手',
  ATTEMPTS_EXHAUSTED: '重入次数用尽·等待领航员平仓',
  WATCH_TIMEOUT: '观察超时·等待领航员平仓',
  CYCLE_LOSS_CAPPED: '周期亏损熔断·等待领航员平仓（v5 前历史）',
  GUARD_UNPROTECTABLE: '无法保护·裸跟中（高危）',
}
const protectionLabels: Record<string, string> = {
  PENDING: '待建立',
  VERIFIED: '保护有效',
  UNKNOWN: '状态未知',
  DEGRADED: '保护异常',
  TRIGGERED: '已触发',
  CANCELED: '已撤销',
  CLAMPED: '已保护·止损被强平价挤紧',
  UNPROTECTABLE: '无法保护·裸跑（高危）',
}
const accountingLabels: Record<string, string> = {
  OPEN: '交易进行中',
  PENDING: '自动对账中',
  RECONCILED: '已对账',
  DELAYED: '对账延迟·自动重试中',
  UNRECOVERABLE: '数据不可自动恢复',
  NEEDS_REVIEW: '对账延迟·自动重试中', // 兼容旧导出数据
  LEGACY_UNVERIFIED: '历史未验证',
}
const attemptLabels: Record<string, string> = {
  OPEN: '持仓中',
  STOPPED: '已止损',
  CLOSED: '已平仓',
}
const eventLabels: Record<string, string> = {
  INITIAL_ENTRY_FILLED: '首次跟随成交',
  PROTECTIVE_STOP_ACTIVE: '保护单已生效',
  PROTECTION_RETRY: '重试建立保护',
  PROTECTION_CREATE_FAILED: '保护单创建失败',
  PROTECTION_VERIFY_UNKNOWN: '保护单无法验证',
  PROTECTION_DEGRADED: '保护已降级',
  PROTECTION_COVERAGE_LOW: '保护覆盖不足',
  PROTECTION_RECOVERED: '保护已恢复',
  STOP_TRIGGERED: '保护止损触发',
  REENTRY_FILLED: '重入成交',
  LEADER_CLOSED: '领航员平仓',
  LEADER_REVERSED: '领航员反手',
  ACCOUNTING_IDENTITY_CAPTURED: '已记录跟单仓位',
  ACCOUNTING_IDENTITY_CAPTURE_FAILED: '跟单仓位识别失败',
  ACCOUNTING_RECONCILED: '实际盈亏已对账',
  ACCOUNTING_NEEDS_REVIEW: '对账延迟（历史事件）',
  ACCOUNTING_DELAYED: 'OKX 数据延迟，自动重试中',
  ACCOUNTING_UNRECOVERABLE: '对账数据不可自动恢复',
  PROTECTIVE_STOP_ADOPTED: '接管已存在的保护单',
  PROTECTIVE_STOP_TERMINAL: '保护单已自然终结',
  LIQ_PRICE_IGNORED: '强平价方向异常已忽略',
  BASELINE_CALIBRATED: '基线已用领航员历史校准',
  CYCLE_BACKFILLED: '存量仓位补建生命周期',
  STOP_CONFIRMED: '保护止损确认（间接检测）',
  ATTEMPT_RECONCILED: '单次尝试盈亏已对账',
  PROTECTIVE_STOP_GONE: '保护单消失（仓位已平）',
  PROTECTION_CLAMPED: '止损价被强平价挤紧（保护降级）',
  GUARD_UNPROTECTABLE: '确认无法建立保护',
  GUARD_FORCED_EXIT: '无法保护·强制离场',
  REENTRY_FAILED: '重入下单失败',
  REENTRY_REQUESTED: '重入条件满足，已请求下单',
  REENTRY_RECOVERED_AFTER_RESTART: '重启后重入状态已恢复',
  REENTRY_RECOVERY_PENDING: '重启后重入状态待确认',
  ADDON_RISK_WARNING: '加仓风险告警（仍跟随）',
  ADDON_SKIPPED_BUDGET: '加仓超预算被拦截（旧版）',
  CYCLE_LOSS_BREAKER: '周期亏损熔断触发',
  REENTRY_GATE_CHANGED: '重入门控条件变化',
  WATCH_RESUMED: '观察采样断档后恢复',
  WATCH_SUMMARY: '观察期收尾统计',
}

// 观察期采样的门控原因（copy_guard_watch_samples.gate / REENTRY_GATE_CHANGED metadata）
const gateLabels: Record<string, string> = {
  REENTRY_DISABLED: '重入未启用',
  REENTRY_DISABLED_NOISE: '噪音档禁止重入（止损距离过窄）',
  COOLDOWN: '冷却中',
  ATR_EXPANSION: '波动扩张超限',
  CHASE_EXCEEDED: '超出追价上限',
  PRICE_NOT_RETURNED: '价格未回归',
  REENTRY_CANDIDATE: '条件满足·连续确认中',
  MIN_NOTIONAL: '金额低于阈值',
  REENTRY_UNPROTECTABLE: '重入后无法建立止损·放弃',
  REENTRY_TRIGGERED: '已触发重入',
  ATTEMPTS_EXHAUSTED: '重入次数用尽',
  WATCH_TIMEOUT: '观察超时',
  CYCLE_LOSS_CAPPED: '周期亏损熔断（v5 前历史）',
}

const baselineSourceLabels: Record<string, string> = {
  '': '真实跟随结果',
  last_observed: '估算·待领航员历史补全',
  leader_history: '领航员公共历史校准',
}

const localized = (labels: Record<string, string>, value: string) =>
  labels[value] ?? value
const sideLabel = (side: string) =>
  side.toLowerCase() === 'long'
    ? '多'
    : side.toLowerCase() === 'short'
      ? '空'
      : side
const dateLabel = (value?: string) => {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString()
}

export function CopyGuardPage() {
  const [days, setDays] = useState(30)
  const [queryNow] = useState(() => Date.now())
  const [selected, setSelected] = useState<number | null>(null)
  const [traderID, setTraderID] = useState('')
  const [leaderID, setLeaderID] = useState('')
  const [symbol, setSymbol] = useState('')
  const [status, setStatus] = useState('')
  const [resultType, setResultType] = useState('')
  const [page, setPage] = useState(0)
  const params = useMemo(() => {
    const from = new Date(queryNow - days * 86400000).toISOString()
    const q = new URLSearchParams({ from })
    if (traderID) q.set('trader_id', traderID)
    if (leaderID) q.set('leader_id', leaderID)
    if (symbol) q.set('symbol', symbol)
    if (status) q.set('status', status)
    if (resultType) q.set('result_type', resultType)
    return `?${q.toString()}`
  }, [days, traderID, leaderID, symbol, status, resultType, queryNow])
  const cycleParams = `${params}&limit=50&offset=${page * 50}`
  useEffect(
    () => setPage(0),
    [days, traderID, leaderID, symbol, status, resultType]
  )
  const { data: summary } = useSWR(
    `copy-guard-summary-${params}`,
    () => api.getCopyGuardSummary(params),
    { refreshInterval: 30000 }
  )
  const { data: cycles = [] } = useSWR<CopyGuardCycle[]>(
    `copy-guard-cycles-${cycleParams}`,
    () => api.getCopyGuardCycles(cycleParams),
    { refreshInterval: 30000 }
  )
  const { data: detail } = useSWR(
    selected ? `copy-guard-cycle-${selected}` : null,
    () => api.getCopyGuardCycle(selected!)
  )
  const timeline = useMemo(() => {
    if (!detail) return []
    return detail.events.reduce<
      Array<{
        event: (typeof detail.events)[number]
        count: number
        firstAt: string
        lastAt: string
      }>
    >((items, event) => {
      const previous = items[items.length - 1]
      if (
        previous &&
        event.type === 'PROTECTION_RETRY' &&
        previous.event.type === event.type
      ) {
        previous.count += 1
        previous.lastAt = event.created_at
        return items
      }
      items.push({
        event,
        count: 1,
        firstAt: event.created_at,
        lastAt: event.created_at,
      })
      return items
    }, [])
  }, [detail])
  const watchSummary = useMemo(() => {
    const ev = detail?.events.find((e) => e.type === 'WATCH_SUMMARY')
    return ev?.metadata as
      | {
          price_saved_usd?: number
          last_stop_price?: number
          leader_close_price?: number
          watch_seconds?: number
          sample_count?: number
          first_recovery_seconds?: number
          max_favorable_excursion?: number
          max_adverse_excursion?: number
          blocked_when_recovered?: Record<string, number>
          leader_addons?: number
          leader_reductions?: number
        }
      | undefined
  }, [detail])
  const watchChart = useMemo(
    () =>
      (detail?.watch_samples ?? []).map((w) => ({
        time: new Date(w.created_at).toLocaleTimeString(),
        标记价: w.mark_price || null,
        重入边界: w.reentry_boundary > 0 ? w.reentry_boundary : null,
        追价上限: w.chase_limit > 0 ? w.chase_limit : null,
      })),
    [detail]
  )
  const gateCounts = useMemo(() => {
    const counts: Record<string, number> = {}
    for (const w of detail?.watch_samples ?? []) {
      counts[w.gate] = (counts[w.gate] ?? 0) + 1
    }
    return Object.entries(counts).sort((a, b) => b[1] - a[1])
  }, [detail])
  const trend = useMemo(() => {
    return (summary?.trend ?? []).reduce<{
      total: number
      points: Array<
        {
          date: string
          actual: number
          baseline: number
          net_effect: number
        } & {
          cumulative: number
        }
      >
    }>(
      (acc, point) => {
        const cumulative = acc.total + point.net_effect
        return {
          total: cumulative,
          points: [...acc.points, { ...point, cumulative }],
        }
      },
      { total: 0, points: [] }
    ).points
  }, [summary?.trend])
  const exportData = async (format: 'csv' | 'jsonl') => {
    const token = localStorage.getItem('auth_token')
    const res = await fetch(
      `/api/copytrade/risk/export${params}&format=${format}`,
      { headers: token ? { Authorization: `Bearer ${token}` } : {} }
    )
    if (!res.ok) throw new Error('导出失败')
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `copy-guard.${format}`
    a.click()
    URL.revokeObjectURL(url)
  }
  const exportCycle = async (id: number) => {
    const token = localStorage.getItem('auth_token')
    const res = await fetch(`/api/copytrade/risk/cycles/${id}/export`, {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    })
    if (!res.ok) throw new Error('单个仓位日志导出失败')
    const blob = await res.blob()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `copy-guard-cycle-${id}.jsonl`
    a.click()
    URL.revokeObjectURL(url)
  }
  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold">Copy Guard 止损保护统计</h1>
          <p className="text-sm text-[#848E9C] mt-1">
            真实交易结果与未启用兜底估算基线对比
          </p>
        </div>
        <div className="flex gap-2">
          <select
            value={days}
            onChange={(e) => setDays(Number(e.target.value))}
            className="bg-[#181A20] border border-[#2B3139] rounded px-3 py-2"
          >
            <option value={1}>今天</option>
            <option value={7}>7天</option>
            <option value={30}>30天</option>
            <option value={90}>90天</option>
          </select>
          <button
            onClick={() => exportData('csv')}
            className="px-3 py-2 bg-[#2B3139] rounded"
          >
            CSV
          </button>
          <button
            onClick={() => exportData('jsonl')}
            className="px-3 py-2 bg-[#2B3139] rounded"
          >
            JSONL
          </button>
        </div>
      </div>
      <div className="grid md:grid-cols-5 gap-3">
        <input
          value={traderID}
          onChange={(e) => setTraderID(e.target.value)}
          placeholder="跟单员 ID"
          className="bg-[#181A20] border border-[#2B3139] rounded px-3 py-2"
        />
        <input
          value={leaderID}
          onChange={(e) => setLeaderID(e.target.value)}
          placeholder="领航员 ID"
          className="bg-[#181A20] border border-[#2B3139] rounded px-3 py-2"
        />
        <input
          value={symbol}
          onChange={(e) => setSymbol(e.target.value.toUpperCase())}
          placeholder="交易对，如 BTCUSDT"
          className="bg-[#181A20] border border-[#2B3139] rounded px-3 py-2"
        />
        <select
          value={status}
          onChange={(e) => setStatus(e.target.value)}
          className="bg-[#181A20] border border-[#2B3139] rounded px-3 py-2"
        >
          <option value="">全部状态</option>
          <option value="FOLLOWING">正常跟随</option>
          <option value="STOP_TRIGGERED">止损已触发</option>
          <option value="STOPPED_WATCHING">保护观察</option>
          <option value="REENTRY_PENDING">等待重入成交</option>
          <option value="FOLLOWING_REENTRY">重入跟随</option>
          <option value="LEADER_CLOSED">领航员平仓</option>
          <option value="LEADER_REVERSED">领航员反手</option>
          <option value="ATTEMPTS_EXHAUSTED">次数耗尽</option>
          <option value="WATCH_TIMEOUT">观察超时</option>
          <option value="GUARD_UNPROTECTABLE">无法保护·裸跟</option>
          <option value="CYCLE_LOSS_CAPPED">周期亏损熔断（历史）</option>
        </select>
        <select
          value={resultType}
          onChange={(e) => setResultType(e.target.value)}
          className="bg-[#181A20] border border-[#2B3139] rounded px-3 py-2"
        >
          <option value="">全部结果</option>
          <option value="improved">净改善</option>
          <option value="cost">机会成本</option>
          <option value="neutral">无变化</option>
          <option value="open">进行中</option>
        </select>
      </div>
      <div className="grid md:grid-cols-3 gap-4">
        <Card
          title="帮你少亏"
          value={money(summary?.avoided_loss ?? 0)}
          color="#0ECB81"
        />
        <Card
          title="提前离场代价"
          value={money(-(summary?.opportunity_cost ?? 0))}
          color="#F6465D"
        />
        <Card
          title="综合净改善"
          value={money(summary?.net_guard_effect ?? 0)}
          color={(summary?.net_guard_effect ?? 0) >= 0 ? '#0ECB81' : '#F6465D'}
          note={
            (summary?.estimated_baseline_cycles ?? 0) > 0
              ? `其中 ${money(summary?.estimated_net_guard_effect ?? 0)} 来自 ${summary?.estimated_baseline_cycles} 个估算基线周期（待领航员历史补全）；实测口径 ${money((summary?.net_guard_effect ?? 0) - (summary?.estimated_net_guard_effect ?? 0))}`
              : undefined
          }
        />
      </div>
      {(summary?.unprotectable_count ?? 0) > 0 && (
        <div className="border border-[#F6465D] bg-[#F6465D]/10 rounded-lg p-4 text-sm text-[#F6465D]">
          高危：当前有 {summary?.unprotectable_count}{' '}
          个活跃仓位确认无法建立有效止损保护，正在裸跟运行（仅受账户兜底线与交易所强平约束）。建议立即人工检查或手动平仓。
        </div>
      )}
      {(summary?.clamped_count ?? 0) > 0 && (
        <div className="border border-[#F0B90B] bg-[#F0B90B]/10 rounded-lg p-4 text-sm text-[#F0B90B]">
          当前有 {summary?.clamped_count}{' '}
          个活跃仓位的止损价被强平安全线钳紧（比策略目标更近），可能被正常波动提前扫损。这是高杠杆下的保护降级信号。
        </div>
      )}
      {(summary?.pending_protection_count ?? 0) +
        (summary?.unknown_count ?? 0) +
        (summary?.degraded_count ?? 0) >
        0 && (
        <div className="border border-[#F0B90B] bg-[#F0B90B]/10 rounded-lg p-4 text-sm text-[#F0B90B]">
          当前有{' '}
          {(summary?.pending_protection_count ?? 0) +
            (summary?.unknown_count ?? 0) +
            (summary?.degraded_count ?? 0)}{' '}
          个活跃仓位的保护单正在自动重试建立/验证。若持续超过 10
          分钟系统会发送升级告警，届时建议人工检查 OKX。
        </div>
      )}
      {(summary?.accounting_pending_count ?? 0) +
        (summary?.accounting_delayed_count ?? 0) >
        0 && (
        <div className="border border-[#F0B90B] bg-[#F0B90B]/10 rounded-lg p-4 text-sm text-[#F0B90B]">
          当前有 {summary?.accounting_pending_count ?? 0} 个周期正在自动对账，
          {summary?.accounting_delayed_count ?? 0}{' '}
          个周期因 OKX 数据延迟系统继续自动重试。这些周期暂不计入保护效果，无需人工处理。
        </div>
      )}
      {(summary?.accounting_unrecoverable_count ?? 0) > 0 && (
        <div className="border border-[#F6465D] bg-[#F6465D]/10 rounded-lg p-4 text-sm text-[#F6465D]">
          有 {summary?.accounting_unrecoverable_count ?? 0}{' '}
          个周期的对账数据不可自动恢复，请查看日志并人工核对。
        </div>
      )}
      {(summary?.ignored_count ?? 0) > 0 && (
        <div className="border border-[#2B3139] bg-[#181A20] rounded-lg p-4 text-sm text-[#848E9C]">
          当前忽略 {summary?.ignored_count ?? 0} 个启用 v4
          前已存在的存量仓位；它们继续按原配置运行，不纳入 Copy Guard 统计。
        </div>
      )}
      <div className="grid grid-cols-2 md:grid-cols-5 gap-3 text-sm">
        <Metric label="生命周期" value={summary?.cycle_count ?? 0} />
        <Metric label="止损次数" value={summary?.stop_count ?? 0} />
        <Metric label="重入次数" value={summary?.reentry_count ?? 0} />
        <Metric label="费用" value={(summary?.fees ?? 0).toFixed(2)} />
        <Metric label="资金费" value={(summary?.funding_fee ?? 0).toFixed(2)} />
        <Metric
          label="强平罚金"
          value={(summary?.liquidation_penalty ?? 0).toFixed(2)}
        />
        <Metric label="滑点" value={(summary?.slippage ?? 0).toFixed(2)} />
        <Metric label="保护有效" value={summary?.protected_count ?? 0} />
        <Metric
          label="保护待建立"
          value={summary?.pending_protection_count ?? 0}
        />
        <Metric label="保护未知" value={summary?.unknown_count ?? 0} />
        <Metric label="保护降级" value={summary?.degraded_count ?? 0} />
        <Metric label="止损被钳紧" value={summary?.clamped_count ?? 0} />
        <Metric label="无法保护" value={summary?.unprotectable_count ?? 0} />
        <Metric
          label="平均覆盖率"
          value={`${((summary?.average_coverage ?? 0) * 100).toFixed(1)}%`}
        />
        <Metric
          label="保护缺失时长(历史累计)"
          value={`${Math.round((summary?.protection_missing_seconds ?? 0) / 60)} 分钟`}
        />
        <Metric label="第1次重入" value={summary?.reentry_first ?? 0} />
        <Metric label="第2次重入" value={summary?.reentry_second ?? 0} />
        <Metric label="第3次及以上" value={summary?.reentry_third_plus ?? 0} />
        <Metric
          label="重入成功率"
          value={
            (summary?.reentry_sample_count ?? 0) > 0
              ? `${((summary?.reentry_success_rate ?? 0) * 100).toFixed(1)}%（${summary?.reentry_sample_count} 样本${(summary?.reentry_sample_count ?? 0) < 10 ? '·样本过少仅供参考' : ''}）`
              : '暂无样本'
          }
        />
        <Metric
          label="误杀率"
          value={
            (summary?.stopped_cycle_count ?? 0) > 0
              ? `${((summary?.false_kill_rate ?? 0) * 100).toFixed(1)}%（${summary?.false_kill_count}/${summary?.stopped_cycle_count}${(summary?.stopped_cycle_count ?? 0) < 10 ? '·样本过少仅供参考' : ''}）`
              : '暂无样本'
          }
        />
        <Metric label="对账中" value={summary?.accounting_pending_count ?? 0} />
        <Metric
          label="对账延迟"
          value={summary?.accounting_delayed_count ?? 0}
        />
        <Metric
          label="不可自动恢复"
          value={summary?.accounting_unrecoverable_count ?? 0}
        />
        <Metric
          label="历史未验证"
          value={summary?.legacy_unverified_count ?? 0}
        />
      </div>
      {trend.length > 0 && (
        <div className="h-72 border border-[#2B3139] rounded-lg bg-[#181A20] p-4">
          <div className="text-sm font-medium mb-3">
            实际结果、未兜底估算与累计净效果
          </div>
          <ResponsiveContainer width="100%" height="90%">
            <LineChart data={trend}>
              <XAxis dataKey="date" stroke="#848E9C" />
              <YAxis stroke="#848E9C" />
              <Tooltip
                contentStyle={{
                  background: '#181A20',
                  border: '1px solid #2B3139',
                }}
              />
              <Legend />
              <Line
                type="monotone"
                dataKey="actual"
                name="实际净盈亏"
                stroke="#0ECB81"
                dot={false}
              />
              <Line
                type="monotone"
                dataKey="baseline"
                name="未兜底估算"
                stroke="#848E9C"
                dot={false}
              />
              <Line
                type="monotone"
                dataKey="cumulative"
                name="累计净效果"
                stroke="#F0B90B"
                dot={false}
              />
            </LineChart>
          </ResponsiveContainer>
        </div>
      )}
      <div className="overflow-x-auto border border-[#2B3139] rounded-lg">
        <table className="w-full text-sm">
          <thead className="bg-[#181A20] text-[#848E9C]">
            <tr>
              {[
                '时间',
                '跟单员',
                '领航员',
                '交易对',
                '方向',
                '状态',
                '对账',
                '保护健康',
                '止损/重入',
                '实际盈亏',
                '未兜底估算',
                '跟单偏差',
                '净效果',
                '操作',
              ].map((x) => (
                <th key={x} className="p-3 text-left">
                  {x}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {cycles.map((c) => (
              <tr
                key={c.id}
                onClick={() => setSelected(selected === c.id ? null : c.id)}
                className="border-t border-[#2B3139] cursor-pointer hover:bg-[#181A20]"
              >
                <td className="p-3">{dateLabel(c.opened_at)}</td>
                <td className="p-3" title={c.trader_id}>
                  {c.trader_name || c.trader_id}
                </td>
                <td className="p-3">{c.leader_id}</td>
                <td className="p-3">{c.symbol}</td>
                <td className="p-3">{sideLabel(c.side)}</td>
                <td className="p-3">{localized(statusLabels, c.status)}</td>
                <td className="p-3">
                  {localized(accountingLabels, c.accounting_status)}
                </td>
                <td
                  className={`p-3 ${
                    c.closed_at
                      ? 'text-[#848E9C]'
                      : c.protection_status === 'VERIFIED'
                        ? 'text-[#0ECB81]'
                        : c.protection_status === 'UNKNOWN' ||
                            c.protection_status === 'DEGRADED' ||
                            c.protection_status === 'UNPROTECTABLE'
                          ? 'text-[#F6465D] font-bold'
                          : 'text-[#F0B90B]'
                  }`}
                >
                  {c.closed_at &&
                  (c.protection_status === 'UNKNOWN' ||
                    c.protection_status === 'DEGRADED' ||
                    c.protection_status === 'PENDING' ||
                    c.protection_status === 'UNPROTECTABLE')
                    ? '已结束'
                    : `${localized(protectionLabels, c.protection_status)} · ${(c.protection_coverage * 100).toFixed(0)}%`}
                </td>
                <td className="p-3">
                  {c.stop_count}/{c.reentry_count}
                </td>
                <td className="p-3">
                  {c.accounting_status === 'RECONCILED'
                    ? money(c.actual_pnl)
                    : localized(accountingLabels, c.accounting_status)}
                </td>
                <td className="p-3">{money(c.baseline_pnl)}</td>
                <td className="p-3">
                  {c.accounting_status === 'RECONCILED'
                    ? money(c.tracking_difference)
                    : '-'}
                </td>
                <td
                  className={`p-3 ${c.net_guard_effect >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}
                >
                  {c.accounting_status === 'RECONCILED'
                    ? money(c.net_guard_effect)
                    : '不计入'}
                </td>
                <td className="p-3">
                  <button
                    onClick={(event) => {
                      event.stopPropagation()
                      void exportCycle(c.id)
                    }}
                    className="whitespace-nowrap rounded bg-[#2B3139] px-2 py-1 hover:bg-[#3B424C]"
                  >
                    导出日志
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {cycles.length === 0 && (
          <div className="p-10 text-center text-[#848E9C]">
            当前没有启用 v4 后产生的新仓位。已有存量仓位按照配置保持忽略，不纳入
            Copy Guard 统计。
          </div>
        )}
      </div>
      <div className="flex justify-end gap-2">
        <button
          disabled={page === 0}
          onClick={() => setPage((x) => Math.max(0, x - 1))}
          className="px-3 py-2 bg-[#2B3139] rounded disabled:opacity-40"
        >
          上一页
        </button>
        <span className="px-3 py-2 text-sm text-[#848E9C]">
          第 {page + 1} 页
        </span>
        <button
          disabled={cycles.length < 50}
          onClick={() => setPage((x) => x + 1)}
          className="px-3 py-2 bg-[#2B3139] rounded disabled:opacity-40"
        >
          下一页
        </button>
      </div>
      {detail && (
        <div className="border border-[#2B3139] rounded-lg p-4">
          <div className="mb-3 flex items-center justify-between gap-3">
            <div>
              <h2 className="font-bold">
                {detail.cycle.trader_name || detail.cycle.trader_id} ·{' '}
                {detail.cycle.symbol} {sideLabel(detail.cycle.side)}单
              </h2>
              <div
                className="text-xs text-[#848E9C]"
                title={detail.cycle.trader_id}
              >
                生命周期 #{detail.cycle.id} · 跟单员ID {detail.cycle.trader_id}
              </div>
            </div>
            <button
              onClick={() => void exportCycle(detail.cycle.id)}
              className="rounded bg-[#2B3139] px-3 py-2 text-sm"
            >
              导出本仓位JSONL
            </button>
          </div>
          <div className="grid md:grid-cols-3 gap-3 mb-4 text-sm">
            <Metric
              label="保护状态"
              value={`${localized(protectionLabels, detail.cycle.protection_status)} / ${(detail.cycle.protection_coverage * 100).toFixed(0)}%`}
            />
            <Metric
              label="保护单"
              value={detail.protection?.algo_id || '尚未确认'}
            />
            <Metric
              label="自动重试"
              value={`${detail.cycle.protection_retries} 次 · 最后 ${dateLabel(detail.cycle.protection_last_retry_at)}`}
            />
            <Metric
              label="生命周期状态"
              value={localized(statusLabels, detail.cycle.status)}
            />
            <Metric
              label="对账状态"
              value={localized(
                accountingLabels,
                detail.cycle.accounting_status
              )}
            />
            <Metric
              label="基线来源"
              value={localized(
                baselineSourceLabels,
                detail.cycle.baseline_source ?? ''
              )}
            />
            <Metric
              label="跟单执行偏差"
              value={
                detail.cycle.accounting_status === 'RECONCILED'
                  ? money(detail.cycle.tracking_difference)
                  : '-'
              }
            />
          </div>
          {detail.cycle.protection_error && (
            <div className="mb-4 text-sm text-[#F6465D]">
              最后错误：{detail.cycle.protection_error}
            </div>
          )}
          {detail.cycle.accounting_error && (
            <div className="mb-4 text-sm text-[#F0B90B]">
              对账说明：{detail.cycle.accounting_error}
            </div>
          )}
          <div className="mb-4 text-xs text-[#848E9C] break-all">
            策略快照：{detail.cycle.policy_snapshot}
          </div>
          {watchSummary && (
            <div className="mb-4 rounded border border-[#2B3139] bg-[#181A20] p-4 text-sm">
              <div className="mb-2 font-medium">
                观察期复盘（止损出局 → 领航员离场）
              </div>
              <div className="grid md:grid-cols-3 gap-3">
                <Metric
                  label="价格口径挽回/错过"
                  value={
                    watchSummary.price_saved_usd != null
                      ? `${money(watchSummary.price_saved_usd)}${watchSummary.price_saved_usd >= 0 ? '（止损帮忙少亏）' : '（错过恢复）'}`
                      : '-'
                  }
                />
                <Metric
                  label="止损价 → 领航员离场价"
                  value={`${watchSummary.last_stop_price ?? '-'} → ${watchSummary.leader_close_price ?? '-'}`}
                />
                <Metric
                  label="观察时长 / 采样数"
                  value={`${Math.round((watchSummary.watch_seconds ?? 0) / 60)} 分钟 / ${watchSummary.sample_count ?? 0} 条`}
                />
                <Metric
                  label="价格首次回归边界"
                  value={
                    (watchSummary.first_recovery_seconds ?? -1) >= 0
                      ? `${Math.round((watchSummary.first_recovery_seconds ?? 0) / 60)} 分钟后`
                      : '从未回归'
                  }
                />
                <Metric
                  label="最大有利/不利偏移"
                  value={`+${(watchSummary.max_favorable_excursion ?? 0).toFixed(4)} / -${(watchSummary.max_adverse_excursion ?? 0).toFixed(4)}`}
                />
                <Metric
                  label="领航员观察期加/减仓"
                  value={`${watchSummary.leader_addons ?? 0} / ${watchSummary.leader_reductions ?? 0} 次`}
                />
              </div>
              {watchSummary.blocked_when_recovered &&
                Object.keys(watchSummary.blocked_when_recovered).length > 0 && (
                  <div className="mt-3 text-xs text-[#F0B90B]">
                    价格已回归但被其他条件挡住的采样：
                    {Object.entries(watchSummary.blocked_when_recovered)
                      .map(
                        ([gate, count]) =>
                          `${localized(gateLabels, gate)} ×${count}`
                      )
                      .join('、')}
                    （提示：这些门控参数可能设置过紧）
                  </div>
                )}
            </div>
          )}
          {watchChart.length > 1 && (
            <div className="mb-4 rounded border border-[#2B3139] bg-[#181A20] p-4">
              <div className="mb-2 text-sm font-medium">
                观察期价格轨迹（标记价 / 重入边界 / 追价上限）
              </div>
              <div className="h-56">
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={watchChart}>
                    <XAxis dataKey="time" stroke="#848E9C" minTickGap={40} />
                    <YAxis
                      stroke="#848E9C"
                      domain={['auto', 'auto']}
                      tickFormatter={(v: number) => v.toPrecision(6)}
                      width={90}
                    />
                    <Tooltip
                      contentStyle={{
                        background: '#181A20',
                        border: '1px solid #2B3139',
                      }}
                    />
                    <Legend />
                    <Line
                      type="monotone"
                      dataKey="标记价"
                      stroke="#EAECEF"
                      dot={false}
                      connectNulls
                    />
                    <Line
                      type="monotone"
                      dataKey="重入边界"
                      stroke="#0ECB81"
                      dot={false}
                      connectNulls
                      strokeDasharray="4 2"
                    />
                    <Line
                      type="monotone"
                      dataKey="追价上限"
                      stroke="#F6465D"
                      dot={false}
                      connectNulls
                      strokeDasharray="4 2"
                    />
                  </LineChart>
                </ResponsiveContainer>
              </div>
              {gateCounts.length > 0 && (
                <div className="mt-2 text-xs text-[#848E9C]">
                  未重入原因分布：
                  {gateCounts
                    .map(
                      ([gate, count]) =>
                        `${localized(gateLabels, gate)} ×${count}`
                    )
                    .join('、')}
                </div>
              )}
            </div>
          )}
          <div className="mb-4 rounded bg-[#181A20] p-3 text-sm text-[#848E9C]">
            {detail.cycle.stop_count === 0 ? (
              <>
                本周期未触发 Copy Guard，保护效果记为
                0。实际盈亏与估算基线的差额仅作为跟单执行偏差。
              </>
            ) : detail.cycle.accounting_status === 'RECONCILED' ? (
              <>
                公式：实际净盈亏 {detail.cycle.actual_pnl.toFixed(2)} −
                未兜底估算 {detail.cycle.baseline_pnl.toFixed(2)} = 净保护效果{' '}
                <span
                  className={
                    detail.cycle.net_guard_effect >= 0
                      ? 'text-[#0ECB81]'
                      : 'text-[#F6465D]'
                  }
                >
                  {detail.cycle.net_guard_effect.toFixed(2)} USDT
                </span>
              </>
            ) : (
              <>本周期尚未完成对账，暂不计入保护效果。</>
            )}
          </div>
          {detail.attempts.length > 0 && (
            <div className="space-y-2 mb-4">
              {detail.attempts.map((a) => (
                <div
                  key={a.id}
                  className="grid grid-cols-5 gap-3 text-sm bg-[#181A20] rounded p-3"
                >
                  <span>尝试 #{a.attempt_no}</span>
                  <span>{localized(attemptLabels, a.status)}</span>
                  <span>入场 {a.entry_price || '-'}</span>
                  <span>离场 {a.exit_price || '-'}</span>
                  <span>净盈亏 {a.pnl.toFixed(2)}</span>
                </div>
              ))}
            </div>
          )}
          <div className="space-y-2">
            {timeline.map(({ event: e, count, firstAt, lastAt }) => (
              <div
                key={e.id}
                className="grid grid-cols-5 gap-3 text-sm bg-[#181A20] rounded p-3"
              >
                <span
                  title={
                    count > 1
                      ? `${dateLabel(firstAt)} - ${dateLabel(lastAt)}`
                      : undefined
                  }
                >
                  {dateLabel(firstAt)}
                </span>
                <span className="font-medium">
                  {localized(eventLabels, e.type)}
                  {e.type === 'REENTRY_GATE_CHANGED' && e.metadata
                    ? `：${localized(gateLabels, String(e.metadata.from ?? ''))} → ${localized(gateLabels, String(e.metadata.to ?? ''))}`
                    : ''}
                  {count > 1 ? ` ×${count}` : ''}
                </span>
                <span>价格 {e.price || '-'}</span>
                <span>盈亏 {e.pnl?.toFixed(2) ?? '0.00'}</span>
                <span>费用 {e.fee?.toFixed(2) ?? '0.00'}</span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
function Card({
  title,
  value,
  color,
  note,
}: {
  title: string
  value: string
  color: string
  note?: string
}) {
  return (
    <div className="bg-[#181A20] border border-[#2B3139] rounded-lg p-5">
      <div className="text-sm text-[#848E9C]">{title}</div>
      <div className="text-2xl font-bold mt-2" style={{ color }}>
        {value}
      </div>
      {note && <div className="text-xs text-[#848E9C] mt-2">{note}</div>}
    </div>
  )
}
function Metric({ label, value }: { label: string; value: string | number }) {
  return (
    <div className="bg-[#181A20] rounded p-3">
      <div className="text-[#848E9C]">{label}</div>
      <div className="font-bold mt-1">{value}</div>
    </div>
  )
}
