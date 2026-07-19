package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func sqlNullString(v string) sql.NullString {
	return sql.NullString{String: v, Valid: true}
}

func TestCopyTradeSourceHealthStateMachine(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cs := st.CopyTrade()
	base := time.Now().UTC().Truncate(time.Second)
	record := func(obs SourceHealthObservation) *CopyTradeSourceHealth {
		h, _, err := cs.RecordSourceHealthObservation("trader", "leader", "smart_money", 1, obs)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := record(SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: base})
	if h.Status != SourceHealthHealthy || h.LastCompleteSnapshotAt == nil {
		t.Fatalf("initial healthy: %+v", h)
	}
	for i := 1; i <= 2; i++ {
		h = record(SourceHealthObservation{Status: "ERROR", Error: "temporary", CheckedAt: base.Add(time.Duration(i) * time.Second)})
		if h.Status != SourceHealthHealthy {
			t.Fatalf("failure %d should retain healthy, got %s", i, h.Status)
		}
	}
	h = record(SourceHealthObservation{Status: "ERROR", Error: "temporary", CheckedAt: base.Add(3 * time.Second)})
	if h.Status != SourceHealthDegraded || h.ConsecutiveFailures != 3 {
		t.Fatalf("third failure: %+v", h)
	}
	h = record(SourceHealthObservation{Status: SourceHealthPrivate, Error: "hidden", CheckedAt: base.Add(4 * time.Second)})
	if h.Status != SourceHealthPrivate || !h.Frozen() {
		t.Fatalf("private must freeze: %+v", h)
	}
	for i := 5; i <= 8; i++ {
		h = record(SourceHealthObservation{Status: "ERROR", Error: "network after private", CheckedAt: base.Add(time.Duration(i) * time.Second)})
		if h.Status != SourceHealthPrivate || !h.Frozen() {
			t.Fatalf("private must remain sticky through transient failure %d: %+v", i, h)
		}
	}
	h = record(SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: false, CheckedAt: base.Add(9 * time.Second)})
	if h.Status == SourceHealthHealthy {
		t.Fatalf("incomplete snapshot must not recover: %+v", h)
	}
	h = record(SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: base.Add(10 * time.Second)})
	if h.Status != SourceHealthHealthy || h.ConsecutiveFailures != 0 || h.LastError != "" {
		t.Fatalf("complete recovery: %+v", h)
	}
	h = record(SourceHealthObservation{Status: "ERROR", Error: "old snapshot", CheckedAt: base.Add(71 * time.Second)})
	if h.Status != SourceHealthStale || !h.Frozen() {
		t.Fatalf("stale must freeze: %+v", h)
	}
}

// TestCopyTradeSourceHealthBareTimeNowRoundTrip 复现 GLWUSDT 漏跟单根因：
// 生产路径写入的是裸 time.Now()（带单调时钟），旧实现落库为 Go String()
// 格式导致读回 LastCompleteSnapshotAt 恒为 nil，Smart Money 每轮都误判
// "断供恢复"并吸收新开仓。此测试要求裸 time.Now() 写入后跨连接读回非 nil。
func TestCopyTradeSourceHealthBareTimeNowRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-monotonic.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Now() // 故意保留单调时钟，与生产 recordSmartMoneySourceHealth 一致
	if _, _, err := st.CopyTrade().RecordSourceHealthObservation("trader", "leader", "smart_money", 1, SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: checked}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err := st.CopyTrade().GetSourceHealth("trader")
	if err != nil {
		t.Fatal(err)
	}
	if h == nil || h.LastCompleteSnapshotAt == nil || h.LastCheckedAt == nil || h.LastTransitionAt == nil {
		t.Fatalf("bare time.Now() must round-trip to non-nil times: %+v", h)
	}
	if diff := h.LastCompleteSnapshotAt.Sub(checked); diff > time.Millisecond || diff < -time.Millisecond {
		t.Fatalf("round-trip drift %v, stored=%v checked=%v", diff, h.LastCompleteSnapshotAt, checked)
	}
	// 核心回归断言：恢复判定所依赖的时间差计算必须可用
	if gap := checked.Add(3 * time.Second).Sub(*h.LastCompleteSnapshotAt); gap > time.Minute {
		t.Fatalf("3s poll gap must not look like a recovery window: %v", gap)
	}
}

// TestParseOptionalSQLiteTimeLegacyMonotonicSuffix 保证服务器上已损坏的
// 历史行（Go String() 格式 + m=+ 单调后缀）无需修库即可解析自愈。
func TestParseOptionalSQLiteTimeLegacyMonotonicSuffix(t *testing.T) {
	legacy := "2026-07-19 12:04:25.386330096 +0800 CST m=+77990.894116101"
	parsed := parseOptionalSQLiteTime(sqlNullString(legacy))
	if parsed == nil {
		t.Fatalf("legacy monotonic-suffixed timestamp must parse: %q", legacy)
	}
	want := time.Date(2026, 7, 19, 4, 4, 25, 386330096, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("parsed %v, want %v", parsed, want)
	}
	if parseOptionalSQLiteTime(sqlNullString("2026-07-19 12:04:25.386330096 +0800 CST")) == nil {
		t.Fatal("legacy format without monotonic suffix must parse")
	}
}

func TestCopyTradeSourceHealthGenerationAndNotificationPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health-restart.db")
	st, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	checked := time.Now().UTC().Truncate(time.Second)
	h, _, err := st.CopyTrade().RecordSourceHealthObservation("trader", "leader-a", "smart_money", 1, SourceHealthObservation{Status: SourceHealthPrivate, Error: "hidden", CheckedAt: checked})
	if err != nil {
		t.Fatal(err)
	}
	notified := checked.Add(time.Minute)
	if err := st.CopyTrade().MarkSourceHealthNotified("trader", notified); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h, err = st.CopyTrade().GetSourceHealth("trader")
	if err != nil || h.LastNotifiedAt == nil || h.LastNotifiedAt.Unix() != notified.Unix() {
		t.Fatalf("restart persistence: %+v err=%v", h, err)
	}
	h, transitioned, err := st.CopyTrade().RecordSourceHealthObservation("trader", "leader-b", "smart_money", 2, SourceHealthObservation{Status: SourceHealthHealthy, CompleteSnapshot: true, CheckedAt: checked.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if transitioned || h.SourceGeneration != 2 || h.LeaderID != "leader-b" || h.LastNotifiedAt != nil {
		t.Fatalf("generation must reset state: %+v transitioned=%v", h, transitioned)
	}
}
