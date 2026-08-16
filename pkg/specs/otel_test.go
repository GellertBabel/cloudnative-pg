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

package specs

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	apiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	"github.com/cloudnative-pg/cloudnative-pg/internal/configuration"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/postgres"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("the OpenTelemetry log export", func() {
	var cluster apiv1.Cluster

	BeforeEach(func() {
		cluster = apiv1.Cluster{}
		configuration.Current = configuration.NewConfiguration()
	})

	AfterEach(func() {
		configuration.Current = configuration.NewConfiguration()
	})

	envValue := func(name string) string {
		GinkgoHelper()

		for _, variable := range CreatePodEnvConfig(cluster, "cluster-example-1").EnvVars {
			if variable.Name == name {
				return variable.Value
			}
		}

		return ""
	}

	volumeNames := func() []string {
		GinkgoHelper()

		var result []string
		for _, volume := range createPostgresVolumes(&cluster, "cluster-example-1", nil) {
			result = append(result, volume.Name)
		}

		return result
	}

	When("no endpoint is configured", func() {
		It("adds no variable and no volume", func() {
			Expect(envValue("CNPG_LOG_OTEL_ENDPOINT")).To(BeEmpty())
			Expect(volumeNames()).ToNot(ContainElement(otelCertificatesVolumeName))
		})
	})

	When("an endpoint is configured without TLS material", func() {
		BeforeEach(func() {
			configuration.Current.LogOTelEndpoint = "https://collector.example.com:4317"
		})

		It("propagates the endpoint alone", func() {
			Expect(envValue("CNPG_LOG_OTEL_ENDPOINT")).To(Equal("https://collector.example.com:4317"))
			Expect(envValue("CNPG_LOG_OTEL_CA_FILE")).To(BeEmpty())
			Expect(volumeNames()).ToNot(ContainElement(otelCertificatesVolumeName))
		})
	})

	When("the operator mounts its own certificates by hand", func() {
		BeforeEach(func() {
			configuration.Current.LogOTelEndpoint = "https://collector.example.com:4317"
			configuration.Current.LogOTelCAFile = "/projected/otel/ca.crt"
			configuration.Current.LogOTelCertFile = "/projected/otel/tls.crt"
			configuration.Current.LogOTelKeyFile = "/projected/otel/tls.key"
		})

		It("propagates the configured paths as they are", func() {
			Expect(envValue("CNPG_LOG_OTEL_CA_FILE")).To(Equal("/projected/otel/ca.crt"))
			Expect(envValue("CNPG_LOG_OTEL_CERT_FILE")).To(Equal("/projected/otel/tls.crt"))
			Expect(envValue("CNPG_LOG_OTEL_KEY_FILE")).To(Equal("/projected/otel/tls.key"))
		})

		It("mounts no Secret of its own", func() {
			Expect(volumeNames()).ToNot(ContainElement(otelCertificatesVolumeName))
		})
	})

	When("a TLS Secret is configured", func() {
		BeforeEach(func() {
			configuration.Current.LogOTelEndpoint = "https://collector.example.com:4317"
			configuration.Current.LogOTelTLSSecret = "otel-client-tls"
		})

		It("mounts it outside every directory the instance manager writes into", func() {
			// A volume mounted inside the scratch data volume makes the kubelet
			// create the intermediate directories owned by root, and the
			// instance manager then fails to write its own certificates with a
			// permission error, leaving PostgreSQL down.
			Expect(postgres.OTelCertificatesDir).ToNot(HavePrefix(postgres.ScratchDataDirectory))
			Expect(postgres.OTelCertificatesDir).ToNot(HavePrefix(postgres.CertificatesDir))
		})

		It("mounts the Secret in the instance pods", func() {
			volumes := createPostgresVolumes(&cluster, "cluster-example-1", nil)
			Expect(volumes).To(ContainElement(corev1.Volume{
				Name: otelCertificatesVolumeName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName:  "otel-client-tls",
						DefaultMode: ptr.To[int32](0o440),
						Optional:    ptr.To(true),
					},
				},
			}))

			Expect(CreatePostgresVolumeMounts(VolumeMountsConfig{Cluster: cluster})).
				To(ContainElement(corev1.VolumeMount{
					Name:      otelCertificatesVolumeName,
					MountPath: postgres.OTelCertificatesDir,
					ReadOnly:  true,
				}))
		})

		It("is optional, so a namespace without the Secret keeps running", func() {
			volumes := createPostgresVolumes(&cluster, "cluster-example-1", nil)
			for _, volume := range volumes {
				if volume.Name != otelCertificatesVolumeName {
					continue
				}
				Expect(volume.Secret.Optional).ToNot(BeNil())
				Expect(*volume.Secret.Optional).To(BeTrue())
			}
		})

		It("propagates the paths of the mounted Secret, not the operator ones", func() {
			configuration.Current.LogOTelCAFile = "/etc/otel-tls/client/ca.crt"
			configuration.Current.LogOTelCertFile = "/etc/otel-tls/client/tls.crt"
			configuration.Current.LogOTelKeyFile = "/etc/otel-tls/client/tls.key"

			Expect(envValue("CNPG_LOG_OTEL_CA_FILE")).To(Equal(postgres.OTelCACertificateLocation))
			Expect(envValue("CNPG_LOG_OTEL_CERT_FILE")).To(Equal(postgres.OTelClientCertificateLocation))
			Expect(envValue("CNPG_LOG_OTEL_KEY_FILE")).To(Equal(postgres.OTelClientKeyLocation))
		})

		It("changes the pod environment hash, so that the instances are rolled", func() {
			withSecret := CreatePodEnvConfig(cluster, "cluster-example-1").Hash

			configuration.Current.LogOTelTLSSecret = ""
			withoutSecret := CreatePodEnvConfig(cluster, "cluster-example-1").Hash

			Expect(withSecret).ToNot(Equal(withoutSecret))
		})
	})
})
