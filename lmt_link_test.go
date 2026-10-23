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
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

func TestCreateReceiver(t *testing.T) {
	const (
		retimer   = 0x0040 // PCI_EXP_LINKSTA2_RETIMER
		retimers2 = 0x0080 // PCI_EXP_LINKSTA2_2RETIMERS
	)
	testCases := []struct {
		name    string
		rec     lmtpb.LinkMargin_ReceiverEnum
		lnkSta2 uint16
		// indErrorSampler is the capability in the Report response, and noReport
		// leaves the Report command unanswered.
		indErrorSampler bool
		noReport        bool
		wantNil      bool
		wantUSP      bool
		wantHwinfo   string
		wantLanes    int
		wantParallel bool
		wantMessage  string
	}{
		{
			name:    "broadcast",
			rec:     lmtpb.LinkMargin_R_BROADCAST0,
			lnkSta2: retimer | retimers2,
			wantNil: true,
		},
		{
			name:    "reserved",
			rec:     lmtpb.LinkMargin_R_RESERVED,
			lnkSta2: retimer | retimers2,
			wantNil: true,
		},
		{
			name:    "B2 without retimer",
			rec:     lmtpb.LinkMargin_R_RTU_B2,
			wantNil: true,
		},
		{
			name:            "B2 with retimer",
			rec:             lmtpb.LinkMargin_R_RTU_B2,
			lnkSta2:         retimer,
			indErrorSampler: true,
			wantHwinfo:      "BDF=invalid;RX=RTU_B2",
			wantLanes:       4,
			wantParallel:    true,
		},
		{
			name:    "D4 without 2 retimers",
			rec:     lmtpb.LinkMargin_R_RTU_D4,
			lnkSta2: retimer,
			wantNil: true,
		},
		{
			name:       "D4 with 2 retimers",
			rec:        lmtpb.LinkMargin_R_RTU_D4,
			lnkSta2:    retimer | retimers2,
			wantHwinfo: "BDF=invalid;RX=RTU_D4",
			wantLanes:  4,
		},
		{
			name:            "DSP_A1",
			rec:             lmtpb.LinkMargin_R_DSP_A1,
			indErrorSampler: true,
			wantHwinfo:      "BDF=invalid;RX=DSP_A1",
			wantLanes:       4,
			wantParallel:    true,
		},
		{
			name:            "USP_F6",
			rec:             lmtpb.LinkMargin_R_USP_F6,
			indErrorSampler: true,
			wantUSP:         true,
			wantHwinfo:      "BDF=invalid;RX=USP_F6",
			wantLanes:       2,
			wantParallel:    true,
		},
		{
			name:        "Report not answered",
			rec:         lmtpb.LinkMargin_R_DSP_A1,
			noReport:    true,
			wantHwinfo:  "BDF=invalid;RX=DSP_A1",
			wantLanes:   4,
			wantMessage: "LMR command failed",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// The fake LMR echoes every command, except that it answers a Report
			// command with the control capabilities.
			var last uint16
			origRead, origWrite := readWord, writeWord
			t.Cleanup(func() { readWord, writeWord = origRead, origWrite })
			writeWord = func(_ pci.Dev, _ int32, val uint16) { last = val }
			readWord = func(_ pci.Dev, addr int32) uint16 {
				if last>>3&0x7 != MarginTypeReport {
					return last
				}
				if tc.noReport {
					return 0x9c38 // NoCmd
				}
				var caps uint16
				if tc.indErrorSampler {
					caps = MskIndErrorSampler
				}
				return caps<<8 | last&0xff
			}
			lt := &linktest{
				usp: &port{width: 2, lmrAddr: 0x100},
				dsp: &port{width: 4, lmrAddr: 0x100},
				pb:  &lmtpb.LinkMargin{},
				wg:  new(sync.WaitGroup),
			}

			rx := createReceiver(lt, tc.rec, tc.lnkSta2)

			if tc.wantNil {
				if rx != nil {
					t.Errorf("createReceiver(%s, 0x%04x) = %+v, want nil", tc.rec, tc.lnkSta2, rx)
				}
				return
			}
			if rx == nil {
				t.Fatalf("createReceiver(%s, 0x%04x) = nil, want a receiver", tc.rec, tc.lnkSta2)
			}
			wantPort := lt.dsp
			if tc.wantUSP {
				wantPort = lt.usp
			}
			if rx.port != wantPort {
				t.Errorf("createReceiver(%s) port = %p, want %p", tc.rec, rx.port, wantPort)
			}
			if rx.rec != tc.rec {
				t.Errorf("createReceiver(%s) rec = %s, want %s", tc.rec, rx.rec, tc.rec)
			}
			if rx.hwinfo != tc.wantHwinfo {
				t.Errorf("createReceiver(%s) hwinfo = %q, want %q", tc.rec, rx.hwinfo, tc.wantHwinfo)
			}
			if len(rx.lanes) != tc.wantLanes {
				t.Errorf("createReceiver(%s) has %d lanes, want %d", tc.rec, len(rx.lanes), tc.wantLanes)
			}
			if rx.parallel != tc.wantParallel {
				t.Errorf("createReceiver(%s) parallel = %t, want %t", tc.rec, rx.parallel, tc.wantParallel)
			}
			wantHooks := []MarginHook{ocpWriter}
			if !slices.Equal(rx.marginHooks, wantHooks) {
				t.Errorf("createReceiver(%s) marginHooks = %v, want %v", tc.rec, rx.marginHooks, wantHooks)
			}
			msg := lt.pb.GetMessage()
			if tc.wantMessage == "" && msg != "" {
				t.Errorf("createReceiver(%s) set message %q, want none", tc.rec, msg)
			}
			if tc.wantMessage != "" && !strings.Contains(msg, tc.wantMessage) {
				t.Errorf("createReceiver(%s) message = %q, want it to contain %q", tc.rec, msg, tc.wantMessage)
			}
		})
	}
}

func TestOutputLaneError(t *testing.T) {
	testCases := []struct {
		name       string
		hwinfo     string
		laneNumber uint32
		err        error
		wantMsg    string
	}{
		{
			name:       "lane 0",
			hwinfo:     "BDF=0000:16:01.0;RX=USP_F6",
			laneNumber: 0,
			err:        errors.New("LMR command failed"),
			wantMsg:    "Lane 0 failed early: LMR command failed",
		},
		{
			name:       "wrapped error",
			hwinfo:     "BDF=0000:16:01.0;RX=RTD_C3",
			laneNumber: 15,
			err:        fmt.Errorf("setupAspect: %w", errors.New("no dwell time")),
			wantMsg:    "Lane 15 failed early: setupAspect: no dwell time",
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
			r := &receiver{hwinfo: tc.hwinfo}

			r.outputLaneError(&Lane{laneNumber: tc.laneNumber}, tc.err)

			data, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatalf("os.ReadFile(%q) failed: %v", f.Name(), err)
			}
			got := &ocppb.OutputArtifact{}
			if err := protojson.Unmarshal(bytes.TrimSpace(data), got); err != nil {
				t.Fatalf("protojson.Unmarshal(%q) failed: %v", data, err)
			}
			// The sequence number and timestamp vary between runs.
			got.SequenceNumber = 0
			got.Timestamp = nil
			want := &ocppb.OutputArtifact{
				Artifact: &ocppb.OutputArtifact_TestStepArtifact{
					TestStepArtifact: &ocppb.TestStepArtifact{
						Artifact: &ocppb.TestStepArtifact_Error{
							Error: &ocppb.Error{
								Symptom: "pcie-lane-execution-error",
								Message: tc.wantMsg,
							},
						},
						TestStepId: tc.hwinfo,
					},
				},
			}
			if !proto.Equal(got, want) {
				t.Errorf("outputLaneError(lane %d, %v) wrote %v, want %v", tc.laneNumber, tc.err, got, want)
			}
		})
	}
}
