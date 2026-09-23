package copytrade

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOKXSnapshotHotPathIsSharedAndNeverWaitsForAssetsOrHistory(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "position-current") {
			t.Errorf("slow endpoint on hot path: %s", r.URL.Path)
		}
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"code":"0","data":[{"posData":[{"posId":"reused","instId":"ZEC-USDT-SWAP","posSide":"short","pos":"710","avgPx":"1621","mgnMode":"isolated","cTime":"1790148257851"}]}]}`))}, nil
	})
	p := &OKXProvider{client: &http.Client{Transport: transport}}
	p2 := &OKXProvider{client: &http.Client{Transport: transport}}
	var wg sync.WaitGroup
	for _, provider := range []*OKXProvider{p, p2} {
		wg.Add(1)
		go func(p *OKXProvider) {
			defer wg.Done()
			s, err := p.GetPositionSnapshot(t.Name())
			if err != nil {
				t.Error(err)
				return
			}
			if s.Positions["reused"].OpenedMS != 1790148257851 {
				t.Error("source lifecycle time missing")
			}
			s.Positions["reused"].Size = 1
		}(provider)
	}
	wg.Wait()
	s, err := p.GetPositionSnapshot(t.Name())
	if err != nil || calls.Load() != 1 || s.Positions["reused"].Size != 710 {
		t.Fatalf("requests not merged or shared snapshot mutated: %d %+v %v", calls.Load(), s, err)
	}
	if p.SuggestedSourcePollDelay(true) != time.Second || p.SuggestedSourcePollDelay(false) != 3*time.Second {
		t.Fatal("wrong poll schedule")
	}
}
func TestOKXInvalidSnapshotAndRateLimitAreNotFlatPositions(t *testing.T) {
	for n, body := range []string{`{"code":"0"}`, `{"code":"50011","data":[]}`, `{"code":"0","data":[{"posData":[{"pos":"bad"}]}]}`, `{"code":"0","data":[{}]}`} {
		p := &OKXProvider{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}}
		if _, err := p.GetPositionSnapshot(fmt.Sprintf("%s-%d", t.Name(), n)); err == nil {
			t.Fatalf("invalid body became flat: %s", body)
		}
	}
	calls := 0
	p := &OKXProvider{client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	for i := 0; i < 2; i++ {
		if _, err := p.GetPositionSnapshot(t.Name()); err == nil {
			t.Fatal("rate limit became flat")
		}
	}
	if calls != 1 {
		t.Fatal("Retry-After ignored")
	}
}
