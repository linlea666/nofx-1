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
import type { FollowCoinCredentialsView } from '../../types'

/**
 * FollowCoin 全局 token 管理弹窗
 *
 * 所有 FollowCoin 跟单交易员共用一个登录 token；保存后无需重启即生效。
 * token 失效或即将到期时后端会发邮件提醒，回这里重新粘贴即可。
 */

interface Props {
  isOpen: boolean
  onClose: () => void
}

const STATUS_META: Record<
  FollowCoinCredentialsView['last_status'],
  { label: string; color: string; icon: typeof CheckCircle2 }
> = {
  valid: { label: '有效', color: '#0ECB81', icon: CheckCircle2 },
  expired: { label: '已失效', color: '#F6465D', icon: AlertCircle },
  unknown: { label: '未校验', color: '#848E9C', icon: HelpCircle },
  error: { label: '校验异常', color: '#F0B90B', icon: AlertCircle },
}

function formatTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime()) || d.getTime() <= 0) return '—'
  return d.toLocaleString('zh-CN', { hour12: false })
}

function daysUntil(s: string): number | null {
  if (!s) return null
  const t = new Date(s).getTime()
  if (isNaN(t) || t <= 0) return null
  return Math.floor((t - Date.now()) / 86400000)
}

export function FollowCoinCredsModal({ isOpen, onClose }: Props) {
  const [creds, setCreds] = useState<FollowCoinCredentialsView | null>(null)
  const [loading, setLoading] = useState(false)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)
  const [tokenInput, setTokenInput] = useState('')
  const [showTutorial, setShowTutorial] = useState(false)

  const fetchCreds = async () => {
    setLoading(true)
    try {
      setCreds(await api.getFollowCoinCredentials())
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '加载失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    if (isOpen) {
      fetchCreds()
      setTokenInput('')
    }
  }, [isOpen])

  const handleSave = async () => {
    const token = tokenInput.trim()
    if (!token) {
      toast.error('请粘贴 token')
      return
    }
    setSaving(true)
    try {
      const result = await api.setFollowCoinCredentials(token)
      setCreds(result.credentials)
      if (result.credentials?.last_status === 'valid') {
        toast.success(
          `已保存并校验通过${result.nick_name ? `（登录账号 ${result.nick_name}）` : ''}`
        )
      } else {
        toast.warning(
          `已保存，但暂时无法校验：${result.credentials?.last_error || '未知原因'}`
        )
      }
      setTokenInput('')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const handleTest = async () => {
    setTesting(true)
    try {
      const result = await api.testFollowCoinCredentials()
      await fetchCreds()
      if (result.status === 'valid') {
        toast.success(
          `token 有效${result.nick_name ? `（登录账号 ${result.nick_name}）` : ''}`
        )
      } else if (result.status === 'expired') {
        toast.error('token 已失效，请重新获取并粘贴')
      } else {
        toast.warning(`校验异常：${result.error || '未知原因'}`)
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '测试失败')
    } finally {
      setTesting(false)
    }
  }

  const handleDelete = async () => {
    if (
      !confirm('确认删除 FollowCoin token？删除后所有 FollowCoin 跟单将暂停。')
    )
      return
    setSaving(true)
    try {
      await api.deleteFollowCoinCredentials()
      setCreds(null)
      toast.success('已删除')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '删除失败')
    } finally {
      setSaving(false)
    }
  }

  if (!isOpen) return null

  const statusMeta = creds ? STATUS_META[creds.last_status] : null
  const StatusIcon = statusMeta?.icon
  const remainingDays = creds ? daysUntil(creds.expires_at) : null

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70"
      onClick={onClose}
    >
      <div
        className="binance-card w-full max-w-2xl max-h-[90vh] overflow-y-auto p-6"
        onClick={(e) => e.stopPropagation()}
      >
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
                FollowCoin 凭证
              </h2>
              <p className="text-xs text-[#848E9C] mt-0.5">
                所有 FollowCoin 跟单交易员共用此 token；更新后立即生效
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
            <div className="bg-[#0B0E11] border border-[#2B3139] rounded-lg p-4">
              <div className="flex items-center justify-between mb-3">
                <span className="text-sm text-[#848E9C]">当前状态</span>
                {statusMeta && StatusIcon ? (
                  <span
                    className="flex items-center gap-1.5 text-sm font-medium"
                    style={{ color: statusMeta.color }}
                  >
                    <StatusIcon className="w-4 h-4" />
                    {statusMeta.label}
                  </span>
                ) : (
                  <span className="text-sm text-[#848E9C]">未配置</span>
                )}
              </div>

              {creds ? (
                <div className="grid grid-cols-2 gap-3 text-xs">
                  <div>
                    <div className="text-[#848E9C] mb-0.5">token（脱敏）</div>
                    <div className="font-mono text-[#EAECEF]">
                      {creds.masked_token || '—'}
                    </div>
                  </div>
                  <div>
                    <div className="text-[#848E9C] mb-0.5">到期时间</div>
                    <div
                      className="font-mono"
                      style={{
                        color:
                          remainingDays !== null && remainingDays <= 3
                            ? '#F6465D'
                            : '#EAECEF',
                      }}
                    >
                      {formatTime(creds.expires_at)}
                      {remainingDays !== null &&
                        ` (剩 ${Math.max(remainingDays, 0)} 天)`}
                    </div>
                  </div>
                  <div>
                    <div className="text-[#848E9C] mb-0.5">最后校验成功</div>
                    <div className="font-mono text-[#EAECEF]">
                      {formatTime(creds.last_validated_at)}
                    </div>
                  </div>
                  {creds.last_error && (
                    <div className="col-span-2 mt-1 p-2 rounded bg-[#F6465D11] border border-[#F6465D33] text-[#F6465D] text-[11px] break-all">
                      {creds.last_error}
                    </div>
                  )}
                </div>
              ) : (
                <p className="text-xs text-[#848E9C] leading-relaxed">
                  尚未配置 token。在下方粘贴后保存。
                </p>
              )}

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
                    测试 token
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

            <div className="bg-[#0B0E11] border border-[#F0B90B33] rounded-lg p-4 space-y-3">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium text-[#F0B90B]">
                  {creds ? '更新 token' : '配置 token'}
                </span>
                <button
                  type="button"
                  onClick={() => setShowTutorial((v) => !v)}
                  className="text-xs text-[#848E9C] hover:text-[#F0B90B] underline"
                >
                  {showTutorial ? '隐藏说明' : '如何获取？'}
                </button>
              </div>

              {showTutorial && (
                <div className="text-xs text-[#848E9C] bg-[#1E2329] border border-[#2B3139] rounded p-3 leading-relaxed space-y-1">
                  <ol className="list-decimal list-inside space-y-1">
                    <li>
                      手机登录 FollowCoin App，用抓包工具打开任意交易员的实盘页
                    </li>
                    <li>找到发往 api-v2.cryptostrategyget.com 的请求</li>
                    <li>
                      复制请求头里 token 的值（eyJ 开头的一长串）粘贴到下面
                    </li>
                  </ol>
                  <p className="text-[#F0B90B] mt-2">
                    ⚠ token 会过期；失效或到期前 3
                    天会发邮件提醒，回这里重新粘贴即可。
                  </p>
                </div>
              )}

              <div>
                <label className="text-xs text-[#848E9C] block mb-1">
                  token（也可粘贴含 token 请求头的完整 cURL）
                  {tokenInput && (
                    <span className="ml-2 text-[#0ECB81]">
                      ✓ {tokenInput.trim().length} 字符
                    </span>
                  )}
                </label>
                <textarea
                  value={tokenInput}
                  onChange={(e) => setTokenInput(e.target.value)}
                  rows={3}
                  placeholder="eyJraWQiOi..."
                  className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-[11px] leading-tight resize-y break-all"
                />
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
                  disabled={saving || !tokenInput.trim()}
                  className="px-4 py-2 bg-[#F0B90B] text-black text-sm font-semibold rounded hover:bg-[#E1A706] disabled:opacity-50 disabled:cursor-not-allowed flex items-center gap-2"
                >
                  {saving && <Loader2 className="w-4 h-4 animate-spin" />}
                  校验并保存
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
