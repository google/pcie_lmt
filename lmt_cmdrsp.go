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

// This file covers the PCIe LMR basic access operations.

import (
	"fmt"
	"time"

	log "github.com/golang/glog"
	pci "pciutils"
)

// //////////////////////////////////////////////////////////////////////////////
const (
	// Constants specified by the PCIe 5.0 Spec 4.2.13.1
	UsageModel                    = 0 // This must always be 0 as specified in 4.2.13.1
	StepMarginExecutionStatusPos  = 6
	StepMarginExecutionStatusMask = 0xc0
	StepMarginErrorCountMask      = 0x3F
	// The following encoding is specified in PCIe 5.0 Spec 4.2.13.1
	StepMarginExecutionStatusErrorOut  = 0x0
	StepMarginExecutionStatusSettingUp = 0x1
	StepMarginExecutionStatusMargining = 0x2
	StepMarginExecutionStatusNak       = 0x3
	VoltageDirMask                     = 0x80
	TimingDirMask                      = 0x40

	MarginTypeNoCmd   = 7
	MarginTypeReport  = 1
	MarginTypeSet     = 2
	MarginTypeTiming  = 3
	MarginTypeVoltage = 4

	NoCmdPayload = 0x9C
	NoCmdRecNum  = 0

	RptControlCapabilities = 0x88
	RptNumVoltageSteps     = 0x89
	RptNumTimingSteps      = 0x8A
	RptMaxTimingOffset     = 0x8B
	RptMaxVoltageOffset    = 0x8C
	RptSamplingRateVoltage = 0x8D
	RptSamplingRateTiming  = 0x8E
	RptSampleCount         = 0x8F
	RptMaxLanes            = 0x90

	MskIndErrorSampler       = 1 << 4
	MskSampleReportingMethod = 1 << 3
	MskIndLeftRightTiming    = 1 << 2
	MskIndUpDownVoltage      = 1 << 1
	MskVoltageSupported      = 1 << 0

	MskNumVoltageSteps     = 0x7F
	MskNumTimingSteps      = 0x3F
	MskMaxTimingOffset     = 0x7F
	MskMaxVoltageOffset    = 0x7F
	MskSamplingRateVoltage = 0x3F
	MskSamplingRateTiming  = 0x3F
	MskSampleCount         = 0x7F
	MskMaxLanes            = 0x1F

	SetErrorCountLimit    = 0xC0
	SetGoToNormalSettings = 0x0F
	SetClearErrorLog      = 0x55
	// A little extra margin is added to the following wait times.
	CmdWait    = 12 * time.Microsecond // A minimum 10us is required between commands
	CmdTimeout = 12 * time.Millisecond // command timeout 10ms minimum

	// Speed16G is Gen4 speed encoding.
	Speed16G = 4
	// Speed32G is Gen5 speed encoding.
	Speed32G = 5
	// LinkStatusWidthPos is from the PCIe config space register definition.
	LinkStatusWidthPos = 4
	// USP, DSP, and max 2 retimers with 2 Rx each.
	maxRxPerLink = 6
	// ReceiverEnumSize is the 3-bit receiver number encoding size.
	ReceiverEnumSize = 8
)

// cmdRsp is the LMR command and response format of the control and status reg.
type cmdRsp struct {
	raw     uint16
	payload uint16 // bitfield [15:8]
	usage   uint16 // bitfield [6]
	typ     uint16 // bitfield [5:3]
	rec     uint16 // bitfield [2:0]
}

// encode packs fields into the raw data.
func (cr *cmdRsp) encode() uint16 {
	cr.raw = ((cr.payload & 0xFF) << 8) |
		((cr.usage & 0x1) << 6) |
		((cr.typ & 0x7) << 3) |
		((cr.rec & 0x7) << 0)
	return cr.raw
}

// decode unpacks the raw data into fields.
func (cr *cmdRsp) decode(raw uint16) {
	cr.raw = raw
	cr.payload = (cr.raw >> 8) & 0xFF
	cr.usage = (cr.raw >> 6) & 1
	cr.typ = (cr.raw >> 3) & 0x7
	cr.rec = (cr.raw >> 0) & 0x7
}

// lmrCmdRspBase conducts an LMR command response.
func (ln *Lane) lmrCmdRspBase(cmd *cmdRsp, matchPayload bool) (*cmdRsp, error) {
	dev := ln.dev
	addr := ln.addr
	pci.WriteWord(dev, addr, cmd.encode())
	t := time.Now()
	var rsp cmdRsp
	for do := true; do; do = time.Since(t) < CmdTimeout {
		time.Sleep(CmdWait)
		// The response is the next word (byte-address plus 2).
		rsp.decode(uint16(pci.ReadWord(dev, addr+2)))
		if rsp.rec == cmd.rec && rsp.typ == cmd.typ && rsp.usage == 0 &&
			(!matchPayload || rsp.payload == cmd.payload) {
			log.V(2).Infof("lmrCmdRspBase: Pass match=%v; cmd:%#v; rsp:%#v\n", matchPayload, cmd, rsp)
			return &rsp, nil
		}
		log.V(3).Infof("lmrCmdRspBase: Read match=%v; cmd:%#v; rsp:%#v\n", matchPayload, cmd, rsp)
	}
	log.V(1).Infof("lmrCmdRspBase: Fail match=%v; cmd:%#v; rsp:%#v\n", matchPayload, cmd, rsp)
	err := fmt.Errorf("LMR command failed: match=%#v; cmd:%#v; rsp:%#v",
		matchPayload, cmd, rsp)
	return &rsp, err
}

// lmrBroadcastNoCmd broadcasts a No Command and wait for its reflection on
// response. This is required between commands.
func (ln *Lane) lmrBroadcastNoCmd() error {
	cmd := cmdRsp{raw: 0, payload: NoCmdPayload, rec: NoCmdRecNum, typ: MarginTypeNoCmd}
	if _, err := ln.lmrCmdRspBase(&cmd, true); err != nil {
		return err
	}
	return nil
}

// lmrCmdRsp is the common LMR command response use case. It includes the
// no-command broadcasting.
func (ln *Lane) lmrCmdRsp(cmd *cmdRsp) (*cmdRsp, error) {
	if err := ln.lmrBroadcastNoCmd(); err != nil {
		return nil, err
	}
	return ln.lmrCmdRspBase(cmd, false)
}

// lmrCmdRspEcho sends a command and expects the response to echo the command.
func (ln *Lane) lmrCmdRspEcho(cmd *cmdRsp) error {
	if err := ln.lmrBroadcastNoCmd(); err != nil {
		log.V(1).Infof("lmrBroadcastNoCmd failed\n")
		return err
	}
	_, err := ln.lmrCmdRspBase(cmd, true)
	return err
}

// GetIndErrorSampler reads if independent error sampler is supported by the
// PHY. For Receivers where M IndErrorSampler is 1b, any combination of such
// Receivers are permitted to be margined in parallel.
func (ln *Lane) GetIndErrorSampler() (bool, error) {
	var rsp *cmdRsp
	var err error
	var cmd cmdRsp
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.typ = MarginTypeReport
	cmd.payload = RptControlCapabilities
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return false, err
	}
	return (rsp.payload & MskIndErrorSampler) != 0, nil
}
