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
	"os"
	"path"

	"github.com/cloudnative-pg/cloudnative-pg/pkg/certs"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// writeClientMaterial issues a CA and a client certificate signed by it, and
// writes the three files a mutual TLS configuration needs
func writeClientMaterial(directory string) Config {
	GinkgoHelper()

	ca, err := certs.CreateRootCA("otel-test-ca", "cnpg")
	Expect(err).ToNot(HaveOccurred())

	client, err := ca.CreateAndSignPair("otel-test-client", certs.CertTypeClient, nil)
	Expect(err).ToNot(HaveOccurred())

	config := Config{
		Endpoint: "https://collector.example.com:4317",
		CAFile:   path.Join(directory, "ca.crt"),
		CertFile: path.Join(directory, "tls.crt"),
		KeyFile:  path.Join(directory, "tls.key"),
	}

	Expect(os.WriteFile(config.CAFile, ca.Certificate, 0o600)).To(Succeed())
	Expect(os.WriteFile(config.CertFile, client.Certificate, 0o600)).To(Succeed())
	Expect(os.WriteFile(config.KeyFile, client.Private, 0o600)).To(Succeed())

	return config
}

var _ = Describe("reloading transport credentials", func() {
	var directory string

	BeforeEach(func() {
		directory = GinkgoT().TempDir()
	})

	It("loads the certificate material when it is built", func() {
		config := writeClientMaterial(directory)

		credentials, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())
		Expect(credentials.Info().SecurityProtocol).To(Equal("tls"))
	})

	It("reports a CA bundle that cannot be read", func() {
		config := Config{
			Endpoint: "https://collector.example.com:4317",
			CAFile:   path.Join(directory, "missing.crt"),
		}

		_, err := newReloadingCredentials(config)
		Expect(err).To(MatchError(ContainSubstring("while reading the OpenTelemetry CA bundle")))
	})

	It("reports a CA bundle holding no certificate", func() {
		config := Config{
			Endpoint: "https://collector.example.com:4317",
			CAFile:   path.Join(directory, "ca.crt"),
		}
		Expect(os.WriteFile(config.CAFile, []byte("not a certificate"), 0o600)).To(Succeed())

		_, err := newReloadingCredentials(config)
		Expect(err).To(MatchError(ContainSubstring("no certificate found in the OpenTelemetry CA bundle")))
	})

	It("reports a client certificate that does not match its key", func() {
		config := writeClientMaterial(directory)
		unrelated := writeClientMaterial(GinkgoT().TempDir())
		key, err := os.ReadFile(unrelated.KeyFile)
		Expect(err).ToNot(HaveOccurred())
		Expect(os.WriteFile(config.KeyFile, key, 0o600)).To(Succeed())

		_, err = newReloadingCredentials(config)
		Expect(err).To(MatchError(ContainSubstring("while loading the OpenTelemetry client certificate")))
	})

	It("keeps the same credentials while the files do not change", func() {
		config := writeClientMaterial(directory)

		reloading, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())

		first, err := reloading.(*reloadingCredentials).credentials()
		Expect(err).ToNot(HaveOccurred())
		second, err := reloading.(*reloadingCredentials).credentials()
		Expect(err).ToNot(HaveOccurred())

		Expect(second).To(BeIdenticalTo(first))
	})

	It("rebuilds the credentials when the certificate is rotated", func() {
		config := writeClientMaterial(directory)

		reloading, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())

		before, err := reloading.(*reloadingCredentials).credentials()
		Expect(err).ToNot(HaveOccurred())

		// a rotation replaces every entry of the Secret, and therefore every
		// file of the mounted volume
		rotated := writeClientMaterial(directory)
		Expect(rotated.CertFile).To(Equal(config.CertFile))

		after, err := reloading.(*reloadingCredentials).credentials()
		Expect(err).ToNot(HaveOccurred())
		Expect(after).ToNot(BeIdenticalTo(before))
	})

	It("reports a rotation that left unusable material behind", func() {
		config := writeClientMaterial(directory)

		reloading, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())

		Expect(os.WriteFile(config.CAFile, []byte("truncated"), 0o600)).To(Succeed())

		_, err = reloading.(*reloadingCredentials).credentials()
		Expect(err).To(MatchError(ContainSubstring("no certificate found in the OpenTelemetry CA bundle")))
	})

	It("cannot be used to accept connections", func() {
		config := writeClientMaterial(directory)

		reloading, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())

		_, _, err = reloading.ServerHandshake(nil)
		Expect(err).To(HaveOccurred())
	})

	It("keeps the overridden server name across a rotation", func() {
		config := writeClientMaterial(directory)

		reloading, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())
		Expect(reloading.OverrideServerName("collector.example.com")).To(Succeed())

		writeClientMaterial(directory)

		rebuilt, err := reloading.(*reloadingCredentials).credentials()
		Expect(err).ToNot(HaveOccurred())
		Expect(rebuilt.Info().ServerName).To(Equal("collector.example.com"))
	})

	It("clones into independent credentials sharing the same configuration", func() {
		config := writeClientMaterial(directory)

		reloading, err := newReloadingCredentials(config)
		Expect(err).ToNot(HaveOccurred())

		clone, ok := reloading.Clone().(*reloadingCredentials)
		Expect(ok).To(BeTrue())
		Expect(clone.config).To(Equal(config))
		Expect(clone.current).To(BeNil())

		_, err = clone.credentials()
		Expect(err).ToNot(HaveOccurred())
	})
})
