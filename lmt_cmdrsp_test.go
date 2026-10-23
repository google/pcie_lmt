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
	"strings"
	"testing"

	pci "pciutils"
)

func newCmdRsp(raw uint16) *cmdRsp {
	var cr cmdRsp
	cr.decode(raw)
	return &cr
}

func TestRspString(t *testing.T) {
	testCases := []struct {
		name string
		req  uint16
		rsp  uint16
		want string
	}{
		// Status values that do not answer the request.
		{
			// Seen on hardware: the DSP's Rx(C) Report command is read back
			// at the USP while the USP waits for its own NoCmd.
			name: "NoCmd with echoed Report command",
			req:  0x9c38,
			rsp:  0x880b,
			want: "Unexpected (not a response to NoCmd [rec: Broadcast]): Report [rec: C3, usg: 0, pay: 0x88]",
		},
		{
			name: "stale Report during Step command",
			req:  0x0519,
			rsp:  0x880b,
			want: "Unexpected (not a response to StepTMargin [rec: A1]): Report [rec: C3, usg: 0, pay: 0x88]",
		},
		{
			name: "Report from another receiver",
			req:  0x8809,
			rsp:  0x1f0b,
			want: "Unexpected (not a response to Report [rec: A1]): Report [rec: C3, usg: 0, pay: 0x1f]",
		},
		{
			name: "Report with Usage Model 1",
			req:  0x880b,
			rsp:  0x1f4b,
			want: "Unexpected (not a response to Report [rec: C3]): Report [rec: C3, usg: 1, pay: 0x1f]",
		},
		// Non-Report statuses decode from their own fields, matching or not.
		{
			name: "stale Set response during NoCmd",
			req:  0x9c38,
			rsp:  0x0f13,
			want: "ToNrmlSetting   [rec: C3]",
		},
		{
			name: "NoCmd status during Report",
			req:  0x880b,
			rsp:  0x9c38,
			want: "NoCmd",
		},
		{
			name: "stale Step response during Report",
			req:  0x8809,
			rsp:  0x8019,
			want: "StepTMargin     [rec: A1, status: Margining, errCnt: 0]",
		},
		// Normal command and response pairs.
		{
			name: "NoCmd",
			req:  0x9c38,
			rsp:  0x9c38,
			want: "NoCmd",
		},
		{
			name: "ReportCtrlCap",
			req:  0x880b,
			rsp:  0x1f0b,
			want: "ReportCtrlCap   [rec: C3, indErrSampler: true, sampleMethod: rate, indLRTiming: true, indUDVoltage: true, voltageSupported: true]",
		},
		{
			name: "ReportNumVStep",
			req:  0x8909,
			rsp:  0x7f09,
			want: "ReportNumVStep  [rec: A1, numSteps: 127]",
		},
		{
			name: "ReportNumTStep",
			req:  0x8a09,
			rsp:  0x3f09,
			want: "ReportNumTStep  [rec: A1, numSteps: 63]",
		},
		{
			name: "ReportMaxTOfst",
			req:  0x8b09,
			rsp:  0x3209,
			want: "ReportMaxTOfst  [rec: A1, offset: 50]",
		},
		{
			name: "ReportMaxVOfst",
			req:  0x8c09,
			rsp:  0x2309,
			want: "ReportMaxVOfst  [rec: A1, offset: 35]",
		},
		{
			name: "ReportSmplRateV",
			req:  0x8d09,
			rsp:  0x3f09,
			want: "ReportSmplRateV [rec: A1, rate: 63]",
		},
		{
			name: "ReportSmplRateT",
			req:  0x8e09,
			rsp:  0x3f09,
			want: "ReportSmplRateT [rec: A1, rate: 63]",
		},
		{
			name: "ReportSmplCount",
			req:  0x8f09,
			rsp:  0x2709,
			want: "ReportSmplCount [rec: A1, count: 39]",
		},
		{
			name: "ReportMaxLanes",
			req:  0x9009,
			rsp:  0x0009,
			want: "ReportMaxLanes  [rec: A1, lanes: 0]",
		},
		{
			name: "ReportReserved",
			req:  0x9509,
			rsp:  0x0009,
			want: "ReportReserved  [rec: A1, cmd: 0x95, pay: 0x00]",
		},
		{
			name: "AccessRetimerReg",
			req:  0x100a,
			rsp:  0xab0a,
			want: "AccessRetimerReg[rec: B2, regValue: 0xab]",
		},
		{
			name: "ClearErrorLog",
			req:  0x5511,
			rsp:  0x5511,
			want: "ClearErrorLog   [rec: A1]",
		},
		{
			name: "StepTMargin",
			req:  0x0519,
			rsp:  0x8319,
			want: "StepTMargin     [rec: A1, status: Margining, errCnt: 3]",
		},
		{
			name: "StepVMargin",
			req:  0x0521,
			rsp:  0x4021,
			want: "StepVMargin     [rec: A1, status: SettingUp, errCnt: 0]",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := newCmdRsp(tc.req)
			if got := newCmdRsp(tc.rsp).RspString(req); got != tc.want {
				t.Errorf("RspString(0x%04x) for req 0x%04x = %q, want %q", tc.rsp, tc.req, got, tc.want)
			}
		})
	}
}

func TestReqString(t *testing.T) {
	testCases := []struct {
		name string
		req  uint16
		want string
	}{
		{name: "NoCmd", req: 0x9c38, want: "NoCmd"},
		{name: "ReportCtrlCap", req: 0x880b, want: "ReportCtrlCap   [rec: C3]"},
		{name: "ReportReserved", req: 0x9509, want: "ReportReserved  [rec: A1, cmd: 0x95]"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newCmdRsp(tc.req).ReqString(); got != tc.want {
				t.Errorf("ReqString(0x%04x) = %q, want %q", tc.req, got, tc.want)
			}
		})
	}
}

func TestGetIndErrorSampler(t *testing.T) {
	const addr = 0x100
	reported := func(writes []uint16) bool { return slices.Contains(writes, 0x880b) }
	testCases := []struct {
		name string
		// respond returns the status register value given the commands written so far.
		respond    func(writes []uint16) uint16
		want       bool
		wantErr    string
		wantWrites []uint16
	}{
		{
			name: "IndErrorSampler set",
			respond: func(writes []uint16) uint16 {
				if writes[len(writes)-1] == 0x880b {
					return 0x1f0b
				}
				return 0x9c38
			},
			want:       true,
			wantWrites: []uint16{0x9c38, 0x880b, 0x9c38},
		},
		{
			name: "IndErrorSampler clear",
			respond: func(writes []uint16) uint16 {
				if writes[len(writes)-1] == 0x880b {
					return 0x0f0b
				}
				return 0x9c38
			},
			wantWrites: []uint16{0x9c38, 0x880b, 0x9c38},
		},
		{
			// The status keeps the Report response, like a port that mirrors the last command.
			name: "trailing NoCmd not reflected",
			respond: func(writes []uint16) uint16 {
				if reported(writes) {
					return 0x1f0b
				}
				return 0x9c38
			},
			want:       true,
			wantErr:    "no command after report control capabilities",
			wantWrites: []uint16{0x9c38, 0x880b, 0x9c38},
		},
		{
			name:       "Report never answered",
			respond:    func([]uint16) uint16 { return 0x9c38 },
			wantErr:    "LMR command failed",
			wantWrites: []uint16{0x9c38, 0x880b},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var writes []uint16
			origRead, origWrite := readWord, writeWord
			t.Cleanup(func() { readWord, writeWord = origRead, origWrite })
			writeWord = func(_ pci.Dev, a int32, val uint16) {
				if a != addr {
					t.Errorf("writeWord(0x%x, 0x%04x): want address 0x%x", a, val, addr)
				}
				writes = append(writes, val)
			}
			readWord = func(_ pci.Dev, a int32) uint16 {
				if a != addr+2 {
					t.Errorf("readWord(0x%x): want address 0x%x", a, addr+2)
				}
				return tc.respond(writes)
			}
			ln := &Lane{addr: addr, rec: 3} // C3

			got, err := ln.GetIndErrorSampler()
			if got != tc.want {
				t.Errorf("GetIndErrorSampler() = %t, want %t", got, tc.want)
			}
			if tc.wantErr == "" && err != nil {
				t.Errorf("GetIndErrorSampler() error = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("GetIndErrorSampler() error = %v, want error containing %q", err, tc.wantErr)
			}
			if !slices.Equal(writes, tc.wantWrites) {
				t.Errorf("GetIndErrorSampler() wrote %04x, want %04x", writes, tc.wantWrites)
			}
		})
	}
}
