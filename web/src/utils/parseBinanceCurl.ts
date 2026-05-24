/**
 * 从浏览器复制的 cURL 命令（或原始 HTTP Request Headers）中提取
 * Binance Web 跟单凭证：p20t（cookie）+ csrftoken（header）
 *
 * 兼容多种来源格式：
 *   - cURL `-H 'csrftoken: xxx'` / `-H "csrftoken: xxx"`
 *   - 原始 HTTP Headers 行：`csrftoken: xxx`
 *   - cookie 来源：`-b 'p20t=xxx; ...'` / `--cookie 'p20t=xxx; ...'`
 *   - cookie header 来源：`-H 'cookie: p20t=xxx; ...'`
 *   - 原始 cookie 行：`cookie: p20t=xxx; ...`
 *
 * 提取失败的字段返回空字符串，由调用方决定如何提示用户。
 */
export interface ParsedBinanceCredentials {
  p20t: string
  csrfToken: string
}

export function parseBinanceCurl(input: string): ParsedBinanceCredentials {
  const out: ParsedBinanceCredentials = { p20t: '', csrfToken: '' }
  if (!input || !input.trim()) return out

  // 1) csrftoken — 优先 cURL `-H 'csrftoken: xxx'`
  const csrfHeaderMatch = input.match(/-H\s+["']?csrftoken:\s*([^"'\s]+)["']?/i)
  if (csrfHeaderMatch) {
    out.csrfToken = csrfHeaderMatch[1].trim()
  }
  // 兜底：原始 Headers 行
  if (!out.csrfToken) {
    const fallback = input.match(
      /(?:^|[\r\n])\s*csrftoken:\s*([a-zA-Z0-9_.-]+)/i
    )
    if (fallback) {
      out.csrfToken = fallback[1].trim()
    }
  }

  // 2) p20t — 兼容 -b / --cookie / -H 'cookie: ...'
  const cookieGroupMatches = [
    input.match(/-b\s+["']([^"']+)["']/),
    input.match(/--cookie\s+["']([^"']+)["']/),
    input.match(/-H\s+["']cookie:\s*([^"']+)["']/i),
  ]
  for (const m of cookieGroupMatches) {
    if (m && m[1]) {
      const p20tMatch = m[1].match(/p20t=([^;]+)/)
      if (p20tMatch) {
        out.p20t = p20tMatch[1].trim()
        break
      }
    }
  }
  // 兜底：直接在文本中找 p20t=...
  if (!out.p20t) {
    const fallback = input.match(/p20t=([^;"'\s]+)/)
    if (fallback) {
      out.p20t = fallback[1].trim()
    }
  }

  return out
}
