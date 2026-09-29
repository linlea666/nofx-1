import { useEffect, useState } from 'react'
import { httpClient } from '../lib/httpClient'
import type {
  CurrentPositionCopyPreview,
  CurrentPositionCopyResult,
} from '../types'

const reasons: Record<string, string> = {
  SOURCE_IDENTITY_UNAVAILABLE: '源仓身份不完整',
  SOURCE_IDENTITY_CHANGED: '源仓周期或保证金模式已改变',
  SOURCE_VALUE_UNAVAILABLE: '源仓名义金额不可核实',
  ALREADY_PARTICIPATED_OR_UNSETTLED: '已参与过本轮，或存在待对账订单',
  SOURCE_NOT_INITIAL_BASELINE: '已跟随、停跟或退出，本次不复制',
  SOURCE_PREVIOUSLY_SKIPPED: '本轮已有跳过记录',
  GROUP_ENTRY_BLOCKED: '该方向处于暂停或风险退出状态',
  GROUP_PARTICIPATION_CONFLICT: '同方向源仓参与状态不一致',
  INDEPENDENT_SAME_SIDE_POSITION: '账户已有独立同向仓位',
  EXECUTION_INSTRUMENT_UNAVAILABLE: '执行端合约不可用',
  EXECUTION_PRICE_UNAVAILABLE: '当前执行参考价不可用',
  NO_CURRENT_POSITIONS: '无可复制仓位',
  CURRENT_POSITION_COPY_EXPIRED_OR_CHANGED: '复制窗口结束或领航员仓位已改变',
  CONFIGURATION_CHANGED: '配置已改变，请重新开启',
  EXECUTION_ACCOUNT_CHANGED: '执行账户已改变，请重新开启',
  DECISION_MODE_CHANGED: '决策模式已改变',
  OPERATOR_CANCELLED: '已取消',
  OPERATOR_REPLACED: '已重新登记',
  TRADER_STOPPED: '交易员已停止',
}
const statuses: Record<string, string> = {
  PENDING: '待下次手动启动',
  SEALED: '本次复制处理中',
  DONE: '本次复制已结束',
  CANCELLED: '本次复制已取消',
  READY: '符合复制条件',
  SKIPPED: '已跳过',
  RESERVED: '准备执行',
  SUBMITTED: '已提交，等待回执',
  RECONCILING: '对账中',
  FILLED: '已成交',
  PROTECTED: '已成交并接入保护',
  PARTIALLY_FILLED: '部分成交',
  COMPLETED_PARTIAL: '部分成交，补齐已结束',
  FAILED: '未完成',
}
export function CurrentPositionCopyControl({
  enabled,
  disabled,
  traderId,
  exchangeId,
  leaderId,
  ratio,
  result,
  onChange,
}: {
  enabled: boolean
  disabled: boolean
  traderId?: string
  exchangeId: string
  leaderId: string
  ratio: number
  result?: CurrentPositionCopyResult
  onChange: (enabled: boolean) => void
}) {
  const [preview, setPreview] = useState<CurrentPositionCopyPreview>()
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [refresh, setRefresh] = useState(0)
  useEffect(() => {
    let cancelled = false
    setPreview(undefined)
    setError('')
    if (!enabled || !exchangeId || !leaderId) {
      setLoading(false)
      return
    }
    setLoading(true)
    const timer = setTimeout(() => {
      httpClient
        .post<CurrentPositionCopyPreview>(
          '/api/copytrade/current-positions/preview',
          {
            trader_id: traderId,
            exchange_id: exchangeId,
            provider_type: 'okx',
            leader_id: leaderId,
            copy_ratio: ratio,
          }
        )
        .then((response) => {
          if (cancelled) return
          if (!response.success || !response.data)
            throw new Error(response.message || '预览失败')
          setPreview(response.data)
        })
        .catch((err: unknown) => {
          if (!cancelled)
            setError(err instanceof Error ? err.message : '预览失败')
        })
        .finally(() => {
          if (!cancelled) setLoading(false)
        })
    }, 300)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [enabled, traderId, exchangeId, leaderId, ratio, refresh])
  const rows = preview?.positions ?? []
  const total = rows
    .filter((row) => row.status === 'READY')
    .reduce((sum, row) => sum + row.estimated_notional, 0)
  return (
    <div className="p-4 bg-[#0B0E11] border border-[#F0B90B33] rounded-lg space-y-3">
      <label className="flex items-start gap-3 cursor-pointer">
        <input
          type="checkbox"
          className="mt-1 accent-[#F0B90B]"
          checked={enabled}
          disabled={disabled}
          onChange={(event) => onChange(event.target.checked)}
        />
        <span>
          <span className="text-[#EAECEF] font-medium">
            下次启动时复制领航员当前仓位
          </span>
          <span className="block text-xs text-[#848E9C] mt-1">
            默认关闭。保存仅登记请求，下次手动启动执行一次；后续加减仓、平仓与保护沿用现有规则。
          </span>
        </span>
      </label>
      {disabled && (
        <p className="text-xs text-[#F0B90B]">
          请先停止交易员，再登记复制请求。
        </p>
      )}
      {enabled && (
        <>
          <p className="text-xs text-[#848E9C]">
            自动复制所有符合条件的当前仓位。价格对比仅供参考，不限制入场；实际执行仍受余额、合约规则及补齐窗口约束。
          </p>
          <button
            type="button"
            disabled={loading || !leaderId || !exchangeId}
            onClick={() => setRefresh((value) => value + 1)}
            className="text-sm text-[#F0B90B] disabled:opacity-50"
          >
            {loading ? '正在读取仓位…' : '刷新只读预览'}
          </button>
          {(!leaderId || !exchangeId) && (
            <p className="text-xs text-[#848E9C]">
              选择执行账户并填写领航员后显示预览。
            </p>
          )}
          {error && (
            <p role="alert" className="text-sm text-red-400">
              {error}。启动时将重新核验，预览失败不会开仓。
            </p>
          )}
          {preview && (
            <>
              <p className="text-xs text-[#848E9C]">
                快照：{new Date(preview.snapshot_at).toLocaleString()} ·
                符合条件 {rows.filter((row) => row.status === 'READY').length}{' '}
                仓 · 预计名义金额 {total.toFixed(2)}{' '}
                USDT（非保证金，执行时按真实合约最小量取整）
              </p>
              {rows.length === 0 ? (
                <p className="text-sm text-[#EAECEF]">
                  当前无可复制仓位。若启动时仍为空，本次请求将直接结束。
                </p>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-xs text-left text-[#EAECEF]">
                    <thead className="text-[#848E9C]">
                      <tr>
                        <th className="p-2">仓位</th>
                        <th className="p-2">领航员均价</th>
                        <th className="p-2">当前参考价 / 优势</th>
                        <th className="p-2">预计名义金额</th>
                        <th className="p-2">结果</th>
                      </tr>
                    </thead>
                    <tbody>
                      {rows.map((row) => {
                        const advantage =
                          row.leader_entry_price > 0 && row.reference_price > 0
                            ? ((row.side === 'long'
                                ? row.leader_entry_price - row.reference_price
                                : row.reference_price -
                                  row.leader_entry_price) /
                                row.leader_entry_price) *
                              100
                            : undefined
                        return (
                          <tr
                            key={row.leader_pos_id}
                            className="border-t border-[#2B3139]"
                          >
                            <td className="p-2">
                              {row.symbol} {row.side === 'long' ? '多' : '空'}
                              <br />
                              领航员 {row.leverage}× ·{' '}
                              {row.margin_mode === 'cross' ? '全仓' : '逐仓'}
                            </td>
                            <td className="p-2">
                              {row.leader_entry_price || '—'}
                            </td>
                            <td className="p-2">
                              {row.reference_price || '—'}
                              <br />
                              {advantage === undefined
                                ? '—'
                                : `${advantage >= 0 ? '+' : ''}${advantage.toFixed(2)}%`}
                            </td>
                            <td className="p-2">
                              {row.estimated_notional.toFixed(2)}
                            </td>
                            <td className="p-2">
                              {reasons[row.reason] ||
                                statuses[row.status] ||
                                row.reason ||
                                row.status}
                            </td>
                          </tr>
                        )
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </>
          )}
        </>
      )}
      {result && (
        <details className="text-xs text-[#848E9C]">
          <summary>
            最近请求：{statuses[result.status] || result.status}
            {result.reason
              ? ` · ${reasons[result.reason] || result.reason}`
              : ''}
          </summary>
          <ul className="space-y-1 mt-2">
            {result.tasks.map((row) => (
              <li key={row.id}>
                {row.symbol} {row.side === 'long' ? '多' : '空'}：
                {statuses[row.status] || row.status} ·{' '}
                {reasons[row.reason] || row.reason || '—'}
                {row.filled_quantity > 0
                  ? ` · 已成交 ${row.filled_quantity}`
                  : ''}
              </li>
            ))}
          </ul>
        </details>
      )}
    </div>
  )
}
