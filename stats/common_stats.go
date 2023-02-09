// Package stats provides methods and functionality to register, track, log,
// and StatsD-notify statistics that, for the most part, include "counter" and "latency" kinds.
/*
 * Copyright (c) 2018-2023, NVIDIA CORPORATION. All rights reserved.
 */
package stats

import (
	"encoding/json"
	"fmt"
	rfs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	ratomic "sync/atomic"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats/statsd"
	jsoniter "github.com/json-iterator/go"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	dfltPeriodicFlushTime = 40 * time.Second       // when `config.Log.FlushTime` == time.Duration(0)
	dfltPeriodicTimeStamp = time.Hour              // extended date/time complementary to log timestamps (e.g., "11:29:11.644596")
	dfltStatsLogInterval  = int64(time.Minute)     // when `config.Log.StatsTime` == time.Duration(0)
	maxStatsLogInterval   = int64(2 * time.Minute) // when the resulting `statsTime` is greater than
)

// more periodic
const (
	logsMaxSizeCheckTime = 48 * time.Minute       // periodically check the logs for max accumulated size
	startupSleep         = 300 * time.Millisecond // periodically poll ClusterStarted()
	numGorHighCheckTime  = 2 * time.Minute        // periodically log a warning if the number of goroutines remains high
)

// number-of-goroutines watermarks expressed as multipliers over the number of available logical CPUs (GOMAXPROCS)
const (
	numGorHigh    = 100
	numGorExtreme = 1000
)

// metrics
const (
	// KindCounter
	// all basic counters are accompanied by the corresponding (ErrPrefix + kind) error count:
	// "err.get.n", "err.put.n", etc.
	// see: `IncErr`, `regCommon`
	GetCount    = "get.n"    // object
	PutCount    = "put.n"    // ditto
	AppendCount = "append.n" // ditto
	DeleteCount = "del.n"    // ditto
	RenameCount = "ren.n"    // ditto
	ListCount   = "lst.n"    // list-objects

	// statically defined err counts (NOTE: update regCommon when adding/updating)
	ErrHTTPWriteCount = "err.http.write.n"
	ErrDownloadCount  = "err.dl.n"
	ErrPutMirrorCount = "err.put.mirror.n"

	// KindLatency
	GetLatency       = "get.ns"
	ListLatency      = "lst.ns"
	KeepAliveLatency = "kalive.ns"

	// KindSpecial
	Uptime = "up.ns.time"
)

// interface guard
var (
	_ Tracker = (*Prunner)(nil)
	_ Tracker = (*Trunner)(nil)
)

// sample name ais.ip-10-0-2-19.root.log.INFO.20180404-031540.2249
var logtypes = []string{".INFO.", ".WARNING.", ".ERROR."}

var ignoreIdle = []string{"kalive", Uptime, "disk."}

func ignore(s string) bool {
	for _, p := range ignoreIdle {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

//
// private types
//

type (
	metric = statsd.Metric // type alias

	// implemented by the stats runners
	statsLogger interface {
		log(now int64, uptime time.Duration, config *cmn.Config)
		doAdd(nv cos.NamedVal64)
		statsTime(newval time.Duration)
		standingBy() bool
	}
	runnerHost interface {
		ClusterStarted() bool
	}
	// implements Tracker, inherited by Prunner and Trunner
	statsRunner struct {
		daemon      runnerHost
		stopCh      chan struct{}
		workCh      chan cos.NamedVal64
		ticker      *time.Ticker
		Core        *CoreStats  `json:"core"`
		ctracker    copyTracker // to avoid making it at runtime
		name        string      // this stats-runner's name
		nextLogTime int64       // mono.NanoTime()
		startedUp   atomic.Bool
	}
	// Stats are tracked via a map of stats names (key) to statsValue (values).
	// There are two main types of stats: counter and latency declared
	// using the the kind field. Only latency stats have numSamples used to compute latency.
	statsValue struct {
		kind  string // enum { KindCounter, ..., KindSpecial }
		label struct {
			comm string // common part of the metric label (as in: <prefix> . comm . <suffix>)
			stsd string // StatsD label
			prom string // Prometheus label
		}
		Value      int64 `json:"v,string"`
		numSamples int64
		cumulative int64
		mu         sync.RWMutex
	}
	copyValue struct {
		Value int64 `json:"v,string"`
	}
	statsTracker map[string]*statsValue
	copyTracker  map[string]copyValue // NOTE: values aggregated and computed every (log) statsTime
	promDesc     map[string]*prometheus.Desc
)

///////////////
// CoreStats //
///////////////

// interface guard
var (
	_ json.Marshaler   = (*CoreStats)(nil)
	_ json.Unmarshaler = (*CoreStats)(nil)
)

// helper: convert bytes to megabytes with a fixed rounding precision = 2 digits (NOTE: MB not MiB)
func roundMBs(val int64) (mbs float64) {
	mbs = float64(val) / 1000 / 10
	num := int(mbs + 0.5)
	mbs = float64(num) / 100
	return
}

func (s *CoreStats) init(node *cluster.Snode, size int) {
	s.Tracker = make(statsTracker, size)
	s.promDesc = make(promDesc, size)

	// debug.NewExpvar & debug.SetExpvar could be placed here and elsewhere to visualize:
	//     * all counters including errors
	//     * latencies including keepalive
	//     * mountpath capacities
	//     * mountpath (disk) utilizations
	//     * total number of goroutines, etc.
	// (access via host:port/debug/vars in debug mode)

	s.Tracker.regCommon(node)

	// reusable sgl => (udp) => StatsD
	s.sgl = memsys.PageMM().NewSGL(memsys.PageSize)
}

// NOTE: nil StatsD client means that we provide metrics to Prometheus (see below)
func (s *CoreStats) isPrometheus() bool { return s.statsdC == nil }

// vs Collect()
func (s *CoreStats) promRLock() {
	if s.isPrometheus() {
		s.cmu.RLock()
	}
}

func (s *CoreStats) promRUnlock() {
	if s.isPrometheus() {
		s.cmu.RUnlock()
	}
}

func (s *CoreStats) promLock() {
	if s.isPrometheus() {
		s.cmu.Lock()
	}
}

func (s *CoreStats) promUnlock() {
	if s.isPrometheus() {
		s.cmu.Unlock()
	}
}

// init MetricClient client: StatsD (default) or Prometheus
func (s *CoreStats) initMetricClient(node *cluster.Snode, parent *statsRunner) {
	// Either Prometheus
	if prom := os.Getenv("AIS_PROMETHEUS"); prom != "" {
		glog.Infoln("Using Prometheus")
		prometheus.MustRegister(parent) // as prometheus.Collector
		return
	}

	// or StatsD
	var (
		port  = 8125  // StatsD default port, see https://github.com/etsy/stats
		probe = false // test-probe StatsD server at init time
	)
	if portStr := os.Getenv("AIS_STATSD_PORT"); portStr != "" {
		if portNum, err := cmn.ParsePort(portStr); err != nil {
			debug.AssertNoErr(err)
			glog.Error(err)
		} else {
			port = portNum
		}
	}
	if probeStr := os.Getenv("AIS_STATSD_PROBE"); probeStr != "" {
		if probeBool, err := cos.ParseBool(probeStr); err != nil {
			glog.Error(err)
		} else {
			probe = probeBool
		}
	}
	id := strings.ReplaceAll(node.ID(), ":", "_") // ":" delineates name and value for StatsD
	statsD, err := statsd.New("localhost", port, "ais"+node.Type()+"."+id, probe)
	if err != nil {
		glog.Errorf("Starting up without StatsD: %v", err)
	} else {
		glog.Infoln("Using StatsD")
	}
	s.statsdC = statsD
}

// populate *prometheus.Desc and statsValue.label.prom
// NOTE: naming; compare with statsTracker.register()
func (s *CoreStats) initProm(node *cluster.Snode) {
	if !s.isPrometheus() {
		return
	}
	id := strings.ReplaceAll(node.ID(), ".", "_")
	for name, v := range s.Tracker {
		label := strings.ReplaceAll(name, ".", "_")
		v.label.prom = strings.ReplaceAll(label, ":", "_")

		help := v.kind
		if strings.HasSuffix(v.label.prom, "_n") {
			help = "total number of operations"
		} else if strings.HasSuffix(v.label.prom, "_size") {
			help = "total size (MB)"
		} else if strings.HasSuffix(v.label.prom, "avg_rsize") {
			help = "average read size (bytes)"
		} else if strings.HasSuffix(v.label.prom, "avg_wsize") {
			help = "average write size (bytes)"
		} else if strings.HasSuffix(v.label.prom, "_ns") {
			v.label.prom = strings.TrimSuffix(v.label.prom, "_ns") + "_ms"
			help = "latency (milliseconds)"
		} else if strings.Contains(v.label.prom, "_ns_") {
			v.label.prom = strings.ReplaceAll(v.label.prom, "_ns_", "_ms_")
			if name == Uptime {
				v.label.prom = strings.ReplaceAll(v.label.prom, "_ns_", "")
				help = "uptime (seconds)"
			} else {
				help = "latency (milliseconds)"
			}
		} else if strings.HasSuffix(v.label.prom, "_bps") {
			v.label.prom = strings.TrimSuffix(v.label.prom, "_bps") + "_mbps"
			help = "throughput (MB/s)"
		}

		fullqn := prometheus.BuildFQName("ais", node.Type(), id+"_"+v.label.prom)
		s.promDesc[name] = prometheus.NewDesc(fullqn, help, nil /*variableLabels*/, nil /*constLabels*/)
	}
}

func (s *CoreStats) updateUptime(d time.Duration) {
	v := s.Tracker[Uptime]
	ratomic.StoreInt64(&v.Value, d.Nanoseconds())
}

func (s *CoreStats) MarshalJSON() ([]byte, error) { return jsoniter.Marshal(s.Tracker) }
func (s *CoreStats) UnmarshalJSON(b []byte) error { return jsoniter.Unmarshal(b, &s.Tracker) }

func (s *CoreStats) get(name string) (val int64) {
	v := s.Tracker[name]
	switch v.kind {
	case KindLatency, KindThroughput:
		v.mu.RLock()
		val = v.Value
		v.mu.RUnlock()
	default:
		val = ratomic.LoadInt64(&v.Value)
	}
	return
}

// NOTE naming convention: ".n" for the count and ".ns" for duration (nanoseconds)
func (s *CoreStats) doAdd(name, nameSuffix string, val int64) {
	v, ok := s.Tracker[name]
	debug.Assertf(ok, "invalid metric name %q", name)
	switch v.kind {
	case KindLatency:
		v.mu.Lock()
		v.numSamples++
		v.cumulative += val
		v.Value += val
		v.mu.Unlock()
	case KindThroughput:
		v.mu.Lock()
		v.cumulative += val
		v.Value += val
		v.mu.Unlock()
	case KindCounter:
		// NOTE: not locking (KindCounter isn't compound, making an exception to speed-up)
		ratomic.AddInt64(&v.Value, val)

		// - non-empty suffix forces an immediate Tx with no aggregation (see below);
		// - suffix is an arbitrary string that can be defined at runtime;
		// - e.g. usage: per-mountpath error counters.
		if !s.isPrometheus() && nameSuffix != "" {
			s.statsdC.Send(v.label.comm+"."+nameSuffix,
				1, metric{Type: statsd.Counter, Name: "count", Value: val})
		}
	default:
		debug.Assert(false, v.kind)
	}
}

// log + StatsD (Prometheus is done separately via `Collect`)
func (s *CoreStats) copyT(ctracker copyTracker, diskLowUtil ...int64) bool {
	idle := true
	s.sgl.Reset()
	for name, v := range s.Tracker {
		switch v.kind {
		case KindLatency:
			var lat int64
			v.mu.Lock()
			if v.numSamples > 0 {
				lat = v.Value / v.numSamples
				ctracker[name] = copyValue{lat}
				if !ignore(name) {
					idle = false
				}
			}
			v.Value = 0
			v.numSamples = 0
			v.mu.Unlock()
			// NOTE: ns => ms, and not reporting zeros
			millis := cos.DivRound(lat, int64(time.Millisecond))
			if !s.isPrometheus() && millis > 0 && strings.HasSuffix(name, ".ns") {
				s.statsdC.AppMetric(metric{Type: statsd.Timer, Name: v.label.stsd, Value: float64(millis)}, s.sgl)
			}
		case KindThroughput:
			var throughput int64
			v.mu.Lock()
			if v.Value > 0 {
				throughput = v.Value / cos.MaxI64(int64(s.statsTime.Seconds()), 1)
				ctracker[name] = copyValue{throughput}
				if !ignore(name) {
					idle = false
				}
				v.Value = 0
			}
			v.mu.Unlock()
			if !s.isPrometheus() && throughput > 0 {
				fv := roundMBs(throughput)
				s.statsdC.AppMetric(metric{Type: statsd.Gauge, Name: v.label.stsd, Value: fv}, s.sgl)
			}
		case KindComputedThroughput:
			if throughput := ratomic.SwapInt64(&v.Value, 0); throughput > 0 {
				ctracker[name] = copyValue{throughput}
				if !ignore(name) {
					idle = false
				}
				if !s.isPrometheus() {
					fv := roundMBs(throughput)
					s.statsdC.AppMetric(metric{Type: statsd.Gauge, Name: v.label.stsd, Value: fv}, s.sgl)
				}
			}
		case KindCounter:
			cnt := ratomic.LoadInt64(&v.Value)
			if cnt > 0 {
				if prev, ok := ctracker[name]; !ok || prev.Value != cnt {
					ctracker[name] = copyValue{cnt}
					if !ignore(name) {
						idle = false
					}
				} else {
					cnt = 0
				}
			}
			// StatsD iff changed
			if !s.isPrometheus() && cnt > 0 {
				if strings.HasSuffix(name, ".size") {
					// target only suffix
					metricType := statsd.Counter
					if v.label.comm == "dl" {
						metricType = statsd.PersistentCounter
					}
					fv := roundMBs(cnt)
					s.statsdC.AppMetric(metric{Type: metricType, Name: v.label.stsd, Value: fv}, s.sgl)
				} else {
					s.statsdC.AppMetric(metric{Type: statsd.Counter, Name: v.label.stsd, Value: cnt}, s.sgl)
				}
			}
		case KindGauge:
			val := ratomic.LoadInt64(&v.Value)
			ctracker[name] = copyValue{val}
			if !s.isPrometheus() {
				s.statsdC.AppMetric(metric{Type: statsd.Gauge, Name: v.label.stsd, Value: float64(val)}, s.sgl)
			}
			if isDiskUtilMetric(name) && val > diskLowUtil[0] {
				idle = false
			}
		default:
			ctracker[name] = copyValue{ratomic.LoadInt64(&v.Value)} // KindSpecial/KindDelta as is and wo/ lock
		}
	}
	if !s.isPrometheus() {
		s.statsdC.SendSGL(s.sgl)
	}
	return idle
}

// REST API what=stats query
// NOTE: not reporting zero counts
func (s *CoreStats) copyCumulative(ctracker copyTracker) {
	for name, v := range s.Tracker {
		switch v.kind {
		case KindLatency, KindThroughput:
			v.mu.RLock()
			ctracker[name] = copyValue{v.cumulative}
			v.mu.RUnlock()
		case KindCounter:
			if cnt := ratomic.LoadInt64(&v.Value); cnt > 0 {
				ctracker[name] = copyValue{cnt}
			}
		default: // KindSpecial, KindComputedThroughput, KindGauge
			ctracker[name] = copyValue{ratomic.LoadInt64(&v.Value)}
		}
	}
}

////////////////
// statsValue //
////////////////

// interface guard
var (
	_ json.Marshaler   = (*statsValue)(nil)
	_ json.Unmarshaler = (*statsValue)(nil)
)

func (v *statsValue) MarshalJSON() ([]byte, error) {
	var s string
	switch v.kind {
	case KindLatency, KindThroughput:
		v.mu.RLock()
		s = strconv.FormatInt(v.Value, 10)
		v.mu.RUnlock()
	default:
		s = strconv.FormatInt(ratomic.LoadInt64(&v.Value), 10)
	}
	return cos.UnsafeB(s), nil
}

func (v *statsValue) UnmarshalJSON(b []byte) error { return jsoniter.Unmarshal(b, &v.Value) }

///////////////
// copyValue //
///////////////

// interface guard
var (
	_ json.Marshaler   = (*copyValue)(nil)
	_ json.Unmarshaler = (*copyValue)(nil)
)

func (v copyValue) MarshalJSON() (b []byte, err error) { return jsoniter.Marshal(v.Value) }
func (v *copyValue) UnmarshalJSON(b []byte) error      { return jsoniter.Unmarshal(b, &v.Value) }

//////////////////
// statsTracker //
//////////////////

// NOTE: naming; compare with CoreStats.initProm()
func (tracker statsTracker) reg(node *cluster.Snode, name, kind string) {
	debug.Assertf(kind == KindCounter || kind == KindGauge || kind == KindLatency ||
		kind == KindThroughput || kind == KindComputedThroughput || kind == KindSpecial,
		"invalid metric kind %q", kind)

	v := &statsValue{kind: kind}
	// in StatsD metrics ":" delineates the name and the value - replace with underscore
	switch kind {
	case KindCounter:
		if strings.HasSuffix(name, ".size") {
			v.label.comm = strings.TrimSuffix(name, ".size")
			v.label.comm = strings.ReplaceAll(v.label.comm, ":", "_")
			v.label.stsd = fmt.Sprintf("%s.%s.%s.%s", "ais"+node.Type(), node.ID(), v.label.comm, "mbytes")
		} else {
			debug.Assert(strings.HasSuffix(name, ".n"), name)
			v.label.comm = strings.TrimSuffix(name, ".n")
			v.label.comm = strings.ReplaceAll(v.label.comm, ":", "_")
			v.label.stsd = fmt.Sprintf("%s.%s.%s.%s", "ais"+node.Type(), node.ID(), v.label.comm, "count")
		}
	case KindLatency:
		debug.Assert(strings.Contains(name, ".ns"), name)
		v.label.comm = strings.TrimSuffix(name, ".ns")
		v.label.comm = strings.ReplaceAll(v.label.comm, ".ns.", ".")
		v.label.comm = strings.ReplaceAll(v.label.comm, ":", "_")
		v.label.stsd = fmt.Sprintf("%s.%s.%s.%s", "ais"+node.Type(), node.ID(), v.label.comm, "ms")
	case KindThroughput, KindComputedThroughput:
		debug.Assert(strings.HasSuffix(name, ".bps"), name)
		v.label.comm = strings.TrimSuffix(name, ".bps")
		v.label.comm = strings.ReplaceAll(v.label.comm, ":", "_")
		v.label.stsd = fmt.Sprintf("%s.%s.%s.%s", "ais"+node.Type(), node.ID(), v.label.comm, "mbps")
	default:
		v.label.comm = name
		v.label.comm = strings.ReplaceAll(v.label.comm, ":", "_")
		if name == Uptime {
			v.label.comm = strings.ReplaceAll(v.label.comm, ".ns.", ".")
			v.label.stsd = fmt.Sprintf("%s.%s.%s.%s", "ais"+node.Type(), node.ID(), v.label.comm, "seconds")
		} else {
			v.label.stsd = fmt.Sprintf("%s.%s.%s", "ais"+node.Type(), node.ID(), v.label.comm)
		}
	}
	tracker[name] = v
}

// register common metrics; see RegMetrics() in target_stats.go
func (tracker statsTracker) regCommon(node *cluster.Snode) {
	// basic counters
	tracker.reg(node, GetCount, KindCounter)
	tracker.reg(node, PutCount, KindCounter)
	tracker.reg(node, AppendCount, KindCounter)
	tracker.reg(node, DeleteCount, KindCounter)
	tracker.reg(node, RenameCount, KindCounter)
	tracker.reg(node, ListCount, KindCounter)

	// basic error counters, respectively
	tracker.reg(node, ErrPrefix+GetCount, KindCounter)
	tracker.reg(node, ErrPrefix+PutCount, KindCounter)
	tracker.reg(node, ErrPrefix+AppendCount, KindCounter)
	tracker.reg(node, ErrPrefix+DeleteCount, KindCounter)
	tracker.reg(node, ErrPrefix+RenameCount, KindCounter)
	tracker.reg(node, ErrPrefix+ListCount, KindCounter)

	// more error counters
	tracker.reg(node, ErrHTTPWriteCount, KindCounter)
	tracker.reg(node, ErrDownloadCount, KindCounter)
	tracker.reg(node, ErrPutMirrorCount, KindCounter)

	// latency
	tracker.reg(node, GetLatency, KindLatency)
	tracker.reg(node, ListLatency, KindLatency)
	tracker.reg(node, KeepAliveLatency, KindLatency)

	// special uptime
	tracker.reg(node, Uptime, KindSpecial)
}

/////////////////
// statsRunner //
/////////////////

// interface guard
var (
	_ prometheus.Collector = (*statsRunner)(nil)
)

func (r *statsRunner) GetWhatStats() *DaemonStats {
	ctracker := make(copyTracker, 48)
	r.Core.copyCumulative(ctracker)
	return &DaemonStats{Tracker: ctracker}
}

func (r *statsRunner) GetMetricNames() cos.StrKVs {
	out := make(cos.StrKVs, 32)
	for name, v := range r.Core.Tracker {
		out[name] = v.kind
	}
	return out
}

//
// as cos.StatsUpdater
//

func (r *statsRunner) Add(name string, val int64) {
	r.workCh <- cos.NamedVal64{Name: name, Value: val}
}

func (r *statsRunner) Inc(name string) {
	r.workCh <- cos.NamedVal64{Name: name, Value: 1}
}

func (r *statsRunner) AddMany(nvs ...cos.NamedVal64) {
	for _, nv := range nvs {
		r.workCh <- nv
	}
}

func (r *statsRunner) IsPrometheus() bool { return r.Core.isPrometheus() }

func (r *statsRunner) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range r.Core.promDesc {
		ch <- desc
	}
}

func (r *statsRunner) Collect(ch chan<- prometheus.Metric) {
	if !r.StartedUp() {
		return
	}
	r.Core.promRLock()
	for name, v := range r.Core.Tracker {
		var (
			val int64
			fv  float64
		)
		copyV, okc := r.ctracker[name]
		if !okc {
			continue
		}
		val = copyV.Value
		fv = float64(val)
		// 1. convert units
		switch v.kind {
		case KindCounter:
			if strings.HasSuffix(name, ".size") {
				fv = roundMBs(val)
			}
		case KindLatency:
			millis := cos.DivRound(val, int64(time.Millisecond))
			fv = float64(millis)
		case KindThroughput:
			fv = roundMBs(val)
		default:
			if name == Uptime {
				seconds := cos.DivRound(val, int64(time.Second))
				fv = float64(seconds)
			}
		}
		// 2. convert kind
		promMetricType := prometheus.GaugeValue
		if v.kind == KindCounter {
			promMetricType = prometheus.CounterValue
		}
		// 3. publish
		desc, ok := r.Core.promDesc[name]
		debug.Assert(ok, name)
		m, err := prometheus.NewConstMetric(desc, promMetricType, fv)
		debug.AssertNoErr(err)
		ch <- m
	}
	r.Core.promRUnlock()
}

func (r *statsRunner) Name() string { return r.name }

func (r *statsRunner) CoreStats() *CoreStats       { return r.Core }
func (r *statsRunner) Get(name string) (val int64) { return r.Core.get(name) }

func (r *statsRunner) runcommon(logger statsLogger) error {
	var (
		i, j   time.Duration
		sleep  = startupSleep
		ticker = time.NewTicker(sleep)

		// NOTE: the maximum time we agree to wait for r.daemon.ClusterStarted()
		config   = cmn.GCO.Get()
		deadline = config.Timeout.JoinAtStartup.D()
	)
	if logger.standingBy() {
		deadline = 24 * time.Hour
	} else if deadline == 0 {
		deadline = 2 * config.Timeout.Startup.D()
	}
waitStartup:
	for {
		select {
		case <-r.workCh:
			// Drain workCh until the daemon (proxy or target) starts up.
		case <-r.stopCh:
			ticker.Stop()
			return nil
		case <-ticker.C:
			if r.daemon.ClusterStarted() {
				break waitStartup
			}
			if logger.standingBy() && sleep == startupSleep {
				sleep = config.Periodic.StatsTime.D()
				ticker.Reset(sleep)
				deadline = time.Hour
				continue
			}
			j += sleep
			if j > deadline {
				ticker.Stop()
				return cmn.ErrStartupTimeout
			}
			i += sleep
			if i > config.Timeout.Startup.D() && !logger.standingBy() {
				glog.Errorln("startup is taking unusually long time...")
				i = 0
			}
		}
	}
	ticker.Stop()

	config = cmn.GCO.Get()
	goMaxProcs := runtime.GOMAXPROCS(0)
	glog.Infof("Starting %s", r.Name())
	hk.Reg(r.Name()+"-logs"+hk.NameSuffix, recycleLogs, logsMaxSizeCheckTime)

	statsTime := config.Periodic.StatsTime.D() // (NOTE: not to confuse with config.Log.StatsTime)
	r.ticker = time.NewTicker(statsTime)
	r.startedUp.Store(true)
	var (
		checkNumGorHigh   int64
		startTime         = mono.NanoTime()
		lastGlogFlushTime = startTime
		lastDateTimestamp = startTime
	)
	for {
		select {
		case nv, ok := <-r.workCh:
			if ok {
				logger.doAdd(nv)
			}
		case <-r.ticker.C:
			now := mono.NanoTime()
			config = cmn.GCO.Get()
			logger.log(now, time.Duration(now-startTime) /*uptime*/, config)
			checkNumGorHigh = _whingeGoroutines(now, checkNumGorHigh, goMaxProcs)

			if statsTime != config.Periodic.StatsTime.D() {
				statsTime = config.Periodic.StatsTime.D()
				r.ticker.Reset(statsTime)
				logger.statsTime(statsTime)
			}
			now = mono.NanoTime()
			flushTime := dfltPeriodicFlushTime
			if config.Log.FlushTime != 0 {
				flushTime = config.Log.FlushTime.D()
			}
			if time.Duration(now-lastGlogFlushTime) > flushTime {
				glog.Flush()
				lastGlogFlushTime = mono.NanoTime()
			}
			if time.Duration(now-lastDateTimestamp) > dfltPeriodicTimeStamp {
				glog.Infoln(cos.FormatTime(time.Now(), "" /* RFC822 */) + " =============")
				lastDateTimestamp = now
			}
		case <-r.stopCh:
			r.ticker.Stop()
			return nil
		}
	}
}

func _whingeGoroutines(now, checkNumGorHigh int64, goMaxProcs int) int64 {
	var (
		ngr     = runtime.NumGoroutine()
		extreme bool
	)
	if ngr < goMaxProcs*numGorHigh {
		return 0
	}
	if ngr >= goMaxProcs*numGorExtreme {
		extreme = true
		glog.Errorf("Extremely high number of goroutines: %d", ngr)
	}
	if checkNumGorHigh == 0 {
		checkNumGorHigh = now
	} else if time.Duration(now-checkNumGorHigh) > numGorHighCheckTime {
		if !extreme {
			glog.Warningf("High number of goroutines: %d", ngr)
		}
		checkNumGorHigh = 0
	}
	return checkNumGorHigh
}

func (r *statsRunner) StartedUp() bool { return r.startedUp.Load() }

func (r *statsRunner) Stop(err error) {
	glog.Infof("Stopping %s, err: %v", r.Name(), err)
	r.stopCh <- struct{}{}
	if !r.IsPrometheus() {
		r.Core.statsdC.Close()
	}
	close(r.stopCh)
}

func recycleLogs() time.Duration {
	// keep total log size below the configured max
	go removeLogs(cmn.GCO.Get())
	return logsMaxSizeCheckTime
}

func removeLogs(config *cmn.Config) {
	maxtotal := int64(config.Log.MaxTotal)
	dentries, err := os.ReadDir(config.LogDir)
	if err != nil {
		glog.Errorf("GC logs: cannot read log dir %s, err: %v", config.LogDir, err)
		_ = cos.CreateDir(config.LogDir) // FIXME: (local non-containerized + kill/restart under test)
		return
	}
	for _, logtype := range logtypes {
		var tot int64
		finfos := make([]rfs.FileInfo, 0, len(dentries))
		for _, dent := range dentries {
			if dent.IsDir() || !dent.Type().IsRegular() {
				continue
			}
			if n := dent.Name(); !strings.Contains(n, ".log.") || !strings.Contains(n, logtype) {
				continue
			}
			if finfo, err := dent.Info(); err == nil {
				tot += finfo.Size()
				finfos = append(finfos, finfo)
			}
		}
		if tot > maxtotal {
			removeOlderLogs(tot, maxtotal, config.LogDir, logtype, finfos)
		}
	}
}

func removeOlderLogs(tot, maxtotal int64, logdir, logtype string, filteredInfos []rfs.FileInfo) {
	l := len(filteredInfos)
	if l <= 1 {
		glog.Warningf("GC logs: cannot cleanup %s, dir %s, tot %d, max %d", logtype, logdir, tot, maxtotal)
		return
	}
	fiLess := func(i, j int) bool {
		return filteredInfos[i].ModTime().Before(filteredInfos[j].ModTime())
	}
	if glog.FastV(4, glog.SmoduleStats) {
		glog.Infof("GC logs: started")
	}
	sort.Slice(filteredInfos, fiLess)
	filteredInfos = filteredInfos[:l-1] // except the last = current
	for _, logfi := range filteredInfos {
		logfqn := filepath.Join(logdir, logfi.Name())
		if err := cos.RemoveFile(logfqn); err == nil {
			tot -= logfi.Size()
			if glog.FastV(4, glog.SmoduleStats) {
				glog.Infof("GC logs: removed %s", logfqn)
			}
			if tot < maxtotal {
				break
			}
		} else {
			glog.Errorf("GC logs: failed to remove %s", logfqn)
		}
	}
	if glog.FastV(4, glog.SmoduleStats) {
		glog.Infof("GC logs: done")
	}
}

func (r *statsRunner) IncErr(metric string) {
	if strings.HasPrefix(metric, ErrPrefix) { // e.g. ErrHTTPWriteCount
		r.workCh <- cos.NamedVal64{Name: metric, Value: 1}
	} else { // e.g. "err." + GetCount
		r.workCh <- cos.NamedVal64{Name: ErrPrefix + metric, Value: 1}
	}
}
