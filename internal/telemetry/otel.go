package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Options configures the OpenTelemetry exporters.
//
// Off is the default and has to be, because everything else in this product is
// arranged so that a default install talks to nobody — the same reason
// llm.provider is "none". An OTLP endpoint is a network egress, and one that
// switched itself on from an ambient OTEL_EXPORTER_OTLP_ENDPOINT would be
// Veritix deciding, on a machine holding data the customer will not send to a
// vendor, to start sending somewhere.
//
// Once it is on, the standard OTEL_EXPORTER_OTLP_* variables are honored by
// the exporters themselves, so an operator's existing collector configuration
// works. Enabling is Veritix's switch; where to send is theirs.
type Options struct {
	// Enabled turns the exporters on. Nothing is exported without it.
	Enabled bool
	// Endpoint is the collector's OTLP/HTTP base URL, e.g.
	// http://localhost:4318. Empty defers to OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string
	// ServiceName names this instance in the collector.
	ServiceName string
	// Version is the build being reported, for the resource.
	Version string
	// SampleRatio is the fraction of traces recorded, 0 to 1. An audit is a
	// minutes-long operation a person asked for, so the useful default is 1:
	// sampling exists for request floods, and there is no flood here.
	SampleRatio float64
	// ExportTimeout bounds one export attempt and the shutdown flush. A
	// collector that has gone away must not hold up the end of an audit.
	ExportTimeout time.Duration
}

// scope names the instrumentation, which is what a collector groups by. It is
// the module path so that two builds of Veritix are the same instrument.
const scope = "github.com/russellw/veritix"

// Telemetry owns the exporters and is shut down with the process.
type Telemetry struct {
	tracers  *sdktrace.TracerProvider
	meters   *sdkmetric.MeterProvider
	timeout  time.Duration
	Disabled bool
}

// Start installs the global tracer and meter providers.
//
// The providers are global rather than injected, which is the exception to how
// the rest of this repo passes its dependencies. The reason is that
// OpenTelemetry's global is a delegating no-op until something sets it: every
// package can hold a package-level tracer, an unconfigured build pays nothing
// for the spans it starts, and audit.Run does not grow a parameter that exists
// only for observability. A test installs its own provider the same way Start
// does — see otel_test.go, which is what actually holds the attribute policy
// below to its promise.
//
// **What may go in a span, and what may not.** Attributes carry counts,
// durations, severities, stage names, tool names, provider and model
// identifiers, and the run id. They must never carry a table name, a column
// name, a file path, SQL text, a model's prose, or a cell value. A span is an
// access log that leaves the machine, and the argument is finding.Finding.ID's
// argument one step further out: the schema of a customer's export is itself
// commercially sensitive, and a collector is frequently somebody else's
// service. TestNoSpanCarriesCustomerData runs a real audit of the fixtures and
// scans every exported span for exactly that.
func Start(ctx context.Context, opts Options) (*Telemetry, error) {
	if !opts.Enabled {
		return &Telemetry{Disabled: true}, nil
	}
	if opts.ExportTimeout <= 0 {
		opts.ExportTimeout = 10 * time.Second
	}
	if opts.SampleRatio <= 0 {
		opts.SampleRatio = 1
	}

	// The semconv version has to be the one resource.Default() was built with,
	// or Merge refuses the pair with a schema conflict — which is a startup
	// error rather than something anybody would notice in review. Bump this
	// with the SDK.
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(opts.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: describing this service: %w", err)
	}

	traceOpts := []otlptracehttp.Option{otlptracehttp.WithTimeout(opts.ExportTimeout)}
	metricOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithTimeout(opts.ExportTimeout)}
	if opts.Endpoint != "" {
		u, err := url.Parse(opts.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("telemetry: parsing the endpoint: %w", err)
		}
		traceOpts = append(traceOpts, otlptracehttp.WithEndpointURL(signalURL(u, "traces")))
		metricOpts = append(metricOpts, otlpmetrichttp.WithEndpointURL(signalURL(u, "metrics")))
		if u.Scheme == "http" {
			// Plain HTTP has to be asked for explicitly, and the scheme is that
			// request. A collector inside the cluster is the ordinary case and
			// is reached over http; anything crossing a network should not be.
			traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
			metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
		}
	}

	spanExp, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: starting the trace exporter: %w", err)
	}
	metricExp, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: starting the metric exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(spanExp, sdktrace.WithExportTimeout(opts.ExportTimeout)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.SampleRatio))),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)

	return &Telemetry{tracers: tp, meters: mp, timeout: opts.ExportTimeout}, nil
}

// Shutdown flushes what has been recorded and stops exporting.
//
// It takes its own timeout rather than the caller's context alone: this runs
// while a process is exiting, often because somebody pressed Ctrl-C, and a
// collector that has gone away must not turn a clean exit into a hang.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil || t.Disabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	var errs []error
	if t.tracers != nil {
		errs = append(errs, t.tracers.Shutdown(ctx))
	}
	if t.meters != nil {
		errs = append(errs, t.meters.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// signalURL turns a collector's base URL into the URL for one signal.
//
// otel.endpoint is a base — "http://collector:4318" — because that is what
// OTEL_EXPORTER_OTLP_ENDPOINT means and what an operator will paste in. The
// exporter's WithEndpointURL wants the full signal URL, so the "/v1/traces"
// half is added here rather than being a thing every operator has to know. A
// URL that already names a path is left alone, for a collector behind a proxy
// that mounts it somewhere else.
func signalURL(u *url.URL, signal string) string {
	if p := strings.Trim(u.Path, "/"); p != "" {
		return u.String()
	}
	out := *u
	out.Path = "/v1/" + signal
	return out.String()
}

// Tracer returns the tracer every instrumented package uses. Before Start it
// is the no-op the OpenTelemetry global begins as.
func Tracer() trace.Tracer { return otel.Tracer(scope) }

// Meter returns the meter every instrumented package uses.
func Meter() metric.Meter { return otel.Meter(scope) }

// Instruments are the metrics Veritix records. They are created once, against
// the global meter, which delegates: an instrument built before Start still
// records after it.
type Instruments struct {
	// Runs counts audits, labeled by outcome.
	Runs metric.Int64Counter
	// RunDuration is how long an audit took, in seconds.
	RunDuration metric.Float64Histogram
	// Findings counts what audits reported, labeled by severity and origin.
	Findings metric.Int64Counter
	// AgentSteps is how many model turns an agentic run used.
	AgentSteps metric.Int64Histogram
	// AgentTokens counts tokens, labeled by direction.
	AgentTokens metric.Int64Counter
}

// Metrics returns Veritix's instruments, built once.
//
// They can be built before Start, because the global meter delegates: an
// instrument created against the no-op meter records through the real one
// after Start installs it. That is what lets an instrumented package hold them
// without a configuration parameter reaching it.
func Metrics() Instruments {
	metricsOnce.Do(func() { metrics = newInstruments() })
	return metrics
}

var (
	metricsOnce sync.Once
	metrics     Instruments
)

func newInstruments() Instruments {
	m := Meter()
	var in Instruments
	// An instrument that cannot be created is a programming error in its own
	// definition, not a runtime condition, and losing a metric must not fail
	// an audit. The API returns a working no-op alongside the error.
	in.Runs, _ = m.Int64Counter("veritix.audit.runs",
		metric.WithDescription("Audits completed."))
	in.RunDuration, _ = m.Float64Histogram("veritix.audit.duration",
		metric.WithDescription("How long an audit took."), metric.WithUnit("s"))
	in.Findings, _ = m.Int64Counter("veritix.audit.findings",
		metric.WithDescription("Findings reported, by severity and origin."))
	in.AgentSteps, _ = m.Int64Histogram("veritix.agent.steps",
		metric.WithDescription("Model turns used by an agentic audit."))
	in.AgentTokens, _ = m.Int64Counter("veritix.agent.tokens",
		metric.WithDescription("Tokens spent, by direction."), metric.WithUnit("{token}"))
	return in
}

// Attribute keys. They are collected here so that adding one is a deliberate
// act with the policy in Start's comment in front of you, rather than a string
// literal typed at the call site.
const (
	AttrStage     = attribute.Key("veritix.stage")
	AttrOutcome   = attribute.Key("veritix.outcome")
	AttrSeverity  = attribute.Key("veritix.severity")
	AttrOrigin    = attribute.Key("veritix.origin")
	AttrDirection = attribute.Key("veritix.direction")
	AttrTool      = attribute.Key("veritix.tool")
	AttrProvider  = attribute.Key("veritix.llm.provider")
	AttrModel     = attribute.Key("veritix.llm.model")
	AttrStopped   = attribute.Key("veritix.agent.stopped")
	AttrRoute     = attribute.Key("veritix.http.route")
)

// SanitizeEndpoint reports whether an endpoint is one this build will export
// to, with the same reasoning config.Validate applies to server.source_url: a
// value that is not http or https is a configuration mistake worth catching at
// startup rather than a connection failure ten minutes into a run.
func SanitizeEndpoint(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("must be a URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return errors.New(`must be an http:// or https:// URL, e.g. "http://localhost:4318"`)
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}
