// Package tracing provides OpenTelemetry distributed tracing integration
// for the Code Intelligence Agent, enabling end-to-end request visibility.
//
// ============================================================================
//
//	设 计 原 理
//
// ============================================================================
//
// 【为什么要 distributed tracing】
//
//	一次 /chat 请求经过：API gateway → orchestrator → LLM → Docker sandbox
//	→ MCP server → Qdrant。任何一环慢 1s 都会被用户感知，但传统日志很难
//	关联。OTel trace 把一次请求串成一棵 span tree，每个 span 有 parent/child
//	关系 + 时间戳 + 属性，在 Jaeger/Tempo 里一眼看清瓶颈。
//
// 【propagation】
//
//	HTTP inbound: 读 W3C `traceparent` 头；没有就新起一个 trace。
//	HTTP outbound（LLM、MCP、Qdrant）：把当前 trace_id 注入 traceparent 发出去。
//	前提：下游服务也接 OTel 才能形成完整链路。本 Agent 只负责自己的跨进程
//	propagation。
//
// 【采样策略】
//
//	cfg.SampleRate = 0.1 时启用 head-based sampling：每个新 trace 有 10% 概率
//	被记录，被采样的整棵 span tree 都会记录。生产建议 0.01-0.05 避免写爆后端。
//	慢请求想 100% 记录：用 tail-based sampling（Tempo/Jaeger 支持），需要
//	OpenTelemetry Collector 在中间处理。
//
// 【Shutdown 的必要】
//
//	OTLP exporter 是批处理的，关闭前必须 Flush。main.go 的 defer
//	traceProvider.Shutdown(ctx) 就是干这个。不 flush 会丢失最后一批 span。
//
// ============================================================================
package tracing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Config holds OpenTelemetry tracing configuration.
type Config struct {
	Enabled     bool    `yaml:"enabled"`
	Endpoint    string  `yaml:"endpoint"` // OTLP endpoint (e.g. "localhost:4317")
	ServiceName string  `yaml:"service_name"`
	SampleRate  float64 `yaml:"sample_rate"` // 0.0 to 1.0
	Insecure    bool    `yaml:"insecure"`    // Use insecure gRPC connection
}

// DefaultConfig returns sensible defaults for tracing.
func DefaultConfig() *Config {
	return &Config{
		Enabled:     false,
		Endpoint:    "localhost:4317",
		ServiceName: "code-agent",
		SampleRate:  0.1, // Sample 10% in production
		Insecure:    true,
	}
}

// Provider wraps the OpenTelemetry tracer provider lifecycle.
type Provider struct {
	cfg      *Config
	logger   *zap.Logger
	provider *sdktrace.TracerProvider
}

// NewProvider initializes the OpenTelemetry trace provider.
// When enabled, it exports spans to the configured OTLP collector (Jaeger).
func NewProvider(cfg *Config, logger *zap.Logger) (*Provider, error) {
	if !cfg.Enabled {
		logger.Info("tracing disabled")
		return &Provider{cfg: cfg, logger: logger}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Create OTLP gRPC exporter
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Build resource with service metadata (avoid schema merge conflicts)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceVersionKey.String("1.0.0"),
			attribute.String("environment", "all-in-one"),
		),
		resource.WithHost(),
		resource.WithProcessRuntimeName(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Build sampler
	var sampler sdktrace.Sampler
	if cfg.SampleRate >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SampleRate <= 0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRate)
	}

	// Create TracerProvider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxExportBatchSize(512),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	// Set as global tracer provider
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("tracing provider initialized",
		zap.String("endpoint", cfg.Endpoint),
		zap.String("service", cfg.ServiceName),
		zap.Float64("sample_rate", cfg.SampleRate),
	)

	return &Provider{cfg: cfg, logger: logger, provider: tp}, nil
}

// Shutdown gracefully shuts down the trace provider, flushing pending spans.
func (p *Provider) Shutdown(ctx context.Context) error {
	if !p.cfg.Enabled || p.provider == nil {
		return nil
	}
	p.logger.Info("flushing trace spans...")
	return p.provider.Shutdown(ctx)
}

// GinMiddleware returns Gin middleware that creates a span for each HTTP request.
// Span attributes include: http.method, http.url, http.status_code.
func GinMiddleware(serviceName string) gin.HandlerFunc {
	tracer := otel.Tracer(serviceName)

	return func(c *gin.Context) {
		// Extract parent span from incoming headers (propagation)
		ctx := otel.GetTextMapPropagator().Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))

		// Determine span name from route pattern or raw path
		spanName := c.FullPath()
		if spanName == "" {
			spanName = c.Request.URL.Path
		}

		ctx, span := tracer.Start(ctx, fmt.Sprintf("%s %s", c.Request.Method, spanName),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", c.Request.Method),
				attribute.String("url.path", c.Request.URL.Path),
				attribute.String("client.address", c.ClientIP()),
			),
		)
		defer span.End()

		// Replace request context with traced context
		c.Request = c.Request.WithContext(ctx)
		c.Set("trace_start", time.Now())

		c.Next()

		// Record response attributes
		status := c.Writer.Status()
		span.SetAttributes(
			attribute.Int("http.response.status_code", status),
			attribute.Int("http.response.body.size", c.Writer.Size()),
		)

		// Record errors for 5xx responses
		if status >= 500 {
			errMsg := fmt.Sprintf("HTTP %d", status)
			span.RecordError(errors.New(errMsg))
			span.SetStatus(codes.Error, errMsg)
		} else {
			span.SetStatus(codes.Ok, "")
		}
	}
}
