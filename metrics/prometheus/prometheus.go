// Package prometheus exports ratelimit's metrics to Prometheus.
//
// It lives in its own module so that the root module keeps no external
// dependencies. You should never have to implement ratelimit.Metrics yourself:
// this hands you a filled-in one.
//
//	reg := prometheus.NewRegistry()
//	m, err := rlprom.New(reg, rlprom.Options{})
//	if err != nil { return err }
//	lim, err := ratelimit.NewWith(ratelimit.Config{Rules: rules, Metrics: m.Metrics()})
//
// # Cardinality
//
// Rule names are labels. The rule table is fixed when the limiter is built, so
// that label set is bounded and stable.
//
// Rate limit keys are never labels, and there is no way to make them labels.
// A key is client-controlled data, so using it as a label is the same unbounded
// cardinality failure as an unbounded key store, moved into your monitoring.
package prometheus

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/imlargo/ratelimit"
)

// Options configures the exporter.
type Options struct {
	// Namespace and Subsystem prefix every metric name. Subsystem defaults to
	// "ratelimit".
	Namespace string
	Subsystem string

	// LatencyBuckets for the decision latency histogram. Defaults to a spread
	// from 100ns to 1ms, which is the range a local decision lives in.
	//
	// Leave DecisionLatency off entirely if you do not want it: enabling it
	// costs two clock reads per request.
	LatencyBuckets []float64

	// DecisionLatency enables the decision latency histogram. Off by default,
	// because it is the only metric here that costs anything per request.
	DecisionLatency bool
}

// Exporter holds the collectors.
type Exporter struct {
	decisions   *prometheus.CounterVec
	denied      *prometheus.CounterVec
	shadow      *prometheus.CounterVec
	refunded    *prometheus.CounterVec
	saturated   *prometheus.CounterVec
	quotaFailed *prometheus.CounterVec
	evictions   prometheus.Counter
	degraded    prometheus.Gauge
	latency     prometheus.Histogram
	occupancy   prometheus.Gauge
	capacity    prometheus.Gauge
	syncSeconds *prometheus.HistogramVec
	syncCells   prometheus.Histogram

	opts Options
}

// New registers the collectors and returns an exporter.
func New(reg prometheus.Registerer, opts Options) (*Exporter, error) {
	if reg == nil {
		return nil, errors.New("ratelimit/prometheus: registerer is nil")
	}
	if opts.Subsystem == "" {
		opts.Subsystem = "ratelimit"
	}
	if len(opts.LatencyBuckets) == 0 {
		opts.LatencyBuckets = []float64{
			100e-9, 250e-9, 500e-9, 1e-6, 2.5e-6, 5e-6, 10e-6, 25e-6, 50e-6, 100e-6, 250e-6, 1e-3,
		}
	}

	n := func(name string) prometheus.Opts {
		return prometheus.Opts{Namespace: opts.Namespace, Subsystem: opts.Subsystem, Name: name}
	}
	counter := func(name, help string, labels ...string) *prometheus.CounterVec {
		o := n(name)
		o.Help = help
		return prometheus.NewCounterVec(prometheus.CounterOpts(o), labels)
	}

	e := &Exporter{opts: opts}

	e.decisions = counter("decisions_total",
		"Rate limit decisions, by outcome reason and by the rule the reported quota belongs to.", "reason", "rule")
	e.denied = counter("denied_total",
		"Requests denied because a rule's quota was exhausted, by rule.", "rule")
	e.shadow = counter("shadow_denied_total",
		"Requests a rule in shadow mode would have denied, by rule. Compare against denied_total to size a limit before turning it on.", "rule")
	e.refunded = counter("refunded_total",
		"Quota given back to a rule because a later rule denied the request, by rule.", "rule")
	e.saturated = counter("store_saturated_total",
		"New keys refused because every candidate cell held a key with quota still consumed. Anything but zero means the key store is too small.", "rule")
	e.quotaFailed = counter("quota_resolution_failed_total",
		"Times Rule.QuotaFor returned a quota that did not validate and the static quota was used instead, by rule.", "rule")

	e.evictions = prometheus.NewCounter(prometheus.CounterOpts(withHelp(n("store_evictions_total"),
		"Fully recovered cells recycled to make room. Steady eviction is normal.")))
	e.degraded = prometheus.NewGauge(prometheus.GaugeOpts(withHelp(n("degraded"),
		"1 when the remote backend is not answering and limits are enforced per process only.")))
	e.occupancy = prometheus.NewGauge(prometheus.GaugeOpts(withHelp(n("store_cells_used_local"),
		"Cells claimed in this process's key store. Local to this process, as the name says.")))
	e.capacity = prometheus.NewGauge(prometheus.GaugeOpts(withHelp(n("store_cells_capacity_local"),
		"Total cells in this process's key store.")))
	e.syncCells = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: opts.Namespace, Subsystem: opts.Subsystem, Name: "backend_sync_cells",
		Help:    "Keys carried in one backend synchronisation round.",
		Buckets: prometheus.ExponentialBuckets(1, 4, 8),
	})
	e.syncSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: opts.Namespace, Subsystem: opts.Subsystem, Name: "backend_sync_seconds",
		Help:    "Duration of one backend synchronisation round, by outcome.",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})

	cs := []prometheus.Collector{
		e.decisions, e.denied, e.shadow, e.refunded, e.saturated, e.quotaFailed,
		e.evictions, e.degraded, e.occupancy, e.capacity, e.syncCells, e.syncSeconds,
	}
	if opts.DecisionLatency {
		e.latency = prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: opts.Namespace, Subsystem: opts.Subsystem, Name: "decision_seconds_local",
			Help:    "Time spent inside a rate limit decision. Local to this process, as the name says.",
			Buckets: opts.LatencyBuckets,
		})
		cs = append(cs, e.latency)
	}
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func withHelp(o prometheus.Opts, help string) prometheus.Opts {
	o.Help = help
	return o
}

// Metrics returns the struct to hand to [ratelimit.Config].
func (e *Exporter) Metrics() ratelimit.Metrics {
	m := ratelimit.Metrics{
		Decision: func(reason ratelimit.Reason, rule string) {
			e.decisions.WithLabelValues(reason.String(), rule).Inc()
		},
		Denied:                func(rule string) { e.denied.WithLabelValues(rule).Inc() },
		ShadowDenied:          func(rule string) { e.shadow.WithLabelValues(rule).Inc() },
		Refunded:              func(rule string) { e.refunded.WithLabelValues(rule).Inc() },
		Saturated:             func(rule string) { e.saturated.WithLabelValues(rule).Inc() },
		QuotaResolutionFailed: func(rule string) { e.quotaFailed.WithLabelValues(rule).Inc() },
		Evicted:               func() { e.evictions.Inc() },
		DegradedChanged: func(d bool) {
			if d {
				e.degraded.Set(1)
			} else {
				e.degraded.Set(0)
			}
		},
		StoreOccupancyLocal: func(used, capacity int) {
			e.occupancy.Set(float64(used))
			e.capacity.Set(float64(capacity))
		},
		BackendSync: func(cells int, took time.Duration, err error) {
			e.syncCells.Observe(float64(cells))
			outcome := "ok"
			if err != nil {
				outcome = "error"
			}
			e.syncSeconds.WithLabelValues(outcome).Observe(took.Seconds())
		},
	}
	if e.latency != nil {
		m.DecisionLatencyLocal = func(d time.Duration) { e.latency.Observe(d.Seconds()) }
	}
	return m
}

// Observe pushes a [ratelimit.Stats] snapshot into the gauges. Call it from your
// own collector loop if you want store occupancy without a backend, since only
// the backend sync loop reports it on its own.
func (e *Exporter) Observe(s ratelimit.Stats) {
	e.occupancy.Set(float64(s.Occupied))
	e.capacity.Set(float64(s.Capacity))
	if s.Degraded {
		e.degraded.Set(1)
	} else {
		e.degraded.Set(0)
	}
}
