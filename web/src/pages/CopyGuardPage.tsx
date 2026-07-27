import { useEffect, useMemo, useState } from 'react'
import type { MouseEvent as ReactMouseEvent } from 'react'
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
import type {
  AIModel,
  CopyGuardAICandidate,
  CopyGuardCycle,
  CopyGuardManualSignal,
  ReentryAIAnalysis,
  ReentryAIConfig,
  ReentryAIDecisionEvaluation,
  ReentryAIDiagnostic,
  ReentryAIStats,
  ReentryMarketPreview,
} from '../types'

const money = (v: number) => `${v >= 0 ? '+' : ''}${v.toFixed(2)} USDT`

const statusLabels: Record<string, string> = {
  FOLLOWING: '正常跟随',
  STOP_TRIGGERED: '止损已触发',
  STOP_PENDING_FLAT: '止损退出确认中',
  STOP_PARTIAL: '止损部分成交·继续退出',
  STOP_FLAT_CONFIRMED: '止损已完全平仓',
  AI_WATCHING: 'AI 持续观察',
  AI_REVIEWING: 'AI 分析中',
  AI_WAITING: 'AI 等待下一事件',
  STOPPED_WATCHING: '止损后观察',
  REENTRY_PENDING: '等待重入成交',
  FOLLOWING_REENTRY: '重入后跟随',
  LEADER_CLOSED: '领航员已平仓',
  LEADER_REVERSED: '领航员已反手',
  ATTEMPTS_EXHAUSTED: '重入次数用尽',
  AI_ABANDONED: 'AI 已确认放弃',
  BUDGET_SUSPENDED: 'AI 调用额度耗尽',
  WATCH_TIMEOUT: '观察超时·等待领航员平仓',
  PROTECTION_EXITED: '无法保护·已按配置市价离场',
  DETACHED: '跟随仓位已脱离·仅跟踪领航员',
  CYCLE_LOSS_CAPPED: '周期亏损熔断·等待领航员平仓（v5 前历史）',
  // 注：不可保护（裸跑）不是周期状态——warn 模式下周期保持 FOLLOWING，
  // 信号载体是 protection_status=UNPROTECTED_WARNING（下方保护状态标红）
}
// 空仓观察态：止损成交后跟随者无本地持仓，protection_status 残留 TRIGGERED
// 是上一笔保护单的终态，不是"保护异常"——按中性"观察中·无持仓"呈现，
// 避免黄色"已触发·0%"误导（cycle-41 实盘反馈）。
const watchingFlatStatuses = new Set([
  'STOPPED_WATCHING',
  'ATTEMPTS_EXHAUSTED',
  'WATCH_TIMEOUT',
])
const isWatchingFlat = (c: {
  status: string
  closed_at?: string | null
  protection_status: string
}) =>
  !c.closed_at &&
  watchingFlatStatuses.has(c.status) &&
  c.protection_status === 'TRIGGERED'

const protectionLabels: Record<string, string> = {
  PENDING: '待建立',
  VERIFIED: '保护有效',
  UNKNOWN: '状态未知',
  DEGRADED: '保护异常',
  TRIGGERED: '已触发',
  CANCELED: '已撤销',
  CLAMPED: '已保护·止损被强平价挤紧',
  UNPROTECTABLE: '无法保护·裸跑（高危）',
  UNPROTECTED_WARNING: '无保护·已警告并继续重试',
  FORCED_EXIT_PENDING: '无法保护·市价离场确认中',
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
  PROTECTION_PENDING: '保护单建立中',
  PROTECTION_ACTIVE: '保护单已生效',
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
  ACCOUNTING_DELAYED: '交易所结算数据延迟，自动重试中',
  ACCOUNTING_UNRECOVERABLE: '对账数据不可自动恢复',
  PROTECTIVE_STOP_ADOPTED: '接管已存在的保护单',
  PROTECTIVE_STOP_TERMINAL: '保护单已自然终结',
  LIQ_PRICE_IGNORED: '强平价方向异常已忽略',
  BASELINE_CALIBRATED: '基线已用领航员历史校准',
  CYCLE_BACKFILLED: '存量仓位补建生命周期',
  STOP_CONFIRMED: '保护止损确认（间接检测）',
  STOP_RISK_THRESHOLD_EXCEEDED: '预计止损损失超过账户预警线',
  CATCHUP_TIMEOUT: '普通跟单补齐超时',
  CATCHUP_PRICE_LIMIT: '普通跟单补齐触发价格上限',
  AI_ENTRY_LEASE_WAITING_PRICE: 'AI ENTER 租约等待价格回到区间',
  ENTER_WINDOW_EXPIRED: 'AI ENTER 条件租约已到期',
  REENTRY_FILL_INCREMENT: 'AI 重入新增成交',
  ATTEMPT_RECONCILED: '单次尝试盈亏已对账',
  PROTECTIVE_STOP_GONE: '保护单消失（仓位已平）',
  PROTECTION_CLAMPED: '止损价被强平价挤紧（保护降级）',
  GUARD_UNPROTECTABLE: '确认无法建立保护',
  GUARD_FORCED_EXIT: '无法保护·强制离场',
  REENTRY_FAILED: '重入下单失败',
  REENTRY_REQUESTED: '重入条件满足，已请求下单',
  REENTRY_RECOVERED_AFTER_RESTART: '重启后重入状态已恢复',
  REENTRY_RECOVERY_PENDING: '重启后重入状态待确认',
  AI_REVIEW_REQUESTED: '操作员请求 AI 尽快复查',
  AI_DECISION_OUTCOME_FINALIZED: 'AI 单次决策后验评价完成',
  AI_CANDIDATE_OUTCOME_FINALIZED: 'AI 候选周期评价完成',
  ADDON_RISK_WARNING: '加仓风险告警（仍跟随）',
  ADDON_SKIPPED_BUDGET: '加仓超预算被拦截（旧版）',
  CYCLE_LOSS_BREAKER: '周期亏损熔断触发',
  REENTRY_GATE_CHANGED: '重入门控条件变化',
  REENTRY_WINDOW_COLLAPSED: '旧规则重入窗口不可行',
  WATCH_RESUMED: '观察采样断档后恢复',
  WATCH_SUMMARY: '观察期收尾统计',
  // v5.1 人工重入
  GUARD_MANUAL_REENTRY_SIGNAL: '人工重入信号（等待确认）',
  GUARD_MANUAL_REENTRY_CONFIRMED: '人工重入已确认（系统代执行）',
  GUARD_MANUAL_REENTRY_DISMISSED: '人工重入信号已忽略',
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
  MANUAL_REENTRY_SIGNAL: '人工重入信号已生成（等待确认）',
  REENTRY_WINDOW_INFEASIBLE: '自动重入窗口不可行（恢复门槛越过追价上限）',
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

// 重入 AI 助手：可折叠可复制文本区块（对齐 Prompt 透明化样式）
function CopyableSection({
  title,
  content,
}: {
  title: string
  content: string
}) {
  const [copied, setCopied] = useState(false)
  const doCopy = async (e: ReactMouseEvent) => {
    e.preventDefault()
    e.stopPropagation()
    try {
      await navigator.clipboard.writeText(content)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      window.prompt('复制失败，请手动复制：', content.slice(0, 500))
    }
  }
  return (
    <details className="rounded border border-[#2B3139] bg-[#0B0E11]">
      <summary className="flex cursor-pointer items-center justify-between px-3 py-2 text-sm text-[#EAECEF]">
        <span>
          {title}（{content.length} chars）
        </span>
        <button
          onClick={(e) => void doCopy(e)}
          className="rounded bg-[#2B3139] px-2 py-0.5 text-xs hover:bg-[#3B424C]"
        >
          {copied ? '✓ 已复制' : '复制'}
        </button>
      </summary>
      <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all border-t border-[#2B3139] p-3 text-xs leading-relaxed text-[#B7BDC6]">
        {content || '（空）'}
      </pre>
    </details>
  )
}

// 内外部 AI 结论标签样式（ENTER 绿 / WAIT 黄 / SKIP 红）
const verdictStyles: Record<string, string> = {
  ENTER: 'bg-[#0ECB81]/15 text-[#0ECB81] border-[#0ECB81]',
  WAIT: 'bg-[#F0B90B]/15 text-[#F0B90B] border-[#F0B90B]',
  SKIP: 'bg-[#F6465D]/15 text-[#F6465D] border-[#F6465D]',
  ABANDON: 'bg-[#F6465D]/15 text-[#F6465D] border-[#F6465D]',
}
const verdictLabels: Record<string, string> = {
  ENTER: 'ENTER · 建议重入',
  WAIT: 'WAIT · 继续观察',
  SKIP: 'SKIP · 建议忽略',
  ABANDON: 'ABANDON · 建议放弃候选',
}

// 内置 AI 结论卡片：verdict 徽标 + 置信度 + 依据/风险列表（reasons JSON）
function InternalVerdictCard({ analysis }: { analysis: ReentryAIAnalysis }) {
  const parsed = useMemo(() => {
    try {
      return JSON.parse(analysis.reasons || '{}') as {
        reasons?: string[]
        risk_notes?: string[]
        suggested_notional?: number
        regime?: string
        size_factor?: number
        entry_price_low?: number
        entry_price_high?: number
        attention_price_low?: number
        attention_price_high?: number
        ttl_seconds?: number
        next_review_seconds?: number
      }
    } catch {
      return {}
    }
  }, [analysis.reasons])
  if (!analysis.verdict) return null
  return (
    <div
      className={`space-y-2 rounded border p-3 ${verdictStyles[analysis.verdict] || 'border-[#2B3139]'}`}
    >
      <div className="flex flex-wrap items-center gap-3 text-sm font-medium">
        <span>
          内置 AI 结论：{verdictLabels[analysis.verdict] || analysis.verdict}
        </span>
        <span className="text-xs font-normal">
          置信度 {(analysis.confidence * 100).toFixed(0)}%
        </span>
        {parsed.suggested_notional ? (
          <span className="text-xs font-normal">
            建议金额 {parsed.suggested_notional.toFixed(2)} USDT
          </span>
        ) : null}
        {analysis.candidate_id > 0 && parsed.regime && (
          <span className="text-xs font-normal">
            市场状态 {parsed.regime} · 仓位系数{' '}
            {(parsed.size_factor ?? 0).toFixed(2)}
          </span>
        )}
      </div>
      {(parsed.reasons?.length ?? 0) > 0 && (
        <ul className="list-disc space-y-1 pl-5 text-xs text-[#EAECEF]">
          {parsed.reasons!.map((r, i) => (
            <li key={i}>{r}</li>
          ))}
        </ul>
      )}
      {(parsed.risk_notes?.length ?? 0) > 0 && (
        <div className="text-xs text-[#B7BDC6]">
          风险提示：{parsed.risk_notes!.join('；')}
        </div>
      )}
      {analysis.candidate_id > 0 ? (
        <div className="space-y-1 text-xs text-[#848E9C]">
          <div>
            入场区间：{parsed.entry_price_low ?? '—'} –{' '}
            {parsed.entry_price_high ?? '—'}；关注区间：
            {parsed.attention_price_low ?? '—'} –{' '}
            {parsed.attention_price_high ?? '—'}
          </div>
          <div>
            这是生产候选结论；只有
            ENTER、置信度达标且仓位、价格漂移、预算和保护预检全部通过时才可能真实下单。
          </div>
        </div>
      ) : (
        <div className="text-xs text-[#848E9C]">
          这是旧信号的历史分析视图，不具备下单能力。真实重入只会由持久化 AI
          候选调度器在重新校验价格、仓位、预算和保护能力后执行。
        </div>
      )}
    </div>
  )
}

// AnalysisModalTarget 弹窗只依赖信号的标识与展示字段，历史列表（信号可能已
// 关闭）与待确认横幅共用同一弹窗
type AnalysisModalTarget = Pick<CopyGuardManualSignal, 'id' | 'symbol' | 'side'>

// 重入 AI 助手分析弹窗：三段可复制（System Prompt / User Prompt / 纯数据 JSON）
// + 内置 AI 结论（Phase 2）+ 外部 AI 结论粘贴区（永久可编辑，供准确率对比留档）。
// 数据由 reentryadvisor 插件在信号产生后自动生成；可手动重新生成新快照（60s 冷却）。
function AnalysisModal({
  signal,
  onClose,
}: {
  signal: AnalysisModalTarget
  onClose: () => void
}) {
  const {
    data: analyses = [],
    mutate,
    isLoading,
  } = useSWR<ReentryAIAnalysis[]>(
    `reentry-analyses-${signal.id}`,
    () => api.getReentryAnalyses(signal.id),
    { refreshInterval: 10000 }
  )
  const [selectedId, setSelectedId] = useState<number | null>(null)
  const analysis =
    analyses.find((a) => a.id === selectedId) ?? analyses[0] ?? null
  const [externalText, setExternalText] = useState('')
  const [externalVerdict, setExternalVerdict] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  // 切换快照时同步外部结论编辑区（未保存的编辑不跨快照带走）
  const analysisId = analysis?.id
  useEffect(() => {
    if (!analysis) return
    setExternalText(analysis.external_response)
    setExternalVerdict(analysis.external_verdict)
  }, [analysisId])

  const doRegenerate = async () => {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const res = await api.regenerateReentryAnalysis(signal.id)
      setNotice('数据包已重新生成（新快照）')
      setSelectedId(res.analysis.id)
      void mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  const doSaveExternal = async () => {
    if (!analysis) return
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await api.saveReentryExternal(analysis.id, {
        external_response: externalText,
        external_verdict: externalVerdict,
      })
      setNotice('外部 AI 结论已保存')
      void mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  const doAnalyzeInternal = async () => {
    if (!analysis) return
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await api.analyzeReentryAnalysis(analysis.id)
      setNotice('内置 AI 分析已开始，结果稍后自动出现在本页（10 秒轮询）')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="flex max-h-[88vh] w-full max-w-3xl flex-col gap-3 overflow-y-auto rounded-lg border border-[#2B3139] bg-[#181A20] p-6 text-sm">
        <div className="flex items-center justify-between">
          <div className="text-base font-bold">
            重入分析数据：{signal.symbol} {sideLabel(signal.side)}单
            <span className="ml-2 text-xs font-normal text-[#848E9C]">
              信号 #{signal.id}
            </span>
          </div>
          <button
            onClick={onClose}
            className="rounded bg-[#2B3139] px-3 py-1 text-xs hover:bg-[#3B424C]"
          >
            关闭
          </button>
        </div>

        {analyses.length === 0 ? (
          <div className="rounded bg-[#0B0E11] p-4 text-[#848E9C]">
            {isLoading
              ? '加载中…'
              : '分析数据尚未生成（插件每 5 秒自动处理新信号）。若长时间未出现，可能插件已关闭，可点击下方按钮手动生成。'}
          </div>
        ) : (
          analysis && (
            <>
              <div className="flex flex-wrap items-center gap-3 text-xs text-[#848E9C]">
                <span>
                  快照时间 {dateLabel(analysis.snapshot_at)} · 模板{' '}
                  {analysis.prompt_version}
                </span>
                {analyses.length > 1 && (
                  <select
                    value={analysis.id}
                    onChange={(e) => setSelectedId(Number(e.target.value))}
                    className="rounded border border-[#2B3139] bg-[#0B0E11] px-2 py-1 text-[#EAECEF]"
                  >
                    {analyses.map((a) => (
                      <option key={a.id} value={a.id}>
                        快照 #{a.id} · {dateLabel(a.snapshot_at)}
                      </option>
                    ))}
                  </select>
                )}
              </div>
              {!analysis.market_data_available && (
                <div className="rounded border border-[#F0B90B] bg-[#F0B90B]/10 p-3 text-xs text-[#F0B90B]">
                  该币种无 Binance
                  市场数据，市场层缺失（仓位层数据完整）——请结合 OKX
                  行情人工判断。
                </div>
              )}
              {analysis.missing_fields && (
                <div className="text-xs text-[#848E9C]">
                  部分字段降级：{analysis.missing_fields}
                </div>
              )}
              <CopyableSection
                title="System Prompt"
                content={analysis.system_prompt}
              />
              <CopyableSection
                title="User Prompt（含数据包）"
                content={analysis.user_prompt}
              />
              <CopyableSection
                title="纯数据 JSON（喂外部 AI 用）"
                content={analysis.datapack_json}
              />
              <InternalVerdictCard analysis={analysis} />
              {analysis.raw_response && !analysis.verdict && (
                <div className="rounded border border-[#F0B90B] bg-[#F0B90B]/10 p-3 text-xs text-[#F0B90B]">
                  内置 AI 已返回但结论未能解析为结构化 JSON，原始回复见下方。
                </div>
              )}
              {analysis.raw_response && (
                <CopyableSection
                  title="内置 AI 原始回复"
                  content={analysis.raw_response}
                />
              )}
              {analysis.outcome_pnl != null && (
                <div className="text-xs text-[#848E9C]">
                  真实结局：本次重入尝试已闭合，净盈亏{' '}
                  <span
                    className={
                      analysis.outcome_pnl >= 0
                        ? 'text-[#0ECB81]'
                        : 'text-[#F6465D]'
                    }
                  >
                    {money(analysis.outcome_pnl)}
                  </span>
                  （含手续费）
                </div>
              )}
              <div className="space-y-2 rounded border border-[#2B3139] bg-[#0B0E11] p-3">
                <div className="flex items-center justify-between text-sm text-[#EAECEF]">
                  <span>外部 AI 结论（粘贴留档，供准确率对比）</span>
                  <select
                    value={externalVerdict}
                    onChange={(e) => setExternalVerdict(e.target.value)}
                    className="rounded border border-[#2B3139] bg-[#181A20] px-2 py-1 text-xs"
                  >
                    <option value="">结论标签（可选）</option>
                    <option value="ENTER">ENTER · 建议重入</option>
                    <option value="WAIT">WAIT · 继续观察</option>
                    <option value="SKIP">SKIP · 建议忽略</option>
                  </select>
                </div>
                <textarea
                  value={externalText}
                  onChange={(e) => setExternalText(e.target.value)}
                  rows={5}
                  placeholder="将外部 AI（如 ChatGPT / DeepSeek 网页版）的分析结论粘贴到这里保存…"
                  className="w-full rounded border border-[#2B3139] bg-[#181A20] p-2 text-xs text-[#EAECEF]"
                />
                <div className="flex justify-end">
                  <button
                    onClick={() => void doSaveExternal()}
                    disabled={busy}
                    className="rounded bg-[#2B3139] px-3 py-1 text-xs hover:bg-[#3B424C] disabled:opacity-40"
                  >
                    保存外部结论
                  </button>
                </div>
              </div>
            </>
          )
        )}

        {error && <div className="text-xs text-[#F6465D]">{error}</div>}
        {notice && <div className="text-xs text-[#0ECB81]">{notice}</div>}
        <div className="flex items-center justify-between">
          <span className="text-xs text-[#848E9C]">
            数据快照有时效性：距信号产生较久后请先重新生成再发给 AI 判断。
          </span>
          <span className="flex gap-2">
            {analysis && (
              <button
                onClick={() => void doAnalyzeInternal()}
                disabled={busy}
                className="rounded bg-[#2B3139] px-3 py-1 text-xs hover:bg-[#3B424C] disabled:opacity-40"
                title="用配置的内置模型分析当前快照（同一 Prompt，可与外部 AI 对比）"
              >
                {analysis.verdict ? '重跑内置 AI' : '内置 AI 分析'}
              </button>
            )}
            <button
              onClick={() => void doRegenerate()}
              disabled={busy}
              className="rounded bg-[#F0B90B] px-3 py-1 text-xs font-medium text-black hover:opacity-90 disabled:opacity-40"
            >
              {busy
                ? '处理中…'
                : analyses.length === 0
                  ? '立即生成'
                  : '重新生成（新快照）'}
            </button>
          </span>
        </div>
      </div>
    </div>
  )
}

function CandidateAnalysisModal({
  analysisId,
  onClose,
}: {
  analysisId: number
  onClose: () => void
}) {
  const { data, error, isLoading } = useSWR<{
    analysis: ReentryAIAnalysis
    evaluations: ReentryAIDecisionEvaluation[]
  }>(`candidate-analysis-${analysisId}`, () =>
    api.getReentryAnalysis(analysisId)
  )
  const analysis = data?.analysis
  const evaluations = data?.evaluations ?? []
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <div className="flex max-h-[88vh] w-full max-w-3xl flex-col gap-3 overflow-y-auto rounded-lg border border-[#2B3139] bg-[#181A20] p-6 text-sm">
        <div className="flex items-center justify-between">
          <div className="font-bold text-[#EAECEF]">
            生产 AI 候选分析 #{analysisId}
          </div>
          <button
            onClick={onClose}
            className="rounded bg-[#2B3139] px-3 py-1 text-xs"
          >
            关闭
          </button>
        </div>
        {isLoading && <div className="text-[#848E9C]">加载中…</div>}
        {error && (
          <div className="text-[#F6465D]">
            {error instanceof Error ? error.message : String(error)}
          </div>
        )}
        {analysis && (
          <>
            <div className="text-xs text-[#848E9C]">
              {analysis.symbol} {sideLabel(analysis.side)} · cycle{' '}
              {analysis.cycle_id} · attempt {analysis.attempt_no} · 快照{' '}
              {dateLabel(analysis.snapshot_at)} · Prompt{' '}
              {analysis.prompt_version} · 状态 {analysis.call_status}
            </div>
            {analysis.call_error && (
              <div className="rounded border border-[#F6465D] bg-[#F6465D]/10 p-3 text-xs text-[#F6465D]">
                {analysis.call_error}
              </div>
            )}
            <InternalVerdictCard analysis={analysis} />
            <div className="space-y-2 rounded border border-[#2B3139] bg-[#0B0E11] p-3 text-xs">
              <div className="font-medium text-[#EAECEF]">
                决策后验评价（确定性口径）
              </div>
              {evaluations.length === 0 ? (
                <div className="text-[#848E9C]">
                  30 分钟与 2
                  小时窗口成熟后生成；领航员最终窗口在周期闭合后生成。 不会让 AI
                  给自己打分，也不会影响交易。
                </div>
              ) : (
                evaluations.map((evaluation) => (
                  <div
                    key={evaluation.id}
                    className="grid gap-1 rounded bg-[#181A20] p-3 text-[#B7BDC6] md:grid-cols-2"
                  >
                    <span>
                      结论：{evaluation.decision_outcome} · 市场：
                      {evaluation.market_outcome}
                    </span>
                    <span>
                      窗口：{evaluation.horizon} · 数据：
                      {evaluation.data_quality} · 执行证据：
                      {evaluation.execution_data_quality} · 评价 v
                      {evaluation.evaluation_version}
                    </span>
                    <span>
                      MFE +{evaluation.mfe_atr.toFixed(2)} ATR / MAE -
                      {evaluation.mae_atr.toFixed(2)} ATR
                    </span>
                    <span>
                      覆盖率 {(evaluation.coverage_ratio * 100).toFixed(0)}% ·{' '}
                      {evaluation.sample_count} 条采样
                    </span>
                    <span>
                      窗口：{dateLabel(evaluation.window_start_at)} →{' '}
                      {dateLabel(evaluation.window_end_at)}
                    </span>
                    <span>
                      真实执行：{evaluation.actual_executed ? '是' : '否'}
                      {evaluation.actual_pnl != null
                        ? ` · ${money(evaluation.actual_pnl)}`
                        : ''}
                    </span>
                    <span className="md:col-span-2 text-[#848E9C]">
                      {evaluation.reason}
                    </span>
                  </div>
                ))
              )}
            </div>
            <CopyableSection
              title="生产 System Prompt"
              content={analysis.system_prompt}
            />
            <CopyableSection
              title="生产 User Prompt"
              content={analysis.user_prompt}
            />
            <CopyableSection
              title="候选数据快照 JSON"
              content={analysis.datapack_json}
            />
            {analysis.raw_response && (
              <CopyableSection
                title="模型原始回复"
                content={analysis.raw_response}
              />
            )}
          </>
        )}
      </div>
    </div>
  )
}

// 准确率格子（内部/外部 AI 各一行）
function accuracyText(scored: number, correct: number) {
  if (scored === 0) return '暂无可评分样本'
  return `${correct}/${scored} 正确（${((correct / scored) * 100).toFixed(0)}%）`
}

// 重入 AI 助手设置卡片（折叠）：插件开关 / 自动分析开关 / 模型选择 /
// Prompt 模板 / 超时 + 内外部 AI 结论分布与准确率统计。
// 评分口径：仅已执行且重入尝试闭合对账的信号；ENTER 且盈利、SKIP 且亏损记为
// 正确，WAIT 不计入。
function AdvisorSettingsCard() {
  const { data: cfgData, mutate: mutateCfg } = useSWR(
    'reentry-advisor-config',
    () => api.getReentryConfig()
  )
  const { data: stats } = useSWR<ReentryAIStats>(
    'reentry-advisor-stats',
    () => api.getReentryStats(),
    { refreshInterval: 60000 }
  )
  const { data: models = [] } = useSWR<AIModel[]>('model-configs', () =>
    api.getModelConfigs()
  )
  const { data: diagnostics = [], mutate: mutateDiagnostics } = useSWR<
    ReentryAIDiagnostic[]
  >('reentry-ai-diagnostics', () => api.getReentryAIDiagnostics(5))
  const enabledModels = models.filter((m) => m.enabled)
  // A1：分析历史（跨信号，含已执行/已忽略的旧信号，供准确率复盘）
  const { data: history = [] } = useSWR<ReentryAIAnalysis[]>(
    'reentry-advisor-history',
    () => api.getReentryHistory(50),
    { refreshInterval: 30000 }
  )
  const [viewing, setViewing] = useState<AnalysisModalTarget | null>(null)
  const [viewingCandidateAnalysisID, setViewingCandidateAnalysisID] = useState<
    number | null
  >(null)

  const [draft, setDraft] = useState<ReentryAIConfig | null>(null)
  const [busy, setBusy] = useState(false)
  const [testing, setTesting] = useState(false)
  const [diagnostic, setDiagnostic] = useState<ReentryAIDiagnostic | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const cfg = draft ?? cfgData?.config ?? null

  const update = (patch: Partial<ReentryAIConfig>) => {
    if (!cfg) return
    setDraft({ ...cfg, ...patch })
  }
  const doSave = async () => {
    if (!cfg) return
    setBusy(true)
    setError('')
    setNotice('')
    try {
      await api.saveReentryConfig(cfg)
      setNotice('配置已保存')
      setDraft(null)
      void mutateCfg()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  const doConnectionTest = async () => {
    setTesting(true)
    setDiagnostic(null)
    setError('')
    setNotice('')
    try {
      const result = await api.testReentryAIConnection()
      setDiagnostic(result)
      if (result.success) {
        setNotice('AI 连接、Prompt 与严格 JSON Schema 自检通过')
      } else {
        setError(result.error || 'AI 已响应，但严格 JSON Schema 自检未通过')
      }
      void mutateDiagnostics()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setTesting(false)
    }
  }

  return (
    <details className="rounded-lg border border-[#2B3139] bg-[#181A20]">
      <summary className="cursor-pointer px-4 py-3 text-sm font-medium text-[#EAECEF]">
        🤖 重入 AI 助手设置与统计
        <span className="ml-2 text-xs font-normal text-[#848E9C]">
          持续候选调度、内置 AI 分析与全局执行安全开关
        </span>
      </summary>
      <div className="space-y-4 border-t border-[#2B3139] p-4 text-sm">
        {!cfg ? (
          <div className="text-[#848E9C]">配置加载中…</div>
        ) : (
          <>
            <div className="grid gap-3 md:grid-cols-2">
              <label className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={cfg.enabled}
                  onChange={(e) => update({ enabled: e.target.checked })}
                />
                <span>
                  启用候选调度
                  <span className="ml-1 text-xs text-[#848E9C]">
                    （持久化观察、事件触发与额度控制）
                  </span>
                </span>
              </label>
              <label className="flex items-center gap-2">
                <input
                  type="checkbox"
                  checked={cfg.ai_enabled}
                  onChange={(e) =>
                    update({
                      ai_enabled: e.target.checked,
                      // 关闭自动分析时联动关闭自动入场（后端也会拒绝该组合）
                      ...(e.target.checked
                        ? {}
                        : { auto_entry_enabled: false }),
                    })
                  }
                />
                <span>
                  允许内置 AI 分析
                  <span className="ml-1 text-xs text-[#848E9C]">
                    （WAIT 不发邮件，调用受候选额度与退避限制）
                  </span>
                </span>
              </label>
              <label className="flex items-center gap-2">
                <span className="w-24 shrink-0 text-[#848E9C]">分析模型</span>
                <select
                  value={cfg.model}
                  onChange={(e) => update({ model: e.target.value })}
                  className="flex-1 rounded border border-[#2B3139] bg-[#0B0E11] px-2 py-1"
                >
                  <option value="">自动（优先已启用的 DeepSeek）</option>
                  {enabledModels.map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.name}（{m.provider}）
                    </option>
                  ))}
                </select>
              </label>
              <label className="flex items-center gap-2">
                <span className="w-24 shrink-0 text-[#848E9C]">调用超时</span>
                <input
                  type="number"
                  min={10}
                  max={300}
                  value={cfg.timeout_seconds}
                  onChange={(e) =>
                    update({ timeout_seconds: Number(e.target.value) })
                  }
                  className="w-24 rounded border border-[#2B3139] bg-[#0B0E11] px-2 py-1"
                />
                <span className="text-xs text-[#848E9C]">秒</span>
              </label>
            </div>
            <div
              className={`space-y-2 rounded border p-3 ${
                cfg.auto_entry_enabled
                  ? 'border-[#F6465D] bg-[#F6465D]/5'
                  : 'border-[#2B3139] bg-[#0B0E11]'
              }`}
            >
              <div className="flex flex-wrap items-center gap-4">
                <label className="flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={cfg.auto_entry_enabled}
                    disabled={!cfg.ai_enabled}
                    onChange={(e) =>
                      update({ auto_entry_enabled: e.target.checked })
                    }
                  />
                  <span className="font-medium">AI 重入全局执行安全开关</span>
                </label>
                <span className="text-xs text-[#848E9C]">
                  真实入场门槛来自每个交易员配置，默认 0.80、范围
                  0.70–0.95；旧全局 0.7 字段不再控制生产候选。
                </span>
              </div>
              <div className="text-xs leading-relaxed text-[#848E9C]">
                这是 ai_guarded 交易员配置的全局先决安全开关。开启后 AI
                仍只能建议是否进场与缩小仓位；未完全平仓、快照陈旧、价格漂移、预算不足或保护不可用时，确定性风控会拒绝下单并回到观察。
                {!cfg.ai_enabled && (
                  <span className="text-[#F0B90B]">
                    {' '}
                    需先开启「自动内置 AI 分析」。
                  </span>
                )}
                {cfg.auto_entry_enabled && (
                  <span className="text-[#F6465D]">
                    {' '}
                    ⚠️ AI
                    判断可能出错，自动入场直接动用真实资金，请确保已理解全部风险并从小金额开始验证。
                  </span>
                )}
              </div>
            </div>
            <div className="space-y-2">
              <div className="text-xs text-[#848E9C]">
                分析关注点补充（最多 2000 字）。它只提示 AI
                额外检查哪些证据，不能覆盖生产职责、决策枚举、JSON Schema
                或代码风控。
              </div>
              <textarea
                value={cfg.analysis_focus ?? ''}
                onChange={(e) => update({ analysis_focus: e.target.value })}
                rows={4}
                maxLength={2000}
                placeholder="例如：重点检查现货 CVD 与合约 CVD 是否背离，以及领航员最近 30 分钟是否持续减仓。"
                className="w-full rounded border border-[#2B3139] bg-[#0B0E11] p-2 text-xs text-[#EAECEF]"
              />
              <details className="rounded border border-[#2B3139] bg-[#0B0E11] text-xs">
                <summary className="cursor-pointer px-3 py-2 text-[#B7BDC6]">
                  生产 Prompt{' '}
                  {cfgData?.production_prompt_version ?? 'v3-ai-guarded'}
                  （固定核心，只读）
                </summary>
                <pre className="max-h-64 overflow-auto whitespace-pre-wrap border-t border-[#2B3139] p-3 text-[#848E9C]">
                  {cfgData?.production_prompt || '加载中…'}
                </pre>
              </details>
              {(cfg.prompt_template || cfgData?.legacy_default_prompt) && (
                <details className="rounded border border-[#2B3139] bg-[#0B0E11] text-xs">
                  <summary className="cursor-pointer px-3 py-2 text-[#848E9C]">
                    历史人工信号 Prompt（只读兼容，不影响 ai_guarded）
                  </summary>
                  <pre className="max-h-52 overflow-auto whitespace-pre-wrap border-t border-[#2B3139] p-3 text-[#848E9C]">
                    {cfg.prompt_template || cfgData?.legacy_default_prompt}
                  </pre>
                </details>
              )}
            </div>
            <div className="flex items-center justify-between">
              <span className="text-xs">
                {error && <span className="text-[#F6465D]">{error}</span>}
                {notice && <span className="text-[#0ECB81]">{notice}</span>}
              </span>
              <span className="flex gap-2">
                <button
                  onClick={() => void doConnectionTest()}
                  disabled={busy || testing || !!draft}
                  title={
                    draft
                      ? '请先保存配置，再测试服务器当前生效的模型'
                      : '会消耗一次很小的模型调用，但绝不会创建候选或订单'
                  }
                  className="rounded bg-[#2B3139] px-4 py-1.5 text-xs text-[#EAECEF] disabled:opacity-40"
                >
                  {testing ? '自检中…' : 'AI 连接自检（零交易）'}
                </button>
                <button
                  onClick={() => void doSave()}
                  disabled={busy || !draft}
                  className="rounded bg-[#F0B90B] px-4 py-1.5 text-xs font-medium text-black hover:opacity-90 disabled:opacity-40"
                >
                  {busy ? '保存中…' : '保存配置'}
                </button>
              </span>
            </div>
            {(diagnostic || diagnostics[0]) &&
              (() => {
                const d = diagnostic ?? diagnostics[0]
                return (
                  <div
                    className={`space-y-2 rounded border p-3 text-xs ${
                      d.success
                        ? 'border-[#0ECB81] bg-[#0ECB81]/5'
                        : 'border-[#F6465D] bg-[#F6465D]/5'
                    }`}
                  >
                    <div className="font-medium text-[#EAECEF]">
                      最近自检：{d.success ? '通过' : '失败'} · {d.provider}/
                      {d.model || '未解析模型'} · {d.latency_ms} ms ·{' '}
                      {d.prompt_version} · {dateLabel(d.created_at)}
                    </div>
                    {d.error && <div className="text-[#F6465D]">{d.error}</div>}
                    {d.parsed_json && (
                      <CopyableSection
                        title="自检解析结果"
                        content={d.parsed_json}
                      />
                    )}
                    {d.raw_response && (
                      <CopyableSection
                        title="自检原始回复"
                        content={d.raw_response}
                      />
                    )}
                    <div className="text-[#848E9C]">
                      自检不会创建候选、占用交易额度或下单；它会消耗一次模型调用，并有
                      30 秒冷却。
                    </div>
                  </div>
                )
              })()}
          </>
        )}
        {stats && stats.total_analyses > 0 && (
          <div className="space-y-2 rounded bg-[#0B0E11] p-3">
            <div className="text-xs font-medium text-[#EAECEF]">
              结论统计（快照 {stats.total_analyses} 条 / 信号{' '}
              {stats.signals_covered} 个 / 已回填结局 {stats.scored_count} 条）
            </div>
            <div className="grid gap-x-6 gap-y-1 text-xs text-[#B7BDC6] md:grid-cols-2">
              <span>
                内置 AI：
                {['ENTER', 'WAIT', 'SKIP']
                  .map((v) => `${v} ${stats.internal_verdicts[v] || 0}`)
                  .join(' · ')}
              </span>
              <span>
                准确率{' '}
                {accuracyText(stats.internal_scored, stats.internal_correct)}
              </span>
              <span>
                外部 AI：
                {['ENTER', 'WAIT', 'SKIP']
                  .map((v) => `${v} ${stats.external_verdicts[v] || 0}`)
                  .join(' · ')}
              </span>
              <span>
                准确率{' '}
                {accuracyText(stats.external_scored, stats.external_correct)}
              </span>
            </div>
            <div className="text-xs text-[#848E9C]">
              上方为历史人工信号口径，仅作存量兼容。
            </div>
            {stats.candidate_analyses > 0 && (
              <div className="space-y-1 border-t border-[#2B3139] pt-2 text-xs text-[#B7BDC6]">
                <div className="font-medium text-[#EAECEF]">
                  AI 持续候选：{stats.candidate_analyses} 条分析记录
                </div>
                <div>
                  决策：
                  {['ENTER', 'WAIT', 'ABANDON']
                    .map((v) => `${v} ${stats.candidate_decisions?.[v] || 0}`)
                    .join(' · ')}
                </div>
                <div>
                  调用状态：
                  {[
                    'PENDING',
                    'RUNNING',
                    'COMPLETED',
                    'INVALID',
                    'FAILED',
                    'PREPARE_FAILED',
                    'UNACTIONABLE',
                    'SKIPPED',
                  ]
                    .map(
                      (v) => `${v} ${stats.candidate_call_statuses?.[v] || 0}`
                    )
                    .join(' · ')}
                </div>
                <div className="text-[#848E9C]">
                  WAIT、ABANDON、非法输出与失败调用全部保留；仅实际 ENTER 且
                  attempt 已对账的 {stats.candidate_scored}
                  条可回填真实盈亏，其中盈利 {stats.candidate_profitable}
                  条。不会把未执行决策伪装成反事实盈亏。
                </div>
                <div className="text-[#848E9C]">
                  执行漏斗：REQUESTED {stats.candidate_execution_requested ?? 0}{' '}
                  → SUBMITTED {stats.candidate_execution_submitted ?? 0} →
                  FILLED {stats.candidate_execution_filled ?? 0} → PROTECTED{' '}
                  {stats.candidate_execution_protected ?? 0}
                </div>
                <div className="border-t border-[#2B3139] pt-2">
                  后验评价：{stats.candidate_evaluated} 条，其中不可评分{' '}
                  {stats.candidate_unscorable} 条；市场结果{' '}
                  {Object.entries(stats.candidate_market_outcomes ?? {})
                    .map(([outcome, count]) => `${outcome} ${count}`)
                    .join(' · ') || '暂无'}
                </div>
                <div className="text-[#848E9C]">
                  决策效果：
                  {Object.entries(stats.candidate_evaluation_outcomes ?? {})
                    .map(([outcome, count]) => `${outcome} ${count}`)
                    .join(' · ') || '周期闭合后生成'}
                </div>
              </div>
            )}
          </div>
        )}
        {history.length > 0 && (
          <div className="space-y-2 rounded bg-[#0B0E11] p-3">
            <div className="text-xs font-medium text-[#EAECEF]">
              分析历史（最近 {history.length}{' '}
              条快照，含生产候选和历史信号，点击查看详情）
            </div>
            <div className="max-h-72 overflow-y-auto">
              <table className="w-full text-left text-xs">
                <thead className="sticky top-0 bg-[#0B0E11] text-[#848E9C]">
                  <tr>
                    <th className="py-1 pr-3 font-normal">快照时间</th>
                    <th className="py-1 pr-3 font-normal">交易对</th>
                    <th className="py-1 pr-3 font-normal">信号</th>
                    <th className="py-1 pr-3 font-normal">内置 AI</th>
                    <th className="py-1 pr-3 font-normal">外部 AI</th>
                    <th className="py-1 pr-3 font-normal">结局盈亏</th>
                  </tr>
                </thead>
                <tbody className="text-[#B7BDC6]">
                  {history.map((a) => (
                    <tr
                      key={a.id}
                      onClick={() => {
                        if (a.candidate_id > 0) {
                          setViewingCandidateAnalysisID(a.id)
                          return
                        }
                        setViewing({
                          id: a.signal_id,
                          symbol: a.symbol,
                          side: a.side,
                        })
                      }}
                      className="cursor-pointer border-t border-[#181A20] hover:bg-[#181A20]"
                    >
                      <td className="py-1.5 pr-3">
                        {dateLabel(a.snapshot_at)}
                      </td>
                      <td className="py-1.5 pr-3">
                        {a.symbol} {sideLabel(a.side)}单
                      </td>
                      <td className="py-1.5 pr-3">
                        {a.candidate_id > 0
                          ? `候选 #${a.candidate_id} / 第 ${a.attempt_no} 次`
                          : `历史信号 #${a.signal_id}`}
                      </td>
                      <td className="py-1.5 pr-3">
                        {a.verdict
                          ? `${verdictLabels[a.verdict] || a.verdict}（${(a.confidence * 100).toFixed(0)}%）`
                          : a.raw_response
                            ? '未解析'
                            : '—'}
                      </td>
                      <td className="py-1.5 pr-3">
                        {a.external_verdict
                          ? verdictLabels[a.external_verdict] ||
                            a.external_verdict
                          : '—'}
                      </td>
                      <td className="py-1.5 pr-3">
                        {a.outcome_pnl != null ? (
                          <span
                            className={
                              a.outcome_pnl >= 0
                                ? 'text-[#0ECB81]'
                                : 'text-[#F6465D]'
                            }
                          >
                            {money(a.outcome_pnl)}
                          </span>
                        ) : (
                          '—'
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </div>
      {viewing && (
        <AnalysisModal signal={viewing} onClose={() => setViewing(null)} />
      )}
      {viewingCandidateAnalysisID != null && (
        <CandidateAnalysisModal
          analysisId={viewingCandidateAnalysisID}
          onClose={() => setViewingCandidateAnalysisID(null)}
        />
      )}
    </details>
  )
}

// A2：市场指标实时预览卡片（折叠）。与信号无关，任意币种随时查看数据包
// 同口径的市场层指标（后端 60s 结果缓存，打开时前端每 60s 自动刷新），
// 用于持续观察指标质量、评估增删指标。
function MarketPreviewCard() {
  const [symbolInput, setSymbolInput] = useState('BTCUSDT')
  const [symbol, setSymbol] = useState('')
  const {
    data: preview,
    error,
    isValidating,
  } = useSWR<ReentryMarketPreview>(
    symbol ? `reentry-market-preview-${symbol}` : null,
    () => api.getReentryMarketPreview(symbol),
    { refreshInterval: 60000, revalidateOnFocus: false }
  )
  const doQuery = () => {
    const s = symbolInput.trim().toUpperCase()
    if (s) setSymbol(s)
  }
  const m = preview?.market ?? null
  const cvdLine = (
    cvd?: Record<string, { slope_recent: string; divergence_note?: string }>
  ) =>
    cvd
      ? ['5m', '15m', '1h', '4h']
          .filter((tf) => cvd[tf])
          .map((tf) => `${tf} ${cvd[tf].slope_recent}`)
          .join(' · ')
      : '—'

  return (
    <details className="rounded-lg border border-[#2B3139] bg-[#181A20]">
      <summary className="cursor-pointer px-4 py-3 text-sm font-medium text-[#EAECEF]">
        📊 市场指标实时预览
        <span className="ml-2 text-xs font-normal text-[#848E9C]">
          与重入数据包同口径的市场层指标，任意币种随时查看（60s 缓存刷新）
        </span>
      </summary>
      <div className="space-y-3 border-t border-[#2B3139] p-4 text-sm">
        <div className="flex flex-wrap items-center gap-2">
          <input
            value={symbolInput}
            onChange={(e) => setSymbolInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && doQuery()}
            placeholder="BTCUSDT"
            className="w-40 rounded border border-[#2B3139] bg-[#0B0E11] px-2 py-1 text-xs uppercase"
          />
          <button
            onClick={doQuery}
            className="rounded bg-[#F0B90B] px-3 py-1 text-xs font-medium text-black hover:opacity-90"
          >
            查看
          </button>
          {preview && (
            <span className="text-xs text-[#848E9C]">
              快照 {dateLabel(preview.generated_at)}
              {isValidating ? ' · 刷新中…' : ''}
            </span>
          )}
        </div>
        {error != null && (
          <div className="text-xs text-[#F6465D]">
            {error instanceof Error ? error.message : String(error)}
          </div>
        )}
        {preview && !preview.futures_available && (
          <div className="rounded border border-[#F0B90B] bg-[#F0B90B]/10 p-3 text-xs text-[#F0B90B]">
            {preview.symbol} 无 Binance
            合约市场数据，无法预览（重入数据包对此类币种也会缺失市场层）。
          </div>
        )}
        {m && preview && (
          <>
            <div className="grid gap-x-6 gap-y-1.5 text-xs text-[#B7BDC6] md:grid-cols-2">
              <span>
                当前价 <span className="text-[#EAECEF]">{m.current_price}</span>
                <span className="ml-1 text-[#848E9C]">
                  （{m.current_price_source}）
                </span>
              </span>
              <span>
                ATR(OKX 1h){' '}
                <span className="text-[#EAECEF]">
                  {preview.atr_okx_1h > 0 ? preview.atr_okx_1h : '获取失败'}
                </span>
              </span>
              {m.funding && (
                <span>
                  资金费率{' '}
                  <span className="text-[#EAECEF]">
                    {(m.funding.current_rate * 100).toFixed(4)}%
                  </span>
                  <span className="ml-1 text-[#848E9C]">
                    （{m.funding.state} · 10 日分位{' '}
                    {m.funding.current_percentile_10d}% · 距结算{' '}
                    {Math.round(m.funding.next_funding_minutes)} 分钟）
                  </span>
                </span>
              )}
              {m.basis && (
                <span>
                  基差{' '}
                  <span
                    className={
                      m.basis.basis_pct >= 0
                        ? 'text-[#0ECB81]'
                        : 'text-[#F6465D]'
                    }
                  >
                    {m.basis.basis_pct}%
                  </span>
                  <span className="ml-1 text-[#848E9C]">（正=升水）</span>
                </span>
              )}
              {m.open_interest && (
                <span>
                  持仓量{' '}
                  <span className="text-[#EAECEF]">
                    {(m.open_interest.latest_usd / 1e6).toFixed(1)}M USD
                  </span>
                  <span className="ml-1 text-[#848E9C]">
                    （1h {m.open_interest.change_pct['1h'] ?? '—'}% · 4h{' '}
                    {m.open_interest.change_pct['4h'] ?? '—'}% · 24h{' '}
                    {m.open_interest.change_pct['24h'] ?? '—'}%）
                  </span>
                </span>
              )}
              {m.open_interest && (
                <span>
                  价格×OI 解读{' '}
                  <span className="text-[#EAECEF]">
                    {m.open_interest.price_oi_read_4h}
                  </span>
                </span>
              )}
              {m.long_short_ratio && (
                <span>
                  多空比{' '}
                  <span className="text-[#EAECEF]">
                    散户 {m.long_short_ratio.global_accounts_ratio} · 大户{' '}
                    {m.long_short_ratio.top_positions_ratio}
                  </span>
                  <span className="ml-1 text-[#848E9C]">
                    （24h 趋势 {m.long_short_ratio.global_trend_24h}）
                  </span>
                </span>
              )}
              {m.spot_to_contract_volume_ratio_24h != null &&
                m.spot_to_contract_volume_ratio_24h > 0 && (
                  <span>
                    现货/合约成交额比(24h){' '}
                    <span className="text-[#EAECEF]">
                      {m.spot_to_contract_volume_ratio_24h}
                    </span>
                  </span>
                )}
              <span>
                合约 CVD 斜率{' '}
                <span className="text-[#EAECEF]">
                  {cvdLine(m.contract_cvd)}
                </span>
              </span>
              <span>
                现货 CVD 斜率{' '}
                <span className="text-[#EAECEF]">{cvdLine(m.spot_cvd)}</span>
              </span>
              {m.klines &&
                ['1h', '4h', '1d'].map(
                  (tf) =>
                    m.klines![tf] && (
                      <span key={tf}>
                        {tf} 窗口涨跌{' '}
                        <span
                          className={
                            m.klines![tf].pct_change_window >= 0
                              ? 'text-[#0ECB81]'
                              : 'text-[#F6465D]'
                          }
                        >
                          {m.klines![tf].pct_change_window}%
                        </span>
                        <span className="ml-1 text-[#848E9C]">
                          · 量比(5/20) {m.klines![tf].volume_ratio_5_20}
                        </span>
                      </span>
                    )
                )}
              {m.support_resistance &&
                ['1h', '4h'].map(
                  (tf) =>
                    m.support_resistance![tf] && (
                      <span key={tf}>
                        {tf} 支撑/阻力{' '}
                        <span className="text-[#EAECEF]">
                          {m.support_resistance![tf].nearest_support || '—'} /{' '}
                          {m.support_resistance![tf].nearest_resistance || '—'}
                        </span>
                        <span className="ml-1 text-[#848E9C]">
                          （距 {m.support_resistance![tf].support_distance_atr}{' '}
                          / {m.support_resistance![tf].resistance_distance_atr}{' '}
                          ATR）
                        </span>
                      </span>
                    )
                )}
            </div>
            {(preview.missing_fields?.length ?? 0) > 0 && (
              <div className="text-xs text-[#848E9C]">
                部分字段降级：{preview.missing_fields!.join('、')}
              </div>
            )}
            <CopyableSection
              title="完整市场层 JSON（与数据包 market 段同结构）"
              content={JSON.stringify(m, null, 2)}
            />
          </>
        )}
        {!symbol && (
          <div className="text-xs text-[#848E9C]">
            输入 Binance 合约交易对（如
            BTCUSDT、ETHUSDT）后点击查看。指标口径与重入决策数据包完全一致，可用于评估指标有效性、决定后续增删。
          </div>
        )}
      </div>
    </details>
  )
}

/** @deprecated 后端确认入口已退役；仅保留一个发布周期用于历史前端兼容。 */
// v5.1 人工重入待确认横幅：自动重入次数用尽后出现合格信号时（邮件同步提醒），
// 用户在此确认（系统代执行）或忽略。30 秒轮询，无待处理信号时不渲染。
function ManualSignalsBanner() {
  const { data: signals = [], mutate } = useSWR<CopyGuardManualSignal[]>(
    'copy-guard-manual-signals',
    () => api.getCopyGuardManualSignals('?status=PENDING,EXECUTING'),
    { refreshInterval: 30000 }
  )
  const [confirming, setConfirming] = useState<CopyGuardManualSignal | null>(
    null
  )
  const [analyzing, setAnalyzing] = useState<CopyGuardManualSignal | null>(null)
  const [confirmAmount, setConfirmAmount] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [bannerError, setBannerError] = useState('')

  const doConfirm = async () => {
    if (!confirming) return
    // 金额可编辑：预填建议值，允许调低（上界=建议金额，下界由后端按最小下单额复核）
    const amount = Number(confirmAmount)
    if (!Number.isFinite(amount) || amount <= 0) {
      setError('请输入有效的执行金额')
      return
    }
    // +0.01 容差：预填值经 toFixed(2) 四舍五入可能比原始建议值高出半分钱
    if (amount > confirming.recommended_notional + 0.01) {
      setError(
        `执行金额不能超过建议上限 ${confirming.recommended_notional.toFixed(2)} USDT（重入风险以首仓名义为界）`
      )
      return
    }
    setBusy(true)
    setError('')
    try {
      await api.confirmCopyGuardManualSignal(confirming.id, amount)
      setNotice(
        `已确认重入 ${confirming.symbol} ${sideLabel(confirming.side)}单，系统正在执行；执行结果稍后可在周期明细的事件流中查看`
      )
      setConfirming(null)
      void mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      void mutate()
    } finally {
      setBusy(false)
    }
  }
  const doDismiss = async (sig: CopyGuardManualSignal) => {
    if (
      !window.confirm(
        `忽略 ${sig.symbol} ${sideLabel(sig.side)}单的人工重入信号？忽略后本条信号不再提示（后续行情再次满足条件会生成新信号）。`
      )
    )
      return
    setBannerError('')
    try {
      await api.dismissCopyGuardManualSignal(sig.id)
      void mutate()
    } catch (e) {
      setBannerError(e instanceof Error ? e.message : String(e))
      void mutate()
    }
  }

  if (signals.length === 0 && !notice && !bannerError) return null
  return (
    <>
      {notice && (
        <div className="flex items-center justify-between border border-[#0ECB81] bg-[#0ECB81]/10 rounded-lg p-4 text-sm text-[#0ECB81]">
          <span>{notice}</span>
          <button
            onClick={() => setNotice('')}
            className="ml-3 text-xs underline"
          >
            关闭
          </button>
        </div>
      )}
      {bannerError && (
        <div className="flex items-center justify-between border border-[#F6465D] bg-[#F6465D]/10 rounded-lg p-4 text-sm text-[#F6465D]">
          <span>{bannerError}</span>
          <button
            onClick={() => setBannerError('')}
            className="ml-3 text-xs underline"
          >
            关闭
          </button>
        </div>
      )}
      {signals.length > 0 && (
        <div className="border border-[#F0B90B] bg-[#F0B90B]/10 rounded-lg p-4 space-y-3">
          <div className="text-sm font-medium text-[#F0B90B]">
            📣 人工重入待确认（{signals.length}
            ）：自动重入次数已用尽，出现了合格的重入信号。确认后系统将实时复核并代为执行；不需要可忽略。
          </div>
          {signals.map((sig) => (
            <div
              key={sig.id}
              className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded bg-[#181A20] p-3 text-sm"
            >
              <span className="font-medium" title={sig.trader_id}>
                {sig.trader_name || sig.trader_id}
              </span>
              <span>
                {sig.symbol} {sideLabel(sig.side)}单
                {sig.margin_mode ? ` · ${sig.margin_mode}` : ''}
              </span>
              <span>信号价 {sig.trigger_price}</span>
              <span>建议金额 {sig.recommended_notional.toFixed(2)} USDT</span>
              <span>
                已止损 {sig.stop_count} 次 / 已自动重入 {sig.reentry_count} 次
              </span>
              <span
                className={
                  sig.protectable ? 'text-[#0ECB81]' : 'text-[#F6465D]'
                }
              >
                {sig.protectable
                  ? '✓ 预计可挂保护止损'
                  : '⚠️ 预计难以挂保护止损'}
              </span>
              <span className="text-xs text-[#848E9C]">
                {dateLabel(sig.created_at)}
              </span>
              <span className="ml-auto flex gap-2">
                <button
                  onClick={() => setAnalyzing(sig)}
                  className="rounded bg-[#2B3139] px-3 py-1 hover:bg-[#3B424C]"
                  title="查看重入决策数据包与 Prompt（可复制给外部 AI 判断）"
                >
                  分析数据
                </button>
                {sig.status === 'EXECUTING' ? (
                  <span className="px-3 py-1 text-[#F0B90B]">执行中…</span>
                ) : (
                  <>
                    <button
                      onClick={() => {
                        setError('')
                        setConfirmAmount(sig.recommended_notional.toFixed(2))
                        setConfirming(sig)
                      }}
                      className="rounded bg-[#0ECB81] px-3 py-1 font-medium text-black hover:opacity-90"
                    >
                      确认重入
                    </button>
                    <button
                      onClick={() => void doDismiss(sig)}
                      className="rounded bg-[#2B3139] px-3 py-1 hover:bg-[#3B424C]"
                    >
                      忽略
                    </button>
                  </>
                )}
              </span>
            </div>
          ))}
        </div>
      )}
      {analyzing && (
        <AnalysisModal signal={analyzing} onClose={() => setAnalyzing(null)} />
      )}
      {confirming && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
          <div className="w-full max-w-lg space-y-4 rounded-lg border border-[#2B3139] bg-[#181A20] p-6 text-sm">
            <div className="text-base font-bold">
              确认人工重入：{confirming.symbol} {sideLabel(confirming.side)}单
            </div>
            <div className="grid grid-cols-2 gap-2 text-[#EAECEF]">
              <span className="text-[#848E9C]">交易员</span>
              <span>{confirming.trader_name || confirming.trader_id}</span>
              <span className="text-[#848E9C]">信号价格</span>
              <span>{confirming.trigger_price}</span>
              <span className="text-[#848E9C]">执行金额（可编辑）</span>
              <span className="flex items-center gap-2">
                <input
                  type="number"
                  min={0}
                  step="0.01"
                  max={confirming.recommended_notional}
                  value={confirmAmount}
                  onChange={(e) => setConfirmAmount(e.target.value)}
                  className="w-28 rounded border border-[#2B3139] bg-[#0B0E11] px-2 py-1 text-[#EAECEF]"
                />
                <span className="text-xs text-[#848E9C]">
                  USDT · 建议 {confirming.recommended_notional.toFixed(2)}
                  （上限）
                </span>
              </span>
              <span className="text-[#848E9C]">本周期已止损</span>
              <span>{confirming.stop_count} 次</span>
              <span className="text-[#848E9C]">保护单预检</span>
              <span
                className={
                  confirming.protectable ? 'text-[#0ECB81]' : 'text-[#F6465D]'
                }
              >
                {confirming.protectable
                  ? '预计可挂出保护止损'
                  : '预计难以挂出保护止损'}
              </span>
            </div>
            <div className="rounded bg-[#0B0E11] p-3 text-xs leading-relaxed text-[#848E9C]">
              确认后系统会实时复核：领航员是否仍持有该仓位、方向是否一致、您本地是否已有同向仓位、金额是否达到最小下单额。复核通过将
              <span className="text-[#EAECEF]">立即按当前市价下单</span>
              （即使价格与信号价有偏移），并自动挂出保护止损。
              {!confirming.protectable && (
                <span className="text-[#F6465D]">
                  {' '}
                  注意：预检显示重入后可能无法建立有效保护止损；AI
                  重入成交后若无法确认保护，系统始终立即市价退出，且不会把该退出再次计为止损重入信号。
                </span>
              )}
            </div>
            {error && <div className="text-[#F6465D]">{error}</div>}
            <div className="flex justify-end gap-2">
              <button
                onClick={() => setConfirming(null)}
                disabled={busy}
                className="rounded bg-[#2B3139] px-4 py-2 disabled:opacity-40"
              >
                取消
              </button>
              <button
                onClick={() => void doConfirm()}
                disabled={busy}
                className="rounded bg-[#0ECB81] px-4 py-2 font-medium text-black disabled:opacity-40"
              >
                {busy ? '执行中…' : '确认执行'}
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}

// 旧组件保留一个发布周期供历史分析代码兼容，但不再挂载到页面；后端确认接口
// 已返回 410，真实执行入口只有下方 AI candidate 面板。
void ManualSignalsBanner

function AICandidatesPanel() {
  const { data, mutate } = useSWR<{
    candidates: CopyGuardAICandidate[]
    traderNames: Record<string, string>
  }>(
    'copy-guard-ai-candidates',
    () =>
      api.getCopyGuardAICandidates(
        '?status=WATCHING,REVIEWING,WAITING,ENTRY_PENDING,PAUSED,BUDGET_SUSPENDED&limit=100'
      ),
    { refreshInterval: 15000 }
  )
  const [busy, setBusy] = useState<number | null>(null)
  const [error, setError] = useState('')
  const [viewingAnalysisID, setViewingAnalysisID] = useState<number | null>(
    null
  )
  const candidates = data?.candidates ?? []
  if (candidates.length === 0) return null

  const operate = async (
    candidate: CopyGuardAICandidate,
    action: 'pause' | 'resume' | 'terminate'
  ) => {
    if (
      action === 'terminate' &&
      !window.confirm(
        `终止 ${candidate.symbol} 的 AI 重入观察？终止后不会自动恢复。`
      )
    )
      return
    setBusy(candidate.id)
    setError('')
    try {
      if (action === 'pause') await api.pauseCopyGuardAICandidate(candidate.id)
      if (action === 'resume')
        await api.resumeCopyGuardAICandidate(candidate.id)
      if (action === 'terminate')
        await api.terminateCopyGuardAICandidate(candidate.id)
      await mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  const requestReview = async (candidate: CopyGuardAICandidate) => {
    if (
      !window.confirm(
        `请求立即复查 ${candidate.symbol}？\n\n这不是模拟分析：系统仍会遵守最短间隔、额度、数据去重和全部确定性风控，但若 AI 返回高置信度 ENTER 且预检通过，可能直接使用真实资金重入。`
      )
    )
      return
    setBusy(candidate.id)
    setError('')
    try {
      const result = await api.requestCopyGuardAICandidateReview(candidate.id)
      window.alert(
        `复查请求已进入安全调度器，最早处理时间：${dateLabel(result.eligible_at)}。`
      )
      await mutate()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(null)
    }
  }

  return (
    <>
      <section className="rounded border border-[#2B3139] bg-[#181A20] p-4">
        <div className="mb-3 flex items-center justify-between">
          <div>
            <h3 className="font-medium text-[#EAECEF]">AI 持续重入观察</h3>
            <p className="text-xs text-[#848E9C]">
              AI
              只判断反转与入场价值；仓位、预算、价格漂移和保护能力由代码强制复核。
            </p>
          </div>
          <span className="text-xs text-[#F0B90B]">
            {candidates.length} 个候选
          </span>
        </div>
        {error && <div className="mb-2 text-xs text-[#F6465D]">{error}</div>}
        <div className="space-y-2">
          {candidates.map((candidate) => (
            <div
              key={candidate.id}
              className="rounded border border-[#2B3139] bg-[#0B0E11] p-3 text-xs"
            >
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="font-medium text-[#EAECEF]">
                  {data?.traderNames[candidate.trader_id] ??
                    candidate.trader_id}{' '}
                  · {candidate.symbol} {sideLabel(candidate.side)} · 第{' '}
                  {candidate.reentry_count + 1} 次候选
                </div>
                <span className="rounded bg-[#2B3139] px-2 py-1 text-[#F0B90B]">
                  {candidate.status}
                </span>
              </div>
              <div className="mt-2 grid gap-1 text-[#848E9C] md:grid-cols-3">
                <span>
                  上次决策：{candidate.last_decision || '尚未分析'}{' '}
                  {candidate.confidence > 0
                    ? `（对 ${candidate.last_decision} 结论的置信度 ${(candidate.confidence * 100).toFixed(0)}%）`
                    : ''}
                </span>
                <span>
                  复查：{candidate.review_count}/
                  {candidate.ai_lifecycle_call_limit || 30} · 24h{' '}
                  {candidate.ai_daily_calls_used || 0}/
                  {candidate.ai_daily_call_limit || 12} · 下次{' '}
                  {new Date(candidate.next_review_at).toLocaleString()}
                </span>
                <span>
                  预计上限：{candidate.max_notional.toFixed(2)} USDT · factor{' '}
                  {(candidate.size_factor || 0).toFixed(2)}
                </span>
                <span>
                  入场区间：{candidate.entry_price_low || '—'} –{' '}
                  {candidate.entry_price_high || '—'}
                </span>
                <span>
                  关注区间：{candidate.attention_price_low || '—'} –{' '}
                  {candidate.attention_price_high || '—'}
                </span>
                <span>
                  保护：{candidate.protectable ? '可预检' : '不可保护'} ·
                  止损参考 {candidate.last_stop_price || '—'}
                </span>
                <span>
                  真实门槛 ≥{' '}
                  {((candidate.ai_confidence_threshold || 0.8) * 100).toFixed(
                    0
                  )}
                  % · 最短间隔 {candidate.ai_min_review_seconds || 300} 秒
                </span>
                <span>
                  常规复审：
                  {candidate.regular_review_at
                    ? new Date(candidate.regular_review_at).toLocaleString()
                    : '—'}
                  {' · '}事件复审：
                  {candidate.event_review_at
                    ? new Date(candidate.event_review_at).toLocaleString()
                    : '—'}
                </span>
                <span>
                  ENTER 租约：
                  {candidate.decision_expires_at
                    ? new Date(candidate.decision_expires_at).toLocaleString()
                    : '—'}
                  {' · '}预算封锁：
                  {candidate.budget_blocked_until
                    ? new Date(candidate.budget_blocked_until).toLocaleString()
                    : '—'}
                </span>
              </div>
              {(candidate.pending_trigger || candidate.last_error) && (
                <div className="mt-2 text-[#F6465D]">
                  {candidate.pending_trigger || candidate.last_error}
                </div>
              )}
              <div className="mt-2 flex justify-end gap-2">
                {candidate.last_analysis_id > 0 && (
                  <button
                    disabled={busy === candidate.id}
                    onClick={() =>
                      setViewingAnalysisID(candidate.last_analysis_id)
                    }
                    className="rounded bg-[#2B3139] px-3 py-1 disabled:opacity-40"
                  >
                    查看最近分析
                  </button>
                )}
                {(candidate.status === 'WATCHING' ||
                  candidate.status === 'WAITING') && (
                  <button
                    disabled={busy === candidate.id}
                    onClick={() => void requestReview(candidate)}
                    className="rounded bg-[#F0B90B] px-3 py-1 text-black disabled:opacity-40"
                  >
                    请求立即复查（可能下单）
                  </button>
                )}
                {candidate.status === 'PAUSED' ? (
                  <button
                    disabled={busy === candidate.id}
                    onClick={() => void operate(candidate, 'resume')}
                    className="rounded bg-[#2B3139] px-3 py-1 disabled:opacity-40"
                  >
                    恢复
                  </button>
                ) : ['WATCHING', 'WAITING', 'REVIEWING'].includes(
                    candidate.status
                  ) ? (
                  <button
                    disabled={busy === candidate.id}
                    onClick={() => void operate(candidate, 'pause')}
                    className="rounded bg-[#2B3139] px-3 py-1 disabled:opacity-40"
                  >
                    暂停
                  </button>
                ) : null}
                {['WATCHING', 'WAITING', 'REVIEWING', 'PAUSED'].includes(
                  candidate.status
                ) && (
                  <button
                    disabled={busy === candidate.id}
                    onClick={() => void operate(candidate, 'terminate')}
                    className="rounded bg-[#F6465D22] px-3 py-1 text-[#F6465D] disabled:opacity-40"
                  >
                    终止
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      </section>
      {viewingAnalysisID != null && (
        <CandidateAnalysisModal
          analysisId={viewingAnalysisID}
          onClose={() => setViewingAnalysisID(null)}
        />
      )}
    </>
  )
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
  const executionSummary = useMemo(() => {
    const intents = detail?.execution_intents ?? []
    const ordinary = [...intents]
      .reverse()
      .find(
        (intent) =>
          intent.source_kind !== 'AI_REENTRY' &&
          (intent.action === 'open_long' || intent.action === 'open_short')
      )
    const ai = [...intents]
      .reverse()
      .find((intent) => intent.source_kind === 'AI_REENTRY')
    const targetQuantity =
      ordinary?.target_quantity ||
      ordinary?.quantized_quantity ||
      ordinary?.requested_quantity ||
      0
    const filledQuantity = ordinary?.filled_quantity ?? 0
    const actualAccountPct =
      ordinary && ordinary.follower_equity_at_target > 0
        ? (ordinary.filled_notional / ordinary.follower_equity_at_target) * 100
        : 0
    return {
      ordinary,
      ai,
      targetQuantity,
      filledQuantity,
      remainingQuantity: Math.max(0, targetQuantity - filledQuantity),
      actualAccountPct,
    }
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
      <AICandidatesPanel />
      <AdvisorSettingsCard />
      <MarketPreviewCard />
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
      <div className="grid md:grid-cols-5 gap-4">
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
        <Card
          title="仅止损路径"
          value={money(summary?.stop_only_pnl ?? 0)}
          color={(summary?.stop_only_pnl ?? 0) >= 0 ? '#0ECB81' : '#F6465D'}
          note="初始仓位止损后的实际结果，不包含 AI 重入"
        />
        <Card
          title="AI 重入贡献"
          value={money(summary?.reentry_contribution ?? 0)}
          color={
            (summary?.reentry_contribution ?? 0) >= 0 ? '#0ECB81' : '#F6465D'
          }
          note="所有已对账二次入场 attempt 的独立盈亏贡献"
        />
      </div>
      {summary?.mean_net_guard_effect_estimate?.status === 'AVAILABLE' && (
        <div className="border border-[#2B3139] bg-[#181A20] rounded-lg p-4 text-sm text-[#B7BDC6]">
          已验证净保护效果均值：
          {money(summary.mean_net_guard_effect_estimate.mean)}；Bootstrap 95%
          区间 [{money(summary.mean_net_guard_effect_estimate.ci95_low)},{' '}
          {money(summary.mean_net_guard_effect_estimate.ci95_high)}]（
          {summary.mean_net_guard_effect_estimate.sample_count} 个已验证周期）
        </div>
      )}
      {(summary?.unscorable_baseline_cycles ?? 0) > 0 && (
        <div className="border border-[#F0B90B] bg-[#F0B90B]/10 rounded-lg p-4 text-sm text-[#F0B90B]">
          有 {summary?.unscorable_baseline_cycles}{' '}
          个已止损周期缺少有效领航员离场价，已从“帮你少亏 / 提前离场代价 /
          综合净改善”中排除，避免零价格制造虚假收益。
        </div>
      )}
      {(summary?.unprotectable_count ?? 0) > 0 && (
        <div className="border border-[#F6465D] bg-[#F6465D]/10 rounded-lg p-4 text-sm text-[#F6465D]">
          高危：当前有 {summary?.unprotectable_count}{' '}
          个周期确认无法建立有效止损保护，系统正在执行强制退出或等待交易所终态确认；不会继续采用裸仓跟随。
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
          分钟系统会发送升级告警，届时建议人工检查执行交易所。
        </div>
      )}
      {(summary?.accounting_pending_count ?? 0) +
        (summary?.accounting_delayed_count ?? 0) >
        0 && (
        <div className="border border-[#F0B90B] bg-[#F0B90B]/10 rounded-lg p-4 text-sm text-[#F0B90B]">
          当前有 {summary?.accounting_pending_count ?? 0} 个周期正在自动对账，
          {summary?.accounting_delayed_count ?? 0}{' '}
          个周期因执行交易所结算数据延迟系统继续自动重试。这些周期暂不计入保护效果，无需人工处理。
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
          label="真实路径最大回撤"
          value={money(-(summary?.max_realized_drawdown_usd ?? 0))}
        />
        <Metric
          label="最差单周期"
          value={money(-(summary?.worst_cycle_loss_usd ?? 0))}
        />
        <Metric
          label="尾部损失 CVaR95"
          value={money(-(summary?.tail_loss_cvar_95_usd ?? 0))}
        />
        <Metric
          label="重入成功率"
          value={
            (summary?.reentry_success_estimate?.denominator ?? 0) > 0
              ? `${((summary?.reentry_success_estimate?.rate ?? 0) * 100).toFixed(1)}%（${summary?.reentry_success_estimate?.numerator}/${summary?.reentry_success_estimate?.denominator}；95% CI ${((summary?.reentry_success_estimate?.ci95_low ?? 0) * 100).toFixed(1)}%–${((summary?.reentry_success_estimate?.ci95_high ?? 0) * 100).toFixed(1)}%）`
              : '暂无样本'
          }
        />
        <Metric
          label="误杀率"
          value={
            (summary?.false_kill_estimate?.denominator ?? 0) > 0
              ? `${((summary?.false_kill_estimate?.rate ?? 0) * 100).toFixed(1)}%（${summary?.false_kill_estimate?.numerator}/${summary?.false_kill_estimate?.denominator}；95% CI ${((summary?.false_kill_estimate?.ci95_low ?? 0) * 100).toFixed(1)}%–${((summary?.false_kill_estimate?.ci95_high ?? 0) * 100).toFixed(1)}%）`
              : '暂无样本'
          }
        />
        <Metric
          label="已验证基线"
          value={summary?.verified_baseline_cycles ?? 0}
        />
        <Metric
          label="估算基线"
          value={summary?.estimated_baseline_cycles ?? 0}
        />
        <Metric
          label="不可评分"
          value={summary?.unscorable_baseline_cycles ?? 0}
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
                    c.closed_at || isWatchingFlat(c)
                      ? 'text-[#848E9C]'
                      : c.protection_status === 'VERIFIED'
                        ? 'text-[#0ECB81]'
                        : c.protection_status === 'UNKNOWN' ||
                            c.protection_status === 'DEGRADED' ||
                            c.protection_status === 'UNPROTECTABLE' ||
                            c.protection_status === 'UNPROTECTED_WARNING' ||
                            c.protection_status === 'FORCED_EXIT_PENDING'
                          ? 'text-[#F6465D] font-bold'
                          : 'text-[#F0B90B]'
                  }`}
                >
                  {c.closed_at &&
                  (c.protection_status === 'UNKNOWN' ||
                    c.protection_status === 'DEGRADED' ||
                    c.protection_status === 'PENDING' ||
                    c.protection_status === 'UNPROTECTABLE' ||
                    c.protection_status === 'UNPROTECTED_WARNING' ||
                    c.protection_status === 'FORCED_EXIT_PENDING')
                    ? '已结束'
                    : isWatchingFlat(c)
                      ? '观察中·无持仓'
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
              label="最近普通开/加仓目标账户占比"
              value={
                executionSummary.ordinary
                  ? `${executionSummary.ordinary.target_account_pct.toFixed(2)}%`
                  : '-'
              }
            />
            <Metric
              label="最近普通开/加仓实际账户占比"
              value={
                executionSummary.ordinary
                  ? `${executionSummary.actualAccountPct.toFixed(2)}%`
                  : '-'
              }
            />
            <Metric
              label="目标量 / 真实成交 / 待补差额"
              value={
                executionSummary.ordinary
                  ? `${executionSummary.targetQuantity.toPrecision(8)} / ${executionSummary.filledQuantity.toPrecision(8)} / ${executionSummary.remainingQuantity.toPrecision(8)}`
                  : '-'
              }
            />
            <Metric
              label="普通跟单执行状态"
              value={
                executionSummary.ordinary
                  ? `${executionSummary.ordinary.status}${executionSummary.ordinary.reason_code ? ` · ${executionSummary.ordinary.reason_code}` : ''}`
                  : '-'
              }
            />
            <Metric
              label="补齐截止时间"
              value={dateLabel(executionSummary.ordinary?.catchup_deadline_at)}
            />
            <Metric
              label="AI 独立执行状态"
              value={
                executionSummary.ai
                  ? `${executionSummary.ai.status}${executionSummary.ai.reason_code ? ` · ${executionSummary.ai.reason_code}` : ''}`
                  : '未进入 AI 执行链'
              }
            />
            <Metric
              label="保护状态"
              value={
                isWatchingFlat(detail.cycle)
                  ? '观察中·无持仓'
                  : `${localized(protectionLabels, detail.cycle.protection_status)} / ${(detail.cycle.protection_coverage * 100).toFixed(0)}%`
              }
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
          {detail.ai_effect_summary?.total_decisions > 0 && (
            <div className="mb-4 rounded border border-[#2B3139] bg-[#181A20] p-4 text-sm">
              <div className="mb-2 font-medium">
                AI 重入决策效果（评价 v
                {detail.ai_effect_summary.evaluation_version}）
              </div>
              <div className="grid gap-3 md:grid-cols-3">
                <Metric
                  label="决策 / 可评分 / 不可评分"
                  value={`${detail.ai_effect_summary.total_decisions} / ${detail.ai_effect_summary.scorable_decisions} / ${detail.ai_effect_summary.unscorable_decisions}`}
                />
                <Metric
                  label="ENTER → 请求 → 提交 → 成交 → 保护"
                  value={`${detail.ai_effect_summary.enter_decisions} → ${detail.ai_effect_summary.execution_requested} → ${detail.ai_effect_summary.execution_submitted} → ${detail.ai_effect_summary.execution_filled} → ${detail.ai_effect_summary.execution_protected}`}
                />
                <Metric
                  label="错过反转 / 正确放弃"
                  value={`${detail.ai_effect_summary.missed_reversals} / ${detail.ai_effect_summary.correct_abandons}`}
                />
                <Metric
                  label="风控避免继续亏损"
                  value={detail.ai_effect_summary.risk_gate_saved_losses}
                />
                <Metric
                  label="最终 AI 决策"
                  value={detail.ai_effect_summary.final_decision || '-'}
                />
                <Metric
                  label="最终决策效果"
                  value={detail.ai_effect_summary.final_decision_outcome || '-'}
                />
                <Metric
                  label="真实重入净盈亏"
                  value={money(detail.ai_effect_summary.actual_reentry_pnl)}
                />
              </div>
              <div className="mt-3 text-xs text-[#848E9C]">
                评价只基于已保存的观察行情、执行事件和交易所对账；INSUFFICIENT_DATA
                会明确记为不可评分，不用估算盈亏冒充真实结果。
              </div>
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
                  {(e.type === 'PROTECTIVE_STOP_ACTIVE' ||
                    e.type === 'PROTECTION_ACTIVE') &&
                  e.metadata &&
                  (e.metadata.governed_by === 'margin_cap' ||
                    e.metadata.governed_by === 'account_cap' ||
                    e.metadata.governed_by === 'clamp') &&
                  typeof e.metadata.distance_atr_ratio === 'number' &&
                  e.metadata.distance_atr_ratio < 0.5 ? (
                    <span
                      className="ml-2 text-xs text-yellow-500"
                      title={`止损距离仅 ${(e.metadata.distance_atr_ratio as number).toFixed(2)}×ATR（控线=${String(e.metadata.governed_by)}），易被行情噪音扫到`}
                    >
                      ⚠️ 止损偏紧·易扫损
                    </span>
                  ) : null}
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
