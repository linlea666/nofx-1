import { useEffect, useState } from 'react'
import { httpClient } from '../lib/httpClient'
import type {
  CopyTradeProvider,
  CurrentPositionCopyPreview,
  CurrentPositionCopyResult,
  CurrentPositionCopyRow,
  CurrentPositionCopySelection,
} from '../types'

export interface CurrentPositionCopyOptions {
  selection?: CurrentPositionCopySelection[]
  priceFilterEnabled?: boolean
  minAdvantagePct?: number
  manualMaxDisadvantagePct?: number
}

const directionKey = (row: CurrentPositionCopyRow) =>
  `${row.symbol}|${row.side}`

const isSelected = (
  row: CurrentPositionCopyRow,
  selection: CurrentPositionCopySelection[] | undefined
) =>
  !!selection?.some(
    (item) =>
      item.leader_pos_id === row.leader_pos_id &&
      (!item.opened_ms || item.opened_ms === (row.opened_ms ?? 0))
  )

// A manually picked row below the minimum advantage carries an explicit
// acceptance; the backend then rechecks it against the manual tolerance.
const toSelection = (
  rows: CurrentPositionCopyRow[]
): CurrentPositionCopySelection[] =>
  rows.map((row) => ({
    leader_pos_id: row.leader_pos_id,
    opened_ms: row.opened_ms ?? 0,
    ...(row.price_manual_only ? { accept_unfavorable: true } : {}),
  }))

const reasons: Record<string, string> = {
  SOURCE_IDENTITY_UNAVAILABLE: '源仓身份不完整',
  SOURCE_IDENTITY_CHANGED: '源仓周期或保证金模式已改变',
  SOURCE_VALUE_UNAVAILABLE: '源仓名义金额不可核实',
  ALREADY_PARTICIPATED_OR_UNSETTLED:
    '曾跟随过本仓位或存在待对账订单，不支持重新复制',
  SOURCE_NOT_INITIAL_BASELINE: '曾跟随过（已停跟或已结束），不支持重新复制',
  SOURCE_PREVIOUSLY_SKIPPED: '本轮已有跳过记录',
  NOT_SELECTED: '未勾选，本次不复制（下次可再选）',
  PRICE_NOT_FAVORABLE: '价格无优势，本次不复制（下次可再选）',
  LEADER_ENTRY_PRICE_UNAVAILABLE: '领航员均价不可用，无法判断价格优势',
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
  providerType = 'okx',
  selection,
  priceFilterEnabled = false,
  minAdvantagePct = 0,
  manualMaxDisadvantagePct = 0,
  onOptionsChange,
}: {
  enabled: boolean
  disabled: boolean
  traderId?: string
  exchangeId: string
  leaderId: string
  ratio: number
  result?: CurrentPositionCopyResult
  onChange: (enabled: boolean) => void
  providerType?: CopyTradeProvider
  selection?: CurrentPositionCopySelection[]
  priceFilterEnabled?: boolean
  minAdvantagePct?: number
  manualMaxDisadvantagePct?: number
  onOptionsChange?: (options: CurrentPositionCopyOptions) => void
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
            provider_type: providerType,
            leader_id: leaderId,
            copy_ratio: ratio,
            price_filter_enabled: priceFilterEnabled,
            min_advantage_pct: minAdvantagePct,
            manual_max_disadvantage_pct: manualMaxDisadvantagePct,
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
  }, [
    enabled,
    traderId,
    exchangeId,
    leaderId,
    ratio,
    refresh,
    providerType,
    priceFilterEnabled,
    minAdvantagePct,
    manualMaxDisadvantagePct,
  ])
  const rows = preview?.positions ?? []
  const readyRows = rows.filter((row) => row.status === 'READY')
  // Directions with a price-below-minimum leg are only copied when picked by
  // hand (a direction is copied completely or not at all).
  const manualDirections = new Set(
    readyRows.filter((row) => row.price_manual_only).map(directionKey)
  )
  // First preview: default to every copyable position whose price meets the
  // minimum advantage. A saved or edited selection is kept as-is.
  useEffect(() => {
    if (enabled && preview && selection === undefined && onOptionsChange)
      onOptionsChange({
        selection: toSelection(
          readyRows.filter((row) => !manualDirections.has(directionKey(row)))
        ),
      })
  }, [enabled, preview])
  const chosen = readyRows.filter((row) => isSelected(row, selection))
  const total = chosen.reduce((sum, row) => sum + row.estimated_notional, 0)
  // Same symbol+side legs must be copied together (direction-group rule).
  const toggleDirection = (row: CurrentPositionCopyRow, checked: boolean) => {
    if (!onOptionsChange) return
    const group = readyRows.filter(
      (other) => directionKey(other) === directionKey(row)
    )
    const rest = (selection ?? []).filter(
      (item) => !group.some((g) => g.leader_pos_id === item.leader_pos_id)
    )
    onOptionsChange({
      selection: checked ? [...rest, ...toSelection(group)] : rest,
    })
  }
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
            勾选要复制的仓位，未勾选的本次不复制、下次可再选。实际执行仍受余额、合约规则及补齐窗口约束。
          </p>
          {onOptionsChange && (
            <div className="flex flex-wrap items-center gap-3 text-xs text-[#EAECEF]">
              <label className="flex items-center gap-2 cursor-pointer">
                <input
                  type="checkbox"
                  className="accent-[#F0B90B]"
                  checked={priceFilterEnabled}
                  disabled={disabled}
                  onChange={(event) =>
                    onOptionsChange({
                      priceFilterEnabled: event.target.checked,
                    })
                  }
                />
                只复制价格有优势的仓位
              </label>
              <label className="flex items-center gap-1">
                最低优势
                <input
                  type="number"
                  step="0.1"
                  min={-20}
                  max={50}
                  value={minAdvantagePct}
                  disabled={disabled || !priceFilterEnabled}
                  onChange={(event) => {
                    const value = Number(event.target.value)
                    if (Number.isFinite(value))
                      onOptionsChange({ minAdvantagePct: value })
                  }}
                  className="w-16 px-1 py-0.5 bg-[#1E2329] border border-[#2B3139] rounded text-[#EAECEF] disabled:opacity-50"
                />
                %
              </label>
              <label className="flex items-center gap-1">
                手选最大劣势
                <input
                  type="number"
                  step="0.1"
                  min={0}
                  max={20}
                  value={manualMaxDisadvantagePct}
                  disabled={disabled || !priceFilterEnabled}
                  onChange={(event) => {
                    const value = Number(event.target.value)
                    if (Number.isFinite(value))
                      onOptionsChange({ manualMaxDisadvantagePct: value })
                  }}
                  className="w-16 px-1 py-0.5 bg-[#1E2329] border border-[#2B3139] rounded text-[#EAECEF] disabled:opacity-50"
                />
                %
              </label>
              <span className="text-[#848E9C]">
                做多现价低于领航员均价、做空现价高于均价为优势；启动时和下单前各检查一次。未达最低优势但劣势不超过「手选最大劣势」的仓位默认不勾，可手动勾选（设为
                0 关闭）。
              </span>
            </div>
          )}
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
                符合条件 {readyRows.length} 仓 · 已勾选 {chosen.length} 仓 ·
                预计名义金额 {total.toFixed(2)}{' '}
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
                        {onOptionsChange && <th className="p-2">复制</th>}
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
                            {onOptionsChange && (
                              <td className="p-2">
                                <input
                                  type="checkbox"
                                  aria-label={`复制 ${row.symbol}`}
                                  className="accent-[#F0B90B]"
                                  disabled={disabled || row.status !== 'READY'}
                                  checked={
                                    row.status === 'READY' &&
                                    isSelected(row, selection)
                                  }
                                  onChange={(event) =>
                                    toggleDirection(row, event.target.checked)
                                  }
                                />
                              </td>
                            )}
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
                              {row.status === 'READY' && row.price_manual_only
                                ? '价格略差，可手动勾选'
                                : reasons[row.reason] ||
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
