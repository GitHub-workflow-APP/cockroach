// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package metric

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/metric/tick"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/codahale/hdrhistogram"
	prometheusgo "github.com/prometheus/client_model/go"
)

// HdrHistogramMaxLatency is the maximum value tracked in latency histograms. Higher
// values will be recorded as this value instead.
const HdrHistogramMaxLatency = 10 * time.Second

// A HdrHistogram collects observed values by keeping bucketed counts. For
// convenience, internally two sets of buckets are kept: A cumulative set (i.e.
// data is never evicted) and a windowed set (which keeps only recently
// collected samples).
//
// Top-level methods generally apply to the cumulative buckets; the windowed
// variant is exposed through the Windowed method.
//
// TODO(#96357): remove HdrHistogram model entirely once the Prometheus
// backed histogram and its bucket boundaries have been reliably proven in
// production.
type HdrHistogram struct {
	Metadata
	maxVal int64
	mu     struct {
		syncutil.Mutex
		cumulative *hdrhistogram.Histogram
		*tick.Ticker
		sliding *hdrhistogram.WindowedHistogram
	}
}

var _ IHistogram = &HdrHistogram{}
var _ PrometheusExportable = &HdrHistogram{}
var _ Iterable = &HdrHistogram{}

// NewHdrHistogram initializes a given Histogram. The contained windowed histogram
// rotates every 'duration'; both the windowed and the cumulative histogram
// track nonnegative values up to 'maxVal' with 'sigFigs' decimal points of
// precision.
func NewHdrHistogram(
	metadata Metadata, duration time.Duration, maxVal int64, sigFigs int,
) *HdrHistogram {
	h := &HdrHistogram{
		Metadata: metadata,
		maxVal:   maxVal,
	}
	wHist := hdrhistogram.NewWindowed(WindowedHistogramWrapNum, 0, maxVal, sigFigs)
	h.mu.cumulative = hdrhistogram.New(0, maxVal, sigFigs)
	h.mu.sliding = wHist
	h.mu.Ticker = tick.NewTicker(
		now(),
		duration/WindowedHistogramWrapNum,
		func() {
			wHist.Rotate()
		})
	return h
}

// NewHdrLatency is a convenience function which returns a histogram with
// suitable defaults for latency tracking. Values are expressed in ns,
// are truncated into the interval [0, HdrHistogramMaxLatency] and are recorded
// with one digit of precision (i.e. errors of <10ms at 100ms, <6s at 60s).
//
// The windowed portion of the Histogram retains values for approximately
// histogramWindow.
func NewHdrLatency(metadata Metadata, histogramWindow time.Duration) *HdrHistogram {
	return NewHdrHistogram(
		metadata, histogramWindow, HdrHistogramMaxLatency.Nanoseconds(), 1,
	)
}

// RecordValue adds the given value to the histogram. Recording a value in
// excess of the configured maximum value for that histogram results in
// recording the maximum value instead.
func (h *HdrHistogram) RecordValue(v int64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.mu.sliding.Current.RecordValue(v) != nil {
		_ = h.mu.sliding.Current.RecordValue(h.maxVal)
	}
	if h.mu.cumulative.RecordValue(v) != nil {
		_ = h.mu.cumulative.RecordValue(h.maxVal)
	}
}

// Total returns the (cumulative) number of samples and sum of samples.
func (h *HdrHistogram) Total(_ *prometheusgo.Metric) (int64, float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	totalSum := float64(h.mu.cumulative.TotalCount()) * h.mu.cumulative.Mean()
	return h.mu.cumulative.TotalCount(), totalSum
}

// Min returns the minimum.
func (h *HdrHistogram) Min() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mu.cumulative.Min()
}

// Inspect calls the closure with the empty string and the receiver.
func (h *HdrHistogram) Inspect(f func(interface{})) {
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		tick.MaybeTick(h.mu.Ticker)
	}()
	f(h)
}

// NextTick returns the next tick timestamp of the underlying tick.Ticker
// used by this HdrHistogram. Generally not useful - this is part of a band-aid
// fix and should be expected to be removed.
// TODO(obs-infra): remove this once pkg/util/aggmetric is merged with this package.
func (h *HdrHistogram) NextTick() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mu.NextTick()
}

// Tick triggers a tick of this HdrHistogram, regardless of whether we've passed
// the next tick interval. Generally, this should not be used by any caller other
// than aggmetric.AggHistogram. Future work will remove the need to expose this function
// as part of the public API.
// TODO(obs-infra): remove this once pkg/util/aggmetric is merged with this package.
func (h *HdrHistogram) Tick() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mu.Tick()
}

// GetType returns the prometheus type enum for this metric.
func (h *HdrHistogram) GetType() *prometheusgo.MetricType {
	return prometheusgo.MetricType_HISTOGRAM.Enum()
}

// ToPrometheusMetric returns a filled-in prometheus metric of the right type.
func (h *HdrHistogram) ToPrometheusMetric() *prometheusgo.Metric {
	hist := &prometheusgo.Histogram{}

	bars := func() []hdrhistogram.Bar {
		h.mu.Lock()
		defer h.mu.Unlock()
		tick.MaybeTick(h.mu.Ticker)
		return h.mu.cumulative.Distribution()
	}()
	hist.Bucket = make([]*prometheusgo.Bucket, 0, len(bars))

	var cumCount uint64
	var sum float64
	for _, bar := range bars {
		if bar.Count == 0 {
			// No need to expose trivial buckets.
			continue
		}
		upperBound := float64(bar.To)
		sum += upperBound * float64(bar.Count)

		cumCount += uint64(bar.Count)
		curCumCount := cumCount // need a new alloc thanks to bad proto code

		hist.Bucket = append(hist.Bucket, &prometheusgo.Bucket{
			CumulativeCount: &curCumCount,
			UpperBound:      &upperBound,
		})
	}
	hist.SampleCount = &cumCount
	hist.SampleSum = &sum // can do better here; we approximate in the loop

	return &prometheusgo.Metric{
		Histogram: hist,
	}
}

// TotalWindowed implements the WindowedHistogram interface.
func (h *HdrHistogram) TotalWindowed() (int64, float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hist := h.mu.sliding.Merge()
	totalSum := float64(hist.TotalCount()) * hist.Mean()
	return hist.TotalCount(), totalSum
}

func (h *HdrHistogram) toPrometheusMetricWindowedLocked() *prometheusgo.Metric {
	hist := &prometheusgo.Histogram{}

	tick.MaybeTick(h.mu.Ticker)
	mergedHist := h.mu.sliding.Merge()
	bars := mergedHist.Distribution()
	hist.Bucket = make([]*prometheusgo.Bucket, 0, len(bars))

	var cumCount uint64
	var sum float64
	for _, bar := range bars {
		if bar.Count == 0 {
			// No need to expose trivial buckets.
			continue
		}
		upperBound := float64(bar.To)
		sum += upperBound * float64(bar.Count)

		cumCount += uint64(bar.Count)
		curCumCount := cumCount // need a new alloc thanks to bad proto code

		hist.Bucket = append(hist.Bucket, &prometheusgo.Bucket{
			CumulativeCount: &curCumCount,
			UpperBound:      &upperBound,
		})
	}
	hist.SampleCount = &cumCount
	hist.SampleSum = &sum // can do better here; we approximate in the loop
	return &prometheusgo.Metric{
		Histogram: hist,
	}
}

// ToPrometheusMetricWindowed returns a filled-in prometheus metric of the
// right type for the current histogram window.
func (h *HdrHistogram) ToPrometheusMetricWindowed() *prometheusgo.Metric {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.toPrometheusMetricWindowedLocked()
}

// GetMetadata returns the metric's metadata including the Prometheus
// MetricType.
func (h *HdrHistogram) GetMetadata() Metadata {
	baseMetadata := h.Metadata
	baseMetadata.MetricType = prometheusgo.MetricType_HISTOGRAM
	return baseMetadata
}

func (h *HdrHistogram) ValueAtQuantileWindowed(q float64, window *prometheusgo.Metric) float64 {
	return ValueAtQuantileWindowed(window.Histogram, q)
}

func (h *HdrHistogram) Mean() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mu.cumulative.Mean()
}

func (h *HdrHistogram) MeanWindowed() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	hist := h.mu.sliding.Merge()
	return hist.Mean()
}
