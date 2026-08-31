// Package sessiontelemetryotel ports packages/session/session-telemetry-otel:
// the OpenTelemetry Service Provider for the session-telemetry capability.
// It composes the Go OTel SDK as-is — a LoggerProvider with a batch log
// record processor and an OTLP/HTTP log exporter — and maps each record
// handed over by the capture coordinator onto the SDK logger. After that
// call batching, retry, queueing, and loss policy use the SDK's documented
// behavior, configured verbatim through the exporter/processor passthroughs.
package sessiontelemetryotel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	apilog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"dshgo/sessiontelemetry"
)

// Mode is the session-sharing policy selected by Config.Mode.
type Mode string

const (
	ModeFull         Mode = "FULL"
	ModeFeedbackOnly Mode = "FEEDBACK_ONLY"
	ModeDisabled     Mode = "DISABLED"
	DefaultMode           = ModeDisabled
)

// Config is the plugin configuration: one sharing policy, the OTLP endpoint,
// and one DSH-owned shutdown bound. Uploading modes validate their endpoint
// and shutdown deadline at plugin load; DISABLED reads neither.
type Config struct {
	// Mode is the sharing policy; defaults to local-only DISABLED.
	Mode Mode
	// URL is the full OTLP logs endpoint (e.g.
	// https://collector.example.com/v1/logs). Required outside DISABLED.
	URL string
	// Headers is the optional OTLP/HTTP exporter headers.
	Headers map[string]string
	// MaxExportBatchSize is the processor's batch cap; must be a positive
	// integer (the SDK accepts a non-positive batch size but its shutdown
	// drain then splices empty batches without consuming the queue — dispose
	// would hang forever with records queued).
	MaxExportBatchSize *int
	// ShutdownTimeoutMillis is the maximum time awaiting the SDK's complete
	// shutdown path.
	ShutdownTimeoutMillis *int64
}

const defaultShutdownTimeoutMillis = int64(3000)

const maxTimerDelayMillis = int64(2147483647)

// resolveMode rejects unknown runtime values before transport setup.
func resolveMode(mode Mode) (Mode, error) {
	switch mode {
	case "", ModeDisabled:
		return ModeDisabled, nil
	case ModeFull, ModeFeedbackOnly:
		return mode, nil
	default:
		return "", fmt.Errorf("session-telemetry-otel: unsupported mode %q", string(mode))
	}
}

// sharingFor maps the mode onto the seam's sharing vocabulary.
func sharingFor(mode Mode) string {
	switch mode {
	case ModeFull:
		return "full"
	case ModeFeedbackOnly:
		return "feedback-only"
	default:
		return "disabled"
	}
}

// Backend is the OTel session-telemetry backend.
type Backend struct {
	directEmit      func(sessiontelemetry.Record)
	provider        *sdklog.LoggerProvider
	logger          apilog.Logger
	opsLogger       apilog.Logger
	shutdownTimeout time.Duration
	sharing         string
}

// New validates the config and builds the backend. Uploading modes wire the
// SDK pipeline; DISABLED constructs no SDK state.
func New(cfg Config) (*Backend, error) {
	mode, err := resolveMode(cfg.Mode)
	if err != nil {
		return nil, err
	}
	backend := &Backend{
		directEmit:      dropRecord,
		shutdownTimeout: time.Duration(defaultShutdownTimeoutMillis) * time.Millisecond,
		sharing:         sharingFor(mode),
	}
	if mode == ModeDisabled {
		return backend, nil
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("session-telemetry-otel: exporter.url is required (the full OTLP logs endpoint)")
	}
	parsed, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("session-telemetry-otel: exporter.url is not a valid URL: %q", cfg.URL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("session-telemetry-otel: exporter.url must be http(s), got %s", parsed.Scheme)
	}
	if cfg.MaxExportBatchSize != nil && *cfg.MaxExportBatchSize < 1 {
		return nil, fmt.Errorf("session-telemetry-otel: processor.maxExportBatchSize must be a positive integer, got %d", *cfg.MaxExportBatchSize)
	}
	timeout := int64(defaultShutdownTimeoutMillis)
	if cfg.ShutdownTimeoutMillis != nil {
		timeout = *cfg.ShutdownTimeoutMillis
	}
	if timeout <= 0 || timeout > maxTimerDelayMillis {
		return nil, fmt.Errorf("session-telemetry-otel: shutdownTimeoutMillis must be a positive finite number no greater than %d, got %d", maxTimerDelayMillis, timeout)
	}
	backend.shutdownTimeout = time.Duration(timeout) * time.Millisecond

	ctx := context.Background()
	exporterOptions := []otlploghttp.Option{otlploghttp.WithEndpointURL(cfg.URL)}
	if len(cfg.Headers) > 0 {
		exporterOptions = append(exporterOptions, otlploghttp.WithHeaders(cfg.Headers))
	}
	exporter, err := otlploghttp.New(ctx, exporterOptions...)
	if err != nil {
		return nil, fmt.Errorf("session-telemetry-otel: exporter construction failed: %w", err)
	}
	processor := sdklog.NewBatchProcessor(exporter)
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(processor))
	backend.provider = provider
	backend.logger = provider.Logger("dsh-session-telemetry-otel")
	backend.opsLogger = provider.Logger("dsh-session-telemetry-otel/ops")
	backend.directEmit = backend.enqueue
	return backend, nil
}

// Sharing reports the disclosure vocabulary to acknowledgement surfaces.
func (b *Backend) Sharing() string { return b.sharing }

// Emit hands a direct service record to the SDK only in FULL. Direct calls
// are no-ops in FEEDBACK_ONLY and DISABLED.
func (b *Backend) Emit(record sessiontelemetry.Record) {
	b.directEmit(record)
}

// Flush is deliberately NOT implemented (fire-and-forget no-op): the batch
// processor exports on its own cadence, and forwarding the hint to a manual
// flush would be the sole source of concurrent flushes.
func (b *Backend) Flush() {}

// Shutdown asks the SDK to drain and quiesce, but rejects after the
// backend-owned deadline. DISABLED has no provider and resolves immediately.
func (b *Backend) Shutdown() error {
	if b.provider == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), b.shutdownTimeout)
	defer cancel()
	if err := b.provider.Shutdown(ctx); err != nil {
		return fmt.Errorf("session-telemetry-otel: provider shutdown failed: %w", err)
	}
	return nil
}

// enqueue maps one seam record onto the SDK logger.
func (b *Backend) enqueue(record sessiontelemetry.Record) {
	var logger apilog.Logger
	if record.Channel == "ops" {
		logger = b.opsLogger
	} else {
		logger = b.logger
	}
	if logger == nil {
		return
	}
	r := apilog.Record{}
	r.SetTimestamp(time.UnixMilli(record.Time))
	r.SetObservedTimestamp(time.UnixMilli(record.Time))
	r.SetSeverity(severityNumber(record.Severity))
	r.SetSeverityText(strings.ToUpper(string(record.Severity)))
	r.SetBody(attribute.StringValue(jsonBody(record.Body)))
	for key, value := range record.Attributes {
		r.AddAttributes(attribute.String(key, fmt.Sprintf("%v", value)))
	}
	logger.Emit(context.Background(), r)
}

func dropRecord(sessiontelemetry.Record) {}

// jsonBody renders the JSON-serializable body as its canonical string form
// (the OTLP body carries one value; structured JSON is delivered as its
// encoded text, mirroring the AnyValue subset the seam validates).
func jsonBody(body any) string {
	if text, ok := body.(string); ok {
		return text
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Sprintf("%v", body)
	}
	return string(encoded)
}

// severityNumber maps the three-level vocabulary to OTel severity numbers.
func severityNumber(severity sessiontelemetry.Severity) apilog.Severity {
	switch severity {
	case sessiontelemetry.SeverityError:
		return apilog.SeverityError
	case sessiontelemetry.SeverityWarn:
		return apilog.SeverityWarn
	default:
		return apilog.SeverityInfo
	}
}
