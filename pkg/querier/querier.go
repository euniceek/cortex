package querier

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	v1 "github.com/prometheus/prometheus/web/api/v1"
	"github.com/thanos-io/promql-engine/engine"
	"github.com/thanos-io/promql-engine/logicalplan"
	"github.com/thanos-io/thanos/pkg/strutil"
	"golang.org/x/sync/errgroup"

	"github.com/cortexproject/cortex/pkg/chunk"
	"github.com/cortexproject/cortex/pkg/ingester/client"
	"github.com/cortexproject/cortex/pkg/querier/batch"
	"github.com/cortexproject/cortex/pkg/querier/iterators"
	"github.com/cortexproject/cortex/pkg/querier/lazyquery"
	seriesset "github.com/cortexproject/cortex/pkg/querier/series"
	"github.com/cortexproject/cortex/pkg/tenant"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/limiter"
	util_log "github.com/cortexproject/cortex/pkg/util/log"
	"github.com/cortexproject/cortex/pkg/util/spanlogger"
	"github.com/cortexproject/cortex/pkg/util/validation"
)

// Config contains the configuration require to create a querier
type Config struct {
	MaxConcurrent             int           `yaml:"max_concurrent"`
	Timeout                   time.Duration `yaml:"timeout"`
	Iterators                 bool          `yaml:"iterators"`
	BatchIterators            bool          `yaml:"batch_iterators"`
	IngesterStreaming         bool          `yaml:"ingester_streaming"`
	IngesterMetadataStreaming bool          `yaml:"ingester_metadata_streaming"`
	MaxSamples                int           `yaml:"max_samples"`
	QueryIngestersWithin      time.Duration `yaml:"query_ingesters_within"`
	QueryStoreForLabels       bool          `yaml:"query_store_for_labels_enabled"`
	AtModifierEnabled         bool          `yaml:"at_modifier_enabled" doc:"hidden"`
	EnablePerStepStats        bool          `yaml:"per_step_stats_enabled"`

	// QueryStoreAfter the time after which queries should also be sent to the store and not just ingesters.
	QueryStoreAfter    time.Duration `yaml:"query_store_after"`
	MaxQueryIntoFuture time.Duration `yaml:"max_query_into_future"`

	// The default evaluation interval for the promql engine.
	// Needs to be configured for subqueries to work as it is the default
	// step if not specified.
	DefaultEvaluationInterval time.Duration `yaml:"default_evaluation_interval"`

	// Limit of number of steps allowed for every subquery expression in a query.
	MaxSubQuerySteps int64 `yaml:"max_subquery_steps"`

	// Directory for ActiveQueryTracker. If empty, ActiveQueryTracker will be disabled and MaxConcurrent will not be applied (!).
	// ActiveQueryTracker logs queries that were active during the last crash, but logs them on the next startup.
	// However, we need to use active query tracker, otherwise we cannot limit Max Concurrent queries in the PromQL
	// engine.
	ActiveQueryTrackerDir string `yaml:"active_query_tracker_dir"`
	// LookbackDelta determines the time since the last sample after which a time
	// series is considered stale.
	LookbackDelta time.Duration `yaml:"lookback_delta"`

	// Blocks storage only.
	StoreGatewayAddresses         string       `yaml:"store_gateway_addresses"`
	StoreGatewayClient            ClientConfig `yaml:"store_gateway_client"`
	StoreGatewayQueryStatsEnabled bool         `yaml:"store_gateway_query_stats"`

	ShuffleShardingIngestersLookbackPeriod time.Duration `yaml:"shuffle_sharding_ingesters_lookback_period"`

	// Experimental. Use https://github.com/thanos-io/promql-engine rather than
	// the Prometheus query engine.
	ThanosEngine bool `yaml:"thanos_engine"`
}

var (
	errBadLookbackConfigs                             = errors.New("bad settings, query_store_after >= query_ingesters_within which can result in queries not being sent")
	errShuffleShardingLookbackLessThanQueryStoreAfter = errors.New("the shuffle-sharding lookback period should be greater or equal than the configured 'query store after'")
	errEmptyTimeRange                                 = errors.New("empty time range")
)

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	//lint:ignore faillint Need to pass the global logger like this for warning on deprecated methods
	flagext.DeprecatedFlag(f, "querier.at-modifier-enabled", "This flag is no longer functional; at-modifier is always enabled now.", util_log.Logger)

	cfg.StoreGatewayClient.RegisterFlagsWithPrefix("querier.store-gateway-client", f)
	f.IntVar(&cfg.MaxConcurrent, "querier.max-concurrent", 20, "The maximum number of concurrent queries.")
	f.DurationVar(&cfg.Timeout, "querier.timeout", 2*time.Minute, "The timeout for a query.")
	f.BoolVar(&cfg.Iterators, "querier.iterators", false, "Use iterators to execute query, as opposed to fully materialising the series in memory.")
	f.BoolVar(&cfg.BatchIterators, "querier.batch-iterators", true, "Use batch iterators to execute query, as opposed to fully materialising the series in memory.  Takes precedent over the -querier.iterators flag.")
	f.BoolVar(&cfg.IngesterStreaming, "querier.ingester-streaming", true, "Use streaming RPCs to query ingester.")
	f.BoolVar(&cfg.IngesterMetadataStreaming, "querier.ingester-metadata-streaming", false, "Use streaming RPCs for metadata APIs from ingester.")
	f.IntVar(&cfg.MaxSamples, "querier.max-samples", 50e6, "Maximum number of samples a single query can load into memory.")
	f.DurationVar(&cfg.QueryIngestersWithin, "querier.query-ingesters-within", 0, "Maximum lookback beyond which queries are not sent to ingester. 0 means all queries are sent to ingester.")
	f.BoolVar(&cfg.QueryStoreForLabels, "querier.query-store-for-labels-enabled", false, "Deprecated (Querying long-term store for labels will be always enabled in the future.): Query long-term store for series, label values and label names APIs.")
	f.BoolVar(&cfg.EnablePerStepStats, "querier.per-step-stats-enabled", false, "Enable returning samples stats per steps in query response.")
	f.DurationVar(&cfg.MaxQueryIntoFuture, "querier.max-query-into-future", 10*time.Minute, "Maximum duration into the future you can query. 0 to disable.")
	f.DurationVar(&cfg.DefaultEvaluationInterval, "querier.default-evaluation-interval", time.Minute, "The default evaluation interval or step size for subqueries.")
	f.DurationVar(&cfg.QueryStoreAfter, "querier.query-store-after", 0, "The time after which a metric should be queried from storage and not just ingesters. 0 means all queries are sent to store. When running the blocks storage, if this option is enabled, the time range of the query sent to the store will be manipulated to ensure the query end is not more recent than 'now - query-store-after'.")
	f.StringVar(&cfg.ActiveQueryTrackerDir, "querier.active-query-tracker-dir", "./active-query-tracker", "Active query tracker monitors active queries, and writes them to the file in given directory. If Cortex discovers any queries in this log during startup, it will log them to the log file. Setting to empty value disables active query tracker, which also disables -querier.max-concurrent option.")
	f.StringVar(&cfg.StoreGatewayAddresses, "querier.store-gateway-addresses", "", "Comma separated list of store-gateway addresses in DNS Service Discovery format. This option should be set when using the blocks storage and the store-gateway sharding is disabled (when enabled, the store-gateway instances form a ring and addresses are picked from the ring).")
	f.BoolVar(&cfg.StoreGatewayQueryStatsEnabled, "querier.store-gateway-query-stats-enabled", true, "If enabled, store gateway query stats will be logged using `info` log level.")
	f.DurationVar(&cfg.LookbackDelta, "querier.lookback-delta", 5*time.Minute, "Time since the last sample after which a time series is considered stale and ignored by expression evaluations.")
	f.DurationVar(&cfg.ShuffleShardingIngestersLookbackPeriod, "querier.shuffle-sharding-ingesters-lookback-period", 0, "When distributor's sharding strategy is shuffle-sharding and this setting is > 0, queriers fetch in-memory series from the minimum set of required ingesters, selecting only ingesters which may have received series since 'now - lookback period'. The lookback period should be greater or equal than the configured 'query store after' and 'query ingesters within'. If this setting is 0, queriers always query all ingesters (ingesters shuffle sharding on read path is disabled).")
	f.BoolVar(&cfg.ThanosEngine, "querier.thanos-engine", false, "Experimental. Use Thanos promql engine https://github.com/thanos-io/promql-engine rather than the Prometheus promql engine.")
	f.Int64Var(&cfg.MaxSubQuerySteps, "querier.max-subquery-steps", 0, "Max number of steps allowed for every subquery expression in query. Number of steps is calculated using subquery range / step. A value > 0 enables it.")
}

// Validate the config
func (cfg *Config) Validate() error {
	// Ensure the config won't create a situation where no queriers are returned.
	if cfg.QueryIngestersWithin != 0 && cfg.QueryStoreAfter != 0 {
		if cfg.QueryStoreAfter >= cfg.QueryIngestersWithin {
			return errBadLookbackConfigs
		}
	}

	if cfg.ShuffleShardingIngestersLookbackPeriod > 0 {
		if cfg.ShuffleShardingIngestersLookbackPeriod < cfg.QueryStoreAfter {
			return errShuffleShardingLookbackLessThanQueryStoreAfter
		}
	}

	return nil
}

func (cfg *Config) GetStoreGatewayAddresses() []string {
	if cfg.StoreGatewayAddresses == "" {
		return nil
	}

	return strings.Split(cfg.StoreGatewayAddresses, ",")
}

func getChunksIteratorFunction(cfg Config) chunkIteratorFunc {
	if cfg.BatchIterators {
		return batch.NewChunkMergeIterator
	} else if cfg.Iterators {
		return iterators.NewChunkMergeIterator
	}
	return mergeChunks
}

// New builds a queryable and promql engine.
func New(cfg Config, limits *validation.Overrides, distributor Distributor, stores []QueryableWithFilter, reg prometheus.Registerer, logger log.Logger) (storage.SampleAndChunkQueryable, storage.ExemplarQueryable, v1.QueryEngine) {
	iteratorFunc := getChunksIteratorFunction(cfg)

	distributorQueryable := newDistributorQueryable(distributor, cfg.IngesterStreaming, cfg.IngesterMetadataStreaming, iteratorFunc, cfg.QueryIngestersWithin, cfg.QueryStoreForLabels)

	ns := make([]QueryableWithFilter, len(stores))
	for ix, s := range stores {
		ns[ix] = storeQueryable{
			QueryableWithFilter: s,
			QueryStoreAfter:     cfg.QueryStoreAfter,
		}
	}
	queryable := NewQueryable(distributorQueryable, ns, iteratorFunc, cfg, limits)
	exemplarQueryable := newDistributorExemplarQueryable(distributor)

	lazyQueryable := storage.QueryableFunc(func(mint int64, maxt int64) (storage.Querier, error) {
		querier, err := queryable.Querier(mint, maxt)
		if err != nil {
			return nil, err
		}
		return lazyquery.NewLazyQuerier(querier), nil
	})

	// Emit max_concurrent config as a metric.
	maxConcurrentMetric := promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "max_concurrent_queries",
		Help:      "The maximum number of concurrent queries.",
	})
	maxConcurrentMetric.Set(float64(cfg.MaxConcurrent))

	var queryEngine v1.QueryEngine
	opts := promql.EngineOpts{
		Logger:               logger,
		Reg:                  reg,
		ActiveQueryTracker:   createActiveQueryTracker(cfg, logger),
		MaxSamples:           cfg.MaxSamples,
		Timeout:              cfg.Timeout,
		LookbackDelta:        cfg.LookbackDelta,
		EnablePerStepStats:   cfg.EnablePerStepStats,
		EnableAtModifier:     true,
		EnableNegativeOffset: true,
		NoStepSubqueryIntervalFn: func(int64) int64 {
			return cfg.DefaultEvaluationInterval.Milliseconds()
		},
	}
	if cfg.ThanosEngine {
		queryEngine = engine.New(engine.Opts{
			EngineOpts:        opts,
			LogicalOptimizers: logicalplan.AllOptimizers,
		})
	} else {
		queryEngine = promql.NewEngine(opts)
	}
	return NewSampleAndChunkQueryable(lazyQueryable), exemplarQueryable, queryEngine
}

// NewSampleAndChunkQueryable creates a SampleAndChunkQueryable from a
// Queryable with a ChunkQueryable stub, that errors once it gets called.
func NewSampleAndChunkQueryable(q storage.Queryable) storage.SampleAndChunkQueryable {
	return &sampleAndChunkQueryable{q}
}

type sampleAndChunkQueryable struct {
	storage.Queryable
}

func (q *sampleAndChunkQueryable) ChunkQuerier(mint, maxt int64) (storage.ChunkQuerier, error) {
	return nil, errors.New("ChunkQuerier not implemented")
}

func createActiveQueryTracker(cfg Config, logger log.Logger) promql.QueryTracker {
	dir := cfg.ActiveQueryTrackerDir

	if dir != "" {
		return promql.NewActiveQueryTracker(dir, cfg.MaxConcurrent, logger)
	}

	return nil
}

// QueryableWithFilter extends Queryable interface with `UseQueryable` filtering function.
type QueryableWithFilter interface {
	storage.Queryable

	// UseQueryable returns true if this queryable should be used to satisfy the query for given time range.
	// Query min and max time are in milliseconds since epoch.
	UseQueryable(now time.Time, queryMinT, queryMaxT int64) bool
}

type limiterHolder struct {
	limiter            *limiter.QueryLimiter
	limiterInitializer sync.Once
}

// NewQueryable creates a new Queryable for cortex.
func NewQueryable(distributor QueryableWithFilter, stores []QueryableWithFilter, chunkIterFn chunkIteratorFunc, cfg Config, limits *validation.Overrides) storage.Queryable {
	return storage.QueryableFunc(func(mint, maxt int64) (storage.Querier, error) {
		q := querier{
			now:                 time.Now(),
			mint:                mint,
			maxt:                maxt,
			chunkIterFn:         chunkIterFn,
			limits:              limits,
			maxQueryIntoFuture:  cfg.MaxQueryIntoFuture,
			queryStoreForLabels: cfg.QueryStoreForLabels,
			distributor:         distributor,
			stores:              stores,
			limiterHolder:       &limiterHolder{},
		}

		return q, nil
	})
}

type querier struct {
	chunkIterFn chunkIteratorFunc
	now         time.Time
	mint, maxt  int64

	limits              *validation.Overrides
	maxQueryIntoFuture  time.Duration
	queryStoreForLabels bool
	distributor         QueryableWithFilter
	stores              []QueryableWithFilter
	limiterHolder       *limiterHolder
}

func (q querier) setupFromCtx(ctx context.Context) (context.Context, string, int64, int64, storage.Querier, []storage.Querier, error) {
	userID, err := tenant.TenantID(ctx)
	if err != nil {
		return ctx, userID, 0, 0, nil, nil, err
	}

	q.limiterHolder.limiterInitializer.Do(func() {
		q.limiterHolder.limiter = limiter.NewQueryLimiter(q.limits.MaxFetchedSeriesPerQuery(userID), q.limits.MaxFetchedChunkBytesPerQuery(userID), q.limits.MaxChunksPerQuery(userID), q.limits.MaxFetchedDataBytesPerQuery(userID))
	})

	ctx = limiter.AddQueryLimiterToContext(ctx, q.limiterHolder.limiter)

	mint, maxt, err := validateQueryTimeRange(ctx, userID, q.mint, q.maxt, q.limits, q.maxQueryIntoFuture)
	if err != nil {
		return ctx, userID, 0, 0, nil, nil, err
	}

	dqr, err := q.distributor.Querier(mint, maxt)
	if err != nil {
		return ctx, userID, 0, 0, nil, nil, err
	}
	metadataQuerier := dqr

	queriers := make([]storage.Querier, 0)
	if q.distributor.UseQueryable(q.now, mint, maxt) {
		queriers = append(queriers, dqr)
	}

	for _, s := range q.stores {
		if !s.UseQueryable(q.now, mint, maxt) {
			continue
		}

		cqr, err := s.Querier(mint, maxt)
		if err != nil {
			return ctx, userID, 0, 0, nil, nil, err
		}

		queriers = append(queriers, cqr)
	}
	return ctx, userID, mint, maxt, metadataQuerier, queriers, nil
}

// Select implements storage.Querier interface.
// The bool passed is ignored because the series is always sorted.
func (q querier) Select(ctx context.Context, sortSeries bool, sp *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	ctx, userID, mint, maxt, metadataQuerier, queriers, err := q.setupFromCtx(ctx)
	if err == errEmptyTimeRange {
		return storage.EmptySeriesSet()
	} else if err != nil {
		return storage.ErrSeriesSet(err)
	}

	log, ctx := spanlogger.New(ctx, "querier.Select")
	defer log.Span.Finish()

	if sp != nil {
		level.Debug(log).Log("start", util.TimeFromMillis(sp.Start).UTC().String(), "end", util.TimeFromMillis(sp.End).UTC().String(), "step", sp.Step, "matchers", matchers)
	}

	if sp == nil {
		mint, maxt, err = validateQueryTimeRange(ctx, userID, mint, maxt, q.limits, q.maxQueryIntoFuture)
		if err == errEmptyTimeRange {
			return storage.EmptySeriesSet()
		} else if err != nil {
			return storage.ErrSeriesSet(err)
		}
		// if SelectHints is null, rely on minT, maxT of querier to scope in range for Select stmt
		sp = &storage.SelectHints{Start: mint, End: maxt}
	} else if sp.Func == "series" && !q.queryStoreForLabels {
		// Else if the querier receives a 'series' query, it means only metadata is needed.
		// Here we expect that metadataQuerier querier will handle that.
		// Also, in the recent versions of Prometheus, we pass in the hint but with Func set to "series".
		// See: https://github.com/prometheus/prometheus/pull/8050

		// In this case, the query time range has already been validated when the querier has been
		// created.
		return metadataQuerier.Select(ctx, true, sp, matchers...)
	}

	// Validate query time range. Even if the time range has already been validated when we created
	// the querier, we need to check it again here because the time range specified in hints may be
	// different.
	startMs, endMs, err := validateQueryTimeRange(ctx, userID, sp.Start, sp.End, q.limits, q.maxQueryIntoFuture)
	if err == errEmptyTimeRange {
		return storage.NoopSeriesSet()
	} else if err != nil {
		return storage.ErrSeriesSet(err)
	}

	// The time range may have been manipulated during the validation,
	// so we make sure changes are reflected back to hints.
	sp.Start = startMs
	sp.End = endMs

	// For series queries without specifying the start time, we prefer to
	// only query ingesters and not to query maxQueryLength to avoid OOM kill.
	if sp.Func == "series" && startMs == 0 {
		return metadataQuerier.Select(ctx, true, sp, matchers...)
	}

	startTime := model.Time(startMs)
	endTime := model.Time(endMs)

	// Validate query time range. This validation should be done only for instant / range queries and
	// NOT for metadata queries (series, labels) because the query-frontend doesn't support splitting
	// of such queries.
	if maxQueryLength := q.limits.MaxQueryLength(userID); maxQueryLength > 0 && endTime.Sub(startTime) > maxQueryLength {
		limitErr := validation.LimitError(fmt.Sprintf(validation.ErrQueryTooLong, endTime.Sub(startTime), maxQueryLength))
		return storage.ErrSeriesSet(limitErr)
	}

	if len(queriers) == 1 {
		return queriers[0].Select(ctx, sortSeries, sp, matchers...)
	}

	sets := make(chan storage.SeriesSet, len(queriers))
	for _, querier := range queriers {
		go func(querier storage.Querier) {
			// We should always select sorted here as we will need to merge the series
			sets <- querier.Select(ctx, true, sp, matchers...)
		}(querier)
	}

	var result []storage.SeriesSet
	for range queriers {
		select {
		case set := <-sets:
			result = append(result, set)
		case <-ctx.Done():
			return storage.ErrSeriesSet(ctx.Err())
		}
	}

	// we have all the sets from different sources (chunk from store, chunks from ingesters,
	// time series from store and time series from ingesters).
	// mergeSeriesSets will return sorted set.
	return q.mergeSeriesSets(result)
}

// LabelValues implements storage.Querier.
func (q querier) LabelValues(ctx context.Context, name string, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	ctx, _, _, _, metadataQuerier, queriers, err := q.setupFromCtx(ctx)
	if err == errEmptyTimeRange {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	if !q.queryStoreForLabels {
		return metadataQuerier.LabelValues(ctx, name, matchers...)
	}

	if len(queriers) == 1 {
		return queriers[0].LabelValues(ctx, name, matchers...)
	}

	var (
		g, _     = errgroup.WithContext(ctx)
		sets     = [][]string{}
		warnings = annotations.Annotations(nil)

		resMtx sync.Mutex
	)

	for _, querier := range queriers {
		// Need to reassign as the original variable will change and can't be relied on in a goroutine.
		querier := querier
		g.Go(func() error {
			// NB: Values are sorted in Cortex already.
			myValues, myWarnings, err := querier.LabelValues(ctx, name, matchers...)
			if err != nil {
				return err
			}

			resMtx.Lock()
			sets = append(sets, myValues)
			warnings.Merge(myWarnings)
			resMtx.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	return strutil.MergeSlices(sets...), warnings, nil
}

func (q querier) LabelNames(ctx context.Context, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	ctx, _, _, _, metadataQuerier, queriers, err := q.setupFromCtx(ctx)
	if err == errEmptyTimeRange {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}

	if !q.queryStoreForLabels {
		return metadataQuerier.LabelNames(ctx, matchers...)
	}

	if len(queriers) == 1 {
		return queriers[0].LabelNames(ctx, matchers...)
	}

	var (
		g, _     = errgroup.WithContext(ctx)
		sets     = [][]string{}
		warnings = annotations.Annotations(nil)

		resMtx sync.Mutex
	)

	for _, querier := range queriers {
		// Need to reassign as the original variable will change and can't be relied on in a goroutine.
		querier := querier
		g.Go(func() error {
			// NB: Names are sorted in Cortex already.
			myNames, myWarnings, err := querier.LabelNames(ctx, matchers...)
			if err != nil {
				return err
			}

			resMtx.Lock()
			sets = append(sets, myNames)
			warnings.Merge(myWarnings)
			resMtx.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	return strutil.MergeSlices(sets...), warnings, nil
}

func (querier) Close() error {
	return nil
}

func (q querier) mergeSeriesSets(sets []storage.SeriesSet) storage.SeriesSet {
	// Here we deal with sets that are based on chunks and build single set from them.
	// Remaining sets are merged with chunks-based one using storage.NewMergeSeriesSet

	otherSets := []storage.SeriesSet(nil)
	chunks := []chunk.Chunk(nil)

	for _, set := range sets {
		nonChunkSeries := []storage.Series(nil)

		// SeriesSet may have some series backed up by chunks, and some not.
		for set.Next() {
			s := set.At()

			if sc, ok := s.(SeriesWithChunks); ok {
				chunks = append(chunks, sc.Chunks()...)
			} else {
				nonChunkSeries = append(nonChunkSeries, s)
			}
		}

		if err := set.Err(); err != nil {
			otherSets = append(otherSets, storage.ErrSeriesSet(err))
		} else if len(nonChunkSeries) > 0 {
			otherSets = append(otherSets, &sliceSeriesSet{series: nonChunkSeries, ix: -1})
		}
	}

	if len(chunks) == 0 {
		return storage.NewMergeSeriesSet(otherSets, storage.ChainedSeriesMerge)
	}

	// partitionChunks returns set with sorted series, so it can be used by NewMergeSeriesSet
	chunksSet := partitionChunks(chunks, q.mint, q.maxt, q.chunkIterFn)

	if len(otherSets) == 0 {
		return chunksSet
	}

	otherSets = append(otherSets, chunksSet)
	return storage.NewMergeSeriesSet(otherSets, storage.ChainedSeriesMerge)
}

type sliceSeriesSet struct {
	series []storage.Series
	ix     int
}

func (s *sliceSeriesSet) Next() bool {
	s.ix++
	return s.ix < len(s.series)
}

func (s *sliceSeriesSet) At() storage.Series {
	if s.ix < 0 || s.ix >= len(s.series) {
		return nil
	}
	return s.series[s.ix]
}

func (s *sliceSeriesSet) Err() error {
	return nil
}

func (s *sliceSeriesSet) Warnings() annotations.Annotations {
	return nil
}

type storeQueryable struct {
	QueryableWithFilter
	QueryStoreAfter time.Duration
}

func (s storeQueryable) UseQueryable(now time.Time, queryMinT, queryMaxT int64) bool {
	// Include this store only if mint is within QueryStoreAfter w.r.t current time.
	if s.QueryStoreAfter != 0 && queryMinT > util.TimeToMillis(now.Add(-s.QueryStoreAfter)) {
		return false
	}
	return s.QueryableWithFilter.UseQueryable(now, queryMinT, queryMaxT)
}

type alwaysTrueFilterQueryable struct {
	storage.Queryable
}

func (alwaysTrueFilterQueryable) UseQueryable(_ time.Time, _, _ int64) bool {
	return true
}

// Wraps storage.Queryable into QueryableWithFilter, with no query filtering.
func UseAlwaysQueryable(q storage.Queryable) QueryableWithFilter {
	return alwaysTrueFilterQueryable{Queryable: q}
}

type useBeforeTimestampQueryable struct {
	storage.Queryable
	ts int64 // Timestamp in milliseconds
}

func (u useBeforeTimestampQueryable) UseQueryable(_ time.Time, queryMinT, _ int64) bool {
	if u.ts == 0 {
		return true
	}
	return queryMinT < u.ts
}

// Returns QueryableWithFilter, that is used only if query starts before given timestamp.
// If timestamp is zero (time.IsZero), queryable is always used.
func UseBeforeTimestampQueryable(queryable storage.Queryable, ts time.Time) QueryableWithFilter {
	t := int64(0)
	if !ts.IsZero() {
		t = util.TimeToMillis(ts)
	}
	return useBeforeTimestampQueryable{
		Queryable: queryable,
		ts:        t,
	}
}

func validateQueryTimeRange(ctx context.Context, userID string, startMs, endMs int64, limits *validation.Overrides, maxQueryIntoFuture time.Duration) (int64, int64, error) {
	now := model.Now()
	startTime := model.Time(startMs)
	endTime := model.Time(endMs)

	// Clamp time range based on max query into future.
	if maxQueryIntoFuture > 0 && endTime.After(now.Add(maxQueryIntoFuture)) {
		origEndTime := endTime
		endTime = now.Add(maxQueryIntoFuture)

		// Make sure to log it in traces to ease debugging.
		level.Debug(spanlogger.FromContext(ctx)).Log(
			"msg", "the end time of the query has been manipulated because of the 'max query into future' setting",
			"original", util.FormatTimeModel(origEndTime),
			"updated", util.FormatTimeModel(endTime))

		if endTime.Before(startTime) {
			return 0, 0, errEmptyTimeRange
		}
	}

	// Clamp the time range based on the max query lookback.
	if maxQueryLookback := limits.MaxQueryLookback(userID); maxQueryLookback > 0 && startTime.Before(now.Add(-maxQueryLookback)) {
		origStartTime := startTime
		startTime = now.Add(-maxQueryLookback)

		// Make sure to log it in traces to ease debugging.
		level.Debug(spanlogger.FromContext(ctx)).Log(
			"msg", "the start time of the query has been manipulated because of the 'max query lookback' setting",
			"original", util.FormatTimeModel(origStartTime),
			"updated", util.FormatTimeModel(startTime))

		if endTime.Before(startTime) {
			return 0, 0, errEmptyTimeRange
		}
	}

	// start time should be at least non-negative to avoid int64 overflow.
	if startTime < 0 {
		startTime = 0
	}

	return int64(startTime), int64(endTime), nil
}

// Series in the returned set are sorted alphabetically by labels.
func partitionChunks(chunks []chunk.Chunk, mint, maxt int64, iteratorFunc chunkIteratorFunc) storage.SeriesSet {
	chunksBySeries := map[string][]chunk.Chunk{}
	for _, c := range chunks {
		key := client.LabelsToKeyString(c.Metric)
		chunksBySeries[key] = append(chunksBySeries[key], c)
	}

	series := make([]storage.Series, 0, len(chunksBySeries))
	for i := range chunksBySeries {
		series = append(series, &chunkSeries{
			labels:            chunksBySeries[i][0].Metric,
			chunks:            chunksBySeries[i],
			chunkIteratorFunc: iteratorFunc,
			mint:              mint,
			maxt:              maxt,
		})
	}

	return seriesset.NewConcreteSeriesSet(true, series)
}
