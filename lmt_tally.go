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

// Misc. result analysis functions. They are not used by the lmt by default.

import (
	"fmt"
)

// PortResult contains pass-fail info at (pseudo)port-level.
type PortResult struct {
	BDF           string
	NumLaneTested int
	NumLanePassed int
	Message       string
}

// TestResult contains pass-fail info at the top-level of a test run.
type TestResult struct {
	NumLaneTested int
	NumLanePassed int
	PortResults   []*PortResult
	Pass          bool
}

// TallyResults tallies pass-fail info.
func TallyResults() *TestResult {
	res := new(TestResult)
	const numLanes = 16 // estimated array-initial-size of lanes per port.
	// Checks that all lanes pass, or the test fails.
	res.NumLanePassed = -1
	res.NumLaneTested = -1
	res.PortResults = make([]*PortResult, 0, maxRxPerLink*numLanes)
	res.Pass = true
	for _, lt := range lts {
		for _, rx := range lt.allRx {
			if rx == nil {
				continue
			} // Skips R_BROADCAST-1 = 0 and R_RESERVED = 7
			if !rx.testReady {
				continue
			}

			rpt := new(PortResult)
			rpt.NumLanePassed = -1
			rpt.BDF = rx.port.dev.BDFString()
			failedString := ""
			for _, ln := range rx.lanes {
				res.NumLaneTested++
				rpt.NumLaneTested++
				if !ln.Pass {
					res.Pass = false
					failedString = "(Failed)"
				} else {
					res.NumLanePassed++
					rpt.NumLanePassed++
				}
			}
			rpt.Message = fmt.Sprintf("%s on %s: %d lanes tested, %d passed. %s", rx.rec.String(),
				rpt.BDF, rpt.NumLaneTested, rpt.NumLanePassed, failedString)
			res.PortResults = append(res.PortResults, rpt)
		}
	}
	return res
}
