import useSWR from 'swr'
import { api } from '../lib/api'
import type {
  CopyExecutionHealthIssue,
  CopyRuntimeIssue,
  CopyRuntimeSource,
  CopyTraderRuntimeHealth,
} from '../types'

const settlementStages: Record<string, string> = {
  LOCAL_CANDIDATES: '本地记录核验',
  VENUE_HISTORY: '交易所历史读取',
  SCOPED_FILLS: '成交及保证金模式核验',
  LIFECYCLE_PROOF: '完整仓位周期核验',
  LOCAL_ALLOCATION: '费用分配核验',
}

function timestamp(value?: string | null): number | null {
  if (!value || value.startsWith('0001-01-01')) return null
  // SQLite CURRENT_TIMESTAMP is UTC but has no suffix. Do not display it as
  // browser-local time and silently move the incident by eight hours.
  const normalized = /^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)?$/.test(value)
    ? `${value.replace(' ', 'T')}Z`
    : value.replace(' ', 'T')
  const parsed = Date.parse(normalized)
  return Number.isFinite(parsed) ? parsed : null
}

function timeLabel(value?: string | null): string {
  const parsed = timestamp(value)
  return parsed === null
    ? '未记录'
    : new Date(parsed).toLocaleString('zh-CN', { hour12: false })
}

function scopeLabel(symbol: string, side: string, mode = ''): string {
  const direction =
    side?.toLowerCase() === 'long'
      ? '多头'
      : side?.toLowerCase() === 'short'
        ? '空头'
        : ''
  const margin = mode === 'isolated' ? '逐仓' : mode === 'cross' ? '全仓' : ''
  return [symbol || '范围待核实', direction, margin].filter(Boolean).join(' · ')
}

function sourceLabel(source: CopyRuntimeSource | null, checkedAt: number) {
  const success = timestamp(source?.last_success_at)
  if (success === null) return '未取得完整快照'
  if (source?.last_error) return '最近快照读取失败'
  if (success > checkedAt + 5000) return '快照时间待核实'
  if (checkedAt - success > 60000) return '快照超过 60 秒未更新'
  return '已取得近期完整快照'
}

function RuntimeIssue({ issue }: { issue: CopyRuntimeIssue }) {
  return (
    <li className="rounded border border-[#2B3139] bg-[#0B0E11] p-3 text-sm">
      <div className="font-medium text-[#EAECEF]">
        {scopeLabel(issue.symbol, issue.side)}
      </div>
      <p className="mt-1 break-words text-[#B7BDC6]">
        {issue.detail || issue.code}
      </p>
      <p className="mt-2 text-xs text-[#848E9C]">
        首次等待：{timeLabel(issue.first_seen)} · 最近核查：
        {timeLabel(issue.last_seen)}
      </p>
      <details className="mt-2 text-xs text-[#848E9C]">
        <summary className="cursor-pointer">核查详情</summary>
        <p className="mt-1 break-all">
          原因：{issue.code} · 源仓：{issue.leader_pos_id || '未记录'} · 记录：
          {issue.resource_id}
        </p>
      </details>
    </li>
  )
}

function ExecutionIssue({
  issue,
  runtime,
}: {
  issue: CopyExecutionHealthIssue
  runtime: CopyRuntimeIssue[]
}) {
  const observation = runtime.find(
    (item) =>
      item.resource_id === `intent:${issue.intent_id}` ||
      item.resource_id === String(issue.intent_id)
  )
  const action = issue.action.startsWith('close_')
    ? '平仓'
    : issue.action.startsWith('reduce_')
      ? '减仓'
      : issue.action.startsWith('open_')
        ? '开/加仓'
        : '交易'
  return (
    <li className="rounded border border-[#F6465D]/30 bg-[#0B0E11] p-3 text-sm">
      <div className="font-medium text-[#EAECEF]">
        {scopeLabel(issue.symbol, issue.side, issue.margin_mode)} · {action}指令
      </div>
      <p className="mt-1 text-[#B7BDC6]">
        {issue.issue_reason || issue.reason || '执行事实待核实'}
      </p>
      <p className="mt-1 text-xs text-[#848E9C]">
        影响范围：本交易员该合约、该方向的后续跟随。
      </p>
      <p className="mt-1 text-xs text-[#F0B90B]">
        {issue.unresolved_attempts > 0 || issue.effect === 'UNRESOLVED'
          ? '订单或成交入账证据未确认，继续对账'
          : issue.effect === 'BOOKED_FILL'
            ? '成交已入账，等待源状态收尾'
            : '未确认成交，等待源状态处置'}
      </p>
      <p className="mt-2 text-xs text-[#848E9C]">
        {observation
          ? `首次等待：${timeLabel(observation.first_seen)}`
          : `首次等待未记录；指令创建：${timeLabel(issue.created_at)}`}
      </p>
      <details className="mt-2 text-xs text-[#848E9C]">
        <summary className="cursor-pointer">核查详情</summary>
        <p className="mt-1 break-all">
          指令 #{issue.intent_id} · 源仓 {issue.leader_pos_id} · 指令修订{' '}
          {issue.revision} / 映射修订 {issue.mapping_revision} · {issue.status}{' '}
          · {issue.reason || '原因待核实'}
        </p>
      </details>
    </li>
  )
}

function TraderHealth({
  trader,
  checkedAt,
}: {
  trader: CopyTraderRuntimeHealth
  checkedAt: number
}) {
  const sourceIssues = trader.runtime_issues.filter(
    (issue) => issue.area === 'source'
  )
  const executionIssues = trader.runtime_issues.filter(
    (issue) => issue.area === 'execution'
  )
  const protectionIssues = trader.runtime_issues.filter(
    (issue) => issue.area === 'protection'
  )
  const otherIssues = trader.runtime_issues.filter(
    (issue) => !['source', 'execution', 'protection'].includes(issue.area)
  )
  const pendingSettlements = trader.settlement_issues.filter(
    (issue) => issue.status === 'PENDING'
  )
  const extraExecution = executionIssues.filter(
    (issue) =>
      !trader.execution_issues.some(
        (intent) =>
          issue.resource_id === `intent:${intent.intent_id}` ||
          issue.resource_id === String(intent.intent_id)
      )
  )
  const executionCount = trader.execution_issues.length + extraExecution.length
  const name = trader.trader_name || trader.trader_id
  return (
    <article
      aria-label={`${name}运行状态`}
      className="rounded-lg border border-[#2B3139] p-4"
    >
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <h3 className="font-semibold text-[#EAECEF]">{name}</h3>
        <span className="rounded bg-[#2B3139] px-2 py-0.5 text-xs text-[#B7BDC6]">
          {trader.running ? '运行中' : '已停止 · 历史待办'}
        </span>
      </div>
      {!trader.running && (
        <p className="mb-3 text-xs text-[#848E9C]">
          以下为历史状态，不计作当前漏单，也不会自动启动交易员。
        </p>
      )}
      <div className="grid gap-3 lg:grid-cols-2 xl:grid-cols-4">
        <section aria-label={`${name}源快照`} className="space-y-2">
          <h4 className="text-sm font-medium text-[#B7BDC6]">源快照</h4>
          <p className="text-sm text-[#EAECEF]">
            {sourceLabel(trader.source, checkedAt)}
          </p>
          <p className="text-xs text-[#848E9C]">
            最后成功：{timeLabel(trader.source?.last_success_at)}
          </p>
          {trader.source?.last_error && (
            <p className="break-words text-xs text-[#F0B90B]">
              {trader.source.last_error}
            </p>
          )}
          {sourceIssues.length > 0 && (
            <ul className="space-y-2">
              {sourceIssues.map((issue) => (
                <RuntimeIssue key={issue.resource_id} issue={issue} />
              ))}
            </ul>
          )}
        </section>
        <section aria-label={`${name}执行待处理`} className="space-y-2">
          <h4 className="text-sm font-medium text-[#B7BDC6]">
            执行待处理 · {executionCount}
          </h4>
          {executionCount === 0 ? (
            <p className="text-xs text-[#848E9C]">暂无已记录的执行阻塞。</p>
          ) : (
            <ul className="space-y-2">
              {trader.execution_issues.map((issue) => (
                <ExecutionIssue
                  key={issue.intent_id}
                  issue={issue}
                  runtime={executionIssues}
                />
              ))}
              {extraExecution.map((issue) => (
                <RuntimeIssue key={issue.resource_id} issue={issue} />
              ))}
            </ul>
          )}
        </section>
        <section aria-label={`${name}保护待办`} className="space-y-2">
          <h4 className="text-sm font-medium text-[#B7BDC6]">
            保护待办 · {protectionIssues.length}
          </h4>
          <p className="text-xs text-[#848E9C]">
            保护维护与成交状态分别记录，托管效果以具体周期核验为准。
          </p>
          {protectionIssues.length === 0 ? (
            <p className="text-xs text-[#848E9C]">暂无已记录的保护维护待办。</p>
          ) : (
            <ul className="space-y-2">
              {protectionIssues.map((issue) => (
                <RuntimeIssue key={issue.resource_id} issue={issue} />
              ))}
            </ul>
          )}
        </section>
        <section aria-label={`${name}费用待核实`} className="space-y-2">
          <h4 className="text-sm font-medium text-[#B7BDC6]">
            费用待核实 · {pendingSettlements.length}
          </h4>
          <p className="text-xs text-[#848E9C]">历史费用待核实不阻止新交易。</p>
          <p className="text-xs text-[#848E9C]">
            以下为该执行账户的历史费用待办。
          </p>
          {pendingSettlements.length === 0 ? (
            <p className="text-xs text-[#848E9C]">暂无已记录的费用核实待办。</p>
          ) : (
            <ul className="space-y-2">
              {pendingSettlements.map((issue) => (
                <li
                  key={issue.identity}
                  className="rounded border border-[#2B3139] bg-[#0B0E11] p-3 text-sm"
                >
                  <p className="font-medium text-[#EAECEF]">
                    {scopeLabel(issue.symbol, issue.side, issue.margin_mode)}
                  </p>
                  <p className="mt-1 text-[#F0B90B]">
                    {settlementStages[issue.stage] || issue.stage}
                  </p>
                  <p className="mt-1 break-words text-xs text-[#B7BDC6]">
                    {issue.detail || issue.reason_code}
                  </p>
                  <p className="mt-2 text-xs text-[#848E9C]">
                    首次待核实：{timeLabel(issue.first_observed_at)} · 已核查{' '}
                    {issue.attempts} 次
                  </p>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
      {otherIssues.length > 0 && (
        <section className="mt-3" aria-label={`${name}其他待办`}>
          <h4 className="mb-2 text-sm text-[#B7BDC6]">其他待核实项</h4>
          <ul className="space-y-2">
            {otherIssues.map((issue) => (
              <RuntimeIssue
                key={`${issue.area}:${issue.resource_id}`}
                issue={issue}
              />
            ))}
          </ul>
        </section>
      )}
    </article>
  )
}

export function CopyRuntimeHealthPanel({ traderID }: { traderID?: string }) {
  const { data, error, isValidating, mutate } = useSWR(
    `copy-runtime-health-${encodeURIComponent(traderID || '')}`,
    async () => ({
      response: await api.getCopyRuntimeHealth(traderID),
      checkedAt: Date.now(),
    }),
    {
      refreshInterval: 10000,
      shouldRetryOnError: false,
      keepPreviousData: false,
    }
  )
  return (
    <section
      aria-label="跟单运行状态"
      className="rounded-xl border border-[#2B3139] bg-[#181A20] p-4 md:p-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-lg font-semibold text-[#EAECEF]">跟单运行状态</h2>
        <button
          type="button"
          disabled={isValidating}
          onClick={() => void mutate().catch(() => undefined)}
          className="rounded border border-[#474D57] px-3 py-1 text-sm text-[#EAECEF] disabled:opacity-50"
        >
          {isValidating ? '读取中…' : '刷新状态'}
        </button>
      </div>
      <p className="mb-4 mt-1 text-xs text-[#848E9C]">
        {traderID ? '当前选择的交易员' : '当前运行的跟单交易员'}
        ；按币种、方向和处理阶段显示，不将历史费用或单个仓位待办视为全部跟单停止。时间按本机时区显示。
      </p>
      {error ? (
        <div
          role="alert"
          className="rounded border border-[#F6465D]/40 p-3 text-sm text-[#F6465D]"
        >
          运行状态读取失败，当前状态无法确认。请刷新重试。
          <p className="mt-1 text-xs">
            {error instanceof Error ? error.message : '请求失败'}
          </p>
        </div>
      ) : !data ? (
        <p role="status" className="text-sm text-[#848E9C]">
          正在读取运行状态…
        </p>
      ) : data.response.traders.length === 0 ? (
        <p className="text-sm text-[#848E9C]">
          {traderID
            ? '没有可显示的跟单运行记录。'
            : '当前没有运行中的跟单交易员。'}
        </p>
      ) : (
        <div className="space-y-3">
          {data.response.traders.map((trader) => (
            <TraderHealth
              key={trader.trader_id}
              trader={trader}
              checkedAt={data.checkedAt}
            />
          ))}
        </div>
      )}
    </section>
  )
}
