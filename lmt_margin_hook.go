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
	"time"

	lmtpb "lmt_go.proto"
)

// MarginHook is called after each margin point, before the margin commands are cleared.
type MarginHook interface {
	// Name returns the hook name for logging.
	Name() string
	// PostMargin handles point, margined on ln with t. bitCount is the number of sampled bits,
	// and dwellActual is the actual dwell time.
	PostMargin(ln *Lane, t *aspect, point *lmtpb.LinkMargin_Lane_MarginPoint, bitCount float64, dwellActual time.Duration) error
}

// marginHookBuilders build the margin hooks of a receiver. A builder returns nil when its hook does
// not apply.
var marginHookBuilders = []func(rx *receiver) MarginHook{
	newOCPWriter,
	// Future hooks are registered here.
}

// buildMarginHooks returns the margin hooks that apply to rx.
func buildMarginHooks(rx *receiver) []MarginHook {
	var hooks []MarginHook
	for _, builder := range marginHookBuilders {
		if h := builder(rx); h != nil {
			hooks = append(hooks, h)
		}
	}
	return hooks
}
