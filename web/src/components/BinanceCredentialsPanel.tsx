import { useState } from 'react'

interface BinanceCredentialsPanelProps {
  p20t: string
  csrfToken: string
  onChange: (p20t: string, csrfToken: string) => void
}

/**
 * Binance 跟单凭证输入面板
 *
 * 用途：
 *   - 由于 Binance 不开放跟单领航员持仓的官方 API，需要使用浏览器 Web 私有接口
 *   - 该接口依赖 `p20t`（登录 cookie）和 `csrftoken`（CSRF header）
 *   - 凭证会过期（通常 7-30 天），过期时会通过邮件告警，需要重新填入
 *
 * 提供两种输入方式：
 *   1. 直接粘贴浏览器中拷贝的 cURL（Copy as cURL），自动解析两个字段
 *   2. 手动输入 p20t 与 csrftoken
 */
export function BinanceCredentialsPanel({ p20t, csrfToken, onChange }: BinanceCredentialsPanelProps) {
  const [curlInput, setCurlInput] = useState('')
  const [parseHint, setParseHint] = useState<string>('')
  const [showTutorial, setShowTutorial] = useState(false)

  /**
   * 解析 cURL 命令中的 p20t 和 csrftoken
   *
   * 兼容：
   *   - `-H 'csrftoken: xxx'` 和 `-H "csrftoken: xxx"`
   *   - `-b 'p20t=xxx; ...'` 和 `--cookie 'p20t=xxx; ...'`
   *   - `-H 'cookie: p20t=xxx; ...'`（部分浏览器把 cookie 放在 header 中）
   */
  const parseCurl = () => {
    if (!curlInput.trim()) {
      setParseHint('请先粘贴 cURL 内容')
      return
    }

    let p20tValue = ''
    let csrfValue = ''

    // 1. 解析 csrftoken（不区分大小写）
    const csrfMatch = curlInput.match(/-H\s+["']?csrftoken:\s*([^"'\s]+)["']?/i)
    if (csrfMatch) {
      csrfValue = csrfMatch[1].trim()
    }

    // 2. 解析 p20t（兼容 -b / --cookie / -H 'cookie: ...'）
    const cookieMatches = [
      curlInput.match(/-b\s+["']([^"']+)["']/),
      curlInput.match(/--cookie\s+["']([^"']+)["']/),
      curlInput.match(/-H\s+["']cookie:\s*([^"']+)["']/i),
    ]
    for (const m of cookieMatches) {
      if (m && m[1]) {
        const p20tMatch = m[1].match(/p20t=([^;]+)/)
        if (p20tMatch) {
          p20tValue = p20tMatch[1].trim()
          break
        }
      }
    }

    // 兜底：在原始 cURL 中直接找 p20t=...
    if (!p20tValue) {
      const fallback = curlInput.match(/p20t=([^;"'\s]+)/)
      if (fallback) {
        p20tValue = fallback[1].trim()
      }
    }

    if (!p20tValue && !csrfValue) {
      setParseHint('未能从 cURL 中识别出 p20t 或 csrftoken，请确认粘贴的是币安跟单页面的请求')
      return
    }

    const hints: string[] = []
    if (p20tValue) hints.push(`p20t (${p20tValue.length} 字符)`)
    if (csrfValue) hints.push(`csrftoken (${csrfValue.length} 字符)`)
    setParseHint(`已识别：${hints.join(' + ')}`)

    onChange(
      p20tValue || p20t,
      csrfValue || csrfToken,
    )
  }

  return (
    <div className="mt-4 p-4 bg-[#0B0E11] border border-[#F0B90B33] rounded-lg space-y-3">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <span className="text-[#F0B90B] text-sm font-medium">Binance 跟单凭证</span>
          <span className="px-2 py-0.5 text-[10px] bg-[#F0B90B22] text-[#F0B90B] rounded">必填</span>
        </div>
        <button
          type="button"
          onClick={() => setShowTutorial(v => !v)}
          className="text-xs text-[#848E9C] hover:text-[#F0B90B] underline"
        >
          {showTutorial ? '隐藏教程' : '如何获取？'}
        </button>
      </div>

      {showTutorial && (
        <div className="text-xs text-[#848E9C] bg-[#1E2329] border border-[#2B3139] rounded p-3 space-y-1 leading-relaxed">
          <p className="text-[#EAECEF]">获取步骤：</p>
          <ol className="list-decimal list-inside space-y-1">
            <li>登录 binance.com 并进入"跟单交易 → 我的跟单"</li>
            <li>F12 打开开发者工具 → 切到 Network 网络面板</li>
            <li>页面随便点一下要跟的领航员，看到 user-position 或 trade-history 请求</li>
            <li>右键该请求 → Copy → Copy as cURL（bash）</li>
            <li>粘贴到下面文本框，点"自动解析"</li>
          </ol>
          <p className="text-[#F0B90B] mt-2">⚠ 凭证大约 7-30 天后会过期，过期时系统会发邮件提醒，回到这里重新粘贴一次即可。</p>
        </div>
      )}

      {/* cURL 自动解析 */}
      <div>
        <label className="text-xs text-[#848E9C] block mb-1">粘贴 cURL（推荐）</label>
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
            onClick={parseCurl}
            className="px-3 py-1.5 bg-[#F0B90B] text-black text-xs font-medium rounded hover:bg-[#E1A706]"
          >
            自动解析
          </button>
          {parseHint && (
            <span className={`text-xs ${parseHint.startsWith('已识别') ? 'text-[#0ECB81]' : 'text-[#F6465D]'}`}>
              {parseHint}
            </span>
          )}
        </div>
      </div>

      {/* 手动输入 */}
      <div className="grid grid-cols-1 gap-3 pt-2 border-t border-[#2B3139]">
        <div>
          <label className="text-xs text-[#848E9C] block mb-1">
            p20t (登录 cookie)
            {p20t && <span className="ml-2 text-[#0ECB81]">✓ 已填 {p20t.length} 字符</span>}
          </label>
          <input
            type="text"
            value={p20t}
            onChange={(e) => onChange(e.target.value, csrfToken)}
            placeholder="登录态 cookie p20t 的值"
            className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-xs"
          />
        </div>
        <div>
          <label className="text-xs text-[#848E9C] block mb-1">
            csrftoken (CSRF header)
            {csrfToken && <span className="ml-2 text-[#0ECB81]">✓ 已填 {csrfToken.length} 字符</span>}
          </label>
          <input
            type="text"
            value={csrfToken}
            onChange={(e) => onChange(p20t, e.target.value)}
            placeholder="csrftoken header 的值"
            className="w-full px-3 py-2 bg-[#0B0E11] border border-[#2B3139] rounded text-[#EAECEF] focus:border-[#F0B90B] focus:outline-none font-mono text-xs"
          />
        </div>
      </div>
    </div>
  )
}
