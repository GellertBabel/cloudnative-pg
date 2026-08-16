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
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Log export configuration", func() {
	It("is disabled when no endpoint is configured", func() {
		Expect(Config{}.Enabled()).To(BeFalse())
		Expect(Config{}.Validate()).To(Succeed())
	})

	It("is enabled as soon as an endpoint is configured", func() {
		Expect(Config{Endpoint: "collector:4317"}.Enabled()).To(BeTrue())
	})

	DescribeTable("selects the transport security from the endpoint scheme",
		func(endpoint string, expectedHost string, expectedScheme string) {
			parsed, err := Config{Endpoint: endpoint}.endpointURL()
			Expect(err).ToNot(HaveOccurred())
			Expect(parsed.Host).To(Equal(expectedHost))
			Expect(parsed.Scheme).To(Equal(expectedScheme))
		},
		// a bare endpoint has to default to the secure setting: url.Parse would
		// otherwise read "collector" as the scheme, silently dropping TLS and
		// losing the host
		Entry("bare host and port default to TLS",
			"collector.observability.svc:4317", "collector.observability.svc:4317", "https"),
		Entry("explicit https keeps TLS",
			"https://collector:4317", "collector:4317", "https"),
		Entry("explicit http disables TLS",
			"http://collector:4317", "collector:4317", "http"),
		Entry("an IPv6 literal is preserved",
			"[2001:db8::1]:4317", "[2001:db8::1]:4317", "https"),
		Entry("a bare host without port defaults to TLS",
			"collector", "collector", "https"),
	)

	It("rejects a scheme that is not http or https", func() {
		err := Config{Endpoint: "grpc://collector:4317"}.Validate()
		Expect(err).To(MatchError(ContainSubstring("unsupported scheme")))
	})

	It("rejects an endpoint without a host", func() {
		err := Config{Endpoint: "https://"}.Validate()
		Expect(err).To(MatchError(ContainSubstring("missing host")))
	})

	It("accepts a CA bundle without a client certificate", func() {
		Expect(Config{
			Endpoint: "https://collector:4317",
			CAFile:   "/etc/otel/ca.crt",
		}.Validate()).To(Succeed())
	})

	It("accepts a complete mutual TLS configuration", func() {
		Expect(Config{
			Endpoint: "https://collector:4317",
			CertFile: "/etc/otel/tls.crt",
			KeyFile:  "/etc/otel/tls.key",
		}.Validate()).To(Succeed())
	})

	DescribeTable("rejects a partial mutual TLS configuration",
		func(config Config) {
			Expect(config.Validate()).To(MatchError(ContainSubstring("mutual TLS")))
		},
		Entry("certificate without key", Config{
			Endpoint: "https://collector:4317",
			CertFile: "/etc/otel/tls.crt",
		}),
		Entry("key without certificate", Config{
			Endpoint: "https://collector:4317",
			KeyFile:  "/etc/otel/tls.key",
		}),
	)

	It("ignores an incomplete configuration when the export is disabled", func() {
		Expect(Config{CertFile: "/etc/otel/tls.crt"}.Validate()).To(Succeed())
	})

	It("reads the settings the operator propagates to the instance pods", func() {
		GinkgoT().Setenv(EndpointEnvName, "https://collector:4317")
		GinkgoT().Setenv(CAFileEnvName, "/etc/otel/ca.crt")
		GinkgoT().Setenv(CertFileEnvName, "/etc/otel/tls.crt")
		GinkgoT().Setenv(KeyFileEnvName, "/etc/otel/tls.key")

		Expect(ConfigFromEnv()).To(Equal(Config{
			Endpoint: "https://collector:4317",
			CAFile:   "/etc/otel/ca.crt",
			CertFile: "/etc/otel/tls.crt",
			KeyFile:  "/etc/otel/tls.key",
		}))
	})

	It("is disabled when the environment carries no endpoint", func() {
		GinkgoT().Setenv(EndpointEnvName, "")
		Expect(ConfigFromEnv().Enabled()).To(BeFalse())
	})
})

var _ = Describe("Record source", func() {
	It("takes the pod identity the operator injects into the instance pods", func() {
		GinkgoT().Setenv("POD_NAME", "cluster-example-1")
		GinkgoT().Setenv("NAMESPACE", "production")

		Expect(podName()).To(Equal("cluster-example-1"))
		Expect(namespace()).To(Equal("production"))
	})

	It("falls back to the hostname and to the operator namespace for the operator", func() {
		GinkgoT().Setenv("POD_NAME", "")
		GinkgoT().Setenv("NAMESPACE", "")
		GinkgoT().Setenv("OPERATOR_NAMESPACE", "cnpg-system")

		hostname, err := os.Hostname()
		Expect(err).ToNot(HaveOccurred())

		Expect(podName()).To(Equal(hostname))
		Expect(namespace()).To(Equal("cnpg-system"))
	})
})

var _ = Describe("Zap option", func() {
	It("is absent when the export is not configured", func() {
		option, err := NewZapOption(context.Background(), Config{})
		Expect(err).ToNot(HaveOccurred())
		Expect(option).To(BeNil())
	})

	It("refuses an invalid configuration instead of exporting nowhere", func() {
		_, err := NewZapOption(context.Background(), Config{Endpoint: "grpc://collector:4317"})
		Expect(err).To(HaveOccurred())
	})

	It("builds an option without contacting the collector", func() {
		// the exporter connects lazily, so an unreachable endpoint must not
		// prevent the process from starting
		option, err := NewZapOption(context.Background(), Config{
			Endpoint: "http://127.0.0.1:1",
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(option).ToNot(BeNil())

		Expect(Shutdown(context.Background())).To(Succeed())
	})

	It("is a no-op to shut down when nothing was configured", func() {
		Expect(Shutdown(context.Background())).To(Succeed())
	})
})
