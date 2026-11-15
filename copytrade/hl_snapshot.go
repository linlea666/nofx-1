package copytrade

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ============================================================================
// Hyperliquid 快照权威热路径（对齐 okx_snapshot.go 的门闸/能力接口模式）
//
// HL 领航员信号完全由 clearinghouseState 持仓快照差分驱动：
// poll() 跳过 userFills 原始成交，改为每轮对比快照与本地 mapping 的
// last_known_size，合成 open/add/reduce/close 最终状态变化。
// ============================================================================

// HLSourceHTTPError Hyperliquid info API 非 200 响应错误（携带限频信息）
type HLSourceHTTPError struct {
	Status              int
	RetryAfter, Message string
}

func (e *HLSourceHTTPError) Error() string {
	return fmt.Sprintf("Hyperliquid source HTTP %d: %s", e.Status, e.Message)
}

type hlSnapshotGate struct {
	mu    sync.Mutex
	state *AccountState
	// fetchedAt 使用本地时钟判定缓存新鲜度。state.Timestamp 携带交易所
	// 时钟（用于 SnapshotMS），时钟漂移时不能用它做缓存判定。
	fetchedAt time.Time
	until     time.Time
	failures  int
}

var hlSnapshotGates sync.Map

func (p *HyperliquidProvider) snapshotGate(leader string) *hlSnapshotGate {
	// The transport component isolates tests/custom endpoints while default
	// production providers for the same public leader share one request stream.
	key := fmt.Sprintf("%p|%s", p.client.Transport, leader)
	gate, _ := hlSnapshotGates.LoadOrStore(key, &hlSnapshotGate{})
	return gate.(*hlSnapshotGate)
}

func (p *HyperliquidProvider) UseAuthoritativeSnapshotHotPath() bool { return true }

// SuggestedSourcePollDelay clearinghouseState 权重为 2（约 1200 权重/分/IP），
// 活跃 2s = 60 权重/分，为同 IP 多 trader 留足余量；空闲 5s 兜底。
func (p *HyperliquidProvider) SuggestedSourcePollDelay(active bool) time.Duration {
	if active {
		return 2 * time.Second
	}
	return 5 * time.Second
}

func (p *HyperliquidProvider) GetPositionSnapshot(leader string) (*AccountState, error) {
	g := p.snapshotGate(leader)
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Now().Before(g.until) {
		return nil, fmt.Errorf("Hyperliquid source backoff until %s", g.until.UTC().Format(time.RFC3339))
	}
	if g.state != nil && time.Since(g.fetchedAt) < 500*time.Millisecond {
		return cloneLeaderSnapshot(g.state), nil
	}
	state, err := p.readClearinghouseSnapshot(leader)
	if err != nil {
		var httpErr *HLSourceHTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusTooManyRequests {
			g.failures++
			backoff := smartMoneyFallbackBackoff(g.failures)
			if seconds, parseErr := strconv.Atoi(httpErr.RetryAfter); parseErr == nil && seconds > 0 {
				backoff = time.Duration(seconds) * time.Second
			} else if date, parseErr := http.ParseTime(httpErr.RetryAfter); parseErr == nil && time.Until(date) > 0 {
				backoff = time.Until(date)
			}
			g.until = time.Now().Add(backoff)
		}
		return nil, err
	}
	g.state = state
	g.fetchedAt = time.Now()
	g.failures = 0
	return cloneLeaderSnapshot(state), nil
}

// readClearinghouseSnapshot 读取并解析 clearinghouseState 为 AccountState。
// GetAccountState 与 GetPositionSnapshot 共用，保证 InitIgnoredPositions 与
// 轮询差分看到完全一致的 PosID/MarkPrice。
//
// 失败即错（fail-closed）：非 200、JSON 解析失败、Time 缺失、szi 非法、
// 有持仓但权益非法时必须返回 error——绝不能把残缺响应当成权威空仓快照，
// 否则差分会对不存在的"空仓"合成全部平仓信号。
func (p *HyperliquidProvider) readClearinghouseSnapshot(leaderID string) (*AccountState, error) {
	req := map[string]string{
		"type": "clearinghouseState",
		"user": leaderID,
	}

	var raw HLClearinghouseState
	if err := p.postWithStatus(req, &raw); err != nil {
		return nil, fmt.Errorf("get account state failed: %w", err)
	}
	if raw.Time == 0 {
		return nil, fmt.Errorf("Hyperliquid clearinghouse snapshot is incomplete (time missing)")
	}

	state := &AccountState{
		AvailableBalance: parseFloat(raw.Withdrawable),
		Positions:        make(map[string]*Position),
		Timestamp:        time.UnixMilli(raw.Time),
	}

	for _, ap := range raw.AssetPositions {
		pos := ap.Position
		symbol := normalizeSymbol(pos.Coin)

		size, parseErr := strconv.ParseFloat(pos.Szi, 64)
		if parseErr != nil || math.IsNaN(size) || math.IsInf(size, 0) {
			return nil, fmt.Errorf("invalid Hyperliquid source position size for %s", pos.Coin)
		}
		side := SideLong
		if size < 0 {
			side = SideShort
			size = -size
		}
		if size == 0 {
			continue // 跳过空仓位
		}

		marginMode := pos.Leverage.Type // "cross" | "isolated"
		if marginMode == "" {
			marginMode = "cross"
		}

		// HL 同一币种只有一个方向的仓位，symbol_side 即规范仓位标识。
		// 必须与历史 DB 行（InitIgnoredPositions / matchOpenAddSignal 回退）
		// 字节一致，严禁改成其他格式，否则会孤儿化全部既有映射。
		key := PositionKey(symbol, side)
		position := &Position{
			Symbol:        symbol,
			Side:          side,
			Size:          size,
			EntryPrice:    parseFloat(pos.EntryPx),
			Leverage:      pos.Leverage.Value,
			MarginMode:    marginMode,
			UnrealizedPnL: parseFloat(pos.UnrealizedPnl),
			PositionValue: parseFloat(pos.PositionValue),
			PosID:         key,
		}
		// 快照不含 markPx；用权威名义价值反推标记价，使合成信号定价在
		// mark 而不是过时的 entryPx（positionSignalPrice 的回退值）。
		if position.PositionValue > 0 && position.Size > 0 {
			position.MarkPrice = position.PositionValue / position.Size
		}
		state.Positions[key] = position
	}

	equity, equityErr := strconv.ParseFloat(raw.MarginSummary.AccountValue, 64)
	equityValid := equityErr == nil && !math.IsNaN(equity) && !math.IsInf(equity, 0) && equity > 0
	if equityValid {
		state.TotalEquity = equity
	} else if len(state.Positions) > 0 {
		// 有持仓必须有可用权益：它是比例跟单的分母，残缺值会放大跟单金额。
		return nil, fmt.Errorf("Hyperliquid account equity is invalid (%q)", raw.MarginSummary.AccountValue)
	}

	return state, nil
}

// postWithStatus 与 post 行为一致，但非 200 时返回携带状态码与
// Retry-After 的 *HLSourceHTTPError，供快照门闸做限频退避。
func (p *HyperliquidProvider) postWithStatus(req interface{}, result interface{}) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	resp, err := p.client.Post(HLInfoAPI, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HLSourceHTTPError{
			Status:     resp.StatusCode,
			RetryAfter: resp.Header.Get("Retry-After"),
			Message:    string(bodyBytes),
		}
	}

	return json.NewDecoder(resp.Body).Decode(result)
}
