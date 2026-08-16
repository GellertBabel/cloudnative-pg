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
	"slices"

	machinerylog "github.com/cloudnative-pg/machinery/pkg/log"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.uber.org/zap/zapcore"
)

// loggerNameAttribute mirrors the "logger" field of the JSON output
const loggerNameAttribute = "logger"

// core is a zapcore.Core emitting every record it receives as an OpenTelemetry
// log record. It is meant to be tee'd with the core writing to the standard
// output, never to replace it.
type core struct {
	zapcore.LevelEnabler

	provider otellog.LoggerProvider
	logger   otellog.Logger
	attrs    []attribute.KeyValue
}

// newCore builds a Core emitting to the given provider. The enabler is normally
// the core writing to the standard output, so that both destinations observe the
// same records.
func newCore(provider otellog.LoggerProvider, enabler zapcore.LevelEnabler, scope string) *core {
	return &core{
		LevelEnabler: enabler,
		provider:     provider,
		logger:       provider.Logger(scope),
	}
}

// With adds structured context to the Core
func (c *core) With(fields []zapcore.Field) zapcore.Core {
	clone := &core{
		LevelEnabler: c.LevelEnabler,
		provider:     c.provider,
		logger:       c.logger,
		attrs:        slices.Clone(c.attrs),
	}
	clone.attrs = appendFields(clone.attrs, fields)

	return clone
}

// Check determines whether the given entry has to be emitted
func (c *core) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}

	return checked
}

// Write emits the entry as an OpenTelemetry log record
func (c *core) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	logger := c.logger
	if entry.LoggerName != "" {
		// a named logger gets its own instrumentation scope, as the
		// OpenTelemetry zap and logr bridges do
		logger = c.provider.Logger(entry.LoggerName)
	}

	var record otellog.Record
	record.SetTimestamp(entry.Time)
	record.SetBody(attribute.StringValue(entry.Message))
	record.SetSeverity(severityOf(entry.Level))
	record.SetSeverityText(severityTextOf(entry.Level))

	record.AddAttributes(c.attrs...)
	record.AddAttributes(appendFields(nil, fields)...)
	if entry.LoggerName != "" {
		record.AddAttributes(attribute.String(loggerNameAttribute, entry.LoggerName))
	}
	if entry.Stack != "" {
		record.AddAttributes(attribute.String("stacktrace", entry.Stack))
	}
	if entry.Caller.Defined {
		record.AddAttributes(attribute.String("caller", entry.Caller.TrimmedPath()))
	}

	// the emission is asynchronous: the batch processor owns the buffering and
	// the retries, so a slow or unreachable collector never blocks the caller
	logger.Emit(context.Background(), record)

	return nil
}

// Sync implements zapcore.Core. Flushing belongs to the log provider, which
// Shutdown takes care of.
func (*core) Sync() error {
	return nil
}

// severityOf maps the levels this project uses onto OpenTelemetry severities.
//
// CloudNativePG defines two levels below the standard zap ones, trace at -4 and
// debug at -2, so the mapping cannot be left to the generic bridges: they
// resolve every unknown level to an undefined severity.
func severityOf(level zapcore.Level) otellog.Severity {
	switch level {
	case machinerylog.TraceLevel:
		return otellog.SeverityTrace
	case machinerylog.DebugLevel:
		return otellog.SeverityDebug
	case zapcore.DebugLevel:
		// zap's own debug level, one step above the one this project calls debug
		return otellog.SeverityDebug2
	case machinerylog.InfoLevel:
		return otellog.SeverityInfo
	case machinerylog.WarningLevel:
		return otellog.SeverityWarn
	case machinerylog.ErrorLevel:
		return otellog.SeverityError
	case zapcore.DPanicLevel:
		return otellog.SeverityFatal1
	case zapcore.PanicLevel:
		return otellog.SeverityFatal2
	case zapcore.FatalLevel:
		return otellog.SeverityFatal3
	default:
		if level < machinerylog.TraceLevel {
			return otellog.SeverityTrace
		}

		return otellog.SeverityInfo
	}
}

// severityTextOf returns the level names the JSON output uses, so that a record
// read from the collector and one read from the pod log agree
func severityTextOf(level zapcore.Level) string {
	switch level {
	case machinerylog.TraceLevel:
		return machinerylog.TraceLevelString
	case machinerylog.DebugLevel:
		return machinerylog.DebugLevelString
	case machinerylog.InfoLevel:
		return machinerylog.InfoLevelString
	case machinerylog.WarningLevel:
		return machinerylog.WarningLevelString
	case machinerylog.ErrorLevel:
		return machinerylog.ErrorLevelString
	default:
		return level.String()
	}
}
