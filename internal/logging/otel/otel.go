/*
Copyright © contributors to CloudNativePG, established as
CloudNativePG a Series of LF Projects, LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package otel

import (
	"context"
	"fmt"
	"os"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/versions"
)

// instrumentationScope names the library emitting the records produced by a
// logger without a name
const instrumentationScope = "github.com/cloudnative-pg/cloudnative-pg"

// shutdown holds the flush function of the provider created by NewZapOption, so
// that Shutdown can be called from the process entry point without the
// subcommands having to carry the provider around
var shutdown struct {
	sync.Mutex
	flush func(context.Context) error
}

// NewZapOption returns a zap option teeing every log record to an OpenTelemetry
// collector, and a nil option when the export is not configured.
//
// The option is handed to the logging setup of the machinery package, which
// applies it to the logger shared by the operator, the instance manager and the
// PostgreSQL log stream. Buffering, retries and reconnections belong to the batch
// processor, so an unreachable collector never blocks nor fails the process.
//
// Shutdown has to be called before the process exits to flush the records still
// buffered.
func NewZapOption(ctx context.Context, config Config) (uberzap.Option, error) {
	if !config.Enabled() {
		return nil, nil
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	exporter, err := newExporter(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("while creating the OpenTelemetry log exporter: %w", err)
	}

	provider := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(exporter)),
		log.WithResource(newResource()),
	)

	shutdown.Lock()
	shutdown.flush = provider.Shutdown
	shutdown.Unlock()

	return uberzap.WrapCore(func(inner zapcore.Core) zapcore.Core {
		// the core writing to the standard output doubles as the level enabler, so
		// that the exported stream matches the printed one and --log-level stays
		// the only control over verbosity
		return zapcore.NewTee(inner, newCore(provider, inner, instrumentationScope))
	}), nil
}

// Shutdown flushes the log records still buffered. It is a no-op when the export
// was never configured.
func Shutdown(ctx context.Context) error {
	shutdown.Lock()
	flush := shutdown.flush
	shutdown.flush = nil
	shutdown.Unlock()

	if flush == nil {
		return nil
	}

	return flush(ctx)
}

// newExporter builds the OTLP gRPC exporter described by the configuration
func newExporter(ctx context.Context, config Config) (*otlploggrpc.Exporter, error) {
	endpoint, err := config.endpointURL()
	if err != nil {
		return nil, err
	}

	options := []otlploggrpc.Option{
		// the scheme of the URL selects transport security
		otlploggrpc.WithEndpointURL(endpoint.String()),
	}

	if endpoint.Scheme == "https" {
		credentials, err := newReloadingCredentials(config)
		if err != nil {
			return nil, err
		}
		options = append(options, otlploggrpc.WithTLSCredentials(credentials))
	}

	return otlploggrpc.New(ctx, options...)
}

// newResource describes the process producing the records, so that the collector
// can tell the operator apart from each instance manager
func newResource() *resource.Resource {
	attributes := []attribute.KeyValue{
		semconv.ServiceName("cloudnative-pg"),
		semconv.ServiceVersion(versions.Version),
	}

	if podName := podName(); podName != "" {
		attributes = append(attributes, semconv.K8SPodName(podName))
	}
	if namespace := namespace(); namespace != "" {
		attributes = append(attributes, semconv.K8SNamespaceName(namespace))
	}
	if clusterName := os.Getenv("CLUSTER_NAME"); clusterName != "" {
		attributes = append(attributes, attribute.String("cnpg.cluster.name", clusterName))
	}

	return resource.NewWithAttributes(semconv.SchemaURL, attributes...)
}

// podName returns the name of the pod running this process. The operator injects
// POD_NAME into the instance pods; its own Deployment does not carry it, and the
// hostname of a pod is its name unless the spec overrides it.
func podName() string {
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}

	name, err := os.Hostname()
	if err != nil {
		return ""
	}

	return name
}

// namespace returns the namespace of the pod running this process, taking the
// variable the operator injects into the instance pods or, for the operator
// itself, the one its own Deployment carries
func namespace() string {
	if namespace := os.Getenv("NAMESPACE"); namespace != "" {
		return namespace
	}

	return os.Getenv("OPERATOR_NAMESPACE")
}
