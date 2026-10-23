// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lanemargintest

import (
	"flag"
	"os"
	"testing"
)

// captureLog also logs to stderr, and redirects stderr to a file until the test ends. The
// returned func reads what was written so far.
func captureLog(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("os.CreateTemp() failed: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = origStderr
		f.Close()
	})
	origAlsoToStderr := flag.Lookup("alsologtostderr").Value.String()
	if err := flag.Set("alsologtostderr", "true"); err != nil {
		t.Fatalf("flag.Set(alsologtostderr) failed: %v", err)
	}
	t.Cleanup(func() { flag.Set("alsologtostderr", origAlsoToStderr) })
	return func() string {
		data, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("os.ReadFile(%q) failed: %v", f.Name(), err)
		}
		return string(data)
	}
}
