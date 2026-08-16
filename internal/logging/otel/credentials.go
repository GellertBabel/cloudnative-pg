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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
)

// reloadingCredentials are gRPC transport credentials rebuilding their TLS
// configuration from the certificate files whenever those change on disk.
//
// The material reaching the collector is issued by cert-manager, or a comparable
// issuer, and is therefore rotated while the process runs: reading it once at
// startup would keep an expired certificate in memory until a restart, silently
// ending the export. The files are re-read on the handshake instead, which only
// happens when the single long-lived connection to the collector is established
// or re-established.
type reloadingCredentials struct {
	config Config

	mutex   sync.Mutex
	current credentials.TransportCredentials
	loaded  []fileState

	// serverNameOverride carries the deprecated OverrideServerName setting, so
	// that a rebuilt inner credential keeps honouring it
	serverNameOverride string
}

// fileState is the observed state of a certificate file, used to tell a rotation
// from an unchanged file without re-reading and re-parsing it
type fileState struct {
	name     string
	size     int64
	modified time.Time
	missing  bool
}

// newReloadingCredentials builds the credentials used to reach the collector,
// verifying its certificate against the configured CA bundle and presenting a
// client certificate when one is configured.
//
// The material is loaded once here, so that a configuration pointing at
// unreadable or malformed files is reported at startup rather than on the first
// export.
func newReloadingCredentials(config Config) (credentials.TransportCredentials, error) {
	result := &reloadingCredentials{config: config}

	if _, err := result.credentials(); err != nil {
		return nil, err
	}

	return result, nil
}

// files returns the certificate files backing the credentials
func (c *reloadingCredentials) files() []string {
	var result []string
	for _, name := range []string{c.config.CAFile, c.config.CertFile, c.config.KeyFile} {
		if name != "" {
			result = append(result, name)
		}
	}

	return result
}

// observe reads the state of the certificate files. Files that cannot be stat'ed
// are recorded as missing rather than reported as an error: a rotation can
// replace them, and the handshake is where the failure belongs.
func (c *reloadingCredentials) observe() []fileState {
	names := c.files()
	result := make([]fileState, 0, len(names))

	for _, name := range names {
		info, err := os.Stat(name)
		if err != nil {
			result = append(result, fileState{name: name, missing: true})
			continue
		}

		result = append(result, fileState{
			name:     name,
			size:     info.Size(),
			modified: info.ModTime(),
		})
	}

	return result
}

// credentials returns the credentials matching the certificates currently on
// disk, rebuilding them when a file changed since the last handshake
func (c *reloadingCredentials) credentials() (credentials.TransportCredentials, error) {
	observed := c.observe()

	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.current != nil && equalFileStates(c.loaded, observed) {
		return c.current, nil
	}

	tlsConfig, err := c.config.tlsConfig()
	if err != nil {
		return nil, err
	}

	rebuilt := credentials.NewTLS(tlsConfig)
	if c.serverNameOverride != "" {
		if err := rebuilt.OverrideServerName(c.serverNameOverride); err != nil {
			return nil, err
		}
	}

	c.current = rebuilt
	c.loaded = observed

	return c.current, nil
}

func equalFileStates(current, observed []fileState) bool {
	if len(current) != len(observed) {
		return false
	}

	for i := range current {
		if current[i] != observed[i] {
			return false
		}
	}

	return true
}

// ClientHandshake implements credentials.TransportCredentials
func (c *reloadingCredentials) ClientHandshake(
	ctx context.Context,
	authority string,
	connection net.Conn,
) (net.Conn, credentials.AuthInfo, error) {
	current, err := c.credentials()
	if err != nil {
		return nil, nil, err
	}

	return current.ClientHandshake(ctx, authority, connection)
}

// ServerHandshake implements credentials.TransportCredentials. These
// credentials only ever dial a collector, so serving is not supported.
func (c *reloadingCredentials) ServerHandshake(net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, fmt.Errorf("the OpenTelemetry credentials cannot be used to accept connections")
}

// Info implements credentials.TransportCredentials
func (c *reloadingCredentials) Info() credentials.ProtocolInfo {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.current == nil {
		return credentials.ProtocolInfo{SecurityProtocol: "tls"}
	}

	return c.current.Info()
}

// Clone implements credentials.TransportCredentials
func (c *reloadingCredentials) Clone() credentials.TransportCredentials {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	return &reloadingCredentials{
		config:             c.config,
		serverNameOverride: c.serverNameOverride,
	}
}

// OverrideServerName implements credentials.TransportCredentials
func (c *reloadingCredentials) OverrideServerName(name string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.serverNameOverride = name
	if c.current == nil {
		return nil
	}

	return c.current.OverrideServerName(name)
}

// tlsConfig builds the TLS configuration used to reach the collector,
// optionally pinning a CA bundle and presenting a client certificate for
// mutual TLS
func (c Config) tlsConfig() (*tls.Config, error) {
	result := &tls.Config{MinVersion: tls.VersionTLS12}

	if c.CAFile != "" {
		bundle, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("while reading the OpenTelemetry CA bundle %q: %w", c.CAFile, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("no certificate found in the OpenTelemetry CA bundle %q", c.CAFile)
		}
		result.RootCAs = pool
	}

	if c.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("while loading the OpenTelemetry client certificate: %w", err)
		}
		result.Certificates = []tls.Certificate{certificate}
	}

	return result, nil
}
