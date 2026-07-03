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
          <option value="STOPPED_WATCHING">保护观察</option>
          <option value="FOLLOWING_REENTRY">重入跟随</option>
          <option value="LEADER_CLOSED">领航员平仓</option>
          <option value="ATTEMPTS_EXHAUSTED">次数耗尽</option>
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
        />
      </div>
      {(summary?.unknown_count ?? 0) + (summary?.degraded_count ?? 0) > 0 && (
        <div className="border border-[#F6465D] bg-[#F6465D]/10 rounded-lg p-4 text-sm text-[#F6465D]">
          当前有{' '}
          {(summary?.unknown_count ?? 0) + (summary?.degraded_count ?? 0)}{' '}
          个仓位的保护单未知或降级，仓位可能没有有效止损，请人工检查
          OKX。系统仍会继续跟随并重试。
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
        <Metric label="保护未知" value={summary?.unknown_count ?? 0} />
        <Metric label="保护降级" value={summary?.degraded_count ?? 0} />
        <Metric
          label="平均覆盖率"
          value={`${((summary?.average_coverage ?? 0) * 100).toFixed(1)}%`}
        />
        <Metric
          label="保护缺失时长"
          value={`${Math.round((summary?.protection_missing_seconds ?? 0) / 60)} 分钟`}
        />
        <Metric label="第1次重入" value={summary?.reentry_first ?? 0} />
        <Metric label="第2次重入" value={summary?.reentry_second ?? 0} />
        <Metric label="第3次及以上" value={summary?.reentry_third_plus ?? 0} />
        <Metric
          label="重入成功率"
          value={`${((summary?.reentry_success_rate ?? 0) * 100).toFixed(1)}%`}
        />
        <Metric
          label="误杀率"
          value={`${((summary?.false_kill_rate ?? 0) * 100).toFixed(1)}%`}
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
                '保护健康',
                '止损/重入',
                '实际盈亏',
                '未兜底估算',
                '净效果',
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
                <td className="p-3">
                  {new Date(c.opened_at).toLocaleString()}
                </td>
                <td className="p-3">{c.trader_id}</td>
                <td className="p-3">{c.leader_id}</td>
                <td className="p-3">{c.symbol}</td>
                <td className="p-3">{c.side}</td>
                <td className="p-3">{c.status}</td>
                <td
                  className={`p-3 ${c.protection_status === 'VERIFIED' ? 'text-[#0ECB81]' : c.protection_status === 'UNKNOWN' || c.protection_status === 'DEGRADED' ? 'text-[#F6465D]' : 'text-[#F0B90B]'}`}
                >
                  {c.protection_status} ·{' '}
                  {(c.protection_coverage * 100).toFixed(0)}%
                </td>
                <td className="p-3">
                  {c.stop_count}/{c.reentry_count}
                </td>
                <td className="p-3">{money(c.actual_pnl)}</td>
                <td className="p-3">{money(c.baseline_pnl)}</td>
                <td
                  className={`p-3 ${c.net_guard_effect >= 0 ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}
                >
                  {money(c.net_guard_effect)}
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
          <h2 className="font-bold mb-3">生命周期 #{detail.cycle.id} 时间线</h2>
          <div className="grid md:grid-cols-3 gap-3 mb-4 text-sm">
            <Metric
              label="保护状态"
              value={`${detail.cycle.protection_status} / ${(detail.cycle.protection_coverage * 100).toFixed(0)}%`}
            />
            <Metric
              label="保护单"
              value={detail.protection?.algo_id || '尚未确认'}
            />
            <Metric label="重试次数" value={detail.cycle.protection_retries} />
          </div>
          {detail.cycle.protection_error && (
            <div className="mb-4 text-sm text-[#F6465D]">
              最后错误：{detail.cycle.protection_error}
            </div>
          )}
          <div className="mb-4 text-xs text-[#848E9C] break-all">
            策略快照：{detail.cycle.policy_snapshot}
          </div>
          <div className="mb-4 rounded bg-[#181A20] p-3 text-sm text-[#848E9C]">
            公式：实际净盈亏 {detail.cycle.actual_pnl.toFixed(2)} − 未兜底估算{' '}
            {detail.cycle.baseline_pnl.toFixed(2)} = 净保护效果{' '}
            <span
              className={
                detail.cycle.net_guard_effect >= 0
                  ? 'text-[#0ECB81]'
                  : 'text-[#F6465D]'
              }
            >
              {detail.cycle.net_guard_effect.toFixed(2)} USDT
            </span>
          </div>
          {detail.attempts.length > 0 && (
            <div className="space-y-2 mb-4">
              {detail.attempts.map((a) => (
                <div
                  key={a.id}
                  className="grid grid-cols-5 gap-3 text-sm bg-[#181A20] rounded p-3"
                >
                  <span>尝试 #{a.attempt_no}</span>
                  <span>{a.status}</span>
                  <span>入场 {a.entry_price || '-'}</span>
                  <span>离场 {a.exit_price || '-'}</span>
                  <span>净盈亏 {a.pnl.toFixed(2)}</span>
                </div>
              ))}
            </div>
          )}
          <div className="space-y-2">
            {detail.events.map((e) => (
              <div
                key={e.id}
                className="grid grid-cols-5 gap-3 text-sm bg-[#181A20] rounded p-3"
              >
                <span>{new Date(e.created_at).toLocaleString()}</span>
                <span className="font-medium">{e.type}</span>
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
}: {
  title: string
  value: string
  color: string
}) {
  return (
    <div className="bg-[#181A20] border border-[#2B3139] rounded-lg p-5">
      <div className="text-sm text-[#848E9C]">{title}</div>
      <div className="text-2xl font-bold mt-2" style={{ color }}>
        {value}
      </div>
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
