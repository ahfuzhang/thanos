// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package query

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	promgate "github.com/prometheus/prometheus/pkg/gate"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/storage"

	"github.com/thanos-io/thanos/pkg/dedup"
	"github.com/thanos-io/thanos/pkg/extprom"
	"github.com/thanos-io/thanos/pkg/gate"
	"github.com/thanos-io/thanos/pkg/store"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/tracing"
)

// QueryableCreator returns implementation of promql.Queryable that fetches data from the proxy store API endpoints.
// If deduplication is enabled, all data retrieved from it will be deduplicated along all replicaLabels by default.
// When the replicaLabels argument is not empty it overwrites the global replicaLabels flag. This allows specifying
// replicaLabels at query time.
// maxResolutionMillis controls downsampling resolution that is allowed (specified in milliseconds).
// partialResponse controls `partialResponseDisabled` option of StoreAPI and partial response behavior of proxy.
type QueryableCreator func(deduplicate bool, replicaLabels []string, storeDebugMatchers [][]*labels.Matcher, maxResolutionMillis int64, partialResponse, skipChunks bool) storage.Queryable

// NewQueryableCreator creates QueryableCreator.
func NewQueryableCreator(logger log.Logger, reg prometheus.Registerer, proxy storepb.StoreServer, maxConcurrentSelects int, selectTimeout time.Duration) QueryableCreator {
	duration := promauto.With(
		extprom.WrapRegistererWithPrefix("concurrent_selects_", reg),
	).NewHistogram(gate.DurationHistogramOpts)
	initMetric(reg)

	return func(deduplicate bool, replicaLabels []string, storeDebugMatchers [][]*labels.Matcher, maxResolutionMillis int64, partialResponse, skipChunks bool) storage.Queryable {
		return &queryable{
			logger:              logger,
			replicaLabels:       replicaLabels,
			storeDebugMatchers:  storeDebugMatchers,
			proxy:               proxy,
			deduplicate:         deduplicate,
			maxResolutionMillis: maxResolutionMillis,
			partialResponse:     partialResponse,
			skipChunks:          skipChunks,
			gateProviderFn: func() gate.Gate {
				return gate.InstrumentGateDuration(duration, promgate.New(maxConcurrentSelects))
			},
			maxConcurrentSelects: maxConcurrentSelects,
			selectTimeout:        selectTimeout,
		}
	}
}

type queryable struct {
	logger               log.Logger
	replicaLabels        []string
	storeDebugMatchers   [][]*labels.Matcher
	proxy                storepb.StoreServer
	deduplicate          bool
	maxResolutionMillis  int64
	partialResponse      bool
	skipChunks           bool
	gateProviderFn       func() gate.Gate
	maxConcurrentSelects int
	selectTimeout        time.Duration
}

// Querier returns a new storage querier against the underlying proxy store API.
func (q *queryable) Querier(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
	return newQuerier(ctx, q.logger, mint, maxt, q.replicaLabels, q.storeDebugMatchers, q.proxy, q.deduplicate, q.maxResolutionMillis, q.partialResponse, q.skipChunks, q.gateProviderFn(), q.selectTimeout), nil
}

type querier struct {
	ctx                 context.Context
	logger              log.Logger
	cancel              func()
	mint, maxt          int64
	replicaLabels       map[string]struct{}
	storeDebugMatchers  [][]*labels.Matcher
	proxy               storepb.StoreServer
	deduplicate         bool
	maxResolutionMillis int64
	partialResponse     bool
	skipChunks          bool
	selectGate          gate.Gate
	selectTimeout       time.Duration
}

// newQuerier creates implementation of storage.Querier that fetches data from the proxy
// store API endpoints.
func newQuerier(
	ctx context.Context,
	logger log.Logger,
	mint, maxt int64,
	replicaLabels []string,
	storeDebugMatchers [][]*labels.Matcher,
	proxy storepb.StoreServer,
	deduplicate bool,
	maxResolutionMillis int64,
	partialResponse, skipChunks bool,
	selectGate gate.Gate,
	selectTimeout time.Duration,
) *querier {
	if logger == nil {
		logger = log.NewNopLogger()
	}
	ctx, cancel := context.WithCancel(ctx)

	rl := make(map[string]struct{})
	for _, replicaLabel := range replicaLabels {
		rl[replicaLabel] = struct{}{}
	}
	return &querier{
		ctx:           ctx,
		logger:        logger,
		cancel:        cancel,
		selectGate:    selectGate,
		selectTimeout: selectTimeout,

		mint:                mint,
		maxt:                maxt,
		replicaLabels:       rl,
		storeDebugMatchers:  storeDebugMatchers,
		proxy:               proxy,
		deduplicate:         deduplicate,
		maxResolutionMillis: maxResolutionMillis,
		partialResponse:     partialResponse,
		skipChunks:          skipChunks,
	}
}

func (q *querier) isDedupEnabled() bool {
	return q.deduplicate && len(q.replicaLabels) > 0
}

type seriesServer struct {
	// This field just exist to pseudo-implement the unused methods of the interface.
	storepb.Store_SeriesServer
	ctx context.Context

	seriesSet []storepb.Series
	warnings  []string
}

func (s *seriesServer) Send(r *storepb.SeriesResponse) error {
	if r.GetWarning() != "" {
		s.warnings = append(s.warnings, r.GetWarning())
		return nil
	}

	if r.GetSeries() != nil {
		s.seriesSet = append(s.seriesSet, *r.GetSeries())
		return nil
	}

	// Unsupported field, skip.
	return nil
}

func (s *seriesServer) Context() context.Context {
	return s.ctx
}

// aggrsFromFunc infers aggregates of the underlying data based on the wrapping
// function of a series selection.
func aggrsFromFunc(f string) []storepb.Aggr {
	if f == "min" || strings.HasPrefix(f, "min_") {
		return []storepb.Aggr{storepb.Aggr_MIN}
	}
	if f == "max" || strings.HasPrefix(f, "max_") {
		return []storepb.Aggr{storepb.Aggr_MAX}
	}
	if f == "count" || strings.HasPrefix(f, "count_") {
		return []storepb.Aggr{storepb.Aggr_COUNT}
	}
	// f == "sum" falls through here since we want the actual samples.
	if strings.HasPrefix(f, "sum_") {
		return []storepb.Aggr{storepb.Aggr_SUM}
	}
	if f == "increase" || f == "rate" || f == "irate" || f == "resets" {
		return []storepb.Aggr{storepb.Aggr_COUNTER}
	}
	// In the default case, we retrieve count and sum to compute an average.
	return []storepb.Aggr{storepb.Aggr_COUNT, storepb.Aggr_SUM}
}

func (q *querier) Select(_ bool, hints *storage.SelectHints, ms ...*labels.Matcher) storage.SeriesSet {
	defer histOfSelectLatency.Observe(float64(time.Since(time.Now()).Milliseconds()))
	counterOfSelectTimes.Inc()

	if hints == nil {
		hints = &storage.SelectHints{
			Start: q.mint,
			End:   q.maxt,
		}
	}

	matchers := make([]string, len(ms))
	for i, m := range ms {
		matchers[i] = m.String()
	}

	// The querier has a context but it gets canceled, as soon as query evaluation is completed, by the engine.
	// We want to prevent this from happening for the async store API calls we make while preserving tracing context.
	ctx := tracing.CopyTraceContext(context.Background(), q.ctx)
	ctx, cancel := context.WithTimeout(ctx, q.selectTimeout)
	span, ctx := tracing.StartSpan(ctx, "querier_select", opentracing.Tags{
		"minTime":  hints.Start,
		"maxTime":  hints.End,
		"matchers": "{" + strings.Join(matchers, ",") + "}",
	})

	promise := make(chan storage.SeriesSet, 1)
	go func() {
		defer close(promise)

		var err error
		tracing.DoInSpan(ctx, "querier_select_gate_ismyturn", func(ctx context.Context) {
			err = q.selectGate.Start(ctx)
		})
		if err != nil {
			promise <- storage.ErrSeriesSet(errors.Wrap(err, "failed to wait for turn"))
			return
		}
		defer q.selectGate.Done()

		span, ctx := tracing.StartSpan(ctx, "querier_select_select_fn")
		defer span.Finish()

		set, err := q.selectFn(ctx, hints, ms...)
		if err != nil {
			promise <- storage.ErrSeriesSet(err)
			return
		}

		promise <- set
	}()

	return &lazySeriesSet{create: func() (storage.SeriesSet, bool) {
		defer cancel()
		defer span.Finish()

		// Only gets called once, for the first Next() call of the series set.
		set, ok := <-promise
		if !ok {
			return storage.ErrSeriesSet(errors.New("channel closed before a value received")), false
		}
		return set, set.Next()
	}}
}

func (q *querier) selectFn(ctx context.Context, hints *storage.SelectHints, ms ...*labels.Matcher) (storage.SeriesSet, error) {

	sms, err := storepb.PromMatchersToMatchers(ms...)
	if err != nil {
		return nil, errors.Wrap(err, "convert matchers")
	}

	aggrs := aggrsFromFunc(hints.Func)

	// TODO(bwplotka): Pass it using the SeriesRequest instead of relying on context.
	ctx = context.WithValue(ctx, store.StoreMatcherKey, q.storeDebugMatchers)

	// TODO(bwplotka): Use inprocess gRPC.
	resp := &seriesServer{ctx: ctx}
	if err := q.proxy.Series(&storepb.SeriesRequest{
		MinTime:                 hints.Start,
		MaxTime:                 hints.End,
		Matchers:                sms,
		MaxResolutionWindow:     q.maxResolutionMillis,
		Aggregates:              aggrs,
		PartialResponseDisabled: !q.partialResponse,
		SkipChunks:              q.skipChunks,
	}, resp); err != nil {
		return nil, errors.Wrap(err, "proxy Series()")
	}
	{
		counterOfSelectWarning.Add(float64(len(resp.warnings)))
		counterOfSelectSeries.Add(float64(len(resp.seriesSet)))
		bytesTotal := 0
		for _, item := range resp.seriesSet {
			counterOfSelectSeriesLabels.Add(float64(len(item.Labels)))
			counterOfSelectSeriesChunks.Add(float64(len(item.Chunks)))
			for _, chunk := range item.Chunks {
				if chunk.Counter != nil && chunk.Counter.Data != nil {
					bytesTotal += len(chunk.Counter.Data)
				}
				if chunk.Max != nil && chunk.Max.Data != nil {
					bytesTotal += len(chunk.Max.Data)
				}
				if chunk.Min != nil && chunk.Min.Data != nil {
					bytesTotal += len(chunk.Min.Data)
				}
				if chunk.Sum != nil && chunk.Sum.Data != nil {
					bytesTotal += len(chunk.Sum.Data)
				}
				if chunk.Count != nil && chunk.Count.Data != nil {
					bytesTotal += len(chunk.Count.Data)
				}
				if chunk.Raw != nil && chunk.Raw.Data != nil {
					bytesTotal += len(chunk.Raw.Data)
				}
			}
		}
		counterOfSelectSeriesChunkBytes.Add(float64(bytesTotal))
	}

	var warns storage.Warnings
	for _, w := range resp.warnings {
		warns = append(warns, errors.New(w))
	}

	if !q.isDedupEnabled() {
		// Return data without any deduplication.
		return &promSeriesSet{
			mint:  q.mint,
			maxt:  q.maxt,
			set:   newStoreSeriesSet(resp.seriesSet),
			aggrs: aggrs,
			warns: warns,
		}, nil
	}

	// TODO(fabxc): this could potentially pushed further down into the store API to make true streaming possible.
	sortDedupLabels(resp.seriesSet, q.replicaLabels)
	set := &promSeriesSet{
		mint:  q.mint,
		maxt:  q.maxt,
		set:   newStoreSeriesSet(resp.seriesSet),
		aggrs: aggrs,
		warns: warns,
	}

	// The merged series set assembles all potentially-overlapping time ranges of the same series into a single one.
	// TODO(bwplotka): We could potentially dedup on chunk level, use chunk iterator for that when available.
	return dedup.NewSeriesSet(set, q.replicaLabels, len(aggrs) == 1 && aggrs[0] == storepb.Aggr_COUNTER), nil
}

// sortDedupLabels re-sorts the set so that the same series with different replica
// labels are coming right after each other.
func sortDedupLabels(set []storepb.Series, replicaLabels map[string]struct{}) {
	for _, s := range set {
		// Move the replica labels to the very end.
		sort.Slice(s.Labels, func(i, j int) bool {
			if _, ok := replicaLabels[s.Labels[i].Name]; ok {
				return false
			}
			if _, ok := replicaLabels[s.Labels[j].Name]; ok {
				return true
			}
			return s.Labels[i].Name < s.Labels[j].Name
		})
	}
	// With the re-ordered label sets, re-sorting all series aligns the same series
	// from different replicas sequentially.
	sort.Slice(set, func(i, j int) bool {
		return labels.Compare(labelpb.ZLabelsToPromLabels(set[i].Labels), labelpb.ZLabelsToPromLabels(set[j].Labels)) < 0
	})
}

// LabelValues returns all potential values for a label name.
func (q *querier) LabelValues(name string, matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	span, ctx := tracing.StartSpan(q.ctx, "querier_label_values")
	defer span.Finish()

	// TODO(bwplotka): Pass it using the SeriesRequest instead of relying on context.
	ctx = context.WithValue(ctx, store.StoreMatcherKey, q.storeDebugMatchers)

	pbMatchers, err := storepb.PromMatchersToMatchers(matchers...)
	if err != nil {
		return nil, nil, errors.Wrap(err, "converting prom matchers to storepb matchers")
	}

	resp, err := q.proxy.LabelValues(ctx, &storepb.LabelValuesRequest{
		Label:                   name,
		PartialResponseDisabled: !q.partialResponse,
		Start:                   q.mint,
		End:                     q.maxt,
		Matchers:                pbMatchers,
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "proxy LabelValues()")
	}

	var warns storage.Warnings
	for _, w := range resp.Warnings {
		warns = append(warns, errors.New(w))
	}

	return resp.Values, warns, nil
}

// LabelNames returns all the unique label names present in the block in sorted order constrained
// by the given matchers.
func (q *querier) LabelNames(matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	span, ctx := tracing.StartSpan(q.ctx, "querier_label_names")
	defer span.Finish()

	// TODO(bwplotka): Pass it using the SeriesRequest instead of relying on context.
	ctx = context.WithValue(ctx, store.StoreMatcherKey, q.storeDebugMatchers)

	pbMatchers, err := storepb.PromMatchersToMatchers(matchers...)
	if err != nil {
		return nil, nil, errors.Wrap(err, "converting prom matchers to storepb matchers")
	}

	resp, err := q.proxy.LabelNames(ctx, &storepb.LabelNamesRequest{
		PartialResponseDisabled: !q.partialResponse,
		Start:                   q.mint,
		End:                     q.maxt,
		Matchers:                pbMatchers,
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "proxy LabelNames()")
	}

	var warns storage.Warnings
	for _, w := range resp.Warnings {
		warns = append(warns, errors.New(w))
	}

	return resp.Names, warns, nil
}

func (q *querier) Close() error {
	q.cancel()
	return nil
}

var (
	counterOfSelectTimes            prometheus.Counter
	histOfSelectLatency             prometheus.Observer
	counterOfSelectWarning          prometheus.Counter
	counterOfSelectSeries           prometheus.Counter
	counterOfSelectSeriesLabels     prometheus.Counter
	counterOfSelectSeriesChunks     prometheus.Counter
	counterOfSelectSeriesChunkBytes prometheus.Counter
)

func initMetric(reg prometheus.Registerer) {
	podName := os.Getenv("POD_NAME")
	podIP := os.Getenv("POD_IP")
	labels := []string{"container_name", "container_ip"}
	vec := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_select_times_total",
	}, labels)
	//prometheus.MustRegister(vec)
	counterOfSelectTimes = vec.WithLabelValues(podName, podIP)

	hist := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "external_select_latency",
		Buckets: []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 200, 300, 500, 800, 1000, 2000, 3000, 4000, 5000, 10000},
	}, labels)
	//prometheus.MustRegister(hist)
	histOfSelectLatency = hist.WithLabelValues(podName, podIP)
	//
	vecOfSelectWarning := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_select_warning_total",
	}, labels)
	//prometheus.MustRegister(vec)
	counterOfSelectWarning = vecOfSelectWarning.WithLabelValues(podName, podIP)
	//
	vecOfSelectSeries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_select_series_total",
	}, labels)
	counterOfSelectSeries = vecOfSelectSeries.WithLabelValues(podName, podIP)
	//
	vecOfSelectSeriesLabels := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_select_labels_total",
	}, labels)
	counterOfSelectSeriesLabels = vecOfSelectSeriesLabels.WithLabelValues(podName, podIP)
	//
	vecOfSelectSeriesChunks := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_select_chunks_total",
	}, labels)
	counterOfSelectSeriesChunks = vecOfSelectSeriesChunks.WithLabelValues(podName, podIP)
	//
	vecOfSelectSeriesChunkBytes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_select_chunks_bytes_total",
	}, labels)
	counterOfSelectSeriesChunkBytes = vecOfSelectSeriesChunkBytes.WithLabelValues(podName, podIP)
	//
	reg.Register(vec)
	reg.Register(hist)
	reg.Register(vecOfSelectWarning)
	reg.Register(vecOfSelectSeries)
	reg.Register(vecOfSelectSeriesLabels)
	reg.Register(vecOfSelectSeriesChunks)
	reg.Register(vecOfSelectSeriesChunkBytes)
	//prometheus.MustRegister(vec, hist, vecOfSelectWarning, vecOfSelectSeries, vecOfSelectSeriesLabels,
	//	vecOfSelectSeriesChunks, vecOfSelectSeriesChunkBytes)
}
