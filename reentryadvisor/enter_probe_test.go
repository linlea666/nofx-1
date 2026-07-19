package reentryadvisor

// TestEnterProbe 是一次性的"AI 能不能进"验证探针（默认跳过）。
// 它复用生产同款的 system prompt、模型客户端与严格解析契约，用线上真实
// 落库的 datapack 直接向真实模型发问，回答两个问题：
//   1) 回放：真实历史快照（含事后被证实反转的周期187）此刻模型会不会 ENTER
//   2) 理想：把真实 datapack 改造成"教科书级反转"后，模型能否吐出 ENTER
//
// 运行（在项目根目录，DB 用副本避免碰线上库）：
//   REENTRY_ENTER_PROBE=1 PROBE_DB=/tmp/probe.db \
//   PROBE_REPLAY_IDS=6,7,92,98,87,95 PROBE_MUTATE_IDS=6,92 \
//   /usr/local/btgojdk/go1.25.0/bin/go test ./reentryadvisor -run EnterProbe -v -count=1 -timeout 20m

import (
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"nofx/crypto"
	"nofx/store"
)

func probeIDs(env string) []int64 {
	var out []int64
	for _, part := range strings.Split(os.Getenv(env), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if v, err := strconv.ParseInt(part, 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func TestEnterProbe(t *testing.T) {
	if os.Getenv("REENTRY_ENTER_PROBE") == "" {
		t.Skip("set REENTRY_ENTER_PROBE=1 to run the ENTER-capability probe")
	}
	_ = godotenv.Load("../.env")
	_ = godotenv.Load(".env")

	dbPath := os.Getenv("PROBE_DB")
	if dbPath == "" {
		t.Fatal("PROBE_DB is required (use a COPY of data.db)")
	}

	cs, err := crypto.NewCryptoService()
	if err != nil {
		t.Fatalf("crypto init failed (need DATA_ENCRYPTION_KEY + RSA_PRIVATE_KEY in .env): %v", err)
	}
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store open failed: %v", err)
	}
	defer st.Close()
	st.SetCryptoFuncs(
		func(p string) string {
			if p == "" {
				return p
			}
			e, err := cs.EncryptForStorage(p)
			if err != nil {
				return p
			}
			return e
		},
		func(e string) string {
			if e == "" || !cs.IsEncryptedStorageValue(e) {
				return e
			}
			d, err := cs.DecryptFromStorage(e)
			if err != nil {
				return e
			}
			return d
		},
	)

	cfg, err := st.ReentryAI().GetReentryAIConfig()
	if err != nil {
		t.Fatalf("read reentry_ai_config failed: %v", err)
	}
	model, err := resolveAIModel(st, cfg)
	if err != nil {
		t.Fatalf("resolve model failed: %v", err)
	}
	client, err := newAIClientForModel(model, 180*time.Second)
	if err != nil {
		t.Fatalf("create client failed: %v", err)
	}
	sysPrompt := candidateSystemPrompt(cfg.AnalysisFocus)
	t.Logf("MODEL provider=%s id=%s custom=%s", model.Provider, model.ID, model.CustomModelName)

	call := func(label, system, user string) {
		raw, err := client.CallWithMessages(system, user)
		if err != nil {
			t.Logf("[%s] AI call error: %v", label, err)
			return
		}
		pv, perr := parseAICandidateVerdict(raw)
		if perr != nil {
			t.Logf("[%s] parse failed: %v | RAW=%s", label, perr, probeTruncate(raw, 600))
			return
		}
		t.Logf("[%s] VERDICT=%s regime=%s confidence=%.2f size_factor=%.2f", label, pv.Verdict, pv.Regime, pv.Confidence, pv.SizeFactor)
		t.Logf("[%s] REASONS=%s", label, probeTruncate(pv.ReasonsJSON, 900))
	}

	// ---- Test A: faithful replay of stored prompts ----
	for _, id := range probeIDs("PROBE_REPLAY_IDS") {
		a, err := st.ReentryAI().GetReentryAnalysis(id)
		if err != nil {
			t.Logf("[replay #%d] load failed: %v", id, err)
			continue
		}
		t.Logf("[replay #%d] %s %s cycle=%d stored_verdict=%s stored_conf=%.2f", id, a.Symbol, a.Side, a.CycleID, a.Verdict, a.Confidence)
		call("replay #"+strconv.FormatInt(id, 10), a.SystemPrompt, a.UserPrompt)
	}

	// ---- Test C: run the CURRENT (new) prompt against the real, UNMUTATED
	// stored datapack. This is a clean A/B: only the system prompt differs
	// from the stored replay in Test A, so any verdict change is attributable
	// to the prompt change alone.
	for _, id := range probeIDs("PROBE_NEWPROMPT_IDS") {
		a, err := st.ReentryAI().GetReentryAnalysis(id)
		if err != nil {
			t.Logf("[newprompt #%d] load failed: %v", id, err)
			continue
		}
		var dp DataPack
		if err := json.Unmarshal([]byte(a.DatapackJSON), &dp); err != nil {
			t.Logf("[newprompt #%d] datapack unmarshal failed: %v", id, err)
			continue
		}
		c := &store.CopyGuardReentryCandidate{
			ID: a.CandidateID, CycleID: a.CycleID, Symbol: dp.CopyGuard.Symbol, Side: dp.CopyGuard.Side,
			TriggerPrice: dp.CopyGuard.TriggerPrice, MaxNotional: dp.CopyGuard.RecommendedNotional,
			StopCount: dp.CopyGuard.StopCount, ReentryCount: dp.CopyGuard.AutoReentryCount,
		}
		user := buildCandidateUserPrompt(c, a.DatapackJSON)
		t.Logf("[newprompt #%d] %s %s (real datapack, NEW prompt) stored_verdict=%s", id, dp.CopyGuard.Symbol, dp.CopyGuard.Side, a.Verdict)
		call("newprompt #"+strconv.FormatInt(id, 10), sysPrompt, user)
	}

	// ---- Test B: mutate a real datapack into a textbook reversal ----
	for _, id := range probeIDs("PROBE_MUTATE_IDS") {
		a, err := st.ReentryAI().GetReentryAnalysis(id)
		if err != nil {
			t.Logf("[ideal #%d] load failed: %v", id, err)
			continue
		}
		var dp DataPack
		if err := json.Unmarshal([]byte(a.DatapackJSON), &dp); err != nil {
			t.Logf("[ideal #%d] datapack unmarshal failed: %v", id, err)
			continue
		}
		strengthenReversal(&dp)
		b, err := json.Marshal(&dp)
		if err != nil {
			t.Logf("[ideal #%d] marshal failed: %v", id, err)
			continue
		}
		c := &store.CopyGuardReentryCandidate{
			ID: a.CandidateID, CycleID: a.CycleID, Symbol: dp.CopyGuard.Symbol, Side: dp.CopyGuard.Side,
			TriggerPrice: dp.CopyGuard.TriggerPrice, MaxNotional: dp.CopyGuard.RecommendedNotional,
			StopCount: dp.CopyGuard.StopCount, ReentryCount: dp.CopyGuard.AutoReentryCount,
		}
		user := buildCandidateUserPrompt(c, string(b))
		t.Logf("[ideal #%d] %s %s (textbook reversal) base_stored_verdict=%s", id, dp.CopyGuard.Symbol, dp.CopyGuard.Side, a.Verdict)
		call("ideal #"+strconv.FormatInt(id, 10), sysPrompt, user)
	}
}

// strengthenReversal 把一个真实 datapack 改造成"教科书级、朝重入方向的确认反转"。
// 只调整方向相关证据，使其内部一致、无数据缺失、无背离，用来探测模型输出
// ENTER 的能力上限。long → 看涨反转确认；short → 看跌反转确认。
func strengthenReversal(dp *DataPack) {
	long := strings.EqualFold(dp.CopyGuard.Side, "long")
	sign := 1.0
	if !long {
		sign = -1.0
	}

	dp.Meta.SnapshotAt = time.Now().UTC().Format(time.RFC3339)
	dp.Meta.MissingFields = nil
	dp.Meta.Note = ""

	// 仓位层：噪声扫损、可保护、领航员仍在且加仓、未追远
	dp.CopyGuard.Protectable = true
	dp.CopyGuard.DistanceATRRatio = 0.12
	dp.CopyGuard.ATRExpansionPct = 5
	dp.CopyGuard.ProtectionStatus = "ACTIVE"
	dp.CopyGuard.ProtectionCoverage = 1
	dp.CopyGuard.ProtectionError = ""
	dp.CopyGuard.PreviousAIDecisions = nil
	dp.CopyGuard.Leader.StillHolding = true
	if dp.CopyGuard.Leader.Size <= 0 {
		dp.CopyGuard.Leader.Size = 1000
	}
	dp.CopyGuard.Leader.SizeVsCycleBaseline = 1.25

	// 价格/止损/领航员成本必须与"已沿方向温和收复"这一叙事完全自洽，否则模型
	// 会交叉核对原始 current_price / stop_price / leader_timeline 后识破矛盾。
	atr := dp.CopyGuard.GateATR
	P := 0.0
	if dp.Market != nil && dp.Market.CurrentPrice > 0 {
		P = dp.Market.CurrentPrice
	} else if dp.CopyGuard.TriggerPrice > 0 {
		P = dp.CopyGuard.TriggerPrice
	}
	if atr <= 0 && P > 0 {
		atr = P * 0.01
	}
	stopPrice := P - sign*0.4*atr  // 现价已沿方向越过止损簇 0.4 ATR
	entryPrice := P - sign*0.2*atr // 领航员成本，现价小幅浮盈
	if P > 0 {
		dp.CopyGuard.TriggerPrice = P
		dp.CopyGuard.ReentryBoundary = P - sign*0.6*atr
		dp.CopyGuard.ChaseLimit = P + sign*1.0*atr
		dp.CopyGuard.Leader.EntryPrice = entryPrice
		dp.CopyGuard.Leader.EntryAtCycleOpen = entryPrice
		if entryPrice > 0 {
			pnl := (P - entryPrice) / entryPrice * 100
			dp.CopyGuard.Leader.UnrealizedPnLPct = sign * pnl
		}
		for _, a := range dp.CopyGuard.Attempts {
			a.EntryPrice = entryPrice
			if a.Status == "STOPPED" {
				a.ExitPrice = stopPrice
			}
		}
	}
	if dp.CopyGuard.LastStop != nil {
		dp.CopyGuard.LastStop.Price = stopPrice
		dp.CopyGuard.LastStop.DistanceFromCurrentATR = 0.4 // 已沿方向收复但未追远
		dp.CopyGuard.LastStop.StopClusterSpreadATR = 0.15
	}

	if dp.Market == nil {
		return
	}
	m := dp.Market

	// 领航员时间线：下探后收复至现价附近，成本设为 entryPrice
	if P > 0 {
		n := len(dp.CopyGuard.LeaderTimeline)
		for i := range dp.CopyGuard.LeaderTimeline {
			frac := 1.0
			if n > 1 {
				frac = float64(i) / float64(n-1)
			}
			trough := P - sign*0.8*atr
			dp.CopyGuard.LeaderTimeline[i].MarkPrice = trough + (P-trough)*frac
			dp.CopyGuard.LeaderTimeline[i].EntryPrice = entryPrice
			if atr > 0 {
				dp.CopyGuard.LeaderTimeline[i].ATR = atr
			}
		}
	}

	for _, k := range m.Klines {
		if k == nil {
			continue
		}
		k.PctChange = sign * 0.8 // 温和沿方向恢复，非追高追空
		k.VolumeRatio520 = 1.5
		rewriteBarsTrend(k, sign)
	}
	for _, cvd := range m.ContractCVD {
		if cvd == nil {
			continue
		}
		cvd.SlopeSign = slopeFor(long)
		cvd.Last = sign * 5_000_000
		cvd.Divergence = ""
		rewriteCVDSeries(cvd, sign)
	}
	for _, cvd := range m.SpotCVD {
		if cvd == nil {
			continue
		}
		cvd.SlopeSign = slopeFor(long)
		cvd.Last = sign * 3_000_000
		cvd.Divergence = ""
		rewriteCVDSeries(cvd, sign)
	}
	m.SpotToContractVolumeRatio24h = 0.8

	if m.OpenInterest != nil {
		if m.OpenInterest.ChangePct == nil {
			m.OpenInterest.ChangePct = map[string]float64{}
		}
		m.OpenInterest.ChangePct["1h"] = 1.2
		m.OpenInterest.ChangePct["4h"] = 3.5
		m.OpenInterest.ChangePct["24h"] = 6.0
		if long {
			m.OpenInterest.PriceOIRead = "价格上涨+OI上涨：新增多头推动，趋势延续偏多"
		} else {
			m.OpenInterest.PriceOIRead = "价格下跌+OI上涨：新增空头推动，趋势延续偏空"
		}
	}
	if m.Funding != nil {
		m.Funding.CurrentRate = 0.00005
		m.Funding.State = "neutral"
		m.Funding.Percentile10d = 50
		m.Funding.Avg10d = 0.00006
		m.Funding.NextFundingMinutes = 220
	}
	if m.LongShort != nil {
		if long {
			m.LongShort.GlobalAccountsRatio = 0.9
			m.LongShort.TopPositionsRatio = 0.95
		} else {
			m.LongShort.GlobalAccountsRatio = 1.1
			m.LongShort.TopPositionsRatio = 1.05
		}
		m.LongShort.GlobalTrend24h = "flat"
	}
	if m.Basis != nil {
		m.Basis.BasisPct = 0.01
	}
	// 支撑/阻力：朝重入方向有空间，反向关口刚被拒绝/收复（价格与 ATR 距离同步）
	for _, sr := range m.SupportResistance {
		if sr == nil {
			continue
		}
		if long {
			sr.NearestSupport = P - 0.3*atr
			sr.SupportDistanceATR = 0.3
			sr.SupportTouches = 3
			sr.NearestResistance = P + 2.5*atr
			sr.ResistanceDistanceATR = 2.5
			sr.ResistanceTouches = 1
		} else {
			sr.NearestResistance = P + 0.3*atr
			sr.ResistanceDistanceATR = 0.3
			sr.ResistanceTouches = 3
			sr.NearestSupport = P - 2.5*atr
			sr.SupportDistanceATR = 2.5
			sr.SupportTouches = 1
		}
	}
}

// rewriteBarsTrend 把 K 线尾部重写成"先被扫损下探、再朝 favor 方向收复"的
// 典型二次入场形态（V/倒V），使原始 bars 与摘要字段内部一致且不呈现追高追空。
// 前 40% 逆 favor 下探（扫损），后 60% 收复至最新收盘锚点附近。
// bars 格式：[open_time_ms, open, high, low, close, volume]，旧→新。
func rewriteBarsTrend(k *KlineSummary, sign float64) {
	n := len(k.Bars)
	if n == 0 {
		return
	}
	last := k.Bars[n-1]
	if len(last) < 6 || last[4] <= 0 {
		return
	}
	anchor := last[4] // 以最新收盘为锚
	unit := anchor * 0.0015
	dipIdx := n * 2 / 5 // 低点位置
	if dipIdx < 1 {
		dipIdx = 1
	}
	troughOffset := -sign * unit * 4 // 相对锚点的低点偏移（逆 favor）
	closeAt := func(i int) float64 {
		if i <= dipIdx {
			// 下探段：从 anchor 线性走到 trough
			frac := float64(i) / float64(dipIdx)
			return anchor + troughOffset*frac
		}
		// 收复段：从 trough 线性回到 anchor
		frac := float64(i-dipIdx) / float64(n-1-dipIdx)
		return (anchor + troughOffset) + (-troughOffset)*frac
	}
	for i := 0; i < n; i++ {
		if len(k.Bars[i]) < 6 {
			continue
		}
		closeP := closeAt(i)
		openP := closeAt(i - 1)
		if i == 0 {
			openP = closeP - sign*unit*0.5
		}
		hi := math.Max(openP, closeP) + unit*0.3
		lo := math.Min(openP, closeP) - unit*0.3
		if lo <= 0 {
			lo = math.Min(openP, closeP) * 0.999
		}
		k.Bars[i][1] = openP
		k.Bars[i][2] = hi
		k.Bars[i][3] = lo
		k.Bars[i][4] = closeP
	}
}

// rewriteCVDSeries 把 CVD 尾部序列重写成朝 favor 方向单调、以 Last 收尾的斜坡。
func rewriteCVDSeries(cvd *CVDSummary, sign float64) {
	n := len(cvd.SeriesTail)
	if n == 0 {
		return
	}
	end := cvd.Last
	stepMag := math.Abs(end) / float64(n)
	for i := 0; i < n; i++ {
		idxFromEnd := float64(n - 1 - i)
		cvd.SeriesTail[i] = end - sign*stepMag*idxFromEnd
	}
}

func slopeFor(long bool) string {
	if long {
		return "rising"
	}
	return "falling"
}

func probeTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
