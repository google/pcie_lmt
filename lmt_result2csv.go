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

// Converts an LMT result pbtxt to a csv for ease of analysis.
// Use pcie-lmt-result2csv-plotter to view the CSV output.
// This converter is intentionally coded in a straightforward way. The user is expected to tweak
// this code to their need.

import (
	"encoding/csv"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"

	log "github.com/golang/glog"
	"google.golang.org/protobuf/encoding/prototext"
	lmtpb "lmt_go.proto"
)

// ReadResult ingests a LinkMarginTest pbtxt file for converting to a csv.
func ReadResult(resfn string) {
	// Checks that the config proto exists.
	if _, err := os.Stat(resfn); os.IsNotExist(err) {
		log.Exit(err)
	}

	data, err := os.ReadFile(resfn)
	if err != nil {
		log.Exit(err)
	}
	// Allocate a new PciConfigField to hold the deserialized proto.
	lmts = new(lmtpb.LinkMarginTest)
	opt := prototext.UnmarshalOptions{
		AllowPartial:   false,
		DiscardUnknown: true,
	}
	if err := opt.Unmarshal(data, lmts); err != nil {
		log.Exit(err)
	}
}

// ConvertToCsv converts the lmts LinkMarginTest protobuf to a csv file.
func ConvertToCsv(csvfn string) {
	f, err := os.Create(csvfn)
	if err != nil {
		log.Exit(err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	const (
		eBDF        = iota
		eReceiver   = iota
		eLane       = iota
		eDirection  = iota
		eSteps      = iota
		eStatus     = iota
		eErrorCount = iota
		eSamples    = iota
		eLog10BER   = iota
		eTmargin    = iota
		eTlane      = iota
		eVmargin    = iota
		eVlane      = iota
		eCorner     = iota
		eLeft       = iota
		eRight      = iota
		eBottom     = iota
		eTop        = iota
		eSize       = iota
	)
	hdr := make([]string, eSize)
	hdr[eBDF] = "BDF"
	hdr[eReceiver] = "Receiver"
	hdr[eLane] = "Lane"
	hdr[eDirection] = "Direction"
	hdr[eSteps] = "Steps"
	hdr[eStatus] = "Status"
	hdr[eErrorCount] = "ErrorCount"
	hdr[eSamples] = "Samples"
	hdr[eLog10BER] = "Log10BER"
	hdr[eTmargin] = "Tmargin"
	hdr[eTlane] = "Tlane"
	hdr[eVmargin] = "Vmargin"
	hdr[eVlane] = "Vlane"
	hdr[eCorner] = "Corner"
	hdr[eLeft] = "Left[UI]"
	hdr[eRight] = "Right[UI]"
	hdr[eBottom] = "Bottom[V]"
	hdr[eTop] = "Top[V]"

	w.Write(hdr)
	w.Flush()

	link := uint32(0) // This is used to separate links in the plot
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
		n := uint32(0)
		for i := uint32(1); i < uint32(lmtpb.LinkMargin_R_RESERVED.Number()); i++ {
			if slices.ContainsFunc(lm.GetReceiverLanes(), func(a *lmtpb.LinkMargin_Lane) bool {
				return uint32(a.GetReceiver().Number()) == i
			}) {
				n = n + portwidth
				portstart[i] = n
			}
		}
		n = n + portwidth

		rbdf := make([]string, eSize)
		rbdf[eBDF] = "\"" + lm.GetUspBdf() + "\"" // Prevents converting to dates.
		w.Write(rbdf)
		w.Flush()
		for _, ln := range lm.GetReceiverLanes() {
			eye := make([]string, eSize)
			eye[eBDF] = "\"" + lm.GetUspBdf() + "\"" // Prevents converting to dates.
			eye[eReceiver] = ln.GetReceiver().String()
			eye[eLane] = fmt.Sprintf("%d", ln.GetLaneNumber())

			r := make([]string, eSize)
			r[eReceiver] = ln.GetReceiver().String()
			r[eLane] = fmt.Sprintf("%d", ln.GetLaneNumber())
			for _, mp := range append(ln.GetTimingMargins(), ln.GetVoltageMargins()...) {
				r[eDirection] = mp.GetDirection().String()
				r[eSteps] = fmt.Sprintf("%d", mp.GetSteps())
				r[eStatus] = mp.GetStatus().String()
				errcnt := mp.GetErrorCount()
				r[eErrorCount] = fmt.Sprintf("%d", errcnt)
				if mp.SampleCount != nil {
					r[eSamples] = fmt.Sprintf("%d", mp.GetSampleCount())
					if errcnt == 0 {
						r[eLog10BER] = "0"
					} else {
						ber := math.Log10(float64(errcnt) / math.Pow(2.0, float64(mp.GetSampleCount())/3.0))
						r[eLog10BER] = fmt.Sprintf("%f", ber)
					}
				} else {
					r[eSamples] = ""
					r[eLog10BER] = ""
				}

				r[eTmargin] = ""
				r[eTlane] = ""
				r[eVmargin] = ""
				r[eVlane] = ""
				var margin float32
				lane := link + portstart[ln.GetReceiver().Number()] + ln.GetLaneNumber()
				if mp.PercentUi != nil {
					// Instead of margin = mp.GetPercentUi()
					// Recalculates the percent UI.
					// This allows the result.pbtxt to be fixed and applied.
					margin = float32(mp.GetSteps()) * float32(ln.GetLaneParameter().GetMaxTimingOffset()) /
						float32(ln.GetLaneParameter().GetNumTimingSteps()*100)
					if mp.GetDirection() == lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT {
						margin = -margin
					}
					r[eTmargin] = fmt.Sprintf("%f", margin)
					r[eTlane] = fmt.Sprintf("%d", lane)
				} else if mp.Voltage != nil {
					// Instead of margin = mp.GetVoltage()
					// Recalculates the voltage, in case of some device reads false parameters.
					// This allows the result.pbtxt to be fixed and applied.
					margin = float32(mp.GetSteps()) * float32(ln.GetLaneParameter().GetMaxVoltageOffset()) /
						float32(ln.GetLaneParameter().GetNumVoltageSteps()*100)
					if mp.GetDirection() == lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN {
						margin = -margin
					}
					r[eVmargin] = fmt.Sprintf("%f", margin)
					r[eVlane] = fmt.Sprintf("%d", lane)
				}

				// wasd vs. hjkl: gamer=pass; vi=fail
				r[eCorner] = ""
				if strings.Contains(mp.GetInfo(), "MAX PASSING") {
					eye[eCorner] = "eye corners"
					switch mp.GetDirection() {
					case lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT:
						r[eCorner] = "A"
						eye[eLeft] = r[eTmargin]
					case lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT:
						r[eCorner] = "D"
						eye[eRight] = r[eTmargin]
					case lmtpb.LinkMargin_Lane_MarginPoint_D_UP:
						r[eCorner] = "W"
						eye[eTop] = r[eVmargin]
					case lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN:
						r[eCorner] = "S"
						eye[eBottom] = r[eVmargin]
					}
				} else if strings.Contains(mp.GetInfo(), "MIN FAILING") {
					switch mp.GetDirection() {
					case lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT:
						r[eCorner] = "H"
					case lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT:
						r[eCorner] = "L"
					case lmtpb.LinkMargin_Lane_MarginPoint_D_UP:
						r[eCorner] = "K"
					case lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN:
						r[eCorner] = "J"
					}
				}
				w.Write(r)
			}
			if eye[eCorner] != "" {
				w.Write(eye)
			}
		}
		w.Flush()
		link = link + n
	}
}
