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
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"

	log "github.com/golang/glog"
	lmtpb "lmt_go.proto"
)

// ConvertToHTML reads the global `lmts` (populated by ReadResult) and creates an HTML file with Google Charts.
func ConvertToHTML(htmlfn string) {
	if lmts == nil {
		log.Exit("No results found to convert to HTML.")
	}
	f, err := os.Create(htmlfn)
	if err != nil {
		log.Exit(err)
	}
	defer f.Close()

	fmt.Fprint(f, `<!DOCTYPE html>
<html>
<head>
    <title>PCIe Lane Margin Test Report</title>
    <script src="https://www.gstatic.com/charts/loader.js"></script>
    <script>
      google.charts.load('current', {'packages':['corechart']});
      google.charts.setOnLoadCallback(drawCharts);

      function drawCharts() {
        var timingData = new google.visualization.DataTable();
        timingData.addColumn('number', 'Lane Index');
        timingData.addColumn('number', 'Timing Margin [UI]');
        timingData.addColumn({type: 'string', role: 'tooltip', p: {html: true}});
        timingData.addColumn({type: 'string', role: 'style'});

        var voltageData = new google.visualization.DataTable();
        voltageData.addColumn('number', 'Lane Index');
        voltageData.addColumn('number', 'Voltage Margin [V]');
        voltageData.addColumn({type: 'string', role: 'tooltip', p: {html: true}});
        voltageData.addColumn({type: 'string', role: 'style'});
`)

	var timingRows []string
	var voltageRows []string
	var ticksBuilder []string

	type ReceiverInfo struct {
		indices []uint32
		bdf     string
		recv    string
	}
	recvMap := make(map[string]*ReceiverInfo)

	link := uint32(0)
	for _, lm := range lmts.GetLinkMargin() {
		if lm.GetReceiverLanes() == nil {
			continue
		}
		// make portwidth a multiple of 5 to leave gap and ease indexing.
		portwidth := slices.MaxFunc(lm.GetReceiverLanes(),
			func(a, b *lmtpb.LinkMargin_Lane) int {
				return int(a.GetLaneNumber()) - int(b.GetLaneNumber())
			}).GetLaneNumber()
		portwidth = ((portwidth / 5) + 1) * 5

		portstart := make([]uint32,
			lmtpb.LinkMargin_R_RESERVED.Number(), lmtpb.LinkMargin_R_RESERVED.Number())
		for i := uint32(1); i < uint32(lmtpb.LinkMargin_R_RESERVED.Number()); i++ {
			if slices.ContainsFunc(lm.GetReceiverLanes(), func(a *lmtpb.LinkMargin_Lane) bool {
				return uint32(a.GetReceiver().Number()) == i
			}) {
				portstart[i] = link
				link += portwidth
			}
		}
		for _, ln := range lm.GetReceiverLanes() {
			laneIndex := portstart[ln.GetReceiver().Number()] + ln.GetLaneNumber()
			bdf := lm.GetUspBdf()
			recvEnum := ln.GetReceiver()
			recv := recvEnum.String()
			lnNum := ln.GetLaneNumber()

			// Add only lane 0 ticks
			if lnNum == 0 {
				ticksBuilder = append(ticksBuilder, fmt.Sprintf("{v: %d, f: 'L0'}", laneIndex))
			}

			// Also collect for port mid-point
			rxName := "Rx" + string(recv[len(recv)-1])
			if recvEnum == lmtpb.LinkMargin_R_DSP_A1 {
				rxName = "RxA"
			} else if recvEnum == lmtpb.LinkMargin_R_USP_F6 {
				rxName = "RxF"
			}
			key := fmt.Sprintf("%s-%s", bdf, recv)
			if _, ok := recvMap[key]; !ok {
				recvMap[key] = &ReceiverInfo{
					bdf:  bdf,
					recv: rxName,
				}
			}
			// Only append unique laneIndex (to avoid inflate mid calc from multiple MarginPoints on same Lane)
			if !slices.Contains(recvMap[key].indices, laneIndex) {
				recvMap[key].indices = append(recvMap[key].indices, laneIndex)
			}

			processMargins := func(mpSet []*lmtpb.LinkMargin_Lane_MarginPoint, isTiming bool) {
				for _, mp := range mpSet {
					var margin float32

					tooltip := fmt.Sprintf("<b>Lane:</b> %d<br/><b>Direction:</b> %s<br/><b>Steps:</b> %d<br/><b>Errors:</b> %d",
						lnNum, mp.GetDirection().String(), mp.GetSteps(), mp.GetErrorCount())
					if mp.GetInfo() != "" {
						tooltip += fmt.Sprintf("<br/><b>Info:</b> %s", mp.GetInfo())
					}
					if mp.GetError() != "" {
						tooltip += fmt.Sprintf("<br/><b>Error:</b> <span style='color:red;'>%s</span>", mp.GetError())
					}

					// Color dots:
					// - Red if status is S_ERROR_OUT
					// - Orange if error_count is not 0
					// - Black if status is not S_MARGINING
					// - Default blue
					color := "blue"
					if mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT {
						color = "red"
					} else if mp.GetErrorCount() > 0 {
						color = "orange"
					} else if mp.GetStatus() != lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING {
						color = "black"
					}
					style := fmt.Sprintf("point { color: %s; }", color)

					if isTiming {
						margin = float32(mp.GetSteps()) * float32(ln.GetLaneParameter().GetMaxTimingOffset()) /
							float32(ln.GetLaneParameter().GetNumTimingSteps()*100)
						if mp.GetDirection() == lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT {
							margin = -margin
						}
						timingRows = append(timingRows, fmt.Sprintf("        [ %d, %f, '%s', '%s' ]", laneIndex, margin, escape(tooltip), style))
					} else {
						margin = float32(mp.GetSteps()) * float32(ln.GetLaneParameter().GetMaxVoltageOffset()) /
							float32(ln.GetLaneParameter().GetNumVoltageSteps()*100)
						if mp.GetDirection() == lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN {
							margin = -margin
						}
						voltageRows = append(voltageRows, fmt.Sprintf("        [ %d, %f, '%s', '%s' ]", laneIndex, margin, escape(tooltip), style))
					}
				}
			}
			processMargins(ln.GetTimingMargins(), true)
			processMargins(ln.GetVoltageMargins(), false)
		}
	}

	fmt.Fprintf(f, "        timingData.addRows([\n%s\n        ]);\n", strings.Join(timingRows, ",\n"))
	fmt.Fprintf(f, "        voltageData.addRows([\n%s\n        ]);\n", strings.Join(voltageRows, ",\n"))

	// Add per-receiver ticks in the middle of each port
	for _, ri := range recvMap {
		if len(ri.indices) == 0 {
			continue
		}
		var sum uint32
		for _, idx := range ri.indices {
			sum += idx
		}
		mid := sum / uint32(len(ri.indices))

		ticksBuilder = append(ticksBuilder, fmt.Sprintf("{v: %d, f: '%s %s'}", mid, ri.bdf, ri.recv))
	}
	ticksStr := fmt.Sprintf("[%s]", strings.Join(ticksBuilder, ", "))

	fmt.Fprintf(f, `
        var timingOptions = {
          title: 'Timing Margin per Lane Index',
          hAxis: {
            title: 'Lanes / Receivers',
            ticks: %s,
            slantedText: false
          },
          vAxis: {title: 'Timing Margin [%% UI]'},
          legend: 'none',
          tooltip: { isHtml: true },
          scatterLogScale: false
        };

        var voltageOptions = {
          title: 'Voltage Margin per Lane Index',
          hAxis: {
            title: 'Lanes / Receivers',
            ticks: %s,
            slantedText: false
          },
          vAxis: {title: 'Voltage Margin'},
          legend: 'none',
          tooltip: { isHtml: true },
          scatterLogScale: false
        };

        var timingChart = new google.visualization.ScatterChart(document.getElementById('timing_chart'));
        timingChart.draw(timingData, timingOptions);
`, ticksStr, ticksStr)

	fmt.Fprint(f, `
        var voltageChart = new google.visualization.ScatterChart(document.getElementById('voltage_chart'));
        voltageChart.draw(voltageData, voltageOptions);
      }
    </script>
    <style>
      body { font-family: sans-serif; margin: 20px; }
      h1, h2 { color: #333; }
      .chart { width: 100%; height: 500px; margin-bottom: 20px;}
      .infosection { margin-top: 20px; padding: 10px; border: 1px solid #ccc; background: #f9f9f9;}
      .error { color: red; font-weight: bold; }
      table { border-collapse: collapse; margin-top: 10px; width: 100%; }
      th, td { border: 1px solid #ddd; padding: 6px 12px; text-align: left; }
      th { background-color: #f2f2f2; }
    </style>
</head>
<body>
    <h1>PCIe Lane Margin Test Report</h1>
    <div id="timing_chart" class="chart"></div>
    <div id="voltage_chart" class="chart"></div>
    <div class="infosection">
`)

	fmt.Fprintln(f, "        <h2>Margining Modes and Test Environment</h2>")
	fmt.Fprintln(f, "        <ul>")

	hasUseCase1 := false
	hasUseCase2 := false
	hasUseCase3 := false
	for _, lm := range lmts.GetLinkMargin() {
		for _, spec := range lm.GetTestSpecs() {
			if spec.StartOffset != nil {
				hasUseCase1 = true
			} else if spec.EyeSize != nil {
				hasUseCase3 = true
			} else if spec.TargetOffset != nil {
				hasUseCase2 = true
			}
		}
		// Also check from lanes just in case
		for _, ln := range lm.GetReceiverLanes() {
			for _, spec := range []*lmtpb.LinkMargin_TestSpec{ln.GetTspec(), ln.GetVspec()} {
				if spec == nil {
					continue
				}
				if spec.StartOffset != nil {
					hasUseCase1 = true
				} else if spec.EyeSize != nil {
					hasUseCase3 = true
				} else if spec.TargetOffset != nil {
					hasUseCase2 = true
				}
			}
		}
	}
	// Fallback to assume Use Case 1 if none is clearly marked, to ensure something prints.
	if !hasUseCase1 && !hasUseCase2 && !hasUseCase3 {
		hasUseCase1 = true
	}

	if hasUseCase1 {
		fmt.Fprintln(f, "            <li><b>Eye Exploration (Use Case 1):</b> Test margining outwards step-by-step until max passing offset or error limit.</li>")
	}
	if hasUseCase2 {
		fmt.Fprintln(f, "            <li><b>Target Offset (Use Case 2):</b> Check only a target passing margin for pass/fail.</li>")
	}
	if hasUseCase3 {
		fmt.Fprintln(f, "            <li><b>Eye Size Check (Use Case 3):</b> Verify minimum off-centered eye size.</li>")
	}
	fmt.Fprintln(f, "        </ul>")

	fmt.Fprintln(f, "        <h2>Global Messages / Link Test Logs</h2>")
	fmt.Fprintln(f, "        <pre>")
	for _, lm := range lmts.GetLinkMargin() {
		if lm.GetMessage() != "" {
			fmt.Fprintf(f, "BDF %s (USP: %s, DSP: %s):\n", lm.GetBdf(), lm.GetUspBdf(), lm.GetDspBdf())
			parts := strings.Split(lm.GetMessage(), "|")
			for _, part := range parts {
				trimmed := strings.TrimSpace(part)
				if trimmed != "" {
					fmt.Fprintf(f, "      %s\n", trimmed)
				}
			}
		}
	}
	fmt.Fprintln(f, "        </pre>")

	fmt.Fprintln(f, "        <h2>LMR Measurements &amp; Non-measurement Errors</h2>")
	fmt.Fprintln(f, "        <table>")
	fmt.Fprintf(f, "            <tr><th>BDF</th><th>Receiver</th><th>Min Width [%%UI]</th><th>Min Height [mV]</th><th>Pass</th><th>Extra Info</th><th>Non-measurement Errors / Nak</th></tr>\n")

	type RecvKey struct {
		bdf  string
		recv string
	}
	type Summary struct {
		minWidth  float32
		minHeight float32
		pass      bool
		extra     string
		errs      []string
	}
	stats := make(map[RecvKey]*Summary)

	for _, lm := range lmts.GetLinkMargin() {
		bdf := lm.GetUspBdf()
		for _, ln := range lm.GetReceiverLanes() {
			recv := ln.GetReceiver().String()
			key := RecvKey{bdf, recv}

			if _, ok := stats[key]; !ok {
				stats[key] = &Summary{
					minWidth:  float32(math.Inf(1)),
					minHeight: float32(math.Inf(1)),
					pass:      true,
				}
			}
			s := stats[key]
			if ln.EyeWidth != nil {
				// Convert to %UI (i.e. * 100)
				w := ln.GetEyeWidth() * 100.0
				if w < s.minWidth {
					s.minWidth = w
				}
			}
			if ln.EyeHeight != nil {
				// Convert to mV (i.e. * 1000)
				h := ln.GetEyeHeight() * 1000.0
				if h < s.minHeight {
					s.minHeight = h
				}
			}

			if ln.Pass != nil && !ln.GetPass() {
				s.pass = false
			}
			if ln.GetExtraInfo() != "" {
				s.extra = ln.GetExtraInfo()
			}
			for _, mp := range append(ln.GetTimingMargins(), ln.GetVoltageMargins()...) {
				if mp.GetError() != "" {
					s.errs = append(s.errs, fmt.Sprintf("Lane %d %s steps %d: err=%s", ln.GetLaneNumber(), mp.GetDirection(), mp.GetSteps(), mp.GetError()))
				}
				if mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_NAK {
					s.errs = append(s.errs, fmt.Sprintf("Lane %d %s steps %d: NAK", ln.GetLaneNumber(), mp.GetDirection(), mp.GetSteps()))
				}
			}
		}
	}

	// Sort keys for stable output
	var keys []RecvKey
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].bdf != keys[j].bdf {
			return keys[i].bdf < keys[j].bdf
		}
		return keys[i].recv < keys[j].recv
	})

	for _, key := range keys {
		s := stats[key]
		wStr := "N/A"
		if !math.IsInf(float64(s.minWidth), 1) {
			wStr = fmt.Sprintf("%.1f%%", s.minWidth)
		}
		hStr := "N/A"
		if !math.IsInf(float64(s.minHeight), 1) {
			hStr = fmt.Sprintf("%.1f mV", s.minHeight)
		}
		errStr := strings.Join(s.errs, "<br/>")

		fmt.Fprintf(f, "            <tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%t</td><td>%s</td><td class='error'>%s</td></tr>\n",
			key.bdf, key.recv, wStr, hStr, s.pass, s.extra, errStr)
	}
	fmt.Fprintln(f, "        </table>")

	fmt.Fprintln(f, "    </div>")
	fmt.Fprintln(f, "</body></html>")
}

func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\"", "&quot;"), "'", "\\'")
}
