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
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

const (
	// fakeLMRAddr is the LMR capability address of the fake receiver.
	fakeLMRAddr = 0x100
	// fakeHWInfo is the OCP hardware info ID of the fake receiver.
	fakeHWInfo = "BDF=0000:17:00.0;RX=DSP_A1"
)

// laneEye fakes the timing and voltage eyes of a lane. Each func reports whether a margin point
// at steps from the centre passes on its side.
type laneEye struct {
	tPos, tNeg, vPos, vNeg func(steps uint16) bool
}

// fakeLMR fakes the Margining Lane Control and Status registers of the lanes of a receiver.
// It answers every command on the Status register, and logs the commands per lane and in
// global order.
type fakeLMR struct {
	caps map[uint16]uint16 // The Report payloads, by the requested Report.
	eyes map[uint32]laneEye
	// fail reports whether lane answers the command raw with No Command, which fails it.
	fail func(lane uint32, raw uint16) bool
	// settingUp makes the first Status read after each step margin command report Setting Up.
	settingUp bool

	mu     sync.Mutex
	rsp    map[uint32]uint16 // The Status register value, by lane.
	setup  map[uint32]bool   // Whether the next Status read of the lane reports Setting Up.
	lanes  map[uint32][]string
	global []string // "L<lane>:<command>" in the order written.
}

// newFakeLMR returns a fakeLMR with independent left/right timing and up/down voltage, without
// an independent error sampler, and 10 timing and voltage steps of 0.5 UI and 0.5 V.
func newFakeLMR(eyes map[uint32]laneEye) *fakeLMR {
	return &fakeLMR{
		caps: map[uint16]uint16{
			RptControlCapabilities: MskIndLeftRightTiming | MskIndUpDownVoltage | MskVoltageSupported,
			RptNumVoltageSteps:     10,
			RptNumTimingSteps:      10,
			RptMaxTimingOffset:     50,
			RptMaxVoltageOffset:    50,
			RptMaxLanes:            15,
		},
		eyes:  eyes,
		fail:  func(uint32, uint16) bool { return false },
		rsp:   map[uint32]uint16{},
		setup: map[uint32]bool{},
		lanes: map[uint32][]string{},
	}
}

// install routes readWord and writeWord to f until the test ends.
func (f *fakeLMR) install(t *testing.T) {
	t.Helper()
	origRead, origWrite := readWord, writeWord
	t.Cleanup(func() { readWord, writeWord = origRead, origWrite })
	readWord = f.read
	writeWord = f.write
}

// laneOf returns the lane whose Control register is at addr.
func laneOf(addr int32) uint32 { return uint32(addr-fakeLMRAddr-8) / 4 }

func (f *fakeLMR) write(_ pci.Dev, addr int32, raw uint16) {
	lane := laneOf(addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	name := cmdName(raw)
	if name != "NoCmd" {
		f.lanes[lane] = append(f.lanes[lane], name)
		f.global = append(f.global, fmt.Sprintf("L%d:%s", lane, name))
	}
	f.rsp[lane] = f.respond(lane, raw)
	if typ := raw >> 3 & 0x7; f.settingUp && (typ == MarginTypeTiming || typ == MarginTypeVoltage) {
		f.setup[lane] = true
	}
}

func (f *fakeLMR) read(_ pci.Dev, addr int32) uint16 {
	lane := laneOf(addr - 2)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setup[lane] {
		f.setup[lane] = false
		return StepMarginExecutionStatusSettingUp<<StepMarginExecutionStatusPos<<8 | f.rsp[lane]&0xff
	}
	return f.rsp[lane]
}

// respond returns the Status register value that answers the command raw on lane.
func (f *fakeLMR) respond(lane uint32, raw uint16) uint16 {
	var cmd cmdRsp
	cmd.decode(raw)
	noCmd := uint16(NoCmdPayload<<8 | MarginTypeNoCmd<<3 | NoCmdRecNum)
	if cmd.typ == MarginTypeNoCmd {
		return raw
	}
	if f.fail(lane, raw) {
		return noCmd
	}
	payload := cmd.payload
	switch cmd.typ {
	case MarginTypeReport:
		payload = f.caps[cmd.payload]
	case MarginTypeTiming, MarginTypeVoltage:
		eye := f.eyes[lane]
		passPos, passNeg, dirMask := eye.tPos, eye.tNeg, uint16(TimingDirMask)
		if cmd.typ == MarginTypeVoltage {
			passPos, passNeg, dirMask = eye.vPos, eye.vNeg, VoltageDirMask
		}
		passed := passPos(cmd.payload &^ dirMask)
		if cmd.payload&dirMask != 0 {
			passed = passNeg(cmd.payload &^ dirMask)
		}
		payload = StepMarginExecutionStatusErrorOut<<StepMarginExecutionStatusPos | 4
		if passed {
			payload = StepMarginExecutionStatusMargining << StepMarginExecutionStatusPos
		}
	}
	return payload<<8 | cmd.typ<<3 | cmd.rec
}

// laneTrace returns the commands written to lane, without the No Commands.
func (f *fakeLMR) laneTrace(lane uint32) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lanes[lane])
}

// globalTrace returns the commands written to all lanes in order, without the No Commands.
func (f *fakeLMR) globalTrace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.global)
}

// cmdName names the LMR command raw, e.g. "Report:0x88", "ErrorLimit=4", "T+2" or "V-3".
func cmdName(raw uint16) string {
	var cmd cmdRsp
	cmd.decode(raw)
	switch cmd.typ {
	case MarginTypeNoCmd:
		return "NoCmd"
	case MarginTypeReport:
		return fmt.Sprintf("Report:%#x", cmd.payload)
	case MarginTypeSet:
		switch {
		case cmd.payload == SetClearErrorLog:
			return "ClearErrorLog"
		case cmd.payload == SetGoToNormalSettings:
			return "GoToNormal"
		case cmd.payload&SetErrorCountLimit == SetErrorCountLimit:
			return fmt.Sprintf("ErrorLimit=%d", cmd.payload&^SetErrorCountLimit)
		}
	case MarginTypeTiming:
		if cmd.payload&TimingDirMask != 0 {
			return fmt.Sprintf("T-%d", cmd.payload&^TimingDirMask)
		}
		return fmt.Sprintf("T+%d", cmd.payload)
	case MarginTypeVoltage:
		if cmd.payload&VoltageDirMask != 0 {
			return fmt.Sprintf("V-%d", cmd.payload&^VoltageDirMask)
		}
		return fmt.Sprintf("V+%d", cmd.payload)
	}
	return fmt.Sprintf("%#04x", raw)
}

// newFakeReceiver returns a receiver of numLanes lanes at fakeLMRAddr, which streams the OCP
// step artifacts, and runs its lanes in parallel if parallel.
func newFakeReceiver(numLanes int, parallel bool) *receiver {
	r := &receiver{
		testReady:   true,
		rec:         lmtpb.LinkMargin_R_DSP_A1,
		parallel:    parallel,
		rxwg:        new(sync.WaitGroup),
		linkwg:      new(sync.WaitGroup),
		hwinfo:      fakeHWInfo,
		marginHooks: []MarginHook{ocpWriter},
	}
	cfg := &lmtpb.LinkMargin{UspBdf: proto.String("0000:17:00.0")}
	for i := range numLanes {
		ln := new(Lane)
		ln.Init(cfg, pci.Dev{}, i, fakeLMRAddr, r.rec, 16e9, r.rxwg, r.linkwg, r)
		r.lanes = append(r.lanes, ln)
	}
	return r
}

// pointCmds returns the commands of a margin point named name, e.g. "T+2": the step margin
// command, then the post-margin Clear Error Log and Go to Normal Settings.
func pointCmds(names ...string) []string {
	var cmds []string
	for _, n := range names {
		cmds = append(cmds, n, "ClearErrorLog", "GoToNormal")
	}
	return cmds
}

// paramCmds are the commands of readLaneParameters.
var paramCmds = []string{
	"ClearErrorLog", "GoToNormal",
	"Report:0x88", "Report:0x89", "Report:0x8a", "Report:0x8b", "Report:0x8c",
	"Report:0x8d", "Report:0x8e", "Report:0x90",
}
