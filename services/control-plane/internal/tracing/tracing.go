package tracing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	sampleRatioEnvironment     = "SYNARA_OTEL_TRACE_SAMPLE_RATIO"
	collectorRegionEnvironment = "SYNARA_OTEL_COLLECTOR_REGION"
	retentionDaysEnvironment   = "SYNARA_OTEL_TRACE_RETENTION_DAYS"
)

type ExportPolicy string

const (
	ExportPolicyDevelopment      ExportPolicy = "development"
	ExportPolicyEnterprise       ExportPolicy = "enterprise"
	ExportPolicyEnterpriseWorker ExportPolicy = "enterprise-worker"
)

type exporterSettings struct {
	configured    bool
	endpoint      *url.URL
	region        string
	retentionDays int
}

var collectorRegionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

var textMapPropagator = propagation.NewCompositeTextMapPropagator(
	propagation.TraceContext{}, propagation.Baggage{},
)

type Shutdown func(context.Context) error

func Configure(
	ctx context.Context,
	serviceName string,
	logger *slog.Logger,
	policy ExportPolicy,
) (Shutdown, error) {
	otel.SetTextMapPropagator(textMapPropagator)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return func(context.Context) error { return nil }, nil
	}
	settings, err := exporterSettingsFromEnvironment(policy)
	if err != nil {
		return nil, err
	}
	if !settings.configured {
		return func(context.Context) error { return nil }, nil
	}
	ratio, err := sampleRatio()
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("configure OTLP trace exporter: %w", err)
	}
	resourceAttributes := []attribute.KeyValue{attribute.String("service.name", serviceName)}
	if settings.region != "" {
		resourceAttributes = append(resourceAttributes, attribute.String("synara.telemetry.region", settings.region))
	}
	if settings.retentionDays > 0 {
		resourceAttributes = append(
			resourceAttributes,
			attribute.Int("synara.telemetry.retention_days", settings.retentionDays),
		)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewSchemaless(resourceAttributes...)),
	)
	otel.SetTracerProvider(provider)
	if logger != nil {
		logger.Info(
			"OpenTelemetry tracing enabled",
			"service", serviceName,
			"exportPolicy", policy,
			"sampleRatio", ratio,
			"collectorRegion", settings.region,
			"retentionDays", settings.retentionDays,
		)
	}
	return provider.Shutdown, nil
}

func NewHTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

func TraceparentFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	flags := "00"
	if spanContext.IsSampled() {
		flags = "01"
	}
	return "00-" + spanContext.TraceID().String() + "-" + spanContext.SpanID().String() + "-" + flags
}

func ContextWithRemoteParent(ctx context.Context, traceparent string) context.Context {
	traceparent = strings.TrimSpace(traceparent)
	if traceparent == "" {
		return ctx
	}
	return textMapPropagator.Extract(
		ctx,
		propagation.MapCarrier{"traceparent": traceparent},
	)
}

func ExtractHTTP(ctx context.Context, header http.Header) context.Context {
	return textMapPropagator.Extract(ctx, propagation.HeaderCarrier(header))
}

func exporterSettingsFromEnvironment(policy ExportPolicy) (exporterSettings, error) {
	if policy != ExportPolicyDevelopment && policy != ExportPolicyEnterprise && policy != ExportPolicyEnterpriseWorker {
		return exporterSettings{}, errors.New("unknown OpenTelemetry export policy")
	}
	rawEndpoint := effectiveEnvironment("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT")
	if rawEndpoint == "" {
		return exporterSettings{}, nil
	}
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || endpoint.IsAbs() == false || endpoint.Hostname() == "" {
		return exporterSettings{}, errors.New("OTLP trace endpoint must be an absolute HTTP(S) URL")
	}
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return exporterSettings{}, errors.New("OTLP trace endpoint must use HTTP or HTTPS")
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return exporterSettings{}, errors.New("OTLP trace endpoint must not contain credentials, query, or fragment")
	}
	protocol := effectiveEnvironment("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL")
	if protocol != "" && protocol != "http/protobuf" {
		return exporterSettings{}, errors.New("OTLP trace protocol must be http/protobuf")
	}
	insecure := effectiveEnvironment("OTEL_EXPORTER_OTLP_TRACES_INSECURE", "OTEL_EXPORTER_OTLP_INSECURE")
	if insecure != "" {
		value, parseErr := strconv.ParseBool(insecure)
		if parseErr != nil {
			return exporterSettings{}, errors.New("OTLP insecure transport flag must be true or false")
		}
		if value {
			return exporterSettings{}, errors.New("OTLP insecure transport override is forbidden; use the endpoint scheme")
		}
	}
	headers := effectiveEnvironment("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "OTEL_EXPORTER_OTLP_HEADERS")
	if headers != "" && endpoint.Scheme != "https" {
		return exporterSettings{}, errors.New("OTLP authentication headers require HTTPS")
	}
	certificate := effectiveEnvironment("OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE", "OTEL_EXPORTER_OTLP_CERTIFICATE")
	clientCertificate := effectiveEnvironment(
		"OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE",
		"OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE",
	)
	clientKey := effectiveEnvironment("OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY", "OTEL_EXPORTER_OTLP_CLIENT_KEY")
	if (clientCertificate == "") != (clientKey == "") {
		return exporterSettings{}, errors.New("OTLP mTLS client certificate and key must be configured together")
	}
	for name, path := range map[string]string{
		"OTLP server certificate": certificate,
		"OTLP client certificate": clientCertificate,
		"OTLP client key":         clientKey,
	} {
		if path == "" {
			continue
		}
		if (policy == ExportPolicyEnterprise || policy == ExportPolicyEnterpriseWorker) && !filepath.IsAbs(path) {
			return exporterSettings{}, fmt.Errorf("%s path must be absolute in enterprise mode", name)
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			return exporterSettings{}, fmt.Errorf("%s path must resolve to a regular file", name)
		}
	}
	region := strings.ToLower(strings.TrimSpace(os.Getenv(collectorRegionEnvironment)))
	retentionDays, err := optionalRetentionDays()
	if err != nil {
		return exporterSettings{}, err
	}
	if region != "" && !collectorRegionPattern.MatchString(region) {
		return exporterSettings{}, errors.New(collectorRegionEnvironment + " has an invalid Region identifier")
	}
	if policy == ExportPolicyEnterprise {
		if endpoint.Scheme != "https" {
			return exporterSettings{}, errors.New("enterprise OTLP trace export requires HTTPS")
		}
		if clientCertificate == "" || clientKey == "" {
			return exporterSettings{}, errors.New("enterprise OTLP trace export requires an mTLS client certificate and key")
		}
		if region == "" {
			return exporterSettings{}, errors.New("enterprise OTLP trace export requires " + collectorRegionEnvironment)
		}
		if retentionDays == 0 {
			return exporterSettings{}, errors.New("enterprise OTLP trace export requires " + retentionDaysEnvironment)
		}
	}
	if policy == ExportPolicyEnterpriseWorker {
		if headers != "" {
			return exporterSettings{}, errors.New("enterprise Worker OTLP trace export forbids authentication Headers")
		}
		if clientCertificate != "" || clientKey != "" {
			return exporterSettings{}, errors.New("enterprise Worker OTLP trace export forbids client identity files")
		}
		if endpoint.Scheme == "http" {
			ip := net.ParseIP(endpoint.Hostname())
			if ip == nil || !ip.IsLoopback() || endpoint.Port() == "" || certificate != "" {
				return exporterSettings{}, errors.New("enterprise Worker HTTP OTLP trace export requires an explicit loopback relay without TLS files")
			}
		}
		if region == "" {
			return exporterSettings{}, errors.New("enterprise Worker OTLP trace export requires " + collectorRegionEnvironment)
		}
		if retentionDays == 0 {
			return exporterSettings{}, errors.New("enterprise Worker OTLP trace export requires " + retentionDaysEnvironment)
		}
	}
	return exporterSettings{
		configured: true, endpoint: endpoint, region: region, retentionDays: retentionDays,
	}, nil
}

func effectiveEnvironment(specific, general string) string {
	if value := strings.TrimSpace(os.Getenv(specific)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv(general))
}

func optionalRetentionDays() (int, error) {
	raw := strings.TrimSpace(os.Getenv(retentionDaysEnvironment))
	if raw == "" {
		return 0, nil
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 1 || days > 90 {
		return 0, errors.New(retentionDaysEnvironment + " must be an integer between 1 and 90")
	}
	return days, nil
}

func sampleRatio() (float64, error) {
	raw := strings.TrimSpace(os.Getenv(sampleRatioEnvironment))
	if raw == "" {
		return 0.1, nil
	}
	ratio, err := strconv.ParseFloat(raw, 64)
	if err != nil || ratio < 0 || ratio > 1 {
		return 0, errors.New(sampleRatioEnvironment + " must be a number between 0 and 1")
	}
	return ratio, nil
}
