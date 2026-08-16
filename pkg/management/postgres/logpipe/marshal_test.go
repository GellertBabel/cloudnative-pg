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

package logpipe

import (
	"encoding/json"
	"reflect"
	"strings"

	"go.uber.org/zap/zapcore"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// encodeLogObject returns the fields a record writes to the log
func encodeLogObject(record zapcore.ObjectMarshaler) map[string]any {
	encoder := zapcore.NewMapObjectEncoder()
	Expect(record.MarshalLogObject(encoder)).To(Succeed())

	return encoder.Fields
}

// encodeJSON returns the fields the JSON representation of a record carries,
// which is what the logger used to produce by reflecting over it
func encodeJSON(record any) map[string]any {
	encoded, err := json.Marshal(record)
	Expect(err).ToNot(HaveOccurred())

	var result map[string]any
	Expect(json.Unmarshal(encoded, &result)).To(Succeed())

	return result
}

// jsonFieldNames returns the JSON field names of a record type
func jsonFieldNames(recordType reflect.Type) []string {
	var result []string
	for i := range recordType.NumField() {
		tag, _, _ := strings.Cut(recordType.Field(i).Tag.Get("json"), ",")
		if tag != "" && tag != "-" {
			result = append(result, tag)
		}
	}

	return result
}

var _ = Describe("Log record marshaling", func() {
	// a distinct value per field, so that a field written under the wrong name
	// is visible instead of matching by accident
	fullRecord := func() *LoggingRecord {
		record := &LoggingRecord{}
		value := reflect.ValueOf(record).Elem()
		for i := range value.NumField() {
			value.Field(i).SetString("value-" + value.Type().Field(i).Name)
		}

		return record
	}

	It("writes every field the JSON representation carries", func() {
		record := fullRecord()
		Expect(encodeLogObject(record)).To(Equal(encodeJSON(record)))

		fields := encodeLogObject(record)
		for _, name := range jsonFieldNames(reflect.TypeOf(*record)) {
			Expect(fields).To(HaveKey(name))
		}
	})

	It("omits the empty fields, as the JSON representation does", func() {
		record := &LoggingRecord{ErrorSeverity: "ERROR", Message: "division by zero"}

		Expect(encodeLogObject(record)).To(Equal(map[string]any{
			"error_severity": "ERROR",
			"message":        "division by zero",
		}))
	})

	It("writes the fields individually, not as an opaque value", func() {
		record := &LoggingRecord{SQLStateCode: "22012", Query: "SELECT 1/0;"}

		Expect(encodeLogObject(record)).To(HaveKeyWithValue("sql_state_code", "22012"))
		Expect(encodeLogObject(record)).To(HaveKeyWithValue("query", "SELECT 1/0;"))
	})

	Context("pgaudit records", func() {
		It("nests the audit fields under the PostgreSQL ones", func() {
			decorator := &PgAuditLoggingDecorator{
				LoggingRecord: &LoggingRecord{ErrorSeverity: "LOG"},
				Audit:         &PgAuditRecord{AuditType: "SESSION", Command: "SELECT"},
			}

			Expect(encodeLogObject(decorator)).To(Equal(map[string]any{
				"error_severity": "LOG",
				"audit": map[string]any{
					"audit_type": "SESSION",
					"command":    "SELECT",
				},
			}))
		})

		It("writes every audit field the JSON representation carries", func() {
			audit := &PgAuditRecord{}
			value := reflect.ValueOf(audit).Elem()
			for i := range value.NumField() {
				value.Field(i).SetString("value-" + value.Type().Field(i).Name)
			}

			Expect(encodeLogObject(audit)).To(Equal(encodeJSON(audit)))
			for _, name := range jsonFieldNames(reflect.TypeOf(*audit)) {
				Expect(encodeLogObject(audit)).To(HaveKey(name))
			}
		})

		It("skips the audit section when absent", func() {
			decorator := &PgAuditLoggingDecorator{
				LoggingRecord: &LoggingRecord{Message: "not an audit record"},
			}

			Expect(encodeLogObject(decorator)).To(Equal(map[string]any{
				"message": "not an audit record",
			}))
		})
	})
})
