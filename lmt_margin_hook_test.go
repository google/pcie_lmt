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
	"slices"
	"testing"
)

func TestBuildMarginHooks(t *testing.T) {
	a, b := &fakeHook{}, &fakeHook{}
	rx := &receiver{}
	// builder returns a builder of h, which checks that it builds for rx.
	builder := func(h MarginHook) func(*receiver) MarginHook {
		return func(got *receiver) MarginHook {
			if got != rx {
				t.Errorf("builder called with receiver %p, want %p", got, rx)
			}
			return h
		}
	}
	testCases := []struct {
		name     string
		builders []func(*receiver) MarginHook
		want     []MarginHook
	}{
		{
			name: "no builders",
		},
		{
			name:     "no hook applies",
			builders: []func(*receiver) MarginHook{builder(nil), builder(nil)},
		},
		{
			name:     "some hooks apply",
			builders: []func(*receiver) MarginHook{builder(a), builder(nil), builder(b)},
			want:     []MarginHook{a, b},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			origBuilders := marginHookBuilders
			t.Cleanup(func() { marginHookBuilders = origBuilders })
			marginHookBuilders = tc.builders

			if got := buildMarginHooks(rx); !slices.Equal(got, tc.want) {
				t.Errorf("buildMarginHooks() = %v, want %v", got, tc.want)
			}
		})
	}
}
