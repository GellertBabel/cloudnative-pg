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

// Package otel exports the log records of the operator, of the instance manager
// and of PostgreSQL to an OpenTelemetry collector over OTLP gRPC, in addition to
// the JSON stream written to the standard output.
//
// The export is additive and opt-in: when no endpoint is configured nothing
// changes, no connection is attempted and no record is buffered.
package otel

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

const (
	// EndpointEnvName carries the OTLP endpoint into the instance pods
	EndpointEnvName = "CNPG_LOG_OTEL_ENDPOINT"

	// CAFileEnvName carries the path of the CA bundle verifying the collector
	// certificate into the instance pods
	CAFileEnvName = "CNPG_LOG_OTEL_CA_FILE"

	// CertFileEnvName carries the path of the client certificate used for mutual
	// TLS into the instance pods
	CertFileEnvName = "CNPG_LOG_OTEL_CERT_FILE"

	// KeyFileEnvName carries the path of the client private key used for mutual
	// TLS into the instance pods
	KeyFileEnvName = "CNPG_LOG_OTEL_KEY_FILE"
)

// Config describes how log records are exported to an OpenTelemetry collector.
//
// Transport security is selected by the endpoint scheme, as in the OTLP
// specification: "https" enables TLS and "http" disables it. An endpoint without
// a scheme is read as "https", so that the secure setting is the default.
type Config struct {
	// Endpoint is the OTLP gRPC endpoint of the collector. When empty, no record
	// is exported.
	Endpoint string

	// CAFile is the path of a PEM CA bundle verifying the collector certificate.
	// When empty, the system pool is used.
	CAFile string

	// CertFile and KeyFile are the paths of the PEM client certificate and
	// private key presented to the collector for mutual TLS. Either both or
	// neither must be set.
	CertFile string
	KeyFile  string
}

// ConfigFromEnv reads the configuration from the environment variables the
// operator sets on the instance pods
func ConfigFromEnv() Config {
	return Config{
		Endpoint: os.Getenv(EndpointEnvName),
		CAFile:   os.Getenv(CAFileEnvName),
		CertFile: os.Getenv(CertFileEnvName),
		KeyFile:  os.Getenv(KeyFileEnvName),
	}
}

// Enabled returns whether log records have to be exported
func (c Config) Enabled() bool {
	return c.Endpoint != ""
}

// endpointURL normalizes the configured endpoint into a URL whose scheme selects
// transport security.
//
// A bare "host:port" cannot be handed to the exporter as-is because url.Parse
// reads "host" as the scheme, which would silently disable TLS and drop the
// host, so it is qualified as "https" here.
func (c Config) endpointURL() (*url.URL, error) {
	raw := c.Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("cannot parse OpenTelemetry endpoint %q: %w", c.Endpoint, err)
	}

	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf(
			"unsupported scheme %q in OpenTelemetry endpoint %q, expected http or https",
			parsed.Scheme, c.Endpoint)
	}

	if parsed.Host == "" {
		return nil, fmt.Errorf("missing host in OpenTelemetry endpoint %q", c.Endpoint)
	}

	return parsed, nil
}

// Validate reports whether the configuration can be used to build an exporter
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}

	if _, err := c.endpointURL(); err != nil {
		return err
	}

	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New(
			"both a client certificate and a private key are needed for mutual TLS " +
				"with the OpenTelemetry collector")
	}

	return nil
}
