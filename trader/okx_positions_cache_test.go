package trader

import (
	"strings"
	"sync/atomic"
	"testing"
)

// 空仓账户的持仓结果（空切片）必须可缓存。
//
// 回归背景（OKX 50011 跟单执行失败根因之一）：旧实现用
// `cachedPositions != nil` 判断缓存有效，空仓时结果为 nil 切片 →
// 缓存永远不生效，空仓账户每次 GetPositions 都打真实 API；跟随者
// 大部分时间空仓等信号、开仓瞬间必然空仓，调用量叠加触发限流。
func TestOKXGetPositionsCachesEmptyResult(t *testing.T) {
	var apiCalls int64
	tr := newOKXTestServer(t, func(pathWithQuery string) (int, string) {
		if strings.HasPrefix(pathWithQuery, "/api/v5/account/positions") {
			atomic.AddInt64(&apiCalls, 1)
			return 200, `{"code":"0","msg":"","data":[]}`
		}
		return 200, `{"code":"0","msg":"","data":[]}`
	})

	for i := 0; i < 3; i++ {
		positions, err := tr.GetPositions()
		if err != nil {
			t.Fatalf("GetPositions #%d: %v", i+1, err)
		}
		if len(positions) != 0 {
			t.Fatalf("GetPositions #%d: expected empty positions, got %d", i+1, len(positions))
		}
	}
	if got := atomic.LoadInt64(&apiCalls); got != 1 {
		t.Fatalf("empty positions must be cached: expected 1 API call, got %d", got)
	}
}

// invalidatePositionsCache 后（如下单变更 / GetPositionsFresh）必须重新拉取。
func TestOKXGetPositionsFreshBypassesEmptyCache(t *testing.T) {
	var apiCalls int64
	tr := newOKXTestServer(t, func(pathWithQuery string) (int, string) {
		if strings.HasPrefix(pathWithQuery, "/api/v5/account/positions") {
			atomic.AddInt64(&apiCalls, 1)
			return 200, `{"code":"0","msg":"","data":[]}`
		}
		return 200, `{"code":"0","msg":"","data":[]}`
	})

	if _, err := tr.GetPositions(); err != nil {
		t.Fatalf("GetPositions: %v", err)
	}
	if _, err := tr.GetPositionsFresh(); err != nil {
		t.Fatalf("GetPositionsFresh: %v", err)
	}
	if got := atomic.LoadInt64(&apiCalls); got != 2 {
		t.Fatalf("GetPositionsFresh must bypass cache: expected 2 API calls, got %d", got)
	}
}
