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

/*
The manager command is the main entrypoint of CloudNativePG operator.
*/
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cloudnative-pg/machinery/pkg/log"
	"github.com/spf13/cobra"

	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/backup"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/bootstrap"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/controller"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/debug"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/instance"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/pgbouncer"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/show"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/walarchive"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/walrestore"
	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/versions"
	"github.com/cloudnative-pg/cloudnative-pg/internal/configuration"
	"github.com/cloudnative-pg/cloudnative-pg/internal/logging/otel"

	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

func main() {
	cobra.EnableTraverseRunHooks = true

	err := newRootCmd().Execute()

	// the log records buffered for the OpenTelemetry collector are flushed
	// here, so that the ones describing a failure are not lost with it
	if shutdownErr := otel.Shutdown(context.Background()); shutdownErr != nil {
		fmt.Fprintf(os.Stderr, "while flushing the exported log records: %v\n", shutdownErr)
	}

	if err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	logFlags := &log.Flags{}

	cmd := &cobra.Command{
		Use:          "manager [cmd]",
		SilenceUsage: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			logFlags.ConfigureLogging(loggingOptions(cmd)...)
		},
	}

	logFlags.AddFlags(cmd.PersistentFlags())

	cmd.AddCommand(backup.NewCmd())
	cmd.AddCommand(bootstrap.NewCmd())
	cmd.AddCommand(controller.NewCmd())
	cmd.AddCommand(instance.NewCmd())
	cmd.AddCommand(show.NewCmd())
	cmd.AddCommand(walarchive.NewCmd())
	cmd.AddCommand(walrestore.NewCmd())
	cmd.AddCommand(versions.NewCmd())
	cmd.AddCommand(pgbouncer.NewCmd())
	cmd.AddCommand(debug.NewCmd())

	return cmd
}

// loggingOptions returns the logging configuration for the subcommand being
// executed. The controller keeps the sampling that controller-runtime applies
// to every operator, since a reconciliation storm can log the same message
// well beyond the sampler threshold. Every other subcommand runs inside a
// Cluster's pods, where the process output is the pod's log stream and
// dropping records is never acceptable, most importantly for the instance
// manager forwarding the PostgreSQL log, whose records share a single message
// and would otherwise be collapsed by the sampler under a burst of activity.
func loggingOptions(cmd *cobra.Command) []log.ConfigureOption {
	var options []log.ConfigureOption

	if topLevelCommand(cmd).Name() != "controller" {
		options = append(options, log.WithDisabledSampling())
	}

	if option, err := otel.NewZapOption(cmd.Context(), otelConfig()); err != nil {
		// the export is an addition to the standard output, so a misconfigured
		// collector degrades the observability instead of stopping the process
		fmt.Fprintf(os.Stderr, "while setting up the log export, continuing without it: %v\n", err)
	} else if option != nil {
		options = append(options, log.WithZapOptions(option))
	}

	return options
}

// otelConfig returns the log export configuration of the process: the operator
// reads it from the operator ConfigMap or Secret, every other subcommand runs
// inside a Cluster's pod and reads the environment the operator set there.
func otelConfig() otel.Config {
	if configuration.Current.LogOTelEndpoint != "" {
		return otel.Config{
			Endpoint: configuration.Current.LogOTelEndpoint,
			CAFile:   configuration.Current.LogOTelCAFile,
			CertFile: configuration.Current.LogOTelCertFile,
			KeyFile:  configuration.Current.LogOTelKeyFile,
		}
	}

	return otel.ConfigFromEnv()
}

// topLevelCommand walks up the command tree until it finds the direct child
// of the root command, i.e. the manager subcommand that was invoked
func topLevelCommand(cmd *cobra.Command) *cobra.Command {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}

	return cmd
}
