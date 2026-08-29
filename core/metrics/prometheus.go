package metrics

import (
	"context"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/singleton"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics 是 Prometheus 指标采集接口。
type Metrics interface {
	WriteInitialMetrics(ctx context.Context)
	WriteAfterScanMetrics(ctx context.Context, success bool)
	RecordRequest(ctx context.Context, endpoint, method, client string, status int32, elapsed int64)
	RecordPluginRequest(ctx context.Context, plugin, method string, ok bool, elapsed int64)
	GetHandler() http.Handler
}

type metrics struct {
	ds model.DataStore
}

// GetPrometheusInstance 返回指标实例。
// 未启用 Prometheus 时返回空实现，让调用方无需到处判断开关。
func GetPrometheusInstance(ds model.DataStore) Metrics {
	if !conf.Server.Prometheus.Enabled {
		return noopMetrics{}
	}

	return singleton.GetInstance(func() *metrics {
		return &metrics{ds: ds}
	})
}

// NewNoopInstance 返回不做任何采集的空实现（用于测试或禁用场景）。
func NewNoopInstance() Metrics {
	return noopMetrics{}
}

// WriteInitialMetrics 在启动时写入版本信息与数据库统计。
func (m *metrics) WriteInitialMetrics(ctx context.Context) {
	getPrometheusMetrics().versionInfo.With(prometheus.Labels{"version": consts.Version}).Set(1)
	processSqlAggregateMetrics(ctx, m.ds, getPrometheusMetrics().dbTotal)
}

// WriteAfterScanMetrics 在扫描结束后刷新统计，并记录本次扫描的时间与成败。
func (m *metrics) WriteAfterScanMetrics(ctx context.Context, success bool) {
	processSqlAggregateMetrics(ctx, m.ds, getPrometheusMetrics().dbTotal)

	scanLabels := prometheus.Labels{"success": strconv.FormatBool(success)}
	getPrometheusMetrics().lastMediaScan.With(scanLabels).SetToCurrentTime()
	getPrometheusMetrics().mediaScansCounter.With(scanLabels).Inc()
}

// RecordRequest 记录一次 HTTP 请求的计数与耗时。
// 耗时指标不带 status 标签，避免标签基数过大。
func (m *metrics) RecordRequest(_ context.Context, endpoint, method, client string, status int32, elapsed int64) {
	httpLabel := prometheus.Labels{
		"endpoint": endpoint,
		"method":   method,
		"client":   client,
		"status":   strconv.FormatInt(int64(status), 10),
	}
	getPrometheusMetrics().httpRequestCounter.With(httpLabel).Inc()

	httpLatencyLabel := prometheus.Labels{
		"endpoint": endpoint,
		"method":   method,
		"client":   client,
	}
	getPrometheusMetrics().httpRequestDuration.With(httpLatencyLabel).Observe(float64(elapsed))
}

// RecordPluginRequest 记录一次插件调用的计数与耗时。
func (m *metrics) RecordPluginRequest(_ context.Context, plugin, method string, ok bool, elapsed int64) {
	pluginLabel := prometheus.Labels{
		"plugin": plugin,
		"method": method,
		"ok":     strconv.FormatBool(ok),
	}
	getPrometheusMetrics().pluginRequestCounter.With(pluginLabel).Inc()

	pluginLatencyLabel := prometheus.Labels{
		"plugin": plugin,
		"method": method,
	}
	getPrometheusMetrics().pluginRequestDuration.With(pluginLatencyLabel).Observe(float64(elapsed))
}

// GetHandler 返回 /metrics 的 HTTP 处理器，配置了密码则加上 Basic Auth 保护。
func (m *metrics) GetHandler() http.Handler {
	r := chi.NewRouter()

	if conf.Server.Prometheus.Password != "" {
		r.Use(middleware.BasicAuth("metrics", map[string]string{
			consts.PrometheusAuthUser: conf.Server.Prometheus.Password,
		}))
	}

	// Enable created at timestamp to handle zero counter on create.
	// This requires --enable-feature=created-timestamp-zero-ingestion to be passed in Prometheus
	r.Handle("/", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{
		EnableOpenMetrics:                   true,
		EnableOpenMetricsTextCreatedSamples: true,
	}))
	return r
}

// prometheusMetrics 汇集所有指标收集器。
type prometheusMetrics struct {
	dbTotal               *prometheus.GaugeVec
	versionInfo           *prometheus.GaugeVec
	lastMediaScan         *prometheus.GaugeVec
	mediaScansCounter     *prometheus.CounterVec
	httpRequestCounter    *prometheus.CounterVec
	httpRequestDuration   *prometheus.SummaryVec
	pluginRequestCounter  *prometheus.CounterVec
	pluginRequestDuration *prometheus.SummaryVec
}

// Prometheus' metrics requires initialization. But not more than once
//
// getPrometheusMetrics 惰性创建并注册所有指标。
// 用 OnceValue 保证只注册一次——重复注册会 panic。
var getPrometheusMetrics = sync.OnceValue(func() *prometheusMetrics {
	quartilesToEstimate := map[float64]float64{0.5: 0.05, 0.75: 0.025, 0.9: 0.01, 0.99: 0.001}

	instance := &prometheusMetrics{
		dbTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "db_model_totals",
				Help: "Total number of DB items per model",
			},
			[]string{"model"},
		),
		versionInfo: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "navidrome_info",
				Help: "Information about Navidrome version",
			},
			[]string{"version"},
		),
		lastMediaScan: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "media_scan_last",
				Help: "Last media scan timestamp by success",
			},
			[]string{"success"},
		),
		mediaScansCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "media_scans",
				Help: "Total success media scans by success",
			},
			[]string{"success"},
		),
		httpRequestCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "http_request_count",
				Help: "Request types by status",
			},
			[]string{"endpoint", "method", "client", "status"},
		),
		httpRequestDuration: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "http_request_latency",
				Help:       "Latency (in ms) of HTTP requests",
				Objectives: quartilesToEstimate,
			},
			[]string{"endpoint", "method", "client"},
		),
		pluginRequestCounter: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "plugin_request_count",
				Help: "Plugin requests by method/status",
			},
			[]string{"plugin", "method", "ok"},
		),
		pluginRequestDuration: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "plugin_request_latency",
				Help:       "Latency (in ms) of plugin requests",
				Objectives: quartilesToEstimate,
			},
			[]string{"plugin", "method"},
		),
	}

	prometheus.DefaultRegisterer.MustRegister(
		instance.dbTotal,
		instance.versionInfo,
		instance.lastMediaScan,
		instance.mediaScansCounter,
		instance.httpRequestCounter,
		instance.httpRequestDuration,
		instance.pluginRequestCounter,
		instance.pluginRequestDuration,
	)

	return instance
})

// processSqlAggregateMetrics 统计各模型的记录总数写入 Gauge。
// 任一统计失败即返回：指标缺失好过写入不完整的数据。
func processSqlAggregateMetrics(ctx context.Context, ds model.DataStore, targetGauge *prometheus.GaugeVec) {
	albumsCount, err := ds.Album(ctx).CountAll()
	if err != nil {
		log.Warn("album CountAll error", err)
		return
	}
	targetGauge.With(prometheus.Labels{"model": "album"}).Set(float64(albumsCount))

	artistCount, err := ds.Artist(ctx).CountAll()
	if err != nil {
		log.Warn("artist CountAll error", err)
		return
	}
	targetGauge.With(prometheus.Labels{"model": "artist"}).Set(float64(artistCount))

	songsCount, err := ds.MediaFile(ctx).CountAll()
	if err != nil {
		log.Warn("media CountAll error", err)
		return
	}
	targetGauge.With(prometheus.Labels{"model": "media"}).Set(float64(songsCount))

	usersCount, err := ds.User(ctx).CountAll()
	if err != nil {
		log.Warn("user CountAll error", err)
		return
	}
	targetGauge.With(prometheus.Labels{"model": "user"}).Set(float64(usersCount))
}

// noopMetrics 是 Prometheus 关闭时使用的空实现。
type noopMetrics struct {
}

func (n noopMetrics) WriteInitialMetrics(context.Context) {}

func (n noopMetrics) WriteAfterScanMetrics(context.Context, bool) {}

func (n noopMetrics) RecordRequest(context.Context, string, string, string, int32, int64) {}

func (n noopMetrics) RecordPluginRequest(context.Context, string, string, bool, int64) {}

func (n noopMetrics) GetHandler() http.Handler { return nil }
