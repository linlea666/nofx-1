import { useEffect, useState } from 'react'
import {
  X,
  Loader2,
  RefreshCw,
  Trash2,
  CheckCircle2,
  AlertCircle,
  HelpCircle,
} from 'lucide-react'
import { toast } from 'sonner'
import { api } from '../../lib/api'
import type { BinanceCredentialsView } from '../../types'
import { parseBinanceCurl } from '../../utils/parseBinanceCurl'

/**
 * Binance 全局共享凭证管理弹窗
 *
 * 用途：
 *   - 所有 Binance 跟单交易员共享一份 p20t / csrftoken
 *   - 一处更新即对所有跟单 trader 立即生效（后端 BinanceCredentialsLoader 热加载）
 *   - 凭证过期时邮件告警全局唯一，不再按 trader 重复发
 *
 * 入口：交易员管理页 → "Binance 凭证" 按钮
 */

interface Props {
  isOpen: boolean
  onClose: () => void
}

const STATUS_META: Record<
  BinanceCredentialsView['last_status'],
  { label: string; color: string; icon: typeof CheckCircle2 }
> = {
  valid: { label: '有效', color: '#0ECB81', icon: CheckCircle2 },
  expired: { label: '已过期', color: '#F6465D', icon: AlertCircle },
  unknown: { label: '未校验', color: '#848E9C', icon: HelpCircle },
  error: { label: '校验异常', color: '#F0B90B', icon: AlertCircle },
}

function formatTime(s: string): string {
  if (!s) return '—'
  try {
    const d = new Date(s)
    if (isNaN(d.getTime()) || d.getTime() === 0) return '—'
    return d.toLocaleString('zh-CN', { hour12: false })
  } catch {
    return '—'
  }
}

export function BinanceGlobalCredsModal({ isOpen, onClose }: Props) {
  const [creds, setCreds] = useState<BinanceCredentialsView | null>(null)
  const [affectedTraders, setAffectedTraders] = useState<string[]>([])
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)

  const [curlInput, setCurlInput] = useState('')
  const [parseHint, setParseHint] = useState('')
  const [showTutorial, setShowTutorial] = useState(false)

  // 直接输入字段（与 cURL 解析二选一）
  const [p20tInput, setP20tInput] = useState('')
  const [csrfInput, setCsrfInput] = useState('')

  const fetchAll = async () => {
    setLoading(true)
    try {
      const [list, traders] = await Promise.all([
        api.listBinanceCredentials(),
        api.getBinanceCredentialsAffectedTraders(),
      ])
      const def = list.find((c) => c.label === 'default') ?? null
      setCreds(def)
      setAffectedTraders(traders)
    } catch (err) {
      const msg = err instanceof Error ? err.message : '加载失败'
      toast.error(msg)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (isOpen) {
      fetchAll()
      setCurlInput('')
      setP20tInput('')
      setCsrfInput('')
      setParseHint('')
    }
  }, [isOpen])

  const handleParseCurl = () => {
    if (!curlInput.trim()) {
      setParseHint('请先粘贴 cURL 内容')
      return
    }
    const { p20t, csrfToken } = parseBinanceCurl(curlInput)
    if (!p20t && !csrfToken) {
      setParseHint('未识别出 p20t / csrftoken，请确认是币安跟单页面的请求')
      return
    }
    if (p20t) setP20tInput(p20t)
    if (csrfToken) setCsrfInput(csrfToken)
    const parts: string[] = []
    if (p20t) parts.push(`p20t (${p20t.length} 字符)`)
    if (csrfToken) parts.push(`csrftoken (${csrfToken.length} 字符)`)
    setParseHint(`已识别：${parts.join(' + ')}`)
  }

  const handleSave = async () => {
    const p = p20tInput.trim()
    const c = csrfInput.trim()
    if (!p || !c) {
      toast.error('p20t 与 csrftoken 不能为空')
      return
    }
    setSaving(true)
    try {
      const updated = await api.setBinanceCredentials({
        p20t: p,
        csrftoken: c,
        label: 'default',
      })
      setCreds(updated)
      // 后端 Set 已自动探活 + 写状态，给用户即时反馈
      if (updated?.last_status === 'valid') {
        toast.success('已保存并校验通过')
      } else if (updated?.last_status === 'expired') {
        toast.error('已保存，但凭证已过期，请检查')
      } else {
        toast.warning(`已保存，但状态为 ${updated?.last_status ?? 'unknown'}`)
      }
      // 清空输入
      setCurlInput('')
      setP20tInput('')
      setCsrfInput('')
      setParseHint('')
    } catch (err) {
      const msg = err instanceof Error ? err.message : '保存失败'
      toast.error(msg)
    } finally {
      setSaving(false)
    }
  }

  const handleTest = async () => {
    setTesting(true)
    try {
      const result = await api.testBinanceCredentials('default')
      // 重新拉一次凭证拿最新 last_status / userID
      await fetchAll()
      if (result.status === 'valid') {
        toast.success(`凭证有效（绑定账号 ${result.binance_user_id || '—'}）`)
      } else if (result.status === 'expired') {
        toast.error('凭证已过期，请重新粘贴 cURL')
      } else {
        toast.warning(
          `状态：${result.status}${result.error ? ' | ' + result.error : ''}`
        )
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : '测试失败'
      toast.error(msg)
    } finally {
      setTesting(false)
    }
  }

  const handleDelete = async () => {
    if (!confirm('确认删除全局凭证？删除后所有 Binance 跟单交易员将停止工作。'))
      return
    setSaving(true)
    try {
      await api.deleteBinanceCredentials('default')
      setCreds(null)
      toast.success('已删除')
    } catch (err) {
      const msg = err instanceof Error ? err.message : '删除失败'
      toast.error(msg)
    } finally {
      setSaving(false)
    }
  }

  if (!isOpen) return null

  const statusMeta = creds ? STATUS_META[creds.last_status] : null
  const StatusIcon = statusMeta?.icon

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70"
      onClick={onClose}
    >
      <div
        className="binance-card w-full max-w-2xl max-h-[90vh] overflow-y-auto p-6"
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between mb-4 pb-3 border-b border-[#2B3139]">
          <div className="flex items-center gap-3">
            <div
              className="w-10 h-10 rounded-lg flex items-center justify-center text-2xl"
              style={{ background: 'rgba(240, 185, 11, 0.15)' }}
            >
              🔑
            </div>
            <div>
              <h2 className="text-xl font-bold text-[#EAECEF]">
                Binance 全局共享凭证
              </h2>
              <p className="text-xs text-[#848E9C] mt-0.5">
                所有 Binance 跟单交易员共享此凭证；一处更新全局生效
              </p>
            </div>
          </div>
          <button
            onClick={onClose}
            className="p-2 hover:bg-[#2B3139] rounded transition-colors"
          >
            <X className="w-5 h-5 text-[#848E9C]" />
          </button>
        </div>

        {loading ? (
          <div className="flex items-center justify-center py-12">
            <Loader2 className="w-8 h-8 animate-spin text-[#F0B90B]" />
          </div>
        ) : (
          <div className="space-y-4">
            {/* 当前状态卡片 */}
            <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-4">
              <div className="flex items-center justify-between mb-3">
                <span className="text-sm text-[#848E9C]">当前状态</span>
                {statusMeta && StatusIcon && (
                  <span
                    className="flex items-center gap-1.5 text-sm font-medium"
                    style={{ color: statusMeta.color }}
                  >
                    <StatusIcon className="w-4 h-4" />
                    {statusMeta.label}
                  </span>
                )}
                {!creds && (
                  <span className="text-sm text-[#848E9C]">未配置</span>
                )}
              </div>

              {creds ? (
                <div className="grid grid-cols-2 gap-3 text-xs">
                  <div>
                    <div className="text-[#848E9C] mb-0.5">绑定账号 ID</div>
                    <div className="font-mono text-[#EAECEF]">
                      {creds.binance_user_id || '—'}
                    </div>
                  </div>
                  <div>
                    <div className="text-[#848E9C] mb-0.5">最后校验</div>
                    <div className="font-mono text-[#EAECEF]">
                      {formatTime(creds.last_validated_at)}
                    </div>
                  </div>
                  <div>
                    <div className="text-[#848E9C] mb-0.5">p20t（脱敏）</div>
                    <div className="font-mono text-[#EAECEF]">
                      {creds.masked_p20t || '—'}
                    </div>
                  </div>
                  <div>
                    <div className="text-[#848E9C] mb-0.5">
                      csrftoken（脱敏）
                    </div>
                    <div className="font-mono text-[#EAECEF]">
                      {creds.masked_csrf_token || '—'}
                    </div>
                  </div>
                  {creds.last_error && (
                    <div className="col-span-2 mt-1 p-2 rounded bg-[#F6465D11] border border-[#F6465D33] text-[#F6465D] text-[11px]">
                      {creds.last_error}
                    </div>
                  )}
                </div>
              ) : (
                <p className="text-xs text-[#848E9C] leading-relaxed">
                  尚未配置全局凭证。在下方粘贴 cURL 或填写 p20t / csrftoken
                  后保存。
                </p>
              )}

              {/* 操作按钮 */}
              {creds && (
                <div className="flex gap-2 mt-4">
                  <button
                    type="button"
                    onClick={handleTest}
                    disabled={testing}
                    className="px-3 py-1.5 text-xs bg-[#2B3139] text-[#EAECEF] rounded hover:bg-[#3C4043] disabled:opacity-50 flex items-center gap-1.5"
                  >
                    {testing ? (
                      <Loader2 className="w-3 h-3 animate-spin" />
                    ) : (
                      <RefreshCw className="w-3 h-3" />
                    )}
                    测试凭证
                  </button>
                  <button
                    type="button"
                    onClick={handleDelete}
                    disabled={saving}
                    className="px-3 py-1.5 text-xs bg-[#F6465D22] text-[#F6465D] rounded hover:bg-[#F6465D44] disabled:opacity-50 flex items-center gap-1.5"
                  >
                    <Trash2 className="w-3 h-3" />
                    删除
                  </button>
                </div>
              )}
            </div>

            {/* 受影响 trader */}
            <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-4">
              <div className="flex items-center justify-between mb-2">
                <span className="text-sm text-[#848E9C]">
                  受此凭证影响的交易员
                </span>
                <span className="text-xs text-[#F0B90B]">
                  {affectedTraders.length} 个
                </span>
              </div>
              {affectedTraders.length === 0 ? (
                <p className="text-xs text-[#848E9C]">
                  暂无 Binance 跟单交易员
                </p>
              ) : (
                <div className="flex flex-wrap gap-1.5">
                  {affectedTraders.map((id) => (
                    <span
                      key={id}
                      className="px-2 py-1 bg-[#1E2329] text-[#EAECEF] text-[11px] rounded font-mono"
                    >
                      {id}
                    </span>
                  ))}
                </div>
              )}
            </div>

            {/* 更新凭证 */}
            <div className="bg-[#0B0E11] border border-[#F0B90B33] rounded-lg p-4 space-y-3">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium text-[#F0B90B]">
                  {creds ? '更新凭证' : '配置凭证'}
                </span>
                <button
                  type="button"
                  onClick={() => setShowTutorial((v) => !v)}
                  className="text-xs text-[#848E9C] hover:text-[#F0B90B] underline"
                >
                  {showTutorial ? '隐藏教程' : '如何获取？'}
                </button>
              </div>

              {showTutorial && (
                <div className="text-xs text-[#848E9C] bg-[#1E2329] border border-[#2B3139] rounded p-3 leading-relaxed space-y-1">
                  <p className="text-[#EAECEF]">获取步骤：</p>
                  <ol className="list-decimal list-inside space-y-1">
                    <li>登录 binance.com 进入"跟单交易 → 我的跟单"</li>
                    <li>F12 打开开发者工具 → 切到 Network 网络面板</li>
                    <li>
                      点击任意领航员，找到 user-position 或 trade-history 请求
                    </li>
                    <li>右键该请求 → Copy → Copy as cURL（bash）</li>
                    <li>粘贴到下面文本框点"自动解析"</li>
                  </ol>
                  <p className="text-[#F0B90B] mt-2">
                    ⚠ 凭证 7-30
                    天后会过期；过期时会发邮件提醒，回这里重新粘贴一次即可。
                  </p>
                </div>
              )}

              <div>
                <label className="text-xs text-[#848E9C] block mb-1">
                  粘贴 cURL（推荐）
                </label>
                <textarea
                  value={curlInput}
                  onChange={(e) => setCurlInput(e.target.value)}
                  rows={3}
                  placeholder={`curl 'https://www.binance.com/bapi/futures/v6/private/future/user-data/user-position' \\\n  -H 'csrftoken: xxxxx' \\\n  -b 'p20t=xxxxx; ...'`}
                  className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-[11px] leading-tight resize-y"
                />
                <div className="flex items-center gap-3 mt-2">
                  <button
                    type="button"
                    onClick={handleParseCurl}
                    className="px-3 py-1.5 bg-[#F0B90B22] text-[#F0B90B] text-xs font-medium rounded hover:bg-[#F0B90B44]"
                  >
                    自动解析
                  </button>
                  {parseHint && (
                    <span
                      className={`text-xs ${parseHint.startsWith('已识别') ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}
                    >
                      {parseHint}
                    </span>
                  )}
                </div>
              </div>

              <div className="grid grid-cols-1 gap-3 pt-2 border-t border-[#2B3139]">
                <div>
                  <label className="text-xs text-[#848E9C] block mb-1">
                    p20t (登录 cookie)
                    {p20tInput && (
                      <span className="ml-2 text-[#0ECB81]">
                        ✓ {p20tInput.length} 字符
                      </span>
                    )}
                  </label>
                  <input
                    type="text"
                    value={p20tInput}
                    onChange={(e) => setP20tInput(e.target.value)}
                    placeholder="登录态 cookie p20t 的值"
                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-xs"
                  />
                </div>
                <div>
                  <label className="text-xs text-[#848E9C] block mb-1">
                    csrftoken (CSRF header)
                    {csrfInput && (
                      <span className="ml-2 text-[#0ECB81]">
                        ✓ {csrfInput.length} 字符
                      </span>
                    )}
                  </label>
                  <input
                    type="text"
                    value={csrfInput}
                    onChange={(e) => setCsrfInput(e.target.value)}
                    placeholder="csrftoken header 的值"
                    className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-xs"
                  />
                </div>
              </div>

              <div className="flex justify-end gap-2 pt-2">
                <button
                  type="button"
                  onClick={onClose}
                  className="px-4 py-2 bg-[#2B3139] text-[#EAECEF] text-sm rounded hover:bg-[#3C4043]"
                >
                  关闭
                </button>
                <button
                  type="button"
                  onClick={handleSave}
                  disabled={saving || !p20tInput.trim() || !csrfInput.trim()}
                  className="px-4 py-2 bg-[#F0B90B] text-black text-sm font-semibold rounded hover:bg-[#E1A706] disabled:opacity-50 disabled:cursor-not-allowed flex items-center gap-2"
                >
                  {saving && <Loader2 className="w-4 h-4 animate-spin" />}
                  保存并立即生效
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
