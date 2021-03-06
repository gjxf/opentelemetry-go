// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transform

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonpb "go.opentelemetry.io/otel/exporters/otlp/internal/opentelemetry-proto-gen/common/v1"
	metricpb "go.opentelemetry.io/otel/exporters/otlp/internal/opentelemetry-proto-gen/metrics/v1"

	"go.opentelemetry.io/otel/api/metric"
	"go.opentelemetry.io/otel/label"
	export "go.opentelemetry.io/otel/sdk/export/metric"
	"go.opentelemetry.io/otel/sdk/export/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/export/metric/metrictest"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/array"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/lastvalue"
	lvAgg "go.opentelemetry.io/otel/sdk/metric/aggregator/lastvalue"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/minmaxsumcount"
	"go.opentelemetry.io/otel/sdk/metric/aggregator/sum"
	sumAgg "go.opentelemetry.io/otel/sdk/metric/aggregator/sum"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/unit"
)

var (
	// Timestamps used in this test:

	intervalStart = time.Now()
	intervalEnd   = intervalStart.Add(time.Hour)
)

func TestStringKeyValues(t *testing.T) {
	tests := []struct {
		kvs      []label.KeyValue
		expected []*commonpb.StringKeyValue
	}{
		{
			nil,
			nil,
		},
		{
			[]label.KeyValue{},
			nil,
		},
		{
			[]label.KeyValue{
				label.Bool("true", true),
				label.Int64("one", 1),
				label.Uint64("two", 2),
				label.Float64("three", 3),
				label.Int32("four", 4),
				label.Uint32("five", 5),
				label.Float32("six", 6),
				label.Int("seven", 7),
				label.Uint("eight", 8),
				label.String("the", "final word"),
			},
			[]*commonpb.StringKeyValue{
				{Key: "eight", Value: "8"},
				{Key: "five", Value: "5"},
				{Key: "four", Value: "4"},
				{Key: "one", Value: "1"},
				{Key: "seven", Value: "7"},
				{Key: "six", Value: "6"},
				{Key: "the", Value: "final word"},
				{Key: "three", Value: "3"},
				{Key: "true", Value: "true"},
				{Key: "two", Value: "2"},
			},
		},
	}

	for _, test := range tests {
		labels := label.NewSet(test.kvs...)
		assert.Equal(t, test.expected, stringKeyValues(labels.Iter()))
	}
}

func TestMinMaxSumCountValue(t *testing.T) {
	mmsc, ckpt := metrictest.Unslice2(minmaxsumcount.New(2, &metric.Descriptor{}))

	assert.NoError(t, mmsc.Update(context.Background(), 1, &metric.Descriptor{}))
	assert.NoError(t, mmsc.Update(context.Background(), 10, &metric.Descriptor{}))

	// Prior to checkpointing ErrNoData should be returned.
	_, _, _, _, err := minMaxSumCountValues(ckpt.(aggregation.MinMaxSumCount))
	assert.EqualError(t, err, aggregation.ErrNoData.Error())

	// Checkpoint to set non-zero values
	require.NoError(t, mmsc.SynchronizedMove(ckpt, &metric.Descriptor{}))
	min, max, sum, count, err := minMaxSumCountValues(ckpt.(aggregation.MinMaxSumCount))
	if assert.NoError(t, err) {
		assert.Equal(t, min, metric.NewInt64Number(1))
		assert.Equal(t, max, metric.NewInt64Number(10))
		assert.Equal(t, sum, metric.NewInt64Number(11))
		assert.Equal(t, count, int64(2))
	}
}

func TestMinMaxSumCountMetricDescriptor(t *testing.T) {
	tests := []struct {
		name        string
		metricKind  metric.Kind
		description string
		unit        unit.Unit
		numberKind  metric.NumberKind
		labels      []label.KeyValue
		expected    *metricpb.MetricDescriptor
	}{
		{
			"mmsc-test-a",
			metric.ValueRecorderKind,
			"test-a-description",
			unit.Dimensionless,
			metric.Int64NumberKind,
			[]label.KeyValue{},
			&metricpb.MetricDescriptor{
				Name:        "mmsc-test-a",
				Description: "test-a-description",
				Unit:        "1",
				Type:        metricpb.MetricDescriptor_SUMMARY,
			},
		},
		{
			"mmsc-test-b",
			metric.CounterKind, // This shouldn't change anything.
			"test-b-description",
			unit.Bytes,
			metric.Float64NumberKind, // This shouldn't change anything.
			[]label.KeyValue{label.String("A", "1")},
			&metricpb.MetricDescriptor{
				Name:        "mmsc-test-b",
				Description: "test-b-description",
				Unit:        "By",
				Type:        metricpb.MetricDescriptor_SUMMARY,
			},
		},
	}

	ctx := context.Background()
	mmsc, ckpt := metrictest.Unslice2(minmaxsumcount.New(2, &metric.Descriptor{}))
	if !assert.NoError(t, mmsc.Update(ctx, 1, &metric.Descriptor{})) {
		return
	}
	require.NoError(t, mmsc.SynchronizedMove(ckpt, &metric.Descriptor{}))
	for _, test := range tests {
		desc := metric.NewDescriptor(test.name, test.metricKind, test.numberKind,
			metric.WithDescription(test.description),
			metric.WithUnit(test.unit))
		labels := label.NewSet(test.labels...)
		record := export.NewRecord(&desc, &labels, nil, ckpt.Aggregation(), intervalStart, intervalEnd)
		got, err := minMaxSumCount(record, ckpt.(aggregation.MinMaxSumCount))
		if assert.NoError(t, err) {
			assert.Equal(t, test.expected, got.MetricDescriptor)
		}
	}
}

func TestMinMaxSumCountDatapoints(t *testing.T) {
	desc := metric.NewDescriptor("", metric.ValueRecorderKind, metric.Int64NumberKind)
	labels := label.NewSet()
	mmsc, ckpt := metrictest.Unslice2(minmaxsumcount.New(2, &desc))

	assert.NoError(t, mmsc.Update(context.Background(), 1, &desc))
	assert.NoError(t, mmsc.Update(context.Background(), 10, &desc))
	require.NoError(t, mmsc.SynchronizedMove(ckpt, &desc))
	expected := []*metricpb.SummaryDataPoint{
		{
			Count: 2,
			Sum:   11,
			PercentileValues: []*metricpb.SummaryDataPoint_ValueAtPercentile{
				{
					Percentile: 0.0,
					Value:      1,
				},
				{
					Percentile: 100.0,
					Value:      10,
				},
			},
			StartTimeUnixNano: uint64(intervalStart.UnixNano()),
			TimeUnixNano:      uint64(intervalEnd.UnixNano()),
		},
	}
	record := export.NewRecord(&desc, &labels, nil, ckpt.Aggregation(), intervalStart, intervalEnd)
	m, err := minMaxSumCount(record, ckpt.(aggregation.MinMaxSumCount))
	if assert.NoError(t, err) {
		assert.Equal(t, []*metricpb.Int64DataPoint(nil), m.Int64DataPoints)
		assert.Equal(t, []*metricpb.DoubleDataPoint(nil), m.DoubleDataPoints)
		assert.Equal(t, []*metricpb.HistogramDataPoint(nil), m.HistogramDataPoints)
		assert.Equal(t, expected, m.SummaryDataPoints)
	}
}

func TestMinMaxSumCountPropagatesErrors(t *testing.T) {
	// ErrNoData should be returned by both the Min and Max values of
	// a MinMaxSumCount Aggregator. Use this fact to check the error is
	// correctly returned.
	mmsc := &minmaxsumcount.New(1, &metric.Descriptor{})[0]
	_, _, _, _, err := minMaxSumCountValues(mmsc)
	assert.Error(t, err)
	assert.Equal(t, aggregation.ErrNoData, err)
}

func TestSumMetricDescriptor(t *testing.T) {
	tests := []struct {
		name        string
		metricKind  metric.Kind
		description string
		unit        unit.Unit
		numberKind  metric.NumberKind
		labels      []label.KeyValue
		expected    *metricpb.MetricDescriptor
	}{
		{
			"sum-test-a",
			metric.CounterKind,
			"test-a-description",
			unit.Dimensionless,
			metric.Int64NumberKind,
			[]label.KeyValue{},
			&metricpb.MetricDescriptor{
				Name:        "sum-test-a",
				Description: "test-a-description",
				Unit:        "1",
				Type:        metricpb.MetricDescriptor_INT64,
			},
		},
		{
			"sum-test-b",
			metric.ValueObserverKind, // This shouldn't change anything.
			"test-b-description",
			unit.Milliseconds,
			metric.Float64NumberKind,
			[]label.KeyValue{label.String("A", "1")},
			&metricpb.MetricDescriptor{
				Name:        "sum-test-b",
				Description: "test-b-description",
				Unit:        "ms",
				Type:        metricpb.MetricDescriptor_DOUBLE,
			},
		},
	}

	for _, test := range tests {
		desc := metric.NewDescriptor(test.name, test.metricKind, test.numberKind,
			metric.WithDescription(test.description),
			metric.WithUnit(test.unit),
		)
		labels := label.NewSet(test.labels...)
		emptyAgg := &sumAgg.New(1)[0]
		record := export.NewRecord(&desc, &labels, nil, emptyAgg, intervalStart, intervalEnd)
		got, err := scalar(record, 0, time.Time{}, time.Time{})
		if assert.NoError(t, err) {
			assert.Equal(t, test.expected, got.MetricDescriptor)
		}
	}
}

func TestSumInt64DataPoints(t *testing.T) {
	desc := metric.NewDescriptor("", metric.ValueRecorderKind, metric.Int64NumberKind)
	labels := label.NewSet()
	s, ckpt := metrictest.Unslice2(sumAgg.New(2))
	assert.NoError(t, s.Update(context.Background(), metric.Number(1), &desc))
	require.NoError(t, s.SynchronizedMove(ckpt, &desc))
	record := export.NewRecord(&desc, &labels, nil, ckpt.Aggregation(), intervalStart, intervalEnd)
	sum, ok := ckpt.(aggregation.Sum)
	require.True(t, ok, "ckpt is not an aggregation.Sum: %T", ckpt)
	value, err := sum.Sum()
	require.NoError(t, err)

	if m, err := scalar(record, value, record.StartTime(), record.EndTime()); assert.NoError(t, err) {
		assert.Equal(t, []*metricpb.Int64DataPoint{{
			Value:             1,
			StartTimeUnixNano: uint64(intervalStart.UnixNano()),
			TimeUnixNano:      uint64(intervalEnd.UnixNano()),
		}}, m.Int64DataPoints)
		assert.Equal(t, []*metricpb.DoubleDataPoint(nil), m.DoubleDataPoints)
		assert.Equal(t, []*metricpb.HistogramDataPoint(nil), m.HistogramDataPoints)
		assert.Equal(t, []*metricpb.SummaryDataPoint(nil), m.SummaryDataPoints)
	}
}

func TestSumFloat64DataPoints(t *testing.T) {
	desc := metric.NewDescriptor("", metric.ValueRecorderKind, metric.Float64NumberKind)
	labels := label.NewSet()
	s, ckpt := metrictest.Unslice2(sumAgg.New(2))
	assert.NoError(t, s.Update(context.Background(), metric.NewFloat64Number(1), &desc))
	require.NoError(t, s.SynchronizedMove(ckpt, &desc))
	record := export.NewRecord(&desc, &labels, nil, ckpt.Aggregation(), intervalStart, intervalEnd)
	sum, ok := ckpt.(aggregation.Sum)
	require.True(t, ok, "ckpt is not an aggregation.Sum: %T", ckpt)
	value, err := sum.Sum()
	require.NoError(t, err)

	if m, err := scalar(record, value, record.StartTime(), record.EndTime()); assert.NoError(t, err) {
		assert.Equal(t, []*metricpb.Int64DataPoint(nil), m.Int64DataPoints)
		assert.Equal(t, []*metricpb.DoubleDataPoint{{
			Value:             1,
			StartTimeUnixNano: uint64(intervalStart.UnixNano()),
			TimeUnixNano:      uint64(intervalEnd.UnixNano()),
		}}, m.DoubleDataPoints)
		assert.Equal(t, []*metricpb.HistogramDataPoint(nil), m.HistogramDataPoints)
		assert.Equal(t, []*metricpb.SummaryDataPoint(nil), m.SummaryDataPoints)
	}
}

func TestLastValueInt64DataPoints(t *testing.T) {
	desc := metric.NewDescriptor("", metric.ValueRecorderKind, metric.Int64NumberKind)
	labels := label.NewSet()
	s, ckpt := metrictest.Unslice2(lvAgg.New(2))
	assert.NoError(t, s.Update(context.Background(), metric.Number(100), &desc))
	require.NoError(t, s.SynchronizedMove(ckpt, &desc))
	record := export.NewRecord(&desc, &labels, nil, ckpt.Aggregation(), intervalStart, intervalEnd)
	sum, ok := ckpt.(aggregation.LastValue)
	require.True(t, ok, "ckpt is not an aggregation.LastValue: %T", ckpt)
	value, timestamp, err := sum.LastValue()
	require.NoError(t, err)

	if m, err := scalar(record, value, time.Time{}, timestamp); assert.NoError(t, err) {
		assert.Equal(t, []*metricpb.Int64DataPoint{{
			Value:             100,
			StartTimeUnixNano: 0,
			TimeUnixNano:      uint64(timestamp.UnixNano()),
		}}, m.Int64DataPoints)
		assert.Equal(t, []*metricpb.DoubleDataPoint(nil), m.DoubleDataPoints)
		assert.Equal(t, []*metricpb.HistogramDataPoint(nil), m.HistogramDataPoints)
		assert.Equal(t, []*metricpb.SummaryDataPoint(nil), m.SummaryDataPoints)
	}
}

func TestSumErrUnknownValueType(t *testing.T) {
	desc := metric.NewDescriptor("", metric.ValueRecorderKind, metric.NumberKind(-1))
	labels := label.NewSet()
	s := &sumAgg.New(1)[0]
	record := export.NewRecord(&desc, &labels, nil, s, intervalStart, intervalEnd)
	value, err := s.Sum()
	require.NoError(t, err)

	_, err = scalar(record, value, record.StartTime(), record.EndTime())
	assert.Error(t, err)
	if !errors.Is(err, ErrUnknownValueType) {
		t.Errorf("expected ErrUnknownValueType, got %v", err)
	}
}

type testAgg struct {
	kind aggregation.Kind
	agg  aggregation.Aggregation
}

func (t *testAgg) Kind() aggregation.Kind {
	return t.kind
}

func (t *testAgg) Aggregation() aggregation.Aggregation {
	return t.agg
}

// None of these three are used:

func (t *testAgg) Update(ctx context.Context, number metric.Number, descriptor *metric.Descriptor) error {
	return nil
}
func (t *testAgg) SynchronizedMove(destination export.Aggregator, descriptor *metric.Descriptor) error {
	return nil
}
func (t *testAgg) Merge(aggregator export.Aggregator, descriptor *metric.Descriptor) error {
	return nil
}

type testErrSum struct {
	err error
}

type testErrLastValue struct {
	err error
}

type testErrMinMaxSumCount struct {
	testErrSum
}

func (te *testErrLastValue) LastValue() (metric.Number, time.Time, error) {
	return 0, time.Time{}, te.err
}
func (te *testErrLastValue) Kind() aggregation.Kind {
	return aggregation.LastValueKind
}

func (te *testErrSum) Sum() (metric.Number, error) {
	return 0, te.err
}
func (te *testErrSum) Kind() aggregation.Kind {
	return aggregation.SumKind
}

func (te *testErrMinMaxSumCount) Min() (metric.Number, error) {
	return 0, te.err
}

func (te *testErrMinMaxSumCount) Max() (metric.Number, error) {
	return 0, te.err
}

func (te *testErrMinMaxSumCount) Count() (int64, error) {
	return 0, te.err
}

var _ export.Aggregator = &testAgg{}
var _ aggregation.Aggregation = &testAgg{}
var _ aggregation.Sum = &testErrSum{}
var _ aggregation.LastValue = &testErrLastValue{}
var _ aggregation.MinMaxSumCount = &testErrMinMaxSumCount{}

func TestRecordAggregatorIncompatibleErrors(t *testing.T) {
	makeMpb := func(kind aggregation.Kind, agg aggregation.Aggregation) (*metricpb.Metric, error) {
		desc := metric.NewDescriptor("things", metric.CounterKind, metric.Int64NumberKind)
		labels := label.NewSet()
		res := resource.New()
		test := &testAgg{
			kind: kind,
			agg:  agg,
		}
		return Record(export.NewRecord(&desc, &labels, res, test, intervalStart, intervalEnd))
	}

	mpb, err := makeMpb(aggregation.SumKind, &lastvalue.New(1)[0])

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, ErrIncompatibleAgg))

	mpb, err = makeMpb(aggregation.LastValueKind, &sum.New(1)[0])

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, ErrIncompatibleAgg))

	mpb, err = makeMpb(aggregation.MinMaxSumCountKind, &lastvalue.New(1)[0])

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, ErrIncompatibleAgg))

	mpb, err = makeMpb(aggregation.ExactKind, &array.New(1)[0])

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, ErrUnimplementedAgg))
}

func TestRecordAggregatorUnexpectedErrors(t *testing.T) {
	makeMpb := func(kind aggregation.Kind, agg aggregation.Aggregation) (*metricpb.Metric, error) {
		desc := metric.NewDescriptor("things", metric.CounterKind, metric.Int64NumberKind)
		labels := label.NewSet()
		res := resource.New()
		return Record(export.NewRecord(&desc, &labels, res, agg, intervalStart, intervalEnd))
	}

	errEx := fmt.Errorf("timeout")

	mpb, err := makeMpb(aggregation.SumKind, &testErrSum{errEx})

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, errEx))

	mpb, err = makeMpb(aggregation.LastValueKind, &testErrLastValue{errEx})

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, errEx))

	mpb, err = makeMpb(aggregation.MinMaxSumCountKind, &testErrMinMaxSumCount{testErrSum{errEx}})

	require.Error(t, err)
	require.Nil(t, mpb)
	require.True(t, errors.Is(err, errEx))
}
