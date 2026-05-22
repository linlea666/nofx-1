package copytrade

// Binance 跟单数据源 Provider
//
// 接口说明：
//   - 使用 Binance 网站私有 web 接口（www.binance.com/bapi/...），非官方 fapi
//   - 官方 API（fapi.binance.com）只能查"自己账户"，无法读其他跟单领航员持仓
//   - 私有 web 接口需要 p20t（登录 cookie）+ csrftoken（CSRF header）双因子认证
//
// portfolioId 双 ID 模型（关键）：
//   - leadPortfolioId：领航员主页 ID（用户配置时填入），可从 binance 领航员主页 URL 复制
//   - copyPortfolioId：跟单关系 ID，用户跟单领航员后由 binance 自动生成
//   - lead-portfolio/detail 接口必须用 leadPortfolioId（返回 marginBalance + copyPortfolioId 映射）
//   - user-position / trade-history 接口必须用 copyPortfolioId
//   - Provider 内部用 leadPortfolioId 调 detail 自动反查得到 copyPortfolioId 后缓存
//
// 关键约束：
//   - 用户必须先在 binance.com 上实际跟单了该领航员，detail.hasCopy 才会为 true
//     否则会得到 copyPortfolioId=null，无法调持仓接口
//   - p20t 会过期（通常 7-30 天），过期时返回错误码 100001005 / 100002002
//   - 凭证过期由 ErrBinanceCredentialsExpired 通报，engine 端会触发邮件告警
//
// 设计原则：
//   - 与现有 OKX/Hyperliquid Provider 物理隔离，互不依赖
//   - 复用 copytrade 包内的标准结构体 Fill/Position/AccountState
//   - 复用工具函数 parseFloat / normalizeSymbol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"nofx/logger"
)

// Binance Web 跟单专用 API
const (
	// BinanceCopyTradePositionAPI 领航员当前持仓（需鉴权 p20t+csrftoken）
	BinanceCopyTradePositionAPI = "https://www.binance.com/bapi/futures/v6/private/future/user-data/user-position"

	// BinanceCopyTradeTradeHistoryAPI 领航员成交记录（需鉴权 p20t+csrftoken）
	BinanceCopyTradeTradeHistoryAPI = "https://www.binance.com/bapi/futures/v1/private/future/copy-trade/copy-portfolio/trade-history"

	// BinanceLeadPortfolioDetailAPI 领航员资料详情接口
	//   - 用 leadPortfolioId 调用，返回 marginBalance + copyPortfolioId 反向映射
	//   - 不带 p20t 也能拿 marginBalance，但 copyPortfolioId 会是 null
	//   - 带 p20t 后返回当前账户对该领航员的 copyPortfolioId（仅当用户已跟单）
	//   - 我们必须带 p20t，因为还要靠这个 copyPortfolioId 调持仓/成交接口
	BinanceLeadPortfolioDetailAPI = "https://www.binance.com/bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/detail"

	// BinanceAccountBaseInfoAPI 当前登录账号基础信息（用于校验 Web 凭证是否仍有效）
	BinanceAccountBaseInfoAPI = "https://www.binance.com/bapi/accounts/v1/private/account/get-user-base-info"

	// BinanceErrPortfolioClosed 领航员已关闭带单：detail 接口对 closed portfolio 返回的码
	// 同时也是用错 ID（如把 copyPortfolioId 当 leadPortfolioId 用）时的报错
	BinanceErrPortfolioClosed = "11012030"

	// Binance 错误码：未登录/认证失败 → 凭证过期
	BinanceErrNotLogin   = "100001005"
	BinanceErrAuthFailed = "100002002"

	// 跟单类型：固定 COPY（跟随者视角）
	BinanceCopyTradeType = "COPY"

	// 成功码
	BinanceCodeSuccess = "000000"

	// marginBalance 缓存 TTL：60s 足以覆盖 3 次 engine poll 周期（默认 20s），
	// 同时领航员盈亏波动后 1 分钟内能刷新到，权衡精度与副接口压力
	binanceMarginBalanceTTL = 60 * time.Second
)

// ErrBinanceCredentialsExpired Binance Web 凭证过期错误
// 上层捕获后应触发邮件告警，提示用户重新粘贴 cURL
var ErrBinanceCredentialsExpired = errors.New("binance web credentials expired or invalid; please update p20t/csrftoken")

// ErrBinanceNotCopying 用户未在 binance 跟单该领航员（leadPortfolioId 配置错误或还没跟单）
// 与凭证过期区分：这是配置层面的错误，需要用户自己去 binance 完成跟单关系建立
var ErrBinanceNotCopying = errors.New("binance: current account not copying this leader; please follow the leader on binance first")

// BinanceProvider Binance 跟单数据提供者（轮询模式）
type BinanceProvider struct {
	client    *http.Client
	p20t      string // 登录 cookie
	csrfToken string // CSRF header

	// 内部状态（线程安全）
	mu             sync.Mutex
	leaderUserID   string          // 从持仓接口 id 字段（如 "1239518824_ETHUSDT_LONG"）首次解析获得
	seenFillKeys   map[string]bool // (time|symbol|side|price|qty|posSide) 五元组指纹去重
	seenKeysExpiry time.Time       // 指纹缓存过期时间（>24h 清理）

	// detail 接口缓存：marginBalance + copyPortfolioId（同一次调用同时拿到，绑定缓存）
	//   - marginBalance: 60s TTL，作为领航员权益（state.TotalEquity）
	//   - copyPortfolioID: 长期缓存（直到下次 detail 成功刷新），用于调持仓/成交接口
	mbValue         float64   // 最近一次成功拉到的 marginBalance
	mbFetchedAt     time.Time // 拉取时间，距今 < TTL 视为有效
	mbValid         bool      // 是否曾成功拉到过 marginBalance
	copyPortfolioID string    // 用户对该领航员的跟单关系 ID（用于 user-position / trade-history）
}

// NewBinanceProvider 创建 Binance 数据源
// p20t/csrfToken 从前端跟单配置传入（用户从浏览器抓取）
func NewBinanceProvider(p20t, csrfToken string) *BinanceProvider {
	return &BinanceProvider{
		client:         &http.Client{Timeout: 15 * time.Second},
		p20t:           strings.TrimSpace(p20t),
		csrfToken:      strings.TrimSpace(csrfToken),
		seenFillKeys:   make(map[string]bool),
		seenKeysExpiry: time.Now().Add(24 * time.Hour),
	}
}

// Type 返回提供者类型
func (p *BinanceProvider) Type() ProviderType {
	return ProviderBinance
}

// ============================================================================
// LeaderProvider 接口实现
// ============================================================================

// GetAccountState 获取领航员账户状态（持仓）
// leadPortfolioID: 领航员主页 portfolioId（用户在前端配置）
// Provider 内部会先调 lead-portfolio/detail 反查 copyPortfolioId 再拉持仓
func (p *BinanceProvider) GetAccountState(leadPortfolioID string) (*AccountState, error) {
	leadPortfolioID = strings.TrimSpace(leadPortfolioID)
	if leadPortfolioID == "" {
		return nil, errors.New("binance: leadPortfolioId is required")
	}

	// 先确保拿到 copyPortfolioId（首次调用会刷新 detail 同时填充 marginBalance 缓存）
	copyPID, err := p.resolveCopyPortfolioID(leadPortfolioID)
	if err != nil {
		return nil, fmt.Errorf("resolve copyPortfolioId: %w", err)
	}

	raw, err := p.postCopyTrade(BinanceCopyTradePositionAPI, map[string]interface{}{
		"copyTradeType": BinanceCopyTradeType,
		"portfolioId":   copyPID,
	})
	if err != nil {
		return nil, err
	}

	var resp BinancePositionResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode binance position response: %w; body=%s", err, truncate(string(raw), 200))
	}

	if isBinanceAuthError(resp.Code) {
		logger.Warnf("⚠️ Binance copy-trade credentials expired | code=%s msg=%s leadPortfolioId=%s",
			resp.Code, resp.Message, leadPortfolioID)
		return nil, ErrBinanceCredentialsExpired
	}

	if resp.Code != BinanceCodeSuccess {
		return nil, fmt.Errorf("binance position api error: code=%s msg=%s", resp.Code, resp.Message)
	}

	state := &AccountState{
		Positions: make(map[string]*Position),
		Timestamp: time.Now(),
	}

	// 解析所有持仓
	totalNotional := 0.0
	for _, rp := range resp.Data {
		// 第一次解析时记录 leader userId（用于 Fill 拼装 PosID）
		p.captureLeaderUserID(rp.ID)

		symbol := normalizeSymbol(rp.Symbol)

		// 方向映射：LONG/SHORT 直接映射；BOTH 模式按 positionAmount 正负号判断
		side := mapBinanceSide(rp.PositionSide)
		amount := parseFloat(rp.PositionAmount)
		if side == "" {
			if amount > 0 {
				side = SideLong
			} else if amount < 0 {
				side = SideShort
			} else {
				continue // 空仓
			}
		}

		size := math.Abs(amount)
		if size == 0 {
			continue
		}

		// 杠杆反推：notionalValue / initialMargin（常见 5x/10x/20x/40x/50x/100x 都精确）
		initialMargin := parseFloat(rp.InitialMargin)
		notional := parseFloat(rp.NotionalValue)
		leverage := 0
		if initialMargin > 0 {
			leverage = int(math.Round(notional / initialMargin))
		}
		totalNotional += notional

		// 全/逐仓判定：isolatedWallet/isolatedMargin > 0 即逐仓，否则全仓
		marginMode := "cross"
		if parseFloat(rp.IsolatedWallet) > 0 || parseFloat(rp.IsolatedMargin) > 0 {
			marginMode = "isolated"
		}

		// PosID 直接使用 Binance 返回的 id："{userId}_{symbol}_{positionSide}"
		// 兜底：若空则按相同格式拼装，保证与 Fill 端一致
		posID := rp.ID
		if posID == "" {
			posID = p.buildPosID(symbol, rp.PositionSide)
		}

		state.Positions[posID] = &Position{
			Symbol:        symbol,
			Side:          side,
			Size:          size,
			EntryPrice:    parseFloat(rp.EntryPrice),
			MarkPrice:     parseFloat(rp.MarkPrice),
			Leverage:      leverage,
			MarginMode:    marginMode,
			UnrealizedPnL: parseFloat(rp.UnrealizedProfit),
			PositionValue: notional,
			PosID:         posID,
		}
	}

	// ============================================================
	// TotalEquity 三级降级策略（与 OKX/HL 钱包余额量纲对齐）
	// ============================================================
	// 由于 user-position 接口不返回钱包余额，必须通过 lead-portfolio/detail
	// 接口获取 marginBalance 作为"领航员权益"
	//   - 错误用 sum(IM)/sum(notional) 会让 engine.go ratio 计算严重失真
	//   - 优先级 1: detail 本次成功 → 用最新 marginBalance
	//   - 优先级 2: detail 本次失败但有缓存 → 用旧 marginBalance + warn
	//   - 优先级 3: 从未成功过 → 用 sum(notional) + warn（仍能跟动作，比例不精确）
	leaderEquity, source := p.resolveLeaderEquity(leadPortfolioID, totalNotional)
	state.TotalEquity = leaderEquity
	state.AvailableBalance = leaderEquity
	logger.Debugf("📊 Binance leader equity | leadPortfolioId=%s copyPortfolioId=%s equity=%.4f source=%s totalNotional=%.4f positions=%d",
		leadPortfolioID, copyPID, leaderEquity, source, totalNotional, len(state.Positions))

	return state, nil
}

// ValidateCredentials 使用 Binance 账号状态接口校验 p20t/csrftoken 是否仍有效。
// 只把明确的登录失效/鉴权失败归类为 ErrBinanceCredentialsExpired；
// 网络错误、5xx 等临时问题返回普通错误，避免误报凭证过期。
func (p *BinanceProvider) ValidateCredentials() error {
	raw, statusCode, err := p.getAccountBaseInfo()
	if err != nil {
		return err
	}

	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return ErrBinanceCredentialsExpired
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("binance account status http %d: %s", statusCode, truncate(string(raw), 200))
	}

	var resp BinanceAccountBaseInfoResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("decode binance account status response: %w; body=%s", err, truncate(string(raw), 200))
	}

	if isBinanceAuthError(resp.Code) {
		return ErrBinanceCredentialsExpired
	}
	if resp.Code != BinanceCodeSuccess {
		return fmt.Errorf("binance account status api error: code=%s msg=%s", resp.Code, resp.Message)
	}
	if strings.TrimSpace(resp.Data.UserID) == "" {
		return ErrBinanceCredentialsExpired
	}
	return nil
}

// GetFills 获取领航员最近成交记录
// leadPortfolioID: 领航员主页 portfolioId（前端配置）
func (p *BinanceProvider) GetFills(leadPortfolioID string, since time.Time) ([]Fill, error) {
	leadPortfolioID = strings.TrimSpace(leadPortfolioID)
	if leadPortfolioID == "" {
		return nil, errors.New("binance: leadPortfolioId is required")
	}

	copyPID, err := p.resolveCopyPortfolioID(leadPortfolioID)
	if err != nil {
		return nil, fmt.Errorf("resolve copyPortfolioId: %w", err)
	}

	raw, err := p.postCopyTrade(BinanceCopyTradeTradeHistoryAPI, map[string]interface{}{
		"copyTradeType": BinanceCopyTradeType,
		"portfolioId":   copyPID,
		"page":          1,
		"rows":          50,
	})
	if err != nil {
		return nil, err
	}

	var resp BinanceTradeHistoryResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode binance trade-history response: %w; body=%s", err, truncate(string(raw), 200))
	}

	if isBinanceAuthError(resp.Code) {
		logger.Warnf("⚠️ Binance copy-trade credentials expired | code=%s msg=%s leadPortfolioId=%s",
			resp.Code, resp.Message, leadPortfolioID)
		return nil, ErrBinanceCredentialsExpired
	}

	if resp.Code != BinanceCodeSuccess {
		return nil, fmt.Errorf("binance trade-history api error: code=%s msg=%s", resp.Code, resp.Message)
	}

	sinceMs := since.UnixMilli()
	var fills []Fill

	p.mu.Lock()
	defer p.mu.Unlock()

	// 定期清理指纹缓存，防止内存累积
	if time.Now().After(p.seenKeysExpiry) {
		p.seenFillKeys = make(map[string]bool)
		p.seenKeysExpiry = time.Now().Add(24 * time.Hour)
	}

	for _, tr := range resp.Data.List {
		// 时间过滤
		if tr.Time < sinceMs {
			continue
		}

		// 五元组指纹去重（Binance 成交无 fillId/orderId）
		fp := fillFingerprint(tr)
		if p.seenFillKeys[fp] {
			continue
		}
		p.seenFillKeys[fp] = true

		symbol := normalizeSymbol(tr.Symbol)
		side := strings.ToLower(tr.Side) // BUY -> buy, SELL -> sell
		posSide := mapBinanceSide(tr.PositionSide)
		// BOTH（单向持仓）模式：positionSide=BOTH 时无法从 fill 自身判断方向，
		// 设为 SideNet 让 engine.normalizeNetModeFill 用 leaderState/db 反推方向，
		// 复用 OKX 单向模式已有的标准化逻辑，避免按 buy/sell 瞎猜导致方向反转
		if posSide == "" {
			posSide = SideNet
		}

		fill := Fill{
			ID:           fp, // 用指纹作为唯一 ID
			Symbol:       symbol,
			Side:         side,
			PositionSide: posSide,
			Price:        tr.Price,
			Size:         tr.Qty,
			Value:        tr.Quantity, // Binance "quantity" 字段 = 成交额 USDT
			Timestamp:    time.UnixMilli(tr.Time),
			ClosedPnL:    tr.RealizedProfit,
			Raw:          tr,
		}

		// 推断 Action：realizedProfit != 0 一定是平仓/减仓
		fill.Action = guessBinanceAction(side, posSide, tr.RealizedProfit)

		fills = append(fills, fill)
	}

	return fills, nil
}

// ============================================================================
// 内部方法
// ============================================================================

// postCopyTrade 发送 Binance 跟单专用 POST 请求
//
// 必需 header：
//   - content-type: application/json
//   - csrftoken: <csrftoken>
//   - clienttype: web
//
// 必需 cookie：
//   - p20t: <p20t value>
func (p *BinanceProvider) postCopyTrade(url string, body interface{}) ([]byte, error) {
	if p.p20t == "" || p.csrfToken == "" {
		return nil, ErrBinanceCredentialsExpired
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("csrftoken", p.csrfToken)
	req.Header.Set("clienttype", "web")
	req.Header.Set("user-agent", "Mozilla/5.0 (compatible; NOFX/1.0)")
	req.Header.Set("accept", "*/*")
	req.AddCookie(&http.Cookie{Name: "p20t", Value: p.p20t})

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binance request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("binance read body failed: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrBinanceCredentialsExpired
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance http %d: %s", resp.StatusCode, truncate(string(rawBody), 200))
	}

	return rawBody, nil
}

func (p *BinanceProvider) getAccountBaseInfo() ([]byte, int, error) {
	if p.p20t == "" || p.csrfToken == "" {
		return nil, 0, ErrBinanceCredentialsExpired
	}

	req, err := http.NewRequest(http.MethodGet, BinanceAccountBaseInfoAPI, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("clienttype", "web")
	req.Header.Set("csrftoken", p.csrfToken)
	req.Header.Set("user-agent", "Mozilla/5.0 (compatible; NOFX/1.0)")
	req.AddCookie(&http.Cookie{Name: "p20t", Value: p.p20t})

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("binance account status request failed: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("binance account status read body failed: %w", err)
	}
	return rawBody, resp.StatusCode, nil
}

// resolveCopyPortfolioID 解析 leadPortfolioId → copyPortfolioId
//
// 流程：
//   - 优先返回内存缓存（用户跟单关系一般稳定，长期有效）
//   - 缓存为空时调 detail 刷新（同时填充 marginBalance 缓存，避免后续重复调）
//   - detail 失败 / hasCopy=false / copyPortfolioId 为空 → 返回错误，调用者无法继续
//
// 返回错误时上层不能降级，因为没有 copyPortfolioId 持仓/成交接口根本无法调用
func (p *BinanceProvider) resolveCopyPortfolioID(leadPortfolioID string) (string, error) {
	if p.p20t == "" || p.csrfToken == "" {
		return "", ErrBinanceCredentialsExpired
	}

	p.mu.Lock()
	cached := p.copyPortfolioID
	p.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	mb, cpid, err := p.fetchLeaderDetail(leadPortfolioID)
	if err != nil {
		return "", err
	}
	if cpid == "" {
		if err := p.ValidateCredentials(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w (leadPortfolioId=%s)", ErrBinanceNotCopying, leadPortfolioID)
	}

	p.mu.Lock()
	p.copyPortfolioID = cpid
	if mb > 0 {
		p.mbValue = mb
		p.mbFetchedAt = time.Now()
		p.mbValid = true
	}
	p.mu.Unlock()
	return cpid, nil
}

// resolveLeaderEquity 解析"领航员权益"，三级降级：
//   - L1 detail 本次成功 → 写缓存后返回新值
//   - L2 detail 失败但有旧缓存 → 用旧值，warn 提示
//   - L3 完全无缓存 → 用 sum(notional) 兜底，warn 提示
//   - L4 sum(notional)==0（领航员空仓） → 返回 1 防止 engine 除零
//
// totalNotional 由调用者传入（GetAccountState 已遍历持仓时顺手累加）
// 返回值: (equity, source) source 用于日志便于排查
func (p *BinanceProvider) resolveLeaderEquity(leadPortfolioID string, totalNotional float64) (float64, string) {
	p.mu.Lock()
	cacheValid := p.mbValid && time.Since(p.mbFetchedAt) < binanceMarginBalanceTTL
	cachedValue := p.mbValue
	hasCache := p.mbValid
	p.mu.Unlock()

	if cacheValid && cachedValue > 0 {
		return cachedValue, "cache"
	}

	mb, cpid, err := p.fetchLeaderDetail(leadPortfolioID)
	if err == nil && mb > 0 {
		p.mu.Lock()
		p.mbValue = mb
		p.mbFetchedAt = time.Now()
		p.mbValid = true
		if cpid != "" {
			p.copyPortfolioID = cpid // 顺便刷新 copyPortfolioId（防用户解除/重建跟单关系）
		}
		p.mu.Unlock()
		return mb, "fresh"
	}

	if hasCache && cachedValue > 0 {
		logger.Warnf("⚠️ Binance lead-portfolio/detail 拉取失败，沿用过期缓存 | leadPortfolioId=%s err=%v cachedValue=%.4f",
			leadPortfolioID, err, cachedValue)
		return cachedValue, "stale_cache"
	}

	if totalNotional > 0 {
		logger.Warnf("⚠️ Binance lead-portfolio/detail 拉取失败且无缓存，降级用 sum(notional) | leadPortfolioId=%s err=%v totalNotional=%.4f",
			leadPortfolioID, err, totalNotional)
		return totalNotional, "fallback_notional"
	}

	logger.Warnf("⚠️ Binance lead-portfolio/detail 拉取失败且领航员空仓，equity 兜底为 1 | leadPortfolioId=%s err=%v",
		leadPortfolioID, err)
	return 1, "fallback_one"
}

// fetchLeaderDetail 调用 lead-portfolio/detail 接口
//
// 接口特性：
//   - GET，公开可访问但**必须带 p20t** 才能返回 copyPortfolioId（用户的跟单关系 ID）
//   - 不带 p20t：仅返回 marginBalance；copyPortfolioId 为 null
//   - 失败场景：
//   - portfolio closed (code=11012030) — 用错 ID（把 copyPortfolioId 当 leadPortfolioId）
//   - 网络超时 / HTTP 非 2xx
//
// 返回 (marginBalance, copyPortfolioId, error)
//   - marginBalance：领航员保证金余额（USDT 钱包余额量纲，与 OKX/HL TotalEquity 等价）
//   - copyPortfolioId：当前账户对该领航员的跟单关系 ID；hasCopy=false 时为 ""
//
// 注意：aumAmount 是"跟单池总规模"（领航员本金 + 所有跟随者本金），不能用作权益
func (p *BinanceProvider) fetchLeaderDetail(leadPortfolioID string) (float64, string, error) {
	if p.p20t == "" || p.csrfToken == "" {
		return 0, "", ErrBinanceCredentialsExpired
	}

	url := BinanceLeadPortfolioDetailAPI + "?portfolioId=" + leadPortfolioID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("accept", "*/*")
	req.Header.Set("clienttype", "web")
	req.Header.Set("user-agent", "Mozilla/5.0 (compatible; NOFX/1.0)")
	// p20t 是关键：没有它 copyPortfolioId 会是 null 导致后续无法调用持仓/成交接口
	if p.p20t != "" {
		req.AddCookie(&http.Cookie{Name: "p20t", Value: p.p20t})
	}
	if p.csrfToken != "" {
		req.Header.Set("csrftoken", p.csrfToken)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("lead-portfolio/detail request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return 0, "", ErrBinanceCredentialsExpired
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("lead-portfolio/detail http %d", resp.StatusCode)
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("lead-portfolio/detail read body failed: %w", err)
	}

	var dr BinanceLeadPortfolioDetailResp
	if err := json.Unmarshal(rawBody, &dr); err != nil {
		return 0, "", fmt.Errorf("decode lead-portfolio/detail: %w; body=%s", err, truncate(string(rawBody), 200))
	}

	if isBinanceAuthError(dr.Code) {
		return 0, "", ErrBinanceCredentialsExpired
	}
	if dr.Code != BinanceCodeSuccess {
		// portfolio closed 通常意味着用错 ID（把 copyPortfolioId 当 leadPortfolioId）
		if dr.Code == BinanceErrPortfolioClosed {
			return 0, "", fmt.Errorf("lead-portfolio/detail: portfolio closed or invalid leadPortfolioId (code=%s, msg=%s)",
				dr.Code, dr.Message)
		}
		return 0, "", fmt.Errorf("lead-portfolio/detail api error: code=%s msg=%s", dr.Code, dr.Message)
	}

	mb := parseFloat(dr.Data.MarginBalance)
	if mb <= 0 {
		return 0, dr.Data.CopyPortfolioID, fmt.Errorf("lead-portfolio/detail marginBalance non-positive: %q", dr.Data.MarginBalance)
	}

	logger.Debugf("📊 Binance leader detail refreshed | leadPortfolioId=%s copyPortfolioId=%s marginBalance=%.4f aum=%s status=%s isPaused=%v hasCopy=%v",
		leadPortfolioID, dr.Data.CopyPortfolioID, mb, dr.Data.AumAmount, dr.Data.Status, dr.Data.IsPaused, dr.Data.HasCopy)

	return mb, dr.Data.CopyPortfolioID, nil
}

// captureLeaderUserID 从持仓 id 字段首次解析 leader userId
// id 形式："1239518824_ETHUSDT_LONG"
func (p *BinanceProvider) captureLeaderUserID(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.leaderUserID != "" {
		return
	}
	parts := strings.SplitN(id, "_", 2)
	if len(parts) >= 1 && parts[0] != "" {
		p.leaderUserID = parts[0]
	}
}

// buildPosID 兜底拼装 PosID（持仓接口未返回 id 时使用）
// 格式与 Binance 原生 id 保持一致：{userId}_{SYMBOL}_{POSSIDE_UPPER}
func (p *BinanceProvider) buildPosID(symbol, positionSide string) string {
	p.mu.Lock()
	userID := p.leaderUserID
	p.mu.Unlock()
	if userID == "" {
		userID = "unknown"
	}
	return userID + "_" + symbol + "_" + strings.ToUpper(positionSide)
}

// ============================================================================
// 工具函数
// ============================================================================

// isBinanceAuthError 判断是否为认证错误（凭证过期）
func isBinanceAuthError(code string) bool {
	return code == BinanceErrNotLogin || code == BinanceErrAuthFailed
}

// mapBinanceSide 将 Binance positionSide 映射到 SideType
// LONG -> long, SHORT -> short, BOTH/其他 -> "" (caller 应用兜底逻辑)
func mapBinanceSide(positionSide string) SideType {
	switch strings.ToUpper(positionSide) {
	case "LONG":
		return SideLong
	case "SHORT":
		return SideShort
	}
	return ""
}

// guessBinanceAction 根据 side+positionSide+realizedProfit 推断动作类型
// Binance 成交记录没有 startPosition 字段，无法精确判定 open vs add / close vs reduce
// 引擎层会用 lastKnownSize 进一步精确判断
func guessBinanceAction(side string, posSide SideType, realized float64) ActionType {
	// realizedProfit 非零 → 一定是平仓/减仓
	if realized != 0 {
		if posSide == SideLong && side == "sell" {
			return ActionClose
		}
		if posSide == SideShort && side == "buy" {
			return ActionClose
		}
	}
	// 否则默认为开仓（引擎会进一步区分 open/add）
	return ActionOpen
}

// fillFingerprint 五元组指纹（用于 Fill 去重）
// Binance 成交无唯一 id，组合 time+symbol+side+price+qty+positionSide 作为去重 key
func fillFingerprint(tr BinanceTradeRecord) string {
	return fmt.Sprintf("%d|%s|%s|%.8f|%.8f|%s",
		tr.Time, tr.Symbol, tr.Side, tr.Price, tr.Qty, tr.PositionSide)
}

// truncate 截断字符串（用于错误日志）
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ============================================================================
// API 响应结构
// ============================================================================

// BinancePositionResp /bapi/futures/v6/private/future/user-data/user-position 响应
type BinancePositionResp struct {
	Code    string               `json:"code"`
	Message string               `json:"message"`
	Success bool                 `json:"success"`
	Data    []BinancePositionRaw `json:"data"`
}

// BinancePositionRaw 单条领航员持仓原始数据
// 字段命名严格对应 Binance 返回 JSON
type BinancePositionRaw struct {
	ID                     string `json:"id"`                     // "{userId}_{symbol}_{positionSide}"
	Symbol                 string `json:"symbol"`                 // "ETHUSDT"
	PositionSide           string `json:"positionSide"`           // "LONG"/"SHORT"/"BOTH"
	PositionAmount         string `json:"positionAmount"`         // 持仓数量（币本位，可带负号表示空头 BOTH 模式）
	EntryPrice             string `json:"entryPrice"`             // 开仓均价
	BreakEvenPrice         string `json:"breakEvenPrice"`         // 保本价
	MarkPrice              string `json:"markPrice"`              // 标记价
	UnrealizedProfit       string `json:"unrealizedProfit"`       // 未实现盈亏
	LiquidationPrice       string `json:"liquidationPrice"`       // 强平价（"0" 表示无）
	IsolatedMargin         string `json:"isolatedMargin"`         // 逐仓保证金（>0 即逐仓）
	NotionalValue          string `json:"notionalValue"`          // 名义价值（USDT）
	Collateral             string `json:"collateral"`             // 保证金币种 ("USDT")
	IsolatedWallet         string `json:"isolatedWallet"`         // 逐仓钱包余额（>0 即逐仓）
	CumRealized            string `json:"cumRealized"`            // 累计已实现盈亏
	InitialMargin          string `json:"initialMargin"`          // 当前占用保证金（用于反推杠杆）
	MaintMargin            string `json:"maintMargin"`            // 维持保证金
	PositionInitialMargin  string `json:"positionInitialMargin"`  // 持仓初始保证金
	OpenOrderInitialMargin string `json:"openOrderInitialMargin"` // 挂单初始保证金
	Adl                    int    `json:"adl"`                    // 自动减仓队列等级
	AskNotional            string `json:"askNotional"`            // 卖盘名义价值
	BidNotional            string `json:"bidNotional"`            // 买盘名义价值
	UpdateTime             int64  `json:"updateTime"`             // 持仓更新时间（毫秒）
}

// BinanceTradeHistoryResp /bapi/futures/v1/private/future/copy-trade/copy-portfolio/trade-history 响应
type BinanceTradeHistoryResp struct {
	Code    string                  `json:"code"`
	Message string                  `json:"message"`
	Success bool                    `json:"success"`
	Data    BinanceTradeHistoryData `json:"data"`
}

// BinanceTradeHistoryData trade-history 数据载荷
type BinanceTradeHistoryData struct {
	IndexValue string               `json:"indexValue"`
	Total      int                  `json:"total"`
	List       []BinanceTradeRecord `json:"list"`
}

// BinanceLeadPortfolioDetailResp /bapi/futures/v1/friendly/future/copy-trade/lead-portfolio/detail 响应
// 公开接口（免鉴权 GET），用于获取领航员资料含 marginBalance
type BinanceLeadPortfolioDetailResp struct {
	Code    string                         `json:"code"`
	Message string                         `json:"message"`
	Success bool                           `json:"success"`
	Data    BinanceLeadPortfolioDetailData `json:"data"`
}

// BinanceLeadPortfolioDetailData 仅保留我们需要的字段（接口返回字段非常多，按需声明）
type BinanceLeadPortfolioDetailData struct {
	LeadPortfolioID string `json:"leadPortfolioId"` // 领航员主页 portfolioId
	CopyPortfolioID string `json:"copyPortfolioId"` // 🔑 当前账户对该领航员的跟单关系 ID（带 p20t 时才有）
	HasCopy         bool   `json:"hasCopy"`         // 当前账户是否在跟单（false 时 copyPortfolioId 为 null）
	Nickname        string `json:"nickname"`        // 昵称
	Status          string `json:"status"`          // "ACTIVE" / "CLOSED" / "PAUSED"
	IsPaused        bool   `json:"isPaused"`        // 是否暂停带单
	MarginBalance   string `json:"marginBalance"`   // 🔑 领航员保证金余额 (USDT) — 用作 leader equity
	AumAmount       string `json:"aumAmount"`       // 跟单池总规模（含跟随者）— 不用作 equity，仅日志
	InitInvestAsset string `json:"initInvestAsset"` // "USDT"
	FuturesType     string `json:"futuresType"`     // "UM"
}

// BinanceAccountBaseInfoResp /bapi/accounts/v1/private/account/get-user-base-info 响应
type BinanceAccountBaseInfoResp struct {
	Code    string                     `json:"code"`
	Message string                     `json:"message"`
	Success bool                       `json:"success"`
	Data    BinanceAccountBaseInfoData `json:"data"`
}

// BinanceAccountBaseInfoData 仅保留凭证校验需要的字段
type BinanceAccountBaseInfoData struct {
	UserID string `json:"userId"`
}

// BinanceTradeRecord 单条成交记录
// 注意：price/qty/fee 等字段 Binance 返回为 number 而非 string
type BinanceTradeRecord struct {
	Time                int64   `json:"time"`                // 成交时间（毫秒）
	Symbol              string  `json:"symbol"`              // "ETHUSDT"
	Side                string  `json:"side"`                // "BUY"/"SELL"
	Price               float64 `json:"price"`               // 成交价
	Fee                 float64 `json:"fee"`                 // 手续费（负数=支出）
	FeeAsset            string  `json:"feeAsset"`            // "USDT"
	Quantity            float64 `json:"quantity"`            // 成交额（USDT，= price * qty）
	QuantityAsset       string  `json:"quantityAsset"`       // 报价币种
	RealizedProfit      float64 `json:"realizedProfit"`      // 已实现盈亏（非零=平仓/减仓）
	RealizedProfitAsset string  `json:"realizedProfitAsset"` // 盈亏币种
	BaseAsset           string  `json:"baseAsset"`           // "ETH"
	Qty                 float64 `json:"qty"`                 // 成交数量（币本位）
	PositionSide        string  `json:"positionSide"`        // "LONG"/"SHORT"/"BOTH"
	ActiveBuy           bool    `json:"activeBuy"`           // 是否主动买入（taker）
}
