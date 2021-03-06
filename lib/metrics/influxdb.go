package metrics

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"time"

	"github.com/Jeffail/benthos/v3/internal/docs"
	"github.com/Jeffail/benthos/v3/lib/log"
	btls "github.com/Jeffail/benthos/v3/lib/util/tls"
	client "github.com/influxdata/influxdb1-client/v2"
	"github.com/rcrowley/go-metrics"
)

func init() {
	Constructors[TypeInfluxDB] = TypeSpec{
		constructor: NewInfluxDB,
		Status:      docs.StatusExperimental,
		Version:     "3.36.0",
		Summary: `
Send metrics to InfluxDB 1.x using the ` + "`/write`" + ` endpoint.`,
		Description: `See https://docs.influxdata.com/influxdb/v1.8/tools/api/#write-http-endpoint for further details on the write API.`,
		FieldSpecs: docs.FieldSpecs{
			docs.FieldCommon("url", "A URL of the format `[https|http|udp]://host:port` to the InfluxDB host."),
			docs.FieldCommon("db", "The name of the database to use."),
			btls.FieldSpec(),
			docs.FieldAdvanced("username", "A username (when applicable)."),
			docs.FieldAdvanced("password", "A password (when applicable)."),
			docs.FieldAdvanced("include", "Optional additional metrics to collect, enabling these metrics may have some performance implications as it acquires a global semaphore and does `stoptheworld()`.").WithChildren(
				docs.FieldCommon("runtime", "A duration string indicating how often to poll and collect runtime metrics. Leave empty to disable this metric", "1m").HasDefault(""),
				docs.FieldCommon("debug_gc", "A duration string indicating how often to poll and collect GC metrics. Leave empty to disable this metric.", "1m").HasDefault(""),
			),
			docs.FieldAdvanced("interval", "A duration string indicating how often metrics should be flushed."),
			docs.FieldAdvanced("ping_interval", "A duration string indicating how often to ping the host."),
			docs.FieldAdvanced("precision", "[ns|us|ms|s] timestamp precision passed to write api."),
			docs.FieldAdvanced("timeout", "How long to wait for response for both ping and writing metrics."),
			docs.FieldAdvanced("tags", "Global tags added to each metric.",
				map[string]string{
					"hostname": "localhost",
					"zone":     "danger",
				},
			).Map(),
			docs.FieldAdvanced("retention_policy", "Sets the retention policy for each write."),
			docs.FieldAdvanced("write_consistency", "[any|one|quorum|all] sets write consistency when available."),
			pathMappingDocs(true, false),
		},
	}
}

// InfluxDBConfig is config for the influx metrics type.
type InfluxDBConfig struct {
	URL string `json:"url" yaml:"url"`
	DB  string `json:"db" yaml:"db"`

	TLS              btls.Config     `json:"tls" yaml:"tls"`
	Interval         string          `json:"interval" yaml:"interval"`
	Password         string          `json:"password" yaml:"password"`
	PingInterval     string          `json:"ping_interval" yaml:"ping_interval"`
	Precision        string          `json:"precision" yaml:"precision"`
	Timeout          string          `json:"timeout" yaml:"timeout"`
	Username         string          `json:"username" yaml:"username"`
	RetentionPolicy  string          `json:"retention_policy" yaml:"retention_policy"`
	WriteConsistency string          `json:"write_consistency" yaml:"write_consistency"`
	Include          InfluxDBInclude `json:"include" yaml:"include"`

	PathMapping string            `json:"path_mapping" yaml:"path_mapping"`
	Tags        map[string]string `json:"tags" yaml:"tags"`
}

// InfluxDBInclude contains configuration parameters for optional metrics to
// include.
type InfluxDBInclude struct {
	Runtime string `json:"runtime" yaml:"runtime"`
	DebugGC string `json:"debug_gc" yaml:"debug_gc"`
}

// NewInfluxDBConfig creates an InfluxDBConfig struct with default values.
func NewInfluxDBConfig() InfluxDBConfig {
	return InfluxDBConfig{
		URL: "",
		DB:  "",
		TLS: btls.NewConfig(),

		Precision:    "s",
		Interval:     "1m",
		PingInterval: "20s",
		Timeout:      "5s",
	}
}

// InfluxDB is the stats and client holder
type InfluxDB struct {
	client      client.Client
	batchConfig client.BatchPointsConfig

	interval     time.Duration
	pingInterval time.Duration
	timeout      time.Duration

	ctx    context.Context
	cancel func()

	pathMapping     *pathMapping
	registry        metrics.Registry
	runtimeRegistry metrics.Registry
	config          InfluxDBConfig
	log             log.Modular
}

// NewInfluxDB creates and returns a new InfluxDB object.
func NewInfluxDB(config Config, opts ...func(Type)) (Type, error) {
	i := &InfluxDB{
		config:          config.InfluxDB,
		registry:        metrics.NewRegistry(),
		runtimeRegistry: metrics.NewRegistry(),
		log:             log.Noop(),
	}

	i.ctx, i.cancel = context.WithCancel(context.Background())

	for _, opt := range opts {
		opt(i)
	}

	if config.InfluxDB.Include.Runtime != "" {
		metrics.RegisterRuntimeMemStats(i.runtimeRegistry)
		interval, err := time.ParseDuration(config.InfluxDB.Include.Runtime)
		if err != nil {
			return nil, fmt.Errorf("failed to parse interval: %s", err)
		}
		go metrics.CaptureRuntimeMemStats(i.runtimeRegistry, interval)
	}

	if config.InfluxDB.Include.DebugGC != "" {
		metrics.RegisterDebugGCStats(i.runtimeRegistry)
		interval, err := time.ParseDuration(config.InfluxDB.Include.DebugGC)
		if err != nil {
			return nil, fmt.Errorf("failed to parse interval: %s", err)
		}
		go metrics.CaptureDebugGCStats(i.runtimeRegistry, interval)
	}

	var err error
	if i.pathMapping, err = newPathMapping(config.InfluxDB.PathMapping, i.log); err != nil {
		return nil, fmt.Errorf("failed to init path mapping: %v", err)
	}

	if i.interval, err = time.ParseDuration(config.InfluxDB.Interval); err != nil {
		return nil, fmt.Errorf("failed to parse interval: %s", err)
	}

	if i.pingInterval, err = time.ParseDuration(config.InfluxDB.PingInterval); err != nil {
		return nil, fmt.Errorf("failed to parse ping interval: %s", err)
	}

	if i.timeout, err = time.ParseDuration(config.InfluxDB.Timeout); err != nil {
		return nil, fmt.Errorf("failed to parse timeout interval: %s", err)
	}

	if err = i.makeClient(); err != nil {
		return nil, err
	}

	i.batchConfig = client.BatchPointsConfig{
		Precision:        config.InfluxDB.Precision,
		Database:         config.InfluxDB.DB,
		RetentionPolicy:  config.InfluxDB.RetentionPolicy,
		WriteConsistency: config.InfluxDB.WriteConsistency,
	}

	go i.loop()

	return i, nil
}

func (i *InfluxDB) toCMName(dotSepName string) (string, []string, []string) {
	return i.pathMapping.mapPathWithTags(dotSepName)
}

func (i *InfluxDB) makeClient() error {
	var c client.Client
	u, err := url.Parse(i.config.URL)
	if err != nil {
		return fmt.Errorf("problem parsing url: %s", err)
	}

	if u.Scheme == "https" {
		tlsConfig := &tls.Config{}
		if i.config.TLS.Enabled {
			tlsConfig, err = i.config.TLS.Get()
			if err != nil {
				return err
			}
		}
		c, err = client.NewHTTPClient(client.HTTPConfig{
			Addr:      u.String(),
			TLSConfig: tlsConfig,
			Username:  i.config.Username,
			Password:  i.config.Password,
		})
	} else if u.Scheme == "http" {
		c, err = client.NewHTTPClient(client.HTTPConfig{
			Addr:     u.String(),
			Username: i.config.Username,
			Password: i.config.Password,
		})
	} else if u.Scheme == "udp" {
		c, err = client.NewUDPClient(client.UDPConfig{
			Addr: u.Host,
		})
	} else {
		return fmt.Errorf("protocol needs to be http, https or udp and is %s", u.Scheme)
	}

	if err == nil {
		i.client = c
	}
	return err
}

func (i *InfluxDB) loop() {
	ticker := time.NewTicker(i.interval)
	pingTicker := time.NewTicker(i.pingInterval)
	defer ticker.Stop()
	defer pingTicker.Stop()
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-ticker.C:
			if err := i.publishRegistry(); err != nil {
				i.log.Errorf("failed to send metrics data: %s", err)
			}
		case <-pingTicker.C:
			_, _, err := i.client.Ping(i.timeout)
			if err != nil {
				i.log.Warnf("unable to ping influx endpoint: %s", err)
				if err = i.makeClient(); err != nil {
					i.log.Errorf("unable to recreate client: %s", err)
				}
			}
		}
	}
}

func (i *InfluxDB) publishRegistry() error {
	points, err := client.NewBatchPoints(i.batchConfig)
	if err != nil {
		return fmt.Errorf("problem creating batch points for influx: %s", err)
	}
	now := time.Now()
	all := i.getAllMetrics()
	for k, v := range all {
		name, normalTags := decodeInfluxDBName(k)
		tags := make(map[string]string, len(i.config.Tags)+len(normalTags))
		// apply normal tags
		for k, v := range normalTags {
			tags[k] = v
		}
		// override with any global
		for k, v := range i.config.Tags {
			tags[k] = v
		}
		p, err := client.NewPoint(name, tags, v, now)
		if err != nil {
			i.log.Debugf("problem formatting metrics on %s: %s", name, err)
		} else {
			points.AddPoint(p)
		}
	}

	return i.client.Write(points)
}

func getMetricValues(i interface{}) map[string]interface{} {
	var values map[string]interface{}
	switch metric := i.(type) {
	case metrics.Counter:
		values = make(map[string]interface{}, 1)
		values["count"] = metric.Count()
	case metrics.Gauge:
		values = make(map[string]interface{}, 1)
		values["value"] = metric.Value()
	case metrics.GaugeFloat64:
		values = make(map[string]interface{}, 1)
		values["value"] = metric.Value()
	case metrics.Timer:
		values = make(map[string]interface{}, 14)
		t := metric.Snapshot()
		ps := t.Percentiles([]float64{0.5, 0.75, 0.95, 0.99, 0.999})
		values["count"] = t.Count()
		values["min"] = t.Min()
		values["max"] = t.Max()
		values["mean"] = t.Mean()
		values["stddev"] = t.StdDev()
		values["p50"] = ps[0]
		values["p75"] = ps[1]
		values["p95"] = ps[2]
		values["p99"] = ps[3]
		values["p999"] = ps[4]
		values["1m.rate"] = t.Rate1()
		values["5m.rate"] = t.Rate5()
		values["15m.rate"] = t.Rate15()
		values["mean.rate"] = t.RateMean()
	case metrics.Histogram:
		values = make(map[string]interface{}, 10)
		t := metric.Snapshot()
		ps := t.Percentiles([]float64{0.5, 0.75, 0.95, 0.99, 0.999})
		values["count"] = t.Count()
		values["min"] = t.Min()
		values["max"] = t.Max()
		values["mean"] = t.Mean()
		values["stddev"] = t.StdDev()
		values["p50"] = ps[0]
		values["p75"] = ps[1]
		values["p95"] = ps[2]
		values["p99"] = ps[3]
		values["p999"] = ps[4]
	}
	return values
}

func (i *InfluxDB) getAllMetrics() map[string]map[string]interface{} {
	data := make(map[string]map[string]interface{})
	i.registry.Each(func(name string, metric interface{}) {
		values := getMetricValues(metric)
		data[name] = values
	})
	i.runtimeRegistry.Each(func(name string, metric interface{}) {
		pathMappedName := i.pathMapping.mapPathNoTags(name)
		values := getMetricValues(metric)
		data[pathMappedName] = values
	})
	return data
}

// GetCounter returns a stat counter object for a path.
func (i *InfluxDB) GetCounter(path string) StatCounter {
	name, labels, values := i.toCMName(path)
	if len(name) == 0 {
		return DudStat{}
	}
	encodedName := encodeInfluxDBName(name, labels, values)
	return i.registry.GetOrRegister(encodedName, func() metrics.Counter {
		return influxDBCounter{
			metrics.NewCounter(),
		}
	}).(influxDBCounter)
}

// GetCounterVec returns a stat counter object for a path with the labels
func (i *InfluxDB) GetCounterVec(path string, n []string) StatCounterVec {
	name, labels, values := i.toCMName(path)
	if len(name) == 0 {
		return fakeCounterVec(func([]string) StatCounter {
			return DudStat{}
		})
	}
	labels = append(labels, n...)
	return &fCounterVec{
		f: func(l []string) StatCounter {
			v := append(values, l...)
			encodedName := encodeInfluxDBName(path, labels, v)
			return i.registry.GetOrRegister(encodedName, func() metrics.Counter {
				return influxDBCounter{
					metrics.NewCounter(),
				}
			}).(influxDBCounter)
		},
	}
}

// GetTimer returns a stat timer object for a path.
func (i *InfluxDB) GetTimer(path string) StatTimer {
	name, labels, values := i.toCMName(path)
	if len(name) == 0 {
		return DudStat{}
	}
	encodedName := encodeInfluxDBName(name, labels, values)
	return i.registry.GetOrRegister(encodedName, func() metrics.Timer {
		return influxDBTimer{
			metrics.NewTimer(),
		}
	}).(influxDBTimer)
}

// GetTimerVec returns a stat timer object for a path with the labels
func (i *InfluxDB) GetTimerVec(path string, n []string) StatTimerVec {
	name, labels, values := i.toCMName(path)
	if len(name) == 0 {
		return fakeTimerVec(func([]string) StatTimer {
			return DudStat{}
		})
	}
	labels = append(labels, n...)
	return &fTimerVec{
		f: func(l []string) StatTimer {
			v := append(values, l...)
			encodedName := encodeInfluxDBName(name, labels, v)
			return i.registry.GetOrRegister(encodedName, func() metrics.Timer {
				return influxDBTimer{
					metrics.NewTimer(),
				}
			}).(influxDBTimer)
		},
	}
}

// GetGauge returns a stat gauge object for a path.
func (i *InfluxDB) GetGauge(path string) StatGauge {
	name, labels, values := i.toCMName(path)
	if len(name) == 0 {
		return DudStat{}
	}
	encodedName := encodeInfluxDBName(name, labels, values)
	var result = i.registry.GetOrRegister(encodedName, func() metrics.Gauge {
		return influxDBGauge{
			metrics.NewGauge(),
		}
	}).(influxDBGauge)
	return result
}

// GetGaugeVec returns a stat timer object for a path with the labels
func (i *InfluxDB) GetGaugeVec(path string, n []string) StatGaugeVec {
	name, labels, values := i.toCMName(path)
	if len(name) == 0 {
		return fakeGaugeVec(func([]string) StatGauge {
			return DudStat{}
		})
	}
	labels = append(labels, n...)
	return &fGaugeVec{
		f: func(l []string) StatGauge {
			v := append(values, l...)
			encodedName := encodeInfluxDBName(name, labels, v)
			return i.registry.GetOrRegister(encodedName, func() metrics.Gauge {
				return influxDBGauge{
					metrics.NewGauge(),
				}
			}).(influxDBGauge)
		},
	}
}

// SetLogger sets the logger used to print connection errors.
func (i *InfluxDB) SetLogger(log log.Modular) {
	i.log = log
}

// Close reports metrics one last time and stops the InfluxDB object and closes the underlying client connection
func (i *InfluxDB) Close() error {
	if err := i.publishRegistry(); err != nil {
		i.log.Errorf("failed to send metrics data: %s", err)
	}
	i.client.Close()
	return nil
}
