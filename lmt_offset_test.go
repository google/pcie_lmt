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
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

func TestPointOcpName(t *testing.T) {
	// newPoint sets both offsets, so each case also checks which offset is used.
	newPoint := func(dir lmtpb.LinkMargin_Lane_MarginPoint_DirectionEnum) *lmtpb.LinkMargin_Lane_MarginPoint {
		return &lmtpb.LinkMargin_Lane_MarginPoint{
			Direction: dir,
			Voltage:   proto.Float32(0.25),
			PercentUi: proto.Float32(0.125),
		}
	}
	testCases := []struct {
		name  string
		point *lmtpb.LinkMargin_Lane_MarginPoint
		want  string
	}{
		{
			name:  "up",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_UP),
			want:  "V:+0.250000V",
		},
		{
			name:  "down",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN),
			want:  "V:-0.250000V",
		},
		{
			name:  "up-down",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_UD),
			want:  "V:0.250000V",
		},
		{
			name:  "right",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT),
			want:  "T:+0.125000UI",
		},
		{
			name:  "left",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT),
			want:  "T:-0.125000UI",
		},
		{
			name:  "left-right",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_LR),
			want:  "T:0.125000UI",
		},
		{
			name:  "unknown",
			point: newPoint(lmtpb.LinkMargin_Lane_MarginPoint_D_UNKNOWN),
			want:  "",
		},
		{
			name:  "nil point",
			point: nil,
			want:  "",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pointOcpName(tc.point); got != tc.want {
				t.Errorf("pointOcpName(%v) = %q, want %q", tc.point, got, tc.want)
			}
		})
	}
}

func TestUnitName(t *testing.T) {
	testCases := []struct {
		name    string
		point   *lmtpb.LinkMargin_Lane_MarginPoint
		voltage bool
		want    string
	}{
		{
			name: "voltage",
			point: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN,
				Steps:     5,
				Voltage:   proto.Float32(0.25),
			},
			voltage: true,
			want:    "Unit=V;Step=005;Dir=DOWN    ;Offset=0.2500",
		},
		{
			name: "timing",
			point: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT,
				Steps:     12,
				PercentUi: proto.Float32(0.125),
			},
			want: "Unit=UI;Step=012;Dir=RIGHT   ;Offset=0.125",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitName(tc.point, &aspect{VnotT: tc.voltage}); got != tc.want {
				t.Errorf("unitName(%v, VnotT=%t) = %q, want %q", tc.point, tc.voltage, got, tc.want)
			}
		})
	}
}

func TestMarginOcpLocation(t *testing.T) {
	const addr = 0x100
	testCases := []struct {
		name         string
		voltage      bool
		offset       uint16
		wantLocation string
		wantUnit     string
	}{
		{
			name:         "timing right",
			offset:       5,
			wantLocation: "BDF=0000:17:00.0;RX=6;LN=03;Offset=T:+0.250000UI",
			wantUnit:     "Unit=UI;Step=005;Dir=RIGHT   ;Offset=0.250",
		},
		{
			name:         "timing left",
			offset:       TimingDirMask | 5,
			wantLocation: "BDF=0000:17:00.0;RX=6;LN=03;Offset=T:-0.250000UI",
			wantUnit:     "Unit=UI;Step=005;Dir=LEFT    ;Offset=0.250",
		},
		{
			name:         "voltage up-down",
			voltage:      true,
			offset:       5,
			wantLocation: "BDF=0000:17:00.0;RX=6;LN=03;Offset=V:0.250000V",
			wantUnit:     "Unit=V;Step=005;Dir=UD      ;Offset=0.2500",
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
			// The fake LMR answers step margin commands with Margining and echoes
			// the other commands.
			var last uint16
			origRead, origWrite := readWord, writeWord
			t.Cleanup(func() { readWord, writeWord = origRead, origWrite })
			writeWord = func(_ pci.Dev, _ int32, val uint16) { last = val }
			readWord = func(pci.Dev, int32) uint16 {
				if typ := last >> 3 & 0x7; typ == MarginTypeTiming || typ == MarginTypeVoltage {
					return StepMarginExecutionStatusMargining<<StepMarginExecutionStatusPos<<8 | last&0xff
				}
				return last
			}
			mStepArti := &ocppb.TestStepArtifact{}
			ln := &Lane{
				cfg:        &lmtpb.LinkMargin{UspBdf: proto.String("0000:17:00.0")},
				laneNumber: 3,
				addr:       addr,
				rec:        lmtpb.LinkMargin_R_USP_F6,
				rx:         &receiver{marginHooks: []MarginHook{ocpWriter}},
				param: &lmtpb.LinkMargin_Lane_Parameters{
					NumTimingSteps:     10,
					MaxTimingOffset:    50,
					IndLeftRightTiming: true,
					NumVoltageSteps:    10,
					MaxVoltageOffset:   50,
				},
				stepArtiOut: &ocppb.OutputArtifact{
					Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: mStepArti},
				},
				mStepArti: mStepArti,
				statusVal: &ocppb.Validator{},
				berVal:    &ocppb.Validator{},
			}

			// A zero dwell ends the step at the first Margining status.
			if _, err := ln.margin(tc.offset, &aspect{VnotT: tc.voltage}); err != nil {
				t.Fatalf("margin(0x%02x) failed: %v", tc.offset, err)
			}

			data, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatalf("os.ReadFile(%q) failed: %v", f.Name(), err)
			}
			line, _, _ := bytes.Cut(data, []byte("\n"))
			got := &ocppb.OutputArtifact{}
			if err := protojson.Unmarshal(line, got); err != nil {
				t.Fatalf("protojson.Unmarshal(%q) failed: %v", line, err)
			}
			m := got.GetTestStepArtifact().GetMeasurement()
			if loc := m.GetSubcomponent().GetLocation(); loc != tc.wantLocation {
				t.Errorf("margin(0x%02x) streamed location %q, want %q", tc.offset, loc, tc.wantLocation)
			}
			if unit := m.GetUnit(); unit != tc.wantUnit {
				t.Errorf("margin(0x%02x) streamed unit %q, want %q", tc.offset, unit, tc.wantUnit)
			}
		})
	}
}

func TestPrepareMarginCmdAndPoint(t *testing.T) {
	testCases := []struct {
		name      string
		voltage   bool
		indDir    bool // IndUpDownVoltage or IndLeftRightTiming
		offset    uint16
		wantCmd   cmdRsp
		wantPoint *lmtpb.LinkMargin_Lane_MarginPoint
	}{
		{
			name:    "up",
			voltage: true,
			indDir:  true,
			offset:  5,
			wantCmd: cmdRsp{payload: 5, usage: UsageModel, typ: MarginTypeVoltage, rec: 6},
			wantPoint: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_UP,
				Steps:     5,
				Voltage:   proto.Float32(0.25),
			},
		},
		{
			name:    "down",
			voltage: true,
			indDir:  true,
			offset:  VoltageDirMask | 5,
			wantCmd: cmdRsp{payload: VoltageDirMask | 5, usage: UsageModel, typ: MarginTypeVoltage, rec: 6},
			wantPoint: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN,
				Steps:     5,
				Voltage:   proto.Float32(0.25),
			},
		},
		{
			name:    "up-down",
			voltage: true,
			offset:  5,
			wantCmd: cmdRsp{payload: 5, usage: UsageModel, typ: MarginTypeVoltage, rec: 6},
			wantPoint: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_UD,
				Steps:     5,
				Voltage:   proto.Float32(0.25),
			},
		},
		{
			name:    "right",
			indDir:  true,
			offset:  5,
			wantCmd: cmdRsp{payload: 5, usage: UsageModel, typ: MarginTypeTiming, rec: 6},
			wantPoint: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT,
				Steps:     5,
				PercentUi: proto.Float32(0.25),
			},
		},
		{
			name:    "left",
			indDir:  true,
			offset:  TimingDirMask | 5,
			wantCmd: cmdRsp{payload: TimingDirMask | 5, usage: UsageModel, typ: MarginTypeTiming, rec: 6},
			wantPoint: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT,
				Steps:     5,
				PercentUi: proto.Float32(0.25),
			},
		},
		{
			name:    "left-right",
			offset:  5,
			wantCmd: cmdRsp{payload: 5, usage: UsageModel, typ: MarginTypeTiming, rec: 6},
			wantPoint: &lmtpb.LinkMargin_Lane_MarginPoint{
				Direction: lmtpb.LinkMargin_Lane_MarginPoint_D_LR,
				Steps:     5,
				PercentUi: proto.Float32(0.25),
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// A point margined before, to check that the new point is appended.
			prev := &lmtpb.LinkMargin_Lane_MarginPoint{}
			ln := &Lane{
				rec: lmtpb.LinkMargin_R_USP_F6,
				param: &lmtpb.LinkMargin_Lane_Parameters{
					NumTimingSteps:     10,
					MaxTimingOffset:    50,
					IndLeftRightTiming: tc.indDir,
					NumVoltageSteps:    10,
					MaxVoltageOffset:   50,
					IndUpDownVoltage:   tc.indDir,
				},
				vsteps: []*lmtpb.LinkMargin_Lane_MarginPoint{prev},
				tsteps: []*lmtpb.LinkMargin_Lane_MarginPoint{prev},
			}

			cmd, point := prepareMarginCmdAndPoint(ln, tc.offset, &aspect{VnotT: tc.voltage})

			if cmd != tc.wantCmd {
				t.Errorf("prepareMarginCmdAndPoint(0x%02x) cmd = %+v, want %+v", tc.offset, cmd, tc.wantCmd)
			}
			if !proto.Equal(point, tc.wantPoint) {
				t.Errorf("prepareMarginCmdAndPoint(0x%02x) point = %v, want %v", tc.offset, point, tc.wantPoint)
			}
			wantVsteps := []*lmtpb.LinkMargin_Lane_MarginPoint{prev}
			wantTsteps := []*lmtpb.LinkMargin_Lane_MarginPoint{prev}
			if tc.voltage {
				wantVsteps = append(wantVsteps, point)
			} else {
				wantTsteps = append(wantTsteps, point)
			}
			if !slices.Equal(ln.vsteps, wantVsteps) {
				t.Errorf("prepareMarginCmdAndPoint(0x%02x) vsteps = %v, want %v", tc.offset, ln.vsteps, wantVsteps)
			}
			if !slices.Equal(ln.tsteps, wantTsteps) {
				t.Errorf("prepareMarginCmdAndPoint(0x%02x) tsteps = %v, want %v", tc.offset, ln.tsteps, wantTsteps)
			}
		})
	}
}

func TestSendPostMarginCmds(t *testing.T) {
	const (
		addr      = 0x100
		noCmd     = 0x9c38 // NoCmd broadcast.
		clearLog  = 0x5516 // Set ClearErrorLog [rec: F6, usg: 0].
		goNormal  = 0x0f16 // Set GoToNormalSettings [rec: F6, usg: 0].
		cmdFailed = "LMR command failed"
	)
	// echoExcept returns a respond func that echoes every command but fail,
	// which it answers with a NoCmd status.
	echoExcept := func(fail uint16) func(writes []uint16) uint16 {
		return func(writes []uint16) uint16 {
			if last := writes[len(writes)-1]; last != fail {
				return last
			}
			return noCmd
		}
	}
	testCases := []struct {
		name string
		// respond returns the status register value given the commands written so far.
		respond    func(writes []uint16) uint16
		wantErr    string
		wantWrites []uint16
	}{
		{
			name:       "success",
			respond:    echoExcept(0),
			wantWrites: []uint16{noCmd, clearLog, noCmd, goNormal},
		},
		{
			name:       "Clear Error Log fails",
			respond:    echoExcept(clearLog),
			wantErr:    cmdFailed,
			wantWrites: []uint16{noCmd, clearLog},
		},
		{
			name:       "Go to Normal Settings fails",
			respond:    echoExcept(goNormal),
			wantErr:    cmdFailed,
			wantWrites: []uint16{noCmd, clearLog, noCmd, goNormal},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var writes []uint16
			origRead, origWrite := readWord, writeWord
			t.Cleanup(func() { readWord, writeWord = origRead, origWrite })
			writeWord = func(_ pci.Dev, _ int32, val uint16) { writes = append(writes, val) }
			readWord = func(pci.Dev, int32) uint16 { return tc.respond(writes) }
			ln := &Lane{addr: addr, rec: lmtpb.LinkMargin_R_USP_F6}

			err := sendPostMarginCmds(ln)

			if tc.wantErr == "" && err != nil {
				t.Errorf("sendPostMarginCmds() error = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Errorf("sendPostMarginCmds() error = %v, want error containing %q", err, tc.wantErr)
			}
			if !slices.Equal(writes, tc.wantWrites) {
				t.Errorf("sendPostMarginCmds() wrote %04x, want %04x", writes, tc.wantWrites)
			}
		})
	}
}

// fakeHook is a MarginHook that records its last PostMargin call and returns err.
type fakeHook struct {
	err         error
	calls       int
	ln          *Lane
	t           *aspect
	point       *lmtpb.LinkMargin_Lane_MarginPoint
	bitCount    float64
	dwellActual time.Duration
}

func (h *fakeHook) Name() string { return "fakeHook" }

func (h *fakeHook) PostMargin(ln *Lane, t *aspect, point *lmtpb.LinkMargin_Lane_MarginPoint, bitCount float64, dwellActual time.Duration) error {
	h.calls++
	h.ln, h.t, h.point, h.bitCount, h.dwellActual = ln, t, point, bitCount, dwellActual
	return h.err
}

func TestMarginHooks(t *testing.T) {
	const (
		addr     = 0x100
		clearLog = 0x5516 // Set ClearErrorLog [rec: F6, usg: 0].
		goNormal = 0x0f16 // Set GoToNormalSettings [rec: F6, usg: 0].
	)
	logs := captureLog(t)
	// The fake LMR answers step margin commands with Margining and echoes the other commands.
	var writes []uint16
	origRead, origWrite := readWord, writeWord
	t.Cleanup(func() { readWord, writeWord = origRead, origWrite })
	writeWord = func(_ pci.Dev, _ int32, val uint16) { writes = append(writes, val) }
	readWord = func(pci.Dev, int32) uint16 {
		last := writes[len(writes)-1]
		if typ := last >> 3 & 0x7; typ == MarginTypeTiming || typ == MarginTypeVoltage {
			return StepMarginExecutionStatusMargining<<StepMarginExecutionStatusPos<<8 | last&0xff
		}
		return last
	}
	failing := &fakeHook{err: errors.New("hook failed")}
	next := &fakeHook{}
	ln := &Lane{
		cfg:        &lmtpb.LinkMargin{UspBdf: proto.String("0000:17:00.0")},
		laneNumber: 3,
		addr:       addr,
		rec:        lmtpb.LinkMargin_R_USP_F6,
		rx:         &receiver{marginHooks: []MarginHook{failing, next}},
		param: &lmtpb.LinkMargin_Lane_Parameters{
			NumTimingSteps:     10,
			MaxTimingOffset:    50,
			IndLeftRightTiming: true,
		},
	}
	asp := &aspect{}

	// A zero dwell ends the step at the first Margining status.
	point, err := ln.margin(5, asp)

	if err != nil {
		t.Fatalf("margin(5) failed: %v", err)
	}
	for _, h := range []*fakeHook{failing, next} {
		if h.calls != 1 {
			t.Errorf("margin(5) called PostMargin(err=%v) %d times, want 1", h.err, h.calls)
		}
		if h.ln != ln || h.t != asp || h.point != point {
			t.Errorf("margin(5) called PostMargin(err=%v) with (%p, %p, %p), want (%p, %p, %p)",
				h.err, h.ln, h.t, h.point, ln, asp, point)
		}
		// Without samples per second, the bit count falls back to 2^(18/3).
		if h.bitCount != 64 {
			t.Errorf("margin(5) called PostMargin(err=%v) with bitCount %v, want 64", h.err, h.bitCount)
		}
		if h.dwellActual < 0 || h.dwellActual >= marginTimeout {
			t.Errorf("margin(5) called PostMargin(err=%v) with dwellActual %v, want in [0, %v)",
				h.err, h.dwellActual, marginTimeout)
		}
	}
	if want := ": fakeHook.PostMargin: hook failed"; !strings.Contains(logs(), want) {
		t.Errorf("margin(5) logs = %q, want them to contain %q", logs(), want)
	}
	if n := len(writes); n < 3 || writes[n-3] != clearLog || writes[n-1] != goNormal {
		t.Errorf("margin(5) wrote %04x, want it to end with %04x, NoCmd, %04x", writes, clearLog, goNormal)
	}
}
