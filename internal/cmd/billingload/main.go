// Command billingload runs the repeatable PostgreSQL period-close workload.
package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	billing "github.com/data-insights-ai/rho-billing"
	"github.com/data-insights-ai/rho-billing/postgres"
	"github.com/data-insights-ai/rho-billing/usage"
)

type result struct {
	Rows            int              `json:"rows"`
	SourceGroups    int              `json:"source_groups"`
	ChunkSize       int              `json:"chunk_size"`
	Seed            int64            `json:"seed"`
	Schema          string           `json:"schema"`
	Account         string           `json:"account"`
	BatchID         string           `json:"batch_id"`
	Watermark       int64            `json:"watermark"`
	FinalState      usage.CloseState `json:"final_state"`
	LineCount       int64            `json:"line_count"`
	ClaimCount      int64            `json:"claim_count"`
	ClaimMismatches int64            `json:"claim_membership_mismatches"`
	Total           int64            `json:"total"`
	Processed       int64            `json:"processed"`
	ExcludedLate    bool             `json:"late_after_watermark_excluded"`
	TotalDuration   time.Duration    `json:"total_duration"`
	TotalSeconds    float64          `json:"total_seconds"`
	ChunkCount      int              `json:"chunk_count"`
	ChunkDurations  []time.Duration  `json:"chunk_durations"`
	ChunkP50        time.Duration    `json:"chunk_p50"`
	ChunkP95        time.Duration    `json:"chunk_p95"`
	ChunkP99        time.Duration    `json:"chunk_p99"`
	ChunkMax        time.Duration    `json:"chunk_max"`
	HeapAllocBytes  uint64           `json:"heap_alloc_bytes_peak"`
	HeapInuseBytes  uint64           `json:"heap_inuse_bytes_peak"`
	RSSBytes        *uint64          `json:"rss_bytes"`
	WALBytesDelta   *int64           `json:"wal_bytes_delta"`
	PGQueryPlan     json.RawMessage  `json:"pg_query_plan,omitempty"`
	PGClaimPlan     json.RawMessage  `json:"pg_claim_plan,omitempty"`
	Statements      []statementStats `json:"statement_stats,omitempty"`
	PGVersion       string           `json:"postgres_version"`
	GoVersion       string           `json:"go_version"`
	GitCommit       string           `json:"git_commit"`
	DirtyWorktree   bool             `json:"dirty_worktree"`
	BaselineCommit  string           `json:"baseline_commit"`
	HarnessHash     string           `json:"harness_hash"`
}

type statementStats struct {
	Fingerprint string          `json:"fingerprint"`
	Phase       string          `json:"phase"`
	Count       int64           `json:"count"`
	Total       time.Duration   `json:"total"`
	Max         time.Duration   `json:"max"`
	SlowSamples []time.Duration `json:"slow_samples,omitempty"`
}

type traceKey struct{}
type traceStart struct {
	Fingerprint string
	Phase       string
	Started     time.Time
}
type queryTracer struct {
	mu        sync.Mutex
	stats     map[string]*statementStats
	SlowLimit int
}

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	hash := sha256.Sum256([]byte(data.SQL))
	return context.WithValue(ctx, traceKey{}, traceStart{Fingerprint: fmt.Sprintf("sha256:%x", hash[:8]), Phase: queryPhase(data.SQL), Started: time.Now()})
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	start, ok := ctx.Value(traceKey{}).(traceStart)
	if !ok {
		return
	}
	d := time.Since(start.Started)
	t.mu.Lock()
	defer t.mu.Unlock()
	stat := t.stats[start.Fingerprint]
	if stat == nil {
		stat = &statementStats{Fingerprint: start.Fingerprint, Phase: start.Phase}
		t.stats[start.Fingerprint] = stat
	}
	stat.Count++
	stat.Total += d
	stat.Max = max(stat.Max, d)
	if d >= 100*time.Millisecond && len(stat.SlowSamples) < t.SlowLimit {
		stat.SlowSamples = append(stat.SlowSamples, d)
	}
}

func queryPhase(sql string) string {
	labels := []struct{ text, label string }{
		{"SELECT ingestion_sequence", "usage-page"},
		{"INSERT INTO billing_settlement_lines", "stage-lines-insert"},
		{"SELECT c.usage_id,c.source_fingerprint", "verify-claims"},
		{"SELECT usage_id,source,occurred_at", "verify-usage-sources"},
		{"SELECT u.usage_id,u.source", "verify-staged-sources"},
		{"INSERT INTO billing_settlement_pending_claims", "stage-claims-insert"},
		{"billing_settlement_usage_claims", "claim-conflict-check"},
		{"UPDATE billing_settlement_batches", "stage-batch-update"},
		{"UPDATE billing_settlement_close_jobs", "close-cursor-update"},
		{"SELECT job_id", "close-read"},
	}
	for _, item := range labels {
		if strings.Contains(sql, item.text) {
			return item.label
		}
	}
	return "other"
}

func (t *queryTracer) Snapshot() []statementStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]statementStats, 0, len(t.stats))
	for _, stat := range t.stats {
		copy := *stat
		copy.SlowSamples = slices.Clone(stat.SlowSamples)
		out = append(out, copy)
	}
	slices.SortFunc(out, func(a, b statementStats) int { return cmp.Compare(b.Max, a.Max) })
	return out
}

func main() {
	defer func() {
		if value := recover(); value != nil {
			fmt.Fprintln(os.Stderr, value)
			os.Exit(1)
		}
	}()
	run()
}

func run() {
	rows := flag.Int("rows", 1000, "number of usage records (1000, 100000, or 1000000)")
	chunk := flag.Int("chunksize", 500, "close page size")
	seed := flag.Int64("seed", 1, "deterministic fixture seed")
	out := flag.String("output-json", "", "write the machine-readable result to this path")
	flag.Parse()
	dsn := os.Getenv("BILLING_TEST_DATABASE_URL")
	if dsn == "" {
		fatal(errors.New("BILLING_TEST_DATABASE_URL is required"))
	}
	if *rows < 1 || *chunk < 1 || *chunk > 1000 {
		fatal(errors.New("rows and chunksize are invalid"))
	}
	ctx := context.Background()
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		fatal(err)
	}
	tracer := &queryTracer{stats: make(map[string]*statementStats), SlowLimit: 100}
	connConfig.Tracer = tracer
	db := sql.OpenDB(stdlib.GetConnector(*connConfig))
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		fatal(err)
	}
	schema := fmt.Sprintf("billing_load_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		fatal(err)
	}
	defer func() { _, _ = db.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) }()
	if _, err := db.ExecContext(ctx, `SET search_path TO `+schema); err != nil {
		fatal(err)
	}
	store := postgres.New(db)
	if err := store.Migrate(ctx); err != nil {
		fatal(err)
	}
	account := billing.AccountID("billing-load-account")
	if err := store.CreateAccount(ctx, account, "billing-load-subject"); err != nil {
		fatal(err)
	}
	config := usage.RuleConfig{Version: "billing-load-rule", Kind: usage.KindFixed, Target: usage.Target{Currency: "EUR"}, Rounding: usage.RoundDown, FixedRate: "1"}
	if err := store.Ratings().PublishRating(ctx, config); err != nil {
		fatal(err)
	}
	rule, err := usage.NewRule(config)
	if err != nil {
		fatal(err)
	}
	periodStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.Add(24 * time.Hour)
	if err := seedUsage(ctx, db, account, rule, *rows, *seed, periodStart); err != nil {
		fatal(err)
	}
	var peakHeapAlloc, peakHeapInuse uint64
	peakHeap(&peakHeapAlloc, &peakHeapInuse)
	beforeWAL := walBytes(ctx, db)
	service := usage.NewSettlement(store.Settlements(), func() time.Time { return periodEnd })
	in := usage.CloseInput{Account: account, Operation: "billing-load-operation", BatchID: "billing-load-batch", Period: usage.BillingPeriod{Start: periodStart, End: periodEnd, Cutoff: periodEnd}, Currency: "EUR", CreatedAt: periodEnd}
	started := time.Now()
	job, err := service.StartClose(ctx, in)
	if err != nil {
		fatal(err)
	}
	plan := explainUsage(ctx, db, account, job.Watermark, periodStart, periodEnd)
	claimPlan := explainClaim(ctx, db, account)
	late := usage.Observation{Account: account, ID: "late-after-watermark", Source: "meter", OccurredAt: periodStart.Add(time.Hour), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: 1}}
	prepared, err := usage.Prepare(late, rule, periodEnd)
	if err != nil {
		fatal(err)
	}
	if err := insertUsage(ctx, db, prepared); err != nil {
		fatal(err)
	}
	chunkDurations := make([]time.Duration, 0, (*rows+*chunk-1) / *chunk)
	for job.State == usage.ClosePreparing {
		stepStart := time.Now()
		job, err = usage.NewSettlement(store.Settlements(), func() time.Time { return periodEnd }).AdvanceClose(ctx, account, job.BatchID, job.Revision, *chunk)
		if err != nil {
			fatal(err)
		}
		peakHeap(&peakHeapAlloc, &peakHeapInuse)
		chunkDurations = append(chunkDurations, time.Since(stepStart))
	}
	if job.State != usage.CloseReady {
		fatal(fmt.Errorf("close ended in %s", job.State))
	}
	expectedTotal := int64(0)
	for i := range *rows {
		expectedTotal += int64(1 + (i+int(*seed))%3)
	}
	if job.Processed != int64(*rows) {
		fatal(fmt.Errorf("processed=%d want %d", job.Processed, *rows))
	}
	job, err = usage.NewSettlement(store.Settlements(), func() time.Time { return periodEnd }).PublishClose(ctx, account, job.BatchID, job.Revision)
	if err != nil {
		fatal(err)
	}
	summary, err := usage.NewSettlement(store.Settlements(), func() time.Time { return periodEnd }).BatchSummary(ctx, account, job.BatchID)
	if err != nil {
		fatal(err)
	}
	if summary.LineCount != int64(*rows) || summary.Total != expectedTotal {
		fatal(fmt.Errorf("close invariant line_count=%d total=%d want line_count=%d total=%d", summary.LineCount, summary.Total, *rows, expectedTotal))
	}
	var claimCount int64
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM billing_settlement_usage_claims WHERE account_id=$1 AND batch_id=$2) + (SELECT count(*) FROM billing_settlement_pending_claims WHERE account_id=$3 AND job_id=$4)`, string(account), job.BatchID, string(account), job.BatchID).Scan(&claimCount); err != nil {
		fatal(err)
	}
	if claimCount != int64(*rows) {
		fatal(fmt.Errorf("claim_count=%d want %d", claimCount, *rows))
	}
	lateExcluded, claimMismatches := verifyMembership(ctx, db, account, job.BatchID)
	if !lateExcluded || claimMismatches != 0 {
		fatal(errors.New("late post-watermark record was included"))
	}
	peakHeap(&peakHeapAlloc, &peakHeapInuse)
	r := result{Rows: *rows, SourceGroups: 100, ChunkSize: *chunk, Seed: *seed, Schema: schema, Account: string(account), BatchID: job.BatchID, Watermark: job.Watermark, FinalState: job.State, LineCount: summary.LineCount, ClaimCount: claimCount, ClaimMismatches: claimMismatches, Total: summary.Total, Processed: job.Processed, ExcludedLate: lateExcluded, TotalDuration: time.Since(started), ChunkCount: len(chunkDurations), ChunkDurations: chunkDurations, HeapAllocBytes: peakHeapAlloc, HeapInuseBytes: peakHeapInuse, PGVersion: pgVersion(ctx, db), GoVersion: runtime.Version(), GitCommit: gitCommit(), DirtyWorktree: dirtyWorktree(), BaselineCommit: os.Getenv("BILLING_LOAD_BASELINE_COMMIT"), HarnessHash: os.Getenv("BILLING_LOAD_HARNESS_HASH"), WALBytesDelta: walDelta(beforeWAL, walBytes(ctx, db)), PGQueryPlan: plan, PGClaimPlan: claimPlan, Statements: tracer.Snapshot()}
	r.TotalSeconds = r.TotalDuration.Seconds()
	r.ChunkP50, r.ChunkP95, r.ChunkP99, r.ChunkMax = percentiles(chunkDurations)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		fatal(err)
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(b, '\n'), 0o600); err != nil {
			fatal(err)
		}
	}
	fmt.Println(string(b))
}

func seedUsage(ctx context.Context, db *sql.DB, account billing.AccountID, rule *usage.Rule, n int, seed int64, start time.Time) error {
	for offset := 0; offset < n; offset += 1000 {
		end := min(offset+1000, n)
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for i := offset; i < end; i++ {
			o := usage.Observation{Account: account, ID: fmt.Sprintf("usage-%010d", i), Source: fmt.Sprintf("meter-%03d", i%100), OccurredAt: start.Add(time.Duration(i%86400) * time.Second), Funding: usage.Postpaid, Input: usage.RateInput{ActionCount: int64(1 + (i+int(seed))%3)}}
			r, err := usage.Prepare(o, rule, start)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			raw, err := json.Marshal(r)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO billing_usage(account_id,usage_id,source,occurred_at,funding,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7)`, string(account), o.ID, o.Source, o.OccurredAt, string(o.Funding), r.Fingerprint, raw); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func insertUsage(ctx context.Context, db *sql.DB, r usage.Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	o := r.Observation
	_, err = db.ExecContext(ctx, `INSERT INTO billing_usage(account_id,usage_id,source,occurred_at,funding,fingerprint,record) VALUES($1,$2,$3,$4,$5,$6,$7)`, string(o.Account), o.ID, o.Source, o.OccurredAt, string(o.Funding), r.Fingerprint, raw)
	return err
}

func percentiles(v []time.Duration) (time.Duration, time.Duration, time.Duration, time.Duration) {
	if len(v) == 0 {
		return 0, 0, 0, 0
	}
	x := slices.Clone(v)
	slices.Sort(x)
	at := func(p float64) time.Duration { return x[min(len(x)-1, int(math.Ceil(p*float64(len(x)))-1))] }
	return at(.50), at(.95), at(.99), x[len(x)-1]
}
func peakHeap(alloc, inuse *uint64) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	*alloc = max(*alloc, ms.HeapAlloc)
	*inuse = max(*inuse, ms.HeapInuse)
}
func walBytes(ctx context.Context, db *sql.DB) *int64 {
	var v sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT wal_bytes FROM pg_stat_wal`).Scan(&v); err != nil || !v.Valid {
		return nil
	}
	return &v.Int64
}
func walDelta(a, b *int64) *int64 {
	if a == nil || b == nil {
		return nil
	}
	v := *b - *a
	return &v
}
func pgVersion(ctx context.Context, db *sql.DB) string {
	var v string
	if err := db.QueryRowContext(ctx, `SELECT current_setting('server_version')`).Scan(&v); err != nil {
		return "unknown"
	}
	return v
}
func explainUsage(ctx context.Context, db *sql.DB, account billing.AccountID, watermark int64, start, end time.Time) json.RawMessage {
	var raw []byte
	err := db.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) SELECT ingestion_sequence,fingerprint,record FROM billing_usage WHERE account_id=$1 AND funding='postpaid' AND ingestion_sequence<=$2 AND ingestion_sequence>0 AND scope_provider='' AND scope_merchant='' AND scope_environment='' AND scope_subscription_id='' AND scope_item_id='' AND occurred_at>=$3 AND occurred_at<$4 AND occurred_at<$4 ORDER BY ingestion_sequence LIMIT 501`, string(account), int64(watermark), start, end).Scan(&raw)
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}
func explainClaim(ctx context.Context, db *sql.DB, account billing.AccountID) json.RawMessage {
	var raw []byte
	err := db.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) SELECT c.usage_id,c.source_fingerprint,c.job_id FROM unnest(ARRAY['usage-0000000000']::text[]) AS wanted(id) CROSS JOIN LATERAL (SELECT usage_id,source_fingerprint,job_id FROM billing_settlement_pending_claims WHERE account_id=$1 AND usage_id=wanted.id OFFSET 0) c`, string(account)).Scan(&raw)
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}
func gitCommit() string {
	b, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return string(b)
}
func fatal(err error) { panic(err) }

func verifyMembership(ctx context.Context, db *sql.DB, account billing.AccountID, batchID string) (bool, int64) {
	var lateLines, lateClaims, lineCount, claimCount, mismatches int64
	err := db.QueryRowContext(ctx, `WITH lines AS (SELECT DISTINCT usage_id FROM billing_settlement_lines WHERE account_id=$1 AND batch_id=$2), claims AS (SELECT DISTINCT usage_id FROM billing_settlement_usage_claims WHERE account_id=$1 AND batch_id=$2 UNION SELECT DISTINCT usage_id FROM billing_settlement_pending_claims WHERE account_id=$1 AND job_id=$2), joined AS (SELECT l.usage_id AS line_id,c.usage_id AS claim_id FROM lines l FULL OUTER JOIN claims c ON c.usage_id=l.usage_id) SELECT (SELECT count(*) FROM lines WHERE usage_id=$3),(SELECT count(*) FROM claims WHERE usage_id=$3),(SELECT count(*) FROM lines),(SELECT count(*) FROM claims),count(*) FILTER (WHERE line_id IS NULL OR claim_id IS NULL) FROM joined`, string(account), batchID, "late-after-watermark").Scan(&lateLines, &lateClaims, &lineCount, &claimCount, &mismatches)
	if err != nil || lineCount != claimCount {
		return false, mismatches + 1
	}
	return lateLines == 0 && lateClaims == 0, mismatches
}

func dirtyWorktree() bool {
	return exec.Command("git", "diff", "--quiet", "--").Run() != nil || exec.Command("git", "diff", "--cached", "--quiet", "--").Run() != nil
}
