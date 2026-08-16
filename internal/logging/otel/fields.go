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
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap/zapcore"
)

// maxAttributeDepth bounds the expansion of nested values, guarding against
// pathological or recursive structures
const maxAttributeDepth = 8

// appendFields converts zap fields into attributes, appending them to dst.
//
// zap knows how to encode every field type into a map, which keeps object and
// array marshalers, namespaces and inline values working without reimplementing
// their encoding here. Records that describe themselves through
// zapcore.ObjectMarshaler, such as the PostgreSQL log ones, therefore keep
// their fields as individual attributes.
func appendFields(dst []attribute.KeyValue, fields []zapcore.Field) []attribute.KeyValue {
	for _, field := range fields {
		if field.Type == zapcore.SkipType {
			continue
		}

		encoder := zapcore.NewMapObjectEncoder()
		field.AddTo(encoder)
		for key, value := range encoder.Fields {
			dst = appendValue(dst, key, value, 0)
		}
	}

	return dst
}

// appendValue converts a single value into one or more attributes, expanding
// composite values into dotted keys the way structured log backends expect
func appendValue(dst []attribute.KeyValue, key string, value any, depth int) []attribute.KeyValue {
	if value == nil {
		return append(dst, attribute.String(key, ""))
	}

	if depth >= maxAttributeDepth {
		return append(dst, attribute.String(key, fmt.Sprintf("%v", value)))
	}

	switch typed := value.(type) {
	case bool:
		return append(dst, attribute.Bool(key, typed))
	case string:
		return append(dst, attribute.String(key, typed))
	case int:
		return append(dst, attribute.Int64(key, int64(typed)))
	case int8:
		return append(dst, attribute.Int64(key, int64(typed)))
	case int16:
		return append(dst, attribute.Int64(key, int64(typed)))
	case int32:
		return append(dst, attribute.Int64(key, int64(typed)))
	case int64:
		return append(dst, attribute.Int64(key, typed))
	case uint:
		return append(dst, unsignedAttribute(key, uint64(typed)))
	case uint8:
		return append(dst, attribute.Int64(key, int64(typed)))
	case uint16:
		return append(dst, attribute.Int64(key, int64(typed)))
	case uint32:
		return append(dst, attribute.Int64(key, int64(typed)))
	case uint64:
		return append(dst, unsignedAttribute(key, typed))
	case uintptr:
		return append(dst, unsignedAttribute(key, uint64(typed)))
	case float32:
		return append(dst, attribute.Float64(key, float64(typed)))
	case float64:
		return append(dst, attribute.Float64(key, typed))
	case []byte:
		return append(dst, attribute.String(key, string(typed)))
	case time.Time:
		// the layout the JSON output uses, so both representations of a record
		// carry comparable timestamps
		return append(dst, attribute.String(key, typed.Format(time.RFC3339Nano)))
	case time.Duration:
		return append(dst, attribute.String(key, typed.String()))
	case error:
		return append(dst, attribute.String(key, typed.Error()))
	case fmt.Stringer:
		return append(dst, attribute.String(key, typed.String()))
	case []any:
		for index, item := range typed {
			dst = appendValue(dst, fmt.Sprintf("%s.%d", key, index), item, depth+1)
		}
		return dst
	case map[string]any:
		for nested, item := range typed {
			dst = appendValue(dst, key+"."+nested, item, depth+1)
		}
		return dst
	}

	return appendReflected(dst, key, value, depth)
}

// appendReflected expands a value zap handed over untouched. Structures are
// projected through their JSON representation, which keeps the field names
// aligned with the JSON log output, and anything else is rendered.
func appendReflected(dst []attribute.KeyValue, key string, value any, depth int) []attribute.KeyValue {
	encoded, err := json.Marshal(value)
	if err != nil {
		return append(dst, attribute.String(key, fmt.Sprintf("%v", value)))
	}

	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return append(dst, attribute.String(key, fmt.Sprintf("%v", value)))
	}

	switch decoded.(type) {
	case map[string]any, []any:
		return appendValue(dst, key, decoded, depth+1)
	default:
		return append(dst, attribute.String(key, fmt.Sprintf("%v", value)))
	}
}

// unsignedAttribute keeps values that do not fit a signed 64 bit integer
// readable instead of wrapping them around
func unsignedAttribute(key string, value uint64) attribute.KeyValue {
	if value > 1<<63-1 {
		return attribute.String(key, fmt.Sprintf("%d", value))
	}

	return attribute.Int64(key, int64(value))
}
