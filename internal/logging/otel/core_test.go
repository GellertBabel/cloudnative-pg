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
	"errors"
	"time"

	machinerylog "github.com/cloudnative-pg/machinery/pkg/log"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/logtest"
	uberzap "go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// pgRecord mimics the PostgreSQL log record, which describes itself through
// zapcore.ObjectMarshaler instead of being serialized as a single opaque value
type pgRecord struct {
	Severity string `json:"error_severity,omitempty"`
	SQLState string `json:"sql_state_code,omitempty"`
	Message  string `json:"message,omitempty"`
}

func (r *pgRecord) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("error_severity", r.Severity)
	enc.AddString("sql_state_code", r.SQLState)
	enc.AddString("message", r.Message)

	return nil
}

// plainStruct is reflected by zap without describing itself
type plainStruct struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// recordedLogger builds a zap logger writing only to the recorder, at the given
// level, mirroring how NewZapOption ties the core to the standard output one
func recordedLogger(level zapcore.Level) (*uberzap.Logger, *logtest.Recorder) {
	recorder := logtest.NewRecorder()
	// a zapcore.Level is itself a LevelEnabler, accepting every level at or
	// above itself
	return uberzap.New(newCore(recorder, level, "test")), recorder
}

// allRecords flattens the recording, dropping the scope
func allRecords(recorder *logtest.Recorder) []logtest.Record {
	var result []logtest.Record
	for _, records := range recorder.Result() {
		result = append(result, records...)
	}
	return result
}

// attributesOf indexes the attributes of a record by key
func attributesOf(record logtest.Record) map[string]string {
	result := map[string]string{}
	for _, attr := range record.Attributes {
		result[string(attr.Key)] = attr.Value.Emit()
	}
	return result
}

var _ = Describe("OpenTelemetry log core", func() {
	It("emits the message as the record body", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		logger.Info("something happened")

		records := allRecords(recorder)
		Expect(records).To(HaveLen(1))
		Expect(records[0].Body.AsString()).To(Equal("something happened"))
	})

	It("preserves the time of the event", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		before := time.Now()
		logger.Info("timed")

		records := allRecords(recorder)
		Expect(records).To(HaveLen(1))
		// a record carrying only the time the exporter observed it could not be
		// correlated with the PostgreSQL log stream
		Expect(records[0].Timestamp).ToNot(BeZero())
		Expect(records[0].Timestamp).To(BeTemporally(">=", before.Add(-time.Minute)))
	})

	DescribeTable("maps the levels of this project onto OpenTelemetry severities",
		func(level zapcore.Level, expected otellog.Severity, expectedText string) {
			Expect(severityOf(level)).To(Equal(expected))
			Expect(severityTextOf(level)).To(Equal(expectedText))
		},
		Entry("trace", machinerylog.TraceLevel, otellog.SeverityTrace, "trace"),
		Entry("debug", machinerylog.DebugLevel, otellog.SeverityDebug, "debug"),
		Entry("info", machinerylog.InfoLevel, otellog.SeverityInfo, "info"),
		Entry("warning", machinerylog.WarningLevel, otellog.SeverityWarn, "warning"),
		Entry("error", machinerylog.ErrorLevel, otellog.SeverityError, "error"),
	)

	It("never reports a severity as undefined for the custom levels", func() {
		// the generic bridges resolve the levels below the standard zap ones to
		// an undefined severity, which is what this mapping exists to avoid
		for _, level := range []zapcore.Level{
			machinerylog.TraceLevel,
			machinerylog.DebugLevel,
			machinerylog.InfoLevel,
			machinerylog.WarningLevel,
			machinerylog.ErrorLevel,
		} {
			Expect(severityOf(level)).ToNot(Equal(otellog.SeverityUndefined),
				"level %d must map to a defined severity", level)
		}
	})

	It("keeps the fields of a self describing record as separate attributes", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		logger.Info("record", uberzap.Any("record", &pgRecord{
			Severity: "ERROR",
			SQLState: "22012",
			Message:  "division by zero",
		}))

		records := allRecords(recorder)
		Expect(records).To(HaveLen(1))

		attributes := attributesOf(records[0])
		Expect(attributes).To(HaveKeyWithValue("record.error_severity", "ERROR"))
		Expect(attributes).To(HaveKeyWithValue("record.sql_state_code", "22012"))
		Expect(attributes).To(HaveKeyWithValue("record.message", "division by zero"))
		Expect(attributes).ToNot(HaveKey("record"),
			"the record must not be collapsed into a single opaque attribute")
	})

	It("expands a reflected structure through its JSON field names", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		logger.Info("reflected", uberzap.Any("payload", plainStruct{Name: "example", Count: 3}))

		attributes := attributesOf(allRecords(recorder)[0])
		Expect(attributes).To(HaveKeyWithValue("payload.name", "example"))
		Expect(attributes).To(HaveKeyWithValue("payload.count", "3"))
	})

	It("converts the common field types", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		logger.Info("typed",
			uberzap.String("string", "value"),
			uberzap.Int("int", 42),
			uberzap.Bool("bool", true),
			uberzap.Float64("float", 1.5),
			uberzap.Duration("duration", 2*time.Second),
			uberzap.Error(errors.New("boom")),
		)

		attributes := attributesOf(allRecords(recorder)[0])
		Expect(attributes).To(HaveKeyWithValue("string", "value"))
		Expect(attributes).To(HaveKeyWithValue("int", "42"))
		Expect(attributes).To(HaveKeyWithValue("bool", "true"))
		Expect(attributes).To(HaveKeyWithValue("float", "1.5"))
		Expect(attributes).To(HaveKeyWithValue("duration", "2s"))
		Expect(attributes).To(HaveKeyWithValue("error", "boom"))
	})

	It("carries the values added with With on every record", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		logger.With(uberzap.String("cluster", "example")).Info("scoped")

		attributes := attributesOf(allRecords(recorder)[0])
		Expect(attributes).To(HaveKeyWithValue("cluster", "example"))
	})

	It("reports the logger name both as scope and as attribute", func() {
		logger, recorder := recordedLogger(machinerylog.InfoLevel)

		logger.Named("postgres").Info("record")

		recording := recorder.Result()
		Expect(recording).To(HaveKey(logtest.Scope{Name: "postgres"}))

		attributes := attributesOf(allRecords(recorder)[0])
		Expect(attributes).To(HaveKeyWithValue("logger", "postgres"))
	})

	It("drops the records the level enabler rejects", func() {
		logger, recorder := recordedLogger(machinerylog.ErrorLevel)

		logger.Info("filtered")
		logger.Error("kept")

		records := allRecords(recorder)
		Expect(records).To(HaveLen(1))
		Expect(records[0].Body.AsString()).To(Equal("kept"))
	})
})
