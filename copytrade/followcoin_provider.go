package copytrade

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// FollowCoin（api-v2.cryptostrategyget.com）实盘数据源
//
// 跟单对象是一个"实盘账户"（apiId）。平台按账户定时（实测 5～10 分钟）
// 拍持仓快照，信号完全由快照差分产生：本 provider 只提供权威持仓快照，
// GetFills 不参与热路径。
// ============================================================================

const (
	FollowCoinAPIBase = "https://api-v2.cryptostrategyget.com"

	followCoinPositionsPath = "/v1/user/api/real/position/current"
	followCoinOverviewPath  = "/v1/user/api/real/stats/overview"
	followCoinAccountsPath  = "/v1/user/api/real/my/list"
	followCoinUserInfoPath  = "/v1/user/info"

	followCoinCodeOK            = "200"
	followCoinCodeTokenMissing  = "20001"
	followCoinCodeTokenInvalid  = "20003"
	followCoinCodeAccountAbsent = "10384"

	DefaultFollowCoinCredentialsLabel = "default"

	followCoinSnapshotCacheTTL = 2 * time.Second
	followCoinEquityCacheTTL   = 60 * time.Second
)

var (
	// ErrFollowCoinTokenInvalid token 未配置、失效或被拒绝（需要用户更新）。
	ErrFollowCoinTokenInvalid = errors.New("FollowCoin token is missing or expired")
	// ErrFollowCoinAccountUnavailable apiId 不存在或实盘已停用。
	ErrFollowCoinAccountUnavailable = errors.New("FollowCoin real account does not exist or has been disabled")
	// ErrFollowCoinEmptyUnconfirmed 领航员快照从有仓变为空仓，等待持续确认。
	ErrFollowCoinEmptyUnconfirmed = errors.New("FollowCoin empty position snapshot is awaiting confirmation")
)

// followCoinEmptyConfirmDelay 空仓需持续的最短时间（约 2 个轮询周期）。
const followCoinEmptyConfirmDelay = 30 * time.Second

// FollowCoinTokenLoader 每次请求前读取全局 token，实现无需重启的热更新。
// 未配置时返回 ("", nil)。
type FollowCoinTokenLoader interface {
	LoadFollowCoinToken(label string) (string, error)
}

// FollowCoinAPIError 平台业务错误（HTTP 200 但 body.code≠200）。
type FollowCoinAPIError struct {
	Code, Msg string
	cause     error
}

func (e *FollowCoinAPIError) Error() string {
	return fmt.Sprintf("FollowCoin API code=%s msg=%s", e.Code, e.Msg)
}

func (e *FollowCoinAPIError) Unwrap() error { return e.cause }

// FollowCoinHTTPError 非 200 HTTP 响应。
type FollowCoinHTTPError struct {
	Status              int
	RetryAfter, Message string
}

func (e *FollowCoinHTTPError) Error() string {
	return fmt.Sprintf("FollowCoin HTTP %d: %s", e.Status, e.Message)
}

// FollowCoinAccount 交易员名下的一个实盘账户。
type FollowCoinAccount struct {
	APIID        int64  `json:"api_id"`
	Alias        string `json:"alias"`
	ExchangeID   int    `json:"exchange_id"`
	ExchangeName string `json:"exchange_name"`
	Status       int    `json:"status"`
}

// FollowCoinUser token 所属登录账号（只暴露昵称，不暴露邮箱等隐私字段）。
type FollowCoinUser struct {
	ID       int64  `json:"id"`
	NickName string `json:"nick_name"`
}

func followCoinExchangeName(id int) string {
	switch id {
	case 1:
		return "Binance"
	case 2:
		return "OKX"
	default:
		return fmt.Sprintf("exchange-%d", id)
	}
}

type FollowCoinProvider struct {
	client  *http.Client
	baseURL string
	loader  FollowCoinTokenLoader
	label   string
	token   string // 显式 token（保存前探测用）；为空时从 loader 读取
}

// NewFollowCoinProviderWithLoader 生产路径：每次请求从全局凭证读取 token。
func NewFollowCoinProviderWithLoader(loader FollowCoinTokenLoader) *FollowCoinProvider {
	return &FollowCoinProvider{
		client:  &http.Client{Timeout: 15 * time.Second},
		baseURL: FollowCoinAPIBase,
		loader:  loader,
		label:   DefaultFollowCoinCredentialsLabel,
	}
}

// NewFollowCoinProviderWithToken 使用显式 token（保存凭证前校验、查询账户）。
func NewFollowCoinProviderWithToken(token string) *FollowCoinProvider {
	p := NewFollowCoinProviderWithLoader(nil)
	p.token = strings.TrimSpace(token)
	return p
}

func (p *FollowCoinProvider) Type() ProviderType { return ProviderFollowCoin }

func (p *FollowCoinProvider) currentToken() (string, error) {
	if p.token != "" {
		return p.token, nil
	}
	if p.loader == nil {
		return "", fmt.Errorf("%w: not configured", ErrFollowCoinTokenInvalid)
	}
	token, err := p.loader.LoadFollowCoinToken(p.label)
	if err != nil {
		return "", fmt.Errorf("load FollowCoin token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("%w: not configured", ErrFollowCoinTokenInvalid)
	}
	return strings.TrimSpace(token), nil
}

// request 发送请求并解出 data。body 为 nil 时用 GET。
func (p *FollowCoinProvider) request(method, path string, query neturl.Values, body interface{}, out interface{}) error {
	token, err := p.currentToken()
	if err != nil {
		return err
	}
	u := p.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("token", token)
	req.Header.Set("os", "ios")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &FollowCoinHTTPError{Status: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After"), Message: msg}
	}
	var envelope struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode FollowCoin response: %w", err)
	}
	switch envelope.Code {
	case followCoinCodeOK:
	case followCoinCodeTokenMissing, followCoinCodeTokenInvalid:
		return &FollowCoinAPIError{Code: envelope.Code, Msg: envelope.Msg, cause: ErrFollowCoinTokenInvalid}
	case followCoinCodeAccountAbsent:
		return &FollowCoinAPIError{Code: envelope.Code, Msg: envelope.Msg, cause: ErrFollowCoinAccountUnavailable}
	default:
		return &FollowCoinAPIError{Code: envelope.Code, Msg: envelope.Msg}
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("FollowCoin response data is missing")
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode FollowCoin data: %w", err)
	}
	return nil
}

// ValidateToken 确认 token 有效并返回所属账号昵称。
func (p *FollowCoinProvider) ValidateToken() (*FollowCoinUser, error) {
	var raw struct {
		ID       int64  `json:"id"`
		NickName string `json:"userNickName"`
	}
	if err := p.request(http.MethodGet, followCoinUserInfoPath, neturl.Values{"loading": {"false"}}, nil, &raw); err != nil {
		return nil, err
	}
	if raw.ID <= 0 {
		return nil, fmt.Errorf("FollowCoin user info is incomplete")
	}
	return &FollowCoinUser{ID: raw.ID, NickName: raw.NickName}, nil
}

// ListAccounts 交易员主页 ID → 其名下的实盘账户（apiId）。
func (p *FollowCoinProvider) ListAccounts(userID int64) ([]FollowCoinAccount, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid FollowCoin user id")
	}
	var raw []struct {
		ID         int64  `json:"id"`
		Alias      string `json:"alias"`
		ExchangeID int    `json:"exchangeId"`
		Status     int    `json:"status"`
	}
	q := neturl.Values{"userId": {strconv.FormatInt(userID, 10)}, "loading": {"false"}}
	if err := p.request(http.MethodGet, followCoinAccountsPath, q, nil, &raw); err != nil {
		return nil, err
	}
	accounts := make([]FollowCoinAccount, 0, len(raw))
	for _, a := range raw {
		if a.ID <= 0 {
			continue
		}
		accounts = append(accounts, FollowCoinAccount{
			APIID: a.ID, Alias: a.Alias, ExchangeID: a.ExchangeID,
			ExchangeName: followCoinExchangeName(a.ExchangeID), Status: a.Status,
		})
	}
	return accounts, nil
}

// ParseFollowCoinAPIID 校验并解析 leader_id（实盘账户 apiId）。
func ParseFollowCoinAPIID(leaderID string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(leaderID), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("FollowCoin 实盘账户 apiId 必须是正整数，当前为 %q", leaderID)
	}
	return id, nil
}

// ============================================================================
// 快照热路径
// ============================================================================

type followCoinSnapshotGate struct {
	mu        sync.Mutex
	state     *AccountState
	fetchedAt time.Time
	until     time.Time
	failures  int
	// 权益来自 30 分钟级刷新的 overview，单独缓存，减少请求。
	walletEquity   float64
	equityAt       time.Time
	equityCheckErr error
}

var followCoinSnapshotGates sync.Map

func (p *FollowCoinProvider) snapshotGate(apiID string) *followCoinSnapshotGate {
	// Engines share one gate per account (they all read the global token).
	// A provider holding an explicit token (save-time probe) gets its own gate
	// so it never returns a snapshot fetched with a different token.
	tokenScope := "global"
	if p.token != "" {
		sum := sha256.Sum256([]byte(p.token))
		tokenScope = hex.EncodeToString(sum[:8])
	}
	key := fmt.Sprintf("%p|%s|%s|%s", p.client.Transport, p.baseURL, tokenScope, apiID)
	gate, _ := followCoinSnapshotGates.LoadOrStore(key, &followCoinSnapshotGate{})
	return gate.(*followCoinSnapshotGate)
}

func (p *FollowCoinProvider) UseAuthoritativeSnapshotHotPath() bool { return true }

// SuggestedSourcePollDelay 平台 5～10 分钟才刷新一次持仓快照；15 秒轮询足以
// 在新快照出现后及时发现，又对平台未公开的限频足够温和。
func (p *FollowCoinProvider) SuggestedSourcePollDelay(bool) time.Duration { return 15 * time.Second }

// GetFills 不参与热路径：信号完全由持仓快照差分产生。
func (p *FollowCoinProvider) GetFills(string, time.Time) ([]Fill, error) { return nil, nil }

func (p *FollowCoinProvider) GetAccountState(apiID string) (*AccountState, error) {
	return p.GetPositionSnapshot(apiID)
}

func (p *FollowCoinProvider) GetPositionSnapshot(apiID string) (*AccountState, error) {
	if _, err := ParseFollowCoinAPIID(apiID); err != nil {
		return nil, err
	}
	g := p.snapshotGate(apiID)
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.until) {
		return nil, fmt.Errorf("FollowCoin source backoff until %s", g.until.UTC().Format(time.RFC3339))
	}
	if g.state != nil && time.Since(g.fetchedAt) < followCoinSnapshotCacheTTL {
		return cloneLeaderSnapshot(g.state), nil
	}
	state, err := p.readPositionSnapshot(apiID)
	if err != nil {
		var httpErr *FollowCoinHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusTooManyRequests {
			g.failures++
			backoff := smartMoneyFallbackBackoff(g.failures)
			if seconds, parseErr := strconv.Atoi(httpErr.RetryAfter); parseErr == nil && seconds > 0 {
				backoff = time.Duration(seconds) * time.Second
			}
			g.until = time.Now().Add(backoff)
		}
		return nil, err
	}
	g.failures = 0

	// 权益：overview.totalEquity 是不含浮盈亏的钱包余额，叠加当前快照的
	// 未实现盈亏后与 App"账户资产"、Binance marginBalance 同口径。拿不到
	// 有效值时 TotalEquity=0：开/加仓在比例计算处 fail-closed 并等待重试，
	// 减/平仓不依赖权益、照常执行。
	if time.Since(g.equityAt) >= followCoinEquityCacheTTL {
		if wallet, eqErr := p.readWalletEquity(apiID); eqErr == nil {
			g.walletEquity, g.equityAt, g.equityCheckErr = wallet, time.Now(), nil
		} else {
			g.equityCheckErr = eqErr
			if errors.Is(eqErr, ErrFollowCoinTokenInvalid) {
				return nil, eqErr
			}
		}
	}
	if g.walletEquity > 0 && time.Since(g.equityAt) < 10*time.Minute {
		upl := 0.0
		for _, pos := range state.Positions {
			upl += pos.UnrealizedPnL
		}
		if equity := g.walletEquity + upl; equity > 0 {
			state.TotalEquity = equity
			state.AvailableBalance = g.walletEquity
		}
	}
	g.state, g.fetchedAt = state, time.Now()
	return cloneLeaderSnapshot(state), nil
}

type followCoinPositionRaw struct {
	Symbol        string   `json:"symbol"`
	InstID        string   `json:"instId"`
	InstType      string   `json:"instType"`
	HoldSide      string   `json:"holdSide"`
	Total         *float64 `json:"total"`
	Leverage      float64  `json:"leverage"`
	OpenPriceAvg  float64  `json:"openPriceAvg"`
	MarketPrice   float64  `json:"marketPrice"`
	MarginMode    string   `json:"marginMode"`
	MarginCoin    string   `json:"marginCoin"`
	UnrealizedPL  float64  `json:"unrealizedPL"`
	CTime         int64    `json:"ctime"`
	UTime         int64    `json:"utime"`
	ExchangeID    int      `json:"exchangeId"`
	MarginSize    float64  `json:"marginSize"`
	LiquidationPx float64  `json:"liquidationPrice"`
}

func finitePositive(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }

// readPositionSnapshot fail-closed：任何残缺数据都返回错误，绝不当作空仓。
func (p *FollowCoinProvider) readPositionSnapshot(apiID string) (*AccountState, error) {
	var raw []followCoinPositionRaw
	q := neturl.Values{"apiId": {apiID}, "loading": {"false"}}
	if err := p.request(http.MethodGet, followCoinPositionsPath, q, nil, &raw); err != nil {
		return nil, err
	}
	state := &AccountState{Positions: make(map[string]*Position), Timestamp: time.Now()}
	var snapshotMS int64
	for _, r := range raw {
		if r.InstType != "" && !strings.EqualFold(r.InstType, "SWAP") {
			continue // 只跟永续合约
		}
		if strings.TrimSpace(r.InstID) == "" || r.Total == nil {
			return nil, fmt.Errorf("FollowCoin snapshot position is incomplete")
		}
		size := *r.Total
		if math.IsNaN(size) || math.IsInf(size, 0) || size < 0 {
			return nil, fmt.Errorf("invalid FollowCoin position size for %s", r.InstID)
		}
		if size == 0 {
			continue
		}
		var side SideType
		switch strings.ToLower(strings.TrimSpace(r.HoldSide)) {
		case "long":
			side = SideLong
		case "short":
			side = SideShort
		default:
			return nil, fmt.Errorf("invalid FollowCoin position side %q for %s", r.HoldSide, r.InstID)
		}
		marginMode := strings.ToLower(strings.TrimSpace(r.MarginMode))
		if marginMode == "" {
			marginMode = "cross"
		}
		posID := fmt.Sprintf("fc_%s_%s_%s", apiID, strings.ToUpper(r.InstID), side)
		pos := &Position{
			Symbol:        normalizeOKXSymbol(r.InstID),
			Side:          side,
			Size:          size,
			EntryPrice:    r.OpenPriceAvg,
			Leverage:      int(math.Round(r.Leverage)),
			MarginMode:    marginMode,
			UnrealizedPnL: r.UnrealizedPL,
			PosID:         posID,
			OpenedMS:      r.CTime,
			ValueCurrency: "USD",
		}
		// total 已统一为币数量（币安与 OKX 来源均已验证），USDT 本位下
		// 名义价值 = 数量 × 标记价。标记价缺失时价值无效，开/加仓会被
		// blockInvalidSourceRiskIncrease 拦截。
		if finitePositive(r.MarketPrice) {
			pos.MarkPrice = r.MarketPrice
			pos.PositionValue = size * r.MarketPrice
			pos.RawPositionValue = pos.PositionValue
			pos.ValueUSDValid = true
		} else {
			pos.ValueError = "FollowCoin position mark price is unavailable"
		}
		state.Positions[posID] = pos
		if r.UTime > snapshotMS {
			snapshotMS = r.UTime
		}
	}
	if snapshotMS > 0 {
		state.Timestamp = time.UnixMilli(snapshotMS)
	}
	return state, nil
}

func (p *FollowCoinProvider) readWalletEquity(apiID string) (float64, error) {
	var raw struct {
		TotalEquity *float64 `json:"totalEquity"`
	}
	q := neturl.Values{"apiId": {apiID}, "period": {"7d"}, "type": {"1"}, "loading": {"false"}}
	if err := p.request(http.MethodGet, followCoinOverviewPath, q, nil, &raw); err != nil {
		return 0, err
	}
	if raw.TotalEquity == nil || !finitePositive(*raw.TotalEquity) {
		return 0, fmt.Errorf("FollowCoin account equity is unavailable")
	}
	return *raw.TotalEquity, nil
}
