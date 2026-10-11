package ingest

import (
	"hash/fnv"
	"math"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/sean-brydon/leadline/internal/store"
)

// noRecordedValue is the data point flag for "this series has stopped":
// the point carries no value.
const noRecordedValue = uint32(metricspb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK)

// Metrics flattens a metrics export to one row per data point. Values stay
// as sent: turning running totals into increases needs the points before,
// which the store has.
func Metrics(req *colmetrics.ExportMetricsServiceRequest) []store.MetricPoint {
	var out []store.MetricPoint
	for _, rm := range req.GetResourceMetrics() {
		service := serviceName(rm.GetResource())
		// Two processes of one service send the same names and attributes;
		// the whole resource (instance id, pid) is what tells them apart.
		resource := toJSON(attrMap(rm.GetResource().GetAttributes()))
		for _, sm := range rm.GetScopeMetrics() {
			scope := sm.GetScope().GetName()
			for _, m := range sm.GetMetrics() {
				base := store.MetricPoint{
					Name:        m.GetName(),
					Unit:        m.GetUnit(),
					Description: m.GetDescription(),
					Service:     service,
				}
				// add finishes a point and keeps it, unless it has no time
				// or says it has no value.
				add := func(p store.MetricPoint, attrs string, ts, start uint64, flags uint32) {
					if ts == 0 || flags&noRecordedValue != 0 {
						return
					}
					p.Attrs, p.TsNs, p.StartNs = attrs, int64(ts), int64(start)
					p.Series = seriesID(resource, scope, p.Name, p.Type, attrs)
					out = append(out, p)
				}

				switch data := m.GetData().(type) {
				case *metricspb.Metric_Gauge:
					base.Type = store.MetricGauge
					for _, dp := range data.Gauge.GetDataPoints() {
						p := base
						p.Value = number(dp)
						add(p, toJSON(attrMap(dp.GetAttributes())), dp.GetTimeUnixNano(), dp.GetStartTimeUnixNano(), dp.GetFlags())
					}
				case *metricspb.Metric_Sum:
					base.Type = store.MetricSum
					base.Cumulative = cumulative(data.Sum.GetAggregationTemporality())
					base.Monotonic = data.Sum.GetIsMonotonic()
					for _, dp := range data.Sum.GetDataPoints() {
						p := base
						p.Value = number(dp)
						add(p, toJSON(attrMap(dp.GetAttributes())), dp.GetTimeUnixNano(), dp.GetStartTimeUnixNano(), dp.GetFlags())
					}
				case *metricspb.Metric_Histogram:
					base.Type = store.MetricHistogram
					base.Cumulative = cumulative(data.Histogram.GetAggregationTemporality())
					for _, dp := range data.Histogram.GetDataPoints() {
						p := base
						p.Count, p.Sum = int64(dp.GetCount()), dp.GetSum()
						// A histogram may come with a count and no buckets.
						if n := len(dp.GetBucketCounts()); n > 0 && n == len(dp.GetExplicitBounds())+1 {
							p.Bounds = dp.GetExplicitBounds()
							p.Counts = make([]int64, n)
							for i, c := range dp.GetBucketCounts() {
								p.Counts[i] = int64(c)
							}
						}
						add(p, toJSON(attrMap(dp.GetAttributes())), dp.GetTimeUnixNano(), dp.GetStartTimeUnixNano(), dp.GetFlags())
					}
				case *metricspb.Metric_ExponentialHistogram:
					// Stored as an ordinary histogram, so one chart and one
					// percentile estimate serve both.
					base.Type = store.MetricHistogram
					base.Cumulative = cumulative(data.ExponentialHistogram.GetAggregationTemporality())
					for _, dp := range data.ExponentialHistogram.GetDataPoints() {
						p := base
						p.Count, p.Sum = int64(dp.GetCount()), dp.GetSum()
						p.Bounds, p.Counts = explicitBuckets(dp)
						add(p, toJSON(attrMap(dp.GetAttributes())), dp.GetTimeUnixNano(), dp.GetStartTimeUnixNano(), dp.GetFlags())
					}
				case *metricspb.Metric_Summary:
					// A summary's count and sum are running totals, which
					// chart like a histogram without buckets. Its quantiles
					// are computed in the app over a window Leadline can't
					// know, so they can't be re-bucketed and are left out.
					base.Type = store.MetricSummary
					base.Cumulative = true
					for _, dp := range data.Summary.GetDataPoints() {
						p := base
						p.Count, p.Sum = int64(dp.GetCount()), dp.GetSum()
						add(p, toJSON(attrMap(dp.GetAttributes())), dp.GetTimeUnixNano(), dp.GetStartTimeUnixNano(), dp.GetFlags())
					}
				}
			}
		}
	}
	return out
}

// cumulative reports whether points are running totals. An SDK must say
// which it sends; the ones that don't, send totals.
func cumulative(t metricspb.AggregationTemporality) bool {
	return t != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
}

func number(dp *metricspb.NumberDataPoint) float64 {
	if v, ok := dp.GetValue().(*metricspb.NumberDataPoint_AsInt); ok {
		return float64(v.AsInt)
	}
	return dp.GetAsDouble()
}

func seriesID(parts ...string) int64 {
	h := fnv.New64a()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return int64(h.Sum64())
}

// explicitBuckets rewrites an exponential histogram's buckets as upper
// bounds and counts, lowest first, with a last count for "above them all".
// Bucket i of scale s covers (base^i, base^(i+1)] where base = 2^(2^-s);
// the negative range mirrors it.
func explicitBuckets(dp *metricspb.ExponentialHistogramDataPoint) ([]float64, []int64) {
	factor := math.Ldexp(1, -int(dp.GetScale()))
	bound := func(index int32) float64 { return math.Exp2(float64(index) * factor) }
	neg, pos := dp.GetNegative(), dp.GetPositive()
	n := len(neg.GetBucketCounts()) + len(pos.GetBucketCounts())
	if n == 0 && dp.GetZeroCount() == 0 {
		return nil, nil
	}
	bounds := make([]float64, 0, n+1)
	counts := make([]int64, 0, n+2)
	for k := len(neg.GetBucketCounts()) - 1; k >= 0; k-- {
		bounds = append(bounds, -bound(neg.GetOffset()+int32(k)))
		counts = append(counts, int64(neg.GetBucketCounts()[k]))
	}
	// The values too close to zero to bucket. Their bucket ends where the
	// positive ones begin, so the first of those keeps its lower edge.
	zero := dp.GetZeroThreshold()
	if len(pos.GetBucketCounts()) > 0 {
		zero = bound(pos.GetOffset())
	}
	bounds = append(bounds, zero)
	counts = append(counts, int64(dp.GetZeroCount()))
	for k, c := range pos.GetBucketCounts() {
		bounds = append(bounds, bound(pos.GetOffset()+int32(k)+1))
		counts = append(counts, int64(c))
	}
	return bounds, append(counts, 0)
}
