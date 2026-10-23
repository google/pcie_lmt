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
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	lmtpb "lmt_go.proto"
)

// maxMargins bounds the points that fakeEye margins, so that a runaway offset sequence fails the
// test instead of hanging it.
const maxMargins = 64

// fakeEye fakes the margin points of a lane, and logs every margined offset as "+N" or "-N".
type fakeEye struct {
	t       *testing.T
	passPos func(steps uint16) bool
	passNeg func(steps uint16) bool
	errAt   map[uint16]bool // The offsets, including the direction bit, that return an error.
	// statusAt overrides the status at the offsets, including the direction bit.
	statusAt map[uint16]lmtpb.LinkMargin_Lane_MarginPoint_StatusEnum
	log      []string
}

func (f *fakeEye) margin(offset uint16) (*lmtpb.LinkMargin_Lane_MarginPoint, error) {
	if len(f.log) >= maxMargins {
		f.t.Fatalf("margined more than %d points: %v", maxMargins, f.log)
	}
	steps := offset &^ TimingDirMask
	sign, passed := "+", f.passPos(steps)
	if offset&TimingDirMask != 0 {
		sign, passed = "-", f.passNeg(steps)
	}
	f.log = append(f.log, fmt.Sprintf("%s%d", sign, steps))
	status := lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT
	if passed {
		status = lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING
	}
	if s, ok := f.statusAt[offset]; ok {
		status = s
	}
	mp := &lmtpb.LinkMargin_Lane_MarginPoint{Steps: uint32(steps), Status: status}
	if f.errAt[offset] {
		return mp, fmt.Errorf("fake error at %s%d", sign, steps)
	}
	return mp, nil
}

func upTo(maxSteps uint16) func(uint16) bool {
	return func(steps uint16) bool { return steps <= maxSteps }
}

func never(uint16) bool { return false }

// runAspect margins the aspect as testAspect does.
func runAspect(t *aspect, margin marginFunc, msg *strings.Builder) {
	if t.eyeSizeMode {
		testEyeSize(t, margin, msg)
	} else {
		scanEye(t, margin, msg)
	}
}

// corners returns the steps of the [Pos/Neg][Pass/Fail] margin points, or -1 if not found.
func corners(t *aspect) [2][2]int {
	var c [2][2]int
	for pn := pos; pn <= neg; pn++ {
		for pf := pass; pf <= fail; pf++ {
			c[pn][pf] = -1
			if mp := t.mp[pn][pf]; mp != nil {
				c[pn][pf] = int(mp.GetSteps())
			}
		}
	}
	return c
}

func TestEyeMarginSequence(t *testing.T) {
	testCases := []struct {
		name        string
		t           aspect
		passPos     func(uint16) bool
		passNeg     func(uint16) bool
		errAt       map[uint16]bool
		statusAt    map[uint16]lmtpb.LinkMargin_Lane_MarginPoint_StatusEnum
		wantLog     []string
		wantCorners [2][2]int
		// wantAliased is whether both sides record the same points, as without independent
		// directions.
		wantAliased bool
		wantMsg     string
	}{
		{
			name:        "eye scan to target with independent directions",
			t:           aspect{indDir: true, start: 2, target: 6, step: 2},
			passPos:     upTo(4),
			passNeg:     upTo(2),
			wantLog:     []string{"+2", "-2", "+4", "-4", "+6"},
			wantCorners: [2][2]int{{4, 6}, {2, 4}},
		},
		{
			name:        "eye scan until fail without independent directions",
			t:           aspect{untilFail: true, start: 1, target: 10, step: 1},
			passPos:     upTo(3),
			passNeg:     upTo(0),
			wantLog:     []string{"+1", "+2", "+3", "+4"},
			wantCorners: [2][2]int{{3, 4}, {3, 4}},
			wantAliased: true,
		},
		{
			name:        "eye scan until both directions fail",
			t:           aspect{indDir: true, untilFail: true, start: 1, target: 10, step: 1},
			passPos:     upTo(1),
			passNeg:     upTo(2),
			wantLog:     []string{"+1", "-1", "+2", "-2", "-3"},
			wantCorners: [2][2]int{{1, 2}, {2, 3}},
		},
		{
			name:        "eye scan stops margining once failed without independent directions",
			t:           aspect{start: 1, target: 4, step: 1},
			passPos:     upTo(2),
			passNeg:     upTo(0),
			wantLog:     []string{"+1", "+2", "+3"},
			wantCorners: [2][2]int{{2, 3}, {2, 3}},
			wantAliased: true,
		},
		{
			name:        "eye scan clamps the last step to the target",
			t:           aspect{indDir: true, start: 1, target: 6, step: 4},
			passPos:     upTo(20),
			passNeg:     upTo(20),
			wantLog:     []string{"+1", "-1", "+5", "-5", "+6", "-6"},
			wantCorners: [2][2]int{{6, -1}, {6, -1}},
		},
		{
			name:        "eye scan continues the positive side after the negative side fails",
			t:           aspect{indDir: true, start: 1, target: 3, step: 1},
			passPos:     upTo(3),
			passNeg:     upTo(1),
			wantLog:     []string{"+1", "-1", "+2", "-2", "+3"},
			wantCorners: [2][2]int{{3, -1}, {1, 2}},
		},
		{
			name:        "eye scan at the target only",
			t:           aspect{indDir: true, start: 5, target: 5, step: 1},
			passPos:     upTo(20),
			passNeg:     upTo(0),
			wantLog:     []string{"+5", "-5"},
			wantCorners: [2][2]int{{5, -1}, {-1, 5}},
		},
		{
			name:        "eye size passing without independent directions",
			t:           aspect{eyeSizeMode: true, start: 9, step: 1},
			passPos:     upTo(5),
			passNeg:     upTo(0),
			wantLog:     []string{"+5"},
			wantCorners: [2][2]int{{5, -1}, {5, -1}},
			wantAliased: true,
		},
		{
			name:        "eye size failing without independent directions",
			t:           aspect{eyeSizeMode: true, start: 9, step: 1},
			passPos:     upTo(3),
			passNeg:     upTo(0),
			wantLog:     []string{"+5"},
			wantCorners: [2][2]int{{-1, 5}, {-1, 5}},
			wantAliased: true,
		},
		{
			name:        "eye size passing on both sides",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, step: 1},
			passPos:     upTo(20),
			passNeg:     upTo(20),
			wantLog:     []string{"+5", "-5"},
			wantCorners: [2][2]int{{5, -1}, {5, -1}},
		},
		{
			name:        "eye size centered more positive",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, step: 1},
			passPos:     upTo(20),
			passNeg:     upTo(3),
			wantLog:     []string{"+5", "-5", "-4", "-3", "+7"},
			wantCorners: [2][2]int{{7, -1}, {3, 4}},
		},
		{
			name:        "eye size redo of the positive side failing then passing",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, step: 2},
			passPos:     upTo(5),
			passNeg:     upTo(3),
			wantLog:     []string{"+5", "-5", "-3", "+7", "+5"},
			wantCorners: [2][2]int{{5, 7}, {3, 5}},
		},
		{
			name:        "eye size clamps the positive side to the target",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, target: 3, step: 3},
			passPos:     upTo(0),
			passNeg:     upTo(20),
			wantLog:     []string{"+5", "+3", "-7"},
			wantCorners: [2][2]int{{-1, 3}, {7, -1}},
		},
		{
			name:        "eye size failing down to the target",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, target: 4, step: 1},
			passPos:     upTo(3),
			passNeg:     upTo(6),
			wantLog:     []string{"+5", "+4", "-6"},
			wantCorners: [2][2]int{{-1, 4}, {6, -1}},
		},
		{
			name:        "eye size failing below the step down to the target",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, step: 2},
			passPos:     never,
			passNeg:     upTo(20),
			wantLog:     []string{"+5", "+3", "+1", "+0", "-10"},
			wantCorners: [2][2]int{{-1, 0}, {10, -1}},
		},
		{
			name:        "eye scan treats NAK and SETTING_UP as failing",
			t:           aspect{indDir: true, start: 1, target: 4, step: 1},
			passPos:     upTo(20),
			passNeg:     upTo(20),
			statusAt:    map[uint16]lmtpb.LinkMargin_Lane_MarginPoint_StatusEnum{2: lmtpb.LinkMargin_Lane_MarginPoint_S_NAK, 3 | TimingDirMask: lmtpb.LinkMargin_Lane_MarginPoint_S_SETTING_UP},
			wantLog:     []string{"+1", "-1", "+2", "-2", "-3"},
			wantCorners: [2][2]int{{1, 2}, {2, 3}},
		},
		{
			name:        "eye size treats UNKNOWN as failing",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 6, step: 1},
			passPos:     upTo(20),
			passNeg:     upTo(20),
			statusAt:    map[uint16]lmtpb.LinkMargin_Lane_MarginPoint_StatusEnum{3: lmtpb.LinkMargin_Lane_MarginPoint_S_UNKNOWN},
			wantLog:     []string{"+3", "+2", "-4"},
			wantCorners: [2][2]int{{2, 3}, {4, -1}},
		},
		{
			name:        "eye size of odd steps starts from the positive half rounding up",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 9, step: 1},
			passPos:     upTo(20),
			passNeg:     upTo(20),
			wantLog:     []string{"+5", "-4"},
			wantCorners: [2][2]int{{5, -1}, {4, -1}},
		},
		{
			name:        "eye scan until fail continues the positive side after the negative side fails",
			t:           aspect{indDir: true, untilFail: true, start: 1, target: 10, step: 1},
			passPos:     upTo(3),
			passNeg:     upTo(1),
			wantLog:     []string{"+1", "-1", "+2", "-2", "+3", "+4"},
			wantCorners: [2][2]int{{3, 4}, {1, 2}},
		},
		{
			name:        "eye size redo after the negative side clamps to the target",
			t:           aspect{indDir: true, eyeSizeMode: true, start: 10, target: 4, step: 2},
			passPos:     upTo(20),
			passNeg:     upTo(4),
			wantLog:     []string{"+5", "-5", "-4", "+6"},
			wantCorners: [2][2]int{{6, -1}, {4, 5}},
		},
		{
			name:        "margin errors accumulate in the message",
			t:           aspect{indDir: true, start: 2, target: 6, step: 2},
			passPos:     upTo(4),
			passNeg:     upTo(2),
			errAt:       map[uint16]bool{2: true, 4 | TimingDirMask: true},
			wantLog:     []string{"+2", "-2", "+4", "-4", "+6"},
			wantCorners: [2][2]int{{4, 6}, {2, 4}},
			wantMsg:     "fake error at +2 | fake error at -4 | ",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.t.dirmask = TimingDirMask
			f := &fakeEye{t: t, passPos: tc.passPos, passNeg: tc.passNeg, errAt: tc.errAt, statusAt: tc.statusAt}
			var msg strings.Builder
			runAspect(&tc.t, f.margin, &msg)
			if !slices.Equal(f.log, tc.wantLog) {
				t.Errorf("runAspect() margined %v, want %v", f.log, tc.wantLog)
			}
			if got := corners(&tc.t); got != tc.wantCorners {
				t.Errorf("runAspect() [Pos/Neg][Pass/Fail] steps = %v, want %v", got, tc.wantCorners)
			}
			aliased := tc.t.mp[pos] == tc.t.mp[neg]
			if aliased != tc.wantAliased {
				t.Errorf("runAspect() positive and negative points aliased = %t, want %t", aliased, tc.wantAliased)
			}
			if got := msg.String(); got != tc.wantMsg {
				t.Errorf("runAspect() message = %q, want %q", got, tc.wantMsg)
			}
		})
	}
}

// setErrorLimitCmd returns the Set Error Count Limit command to limit, addressed to the
// R_DSP_A1 receiver of the fake receiver, as written to the Margining Lane Control register.
func setErrorLimitCmd(limit uint16) uint16 {
	cmd := cmdRsp{rec: uint16(lmtpb.LinkMargin_R_DSP_A1), usage: UsageModel, typ: MarginTypeSet, payload: SetErrorCountLimit | limit}
	return cmd.encode()
}

// rptNumTimingStepsCmd returns the Report Number of Timing Steps command, addressed to the
// R_DSP_A1 receiver of the fake receiver, as written to the Margining Lane Control register.
func rptNumTimingStepsCmd() uint16 {
	cmd := cmdRsp{rec: uint16(lmtpb.LinkMargin_R_DSP_A1), usage: UsageModel, typ: MarginTypeReport, payload: RptNumTimingSteps}
	return cmd.encode()
}

// TestMarginLaneTrace pins the LMR commands that MarginLane writes to one lane.
func TestMarginLaneTrace(t *testing.T) {
	// The lane passes timing up to +4/-2 steps, and voltage up to +20/-3 steps.
	eye := laneEye{tPos: upTo(4), tNeg: upTo(2), vPos: upTo(20), vNeg: upTo(3)}
	tspec := &lmtpb.LinkMargin_TestSpec{
		StartOffset:  proto.Uint32(2),
		TargetOffset: proto.Uint32(6),
		Step:         proto.Uint32(2),
		ErrorLimit:   4,
	}
	// A 0.5 V eye is 10 voltage steps, margined from +5.
	vspec := &lmtpb.LinkMargin_TestSpec{
		EyeSize:    proto.Float32(0.5),
		ErrorLimit: 5,
	}
	tCmds := append([]string{"ErrorLimit=4"}, pointCmds("T+2", "T-2", "T+4", "T-4", "T+6")...)
	vCmds := append([]string{"ErrorLimit=5"}, pointCmds("V+5", "V-5", "V-4", "V-3", "V+7")...)
	testCases := []struct {
		name string
		// fail is the command that the lane fails, or 0.
		fail uint16
		// slow makes each step margin report Setting Up first, then Margining for a 4 ms dwell.
		slow      bool
		wantTrace []string
		wantErr   bool
		wantLane  bool // whether the lane result is gathered, i.e. ln.lane != nil.
		wantMsg   string
	}{
		{
			name:      "timing eye scan then voltage eye size",
			wantTrace: slices.Concat(paramCmds, tCmds, vCmds),
			wantLane:  true,
		},
		{
			name:      "timing eye scan then voltage eye size, setting up and dwelling",
			slow:      true,
			wantTrace: slices.Concat(paramCmds, tCmds, vCmds),
			wantLane:  true,
		},
		{
			name:      "set error limit fails",
			fail:      setErrorLimitCmd(5),
			wantTrace: slices.Concat(paramCmds, tCmds, []string{"ErrorLimit=5"}),
			wantErr:   true,
			wantMsg:   "LMR command failed",
		},
		{
			name:      "read lane parameters fails",
			fail:      rptNumTimingStepsCmd(),
			wantTrace: paramCmds[:5],
			wantErr:   true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLMR(map[uint32]laneEye{0: eye})
			f.fail = func(_ uint32, raw uint16) bool { return raw == tc.fail }
			f.settingUp = tc.slow
			f.install(t)
			r := newFakeReceiver(1, false)
			ln := r.lanes[0]
			ln.Tspec = proto.Clone(tspec).(*lmtpb.LinkMargin_TestSpec)
			ln.Vspec = proto.Clone(vspec).(*lmtpb.LinkMargin_TestSpec)
			if tc.slow {
				ln.Tspec.Dwell = proto.Float32(0.004)
				ln.Vspec.Dwell = proto.Float32(0.004)
			}
			r.rxwg.Add(1)

			err := ln.MarginLane()

			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("MarginLane() error = %v, want error: %t", err, tc.wantErr)
			}
			if got := f.laneTrace(0); !slices.Equal(got, tc.wantTrace) {
				t.Errorf("MarginLane() wrote %q, want %q", got, tc.wantTrace)
			}
			if gotLane := ln.lane != nil; gotLane != tc.wantLane {
				t.Errorf("MarginLane() gathered lane = %t, want %t", gotLane, tc.wantLane)
			}
			if !strings.Contains(ln.msg, tc.wantMsg) {
				t.Errorf("MarginLane() message = %q, want it to contain %q", ln.msg, tc.wantMsg)
			}
		})
	}
}
