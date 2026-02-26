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

package postgres

import "testing"

func FuzzIsTablespaceNameValid(f *testing.F) {
	// Seed corpus covering each validation branch
	f.Add("tablespace1")
	f.Add("_leading_underscore")
	f.Add("with$dollar")
	f.Add("pg_reserved")
	f.Add("1starts_with_digit")
	f.Add("has+special")
	f.Add("")
	f.Add("tablespace1tablespace2tablespace3tablespace4tablespace5_12345678")

	f.Fuzz(func(_ *testing.T, name string) {
		// IsTablespaceNameValid must never panic regardless of input
		_, _ = IsTablespaceNameValid(name)
	})
}
