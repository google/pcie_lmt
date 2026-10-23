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
	"bytes"
	"os"
	"testing"

	structpb "google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
)

func TestOCPWriterPostMargin(t *testing.T) {
	const (
		hwinfo   = "BDF=0000:17:00.0;RX=USP_F6"
		unit     = "Unit=UI;Step=005;Dir=RIGHT   ;Offset=0.250"
		bitCount = 1000
	)
	statusVal := &ocppb.Validator{Name: "status"}
	berVal := &ocppb.Validator{Name: "ber"}
	subcomp := &ocppb.Subcomponent{
		Type:     ocppb.Subcomponent_BUS,
		Name:     "PCIELMT-MARGINPOINT-PCI",
		Location: "BDF=0000:17:00.0;RX=6;LN=03;Offset=T:+0.250000UI",
	}
	statusM := func(status string) *ocppb.Measurement {
		return &ocppb.Measurement{
			Name:           "LN=03;Step-Status",
			Value:          structpb.NewStringValue(status),
			Unit:           unit,
			HardwareInfoId: hwinfo,
			Subcomponent:   subcomp,
			Validators:     []*ocppb.Validator{statusVal},
		}
	}
	berM := func(ber float64, validators ...*ocppb.Validator) *ocppb.Measurement {
		return &ocppb.Measurement{
			Name:           "LN=03;Step-BER",
			Value:          structpb.NewNumberValue(ber),
			Unit:           unit,
			HardwareInfoId: hwinfo,
			Subcomponent:   subcomp,
			Validators:     validators,
		}
	}
	testCases := []struct {
		name       string
		status     lmtpb.LinkMargin_Lane_MarginPoint_StatusEnum
		errorCount uint32
		t          *aspect
		want       []*ocppb.Measurement
	}{
		{
			name:       "margining",
			status:     lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING,
			errorCount: 2,
			t:          &aspect{},
			want:       []*ocppb.Measurement{statusM("MARGINING"), berM(0.002, berVal)},
		},
		{
			name:       "error out allowed",
			status:     lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT,
			errorCount: 5,
			t:          &aspect{errOutOK: true},
			want:       []*ocppb.Measurement{statusM("ERROR_OUT"), berM(0.005)},
		},
		{
			name:   "eye scan margining without errors",
			status: lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING,
			t:      &aspect{eyeScanMode: true},
		},
		{
			name:       "eye scan margining with errors",
			status:     lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING,
			errorCount: 2,
			t:          &aspect{eyeScanMode: true},
			want:       []*ocppb.Measurement{berM(0.002, berVal)},
		},
		{
			name:   "eye scan NAK",
			status: lmtpb.LinkMargin_Lane_MarginPoint_S_NAK,
			t:      &aspect{eyeScanMode: true},
			want:   []*ocppb.Measurement{statusM("NAK")},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "ocp_pipe")
			if err != nil {
				t.Fatalf("os.CreateTemp() failed: %v", err)
			}
			defer f.Close()
			origPipe := ocpPipe
			ocpPipe = f
			t.Cleanup(func() { ocpPipe = origPipe })
			mStepArti := &ocppb.TestStepArtifact{}
			ln := &Lane{
				cfg:        &lmtpb.LinkMargin{UspBdf: proto.String("0000:17:00.0")},
				laneNumber: 3,
				rec:        lmtpb.LinkMargin_R_USP_F6,
				rx:         &receiver{hwinfo: hwinfo},
				stepArtiOut: &ocppb.OutputArtifact{
					Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: mStepArti},
				},
				mStepArti: mStepArti,
				statusVal: statusVal,
				berVal:    berVal,
			}
			point := &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction:  lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT,
				Steps:      5,
				PercentUi:  proto.Float32(0.25),
				Status:     tc.status,
				ErrorCount: tc.errorCount,
			}

			if err := ocpWriter.PostMargin(ln, tc.t, point, bitCount, 0); err != nil {
				t.Errorf("PostMargin() = %v, want nil", err)
			}

			data, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatalf("os.ReadFile(%q) failed: %v", f.Name(), err)
			}
			var got []*ocppb.Measurement
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				arti := &ocppb.OutputArtifact{}
				if err := protojson.Unmarshal(line, arti); err != nil {
					t.Fatalf("protojson.Unmarshal(%q) failed: %v", line, err)
				}
				got = append(got, arti.GetTestStepArtifact().GetMeasurement())
			}
			if len(got) != len(tc.want) {
				t.Fatalf("PostMargin() streamed %d measurements %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if !proto.Equal(got[i], tc.want[i]) {
					t.Errorf("PostMargin() measurement %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestOCPWriterName(t *testing.T) {
	if got, want := ocpWriter.Name(), "OCPWriter"; got != want {
		t.Errorf("OCPWriter.Name() = %q, want %q", got, want)
	}
}
