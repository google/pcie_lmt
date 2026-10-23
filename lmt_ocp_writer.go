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
	"strings"
	"time"

	structpb "google.golang.org/protobuf/types/known/structpb"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
)

// OCPWriter is the MarginHook that streams the OCP TestStepMeasurement artifacts of each margin
// point.
type OCPWriter struct{}

// ocpWriter is the OCPWriter registered on every receiver.
var ocpWriter OCPWriter

// Compile-time check that OCPWriter implements MarginHook.
var _ MarginHook = (*OCPWriter)(nil)

// newOCPWriter returns ocpWriter, which applies to every receiver.
func newOCPWriter(_ *receiver) MarginHook { return ocpWriter }

// Name returns the hook name for logging.
func (OCPWriter) Name() string { return "OCPWriter" }

// PostMargin streams the Step-Status and Step-BER measurements of point.
func (OCPWriter) PostMargin(ln *Lane, t *aspect, point *lmtpb.LinkMargin_Lane_MarginPoint, bitCount float64, _ time.Duration) error {
	unit := unitName(point, t)
	subcomp := &ocppb.Subcomponent{
		Type: ocppb.Subcomponent_BUS,
		Name: "PCIELMT-MARGINPOINT-PCI",
		Location: fmt.Sprintf("BDF=%s;RX=%1d;LN=%02d;Offset=%s",
			ln.cfg.GetUspBdf(), ln.rec.Number(), ln.laneNumber, pointOcpName(point)),
	}

	if !t.eyeScanMode || point.GetStatus() != lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING {
		m := &ocppb.Measurement{
			Name:           fmt.Sprintf("LN=%02d;Step-Status", ln.laneNumber),
			Value:          structpb.NewStringValue(strings.TrimPrefix(point.GetStatus().String(), "S_")),
			Unit:           unit,
			HardwareInfoId: ln.rx.hwinfo,
			Subcomponent:   subcomp,
			Validators:     []*ocppb.Validator{ln.statusVal},
		}
		ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}
		outputArtifact(ln.stepArtiOut)
	}

	if (point.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING ||
		point.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT) && (!t.eyeScanMode ||
		point.ErrorCount != 0) {
		m := &ocppb.Measurement{
			Name:           fmt.Sprintf("LN=%02d;Step-BER", ln.laneNumber),
			Value:          structpb.NewNumberValue(float64(point.ErrorCount) / bitCount),
			Unit:           unit,
			HardwareInfoId: ln.rx.hwinfo,
			Subcomponent:   subcomp,
		}
		if !t.errOutOK {
			m.Validators = []*ocppb.Validator{ln.berVal}
		}
		ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}
		outputArtifact(ln.stepArtiOut)
	}
	return nil
}
