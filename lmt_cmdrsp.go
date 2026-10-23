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

	MarginTypeNoCmd         = 0b111
	MarginTypeReport        = 0b001
	MarginTypeSet           = 0b010
	MarginTypeTiming        = 0b011
	MarginTypeVoltage       = 0b100
	MarginTypeVendorDefined = 0b101

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
	// Speed64G is Gen6 speed encoding.
	Speed64G = 6
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

// readWord, readLong and writeWord access the PCI config space. Tests replace them to
// script the Margining Lane Control and Status registers and the LBE registers.
var (
	readWord  = pci.ReadWord
	readLong  = pci.ReadLong
	writeWord = pci.WriteWord
)

// lmrCmdRspBase conducts an LMR command response.
func (ln *Lane) lmrCmdRspBase(cmd *cmdRsp, matchPayload bool) (*cmdRsp, error) {
	dev := ln.dev
	addr := ln.addr
	writeWord(dev, addr, cmd.encode())
	t := time.Now()
	var rsp cmdRsp
	for do := true; do; do = time.Since(t) < CmdTimeout {
		time.Sleep(CmdWait)
		// The response is the next word (byte-address plus 2).
		rsp.decode(uint16(readWord(dev, addr+2)))
		if rsp.rec == cmd.rec && rsp.typ == cmd.typ && rsp.usage == 0 &&
			(!matchPayload || rsp.payload == cmd.payload) {
			log.V(2).Infof("%s: Pass match=%-5t; cmd: %s %-45s; rsp: %s %s",
				ln.laneRepr(), matchPayload, cmd.RawString(), cmd.ReqString(),
				rsp.RawString(), rsp.RspString(cmd))
			return &rsp, nil
		}
		log.V(3).Infof("%s: Read match=%-5t; cmd: %s %-45s; rsp: %s %s",
			ln.laneRepr(), matchPayload, cmd.RawString(), cmd.ReqString(),
			rsp.RawString(), rsp.RspString(cmd))
	}
	log.V(1).Infof("%s: Fail match=%-5t; cmd: %s %-45s; rsp: %s %s",
		ln.laneRepr(), matchPayload, cmd.RawString(), cmd.ReqString(),
		rsp.RawString(), rsp.RspString(cmd))
	err := fmt.Errorf("LMR command failed: match=%-5t; cmd: %s %-45s; rsp: %s %s",
		matchPayload, cmd.RawString(), cmd.ReqString(), rsp.RawString(), rsp.RspString(cmd))
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
		log.V(1).Infof("%s: lmrBroadcastNoCmd failed", ln.laneRepr())
		return err
	}
	_, err := ln.lmrCmdRspBase(cmd, true)
	return err
}

// GetIndErrorSampler reads if independent error sampler is supported by the
// PHY. For Receivers where M IndErrorSampler is 1b, any combination of such
// Receivers are permitted to be margined in parallel. If the trailing No
// Command fails, it still returns the capability, together with the error.
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
	ind := (rsp.payload & MskIndErrorSampler) != 0
	// Step A4 of the PCIe 6.4 section 4.2.18.4 example flow: return to No
	// Command, so the port stops transmitting this Report before the next
	// receiver is probed, e.g. F6 at the USP after C3.
	if err = ln.lmrBroadcastNoCmd(); err != nil {
		return ind, fmt.Errorf("no command after report control capabilities: %w", err)
	}
	return ind, nil
}

// rxNumString returns a human-readable string for the Receiver Number field in cmdRsp.
func rxNumString(rec uint16) string {
	// For details, please see PCIe 6.4
	// Ch. 4.2.18.1 Receiver Number, Margin Type, Usage Model, and Margin Payload Fields
	switch rec {
	case 0:
		return "Broadcast"
	case 1:
		return "A1"
	case 2:
		return "B2"
	case 3:
		return "C3"
	case 4:
		return "D4"
	case 5:
		return "E5"
	case 6:
		return "F6"
	default:
		return fmt.Sprintf("Unknown(0x%01x)", rec)
	}
}

// marginTypeName returns a short name for the Margin Type field in cmdRsp.
func marginTypeName(typ uint16) string {
	switch typ {
	case MarginTypeNoCmd:
		return "NoCmd"
	case MarginTypeReport:
		return "Report"
	case MarginTypeSet:
		return "Set"
	case MarginTypeTiming:
		return "StepTMargin"
	case MarginTypeVoltage:
		return "StepVMargin"
	case MarginTypeVendorDefined:
		return "VendorDefined"
	default:
		return fmt.Sprintf("Unknown(0x%01x)", typ)
	}
}

// reqNoCmdString returns a human-readable string for the cmdRsp interpretation for
// MarginType==NoCmd(111b).
func (cr *cmdRsp) reqNoCmdString() string {
	if cr.rec != NoCmdRecNum {
		return fmt.Sprintf("NoCmd           [rec: Invalid(%s), pay: 0x%02x]",
			rxNumString(cr.rec), cr.payload)
	}
	if cr.payload != NoCmdPayload {
		return fmt.Sprintf("NoCmd           [pay: Invalid(0x%02x)]", cr.payload)
	}
	return "NoCmd"
}

func isAccessRetimerRegisterPayload(payload uint16) bool {
	return (payload <= 0x87) || (payload >= 0xa0 && payload <= 0xff)
}

// reqReportCmdString returns a human-readable string for the cmdRsp interpretation for
// MarginType==Report(001b).
func (cr *cmdRsp) reqReportCmdString() string {
	if isAccessRetimerRegisterPayload(cr.payload) {
		return fmt.Sprintf("AccessRetimerReg[rec: %s, regOffset: 0x%02x]",
			rxNumString(cr.rec), cr.payload)
	}
	switch cr.payload {
	case RptControlCapabilities:
		return fmt.Sprintf("ReportCtrlCap   [rec: %s]", rxNumString(cr.rec))
	case RptNumVoltageSteps:
		return fmt.Sprintf("ReportNumVStep  [rec: %s]", rxNumString(cr.rec))
	case RptNumTimingSteps:
		return fmt.Sprintf("ReportNumTStep  [rec: %s]", rxNumString(cr.rec))
	case RptMaxTimingOffset:
		return fmt.Sprintf("ReportMaxTOfst  [rec: %s]", rxNumString(cr.rec))
	case RptMaxVoltageOffset:
		return fmt.Sprintf("ReportMaxVOfst  [rec: %s]", rxNumString(cr.rec))
	case RptSamplingRateVoltage:
		return fmt.Sprintf("ReportSmplRateV [rec: %s]", rxNumString(cr.rec))
	case RptSamplingRateTiming:
		return fmt.Sprintf("ReportSmplRateT [rec: %s]", rxNumString(cr.rec))
	case RptSampleCount:
		return fmt.Sprintf("ReportSmplCount [rec: %s]", rxNumString(cr.rec))
	case RptMaxLanes:
		return fmt.Sprintf("ReportMaxLanes  [rec: %s]", rxNumString(cr.rec))
	default:
		// Only 91h-9Fh reach here: "Report Reserved" in PCIe 6.4 Table 4-75.
		return fmt.Sprintf("ReportReserved  [rec: %s, cmd: 0x%02x]", rxNumString(cr.rec), cr.payload)
	}
}

func isSetErrorCountLimit(payload uint16) bool {
	return (payload & 0b1100_0000) == SetErrorCountLimit
}

// reqSetCmdString returns a human-readable string for the cmdRsp interpretation for
// MarginType==Set(010b).
func (cr *cmdRsp) reqSetCmdString() string {
	if isSetErrorCountLimit(cr.payload) {
		limit := cr.payload & 0b0011_1111
		return fmt.Sprintf("SetErrCntLimit  [rec: %s, limit: %d]", rxNumString(cr.rec), limit)
	}
	switch cr.payload {
	case SetGoToNormalSettings:
		return fmt.Sprintf("ToNrmlSetting   [rec: %s]", rxNumString(cr.rec))
	case SetClearErrorLog:
		return fmt.Sprintf("ClearErrorLog   [rec: %s]", rxNumString(cr.rec))
	default:
		return fmt.Sprintf("SetUnknown      [rec: %s, pay: Invalid(0x%02x)]",
			rxNumString(cr.rec), cr.payload)
	}
}

// reqStepTimingMarginCmdString returns a human-readable string for the cmdRsp interpretation for
// MarginType==Timing(011b).
func (cr *cmdRsp) reqStepTimingMarginCmdString() string {
	lr := (cr.payload & 0b100_0000) >> 6 // Only meaningful if IndLeftRightTiming is set.
	lrStr := "R"
	if lr == 1 {
		lrStr = "L"
	}
	stepNum := cr.payload & 0b11_1111
	return fmt.Sprintf("StepTMargin     [rec: %s, lr: %s, step: %d]",
		rxNumString(cr.rec), lrStr, stepNum)
}

// reqStepVoltageMarginCmdString returns a human-readable string for the cmdRsp interpretation for
// MarginType==Voltage(100b).
func (cr *cmdRsp) reqStepVoltageMarginCmdString() string {
	du := (cr.payload & 0b1000_0000) >> 7 // Only meaningful if IndUpDownVoltage is set.
	duStr := "U"
	if du == 1 {
		duStr = "D"
	}
	stepNum := cr.payload & 0b0111_1111
	return fmt.Sprintf("StepVMargin     [rec: %s, du: %s, step: %d]",
		rxNumString(cr.rec), duStr, stepNum)
}

// vendorDefinedCmdString returns a human-readable string for the cmdRsp interpretation for
// MarginType==VendorDefined(101b).
func (cr *cmdRsp) vendorDefinedCmdString() string {
	return fmt.Sprintf("VendorDefined   [rec: %s, pay: 0x%02x]", rxNumString(cr.rec), cr.payload)
}

func (cr *cmdRsp) RawString() string {
	return fmt.Sprintf("[raw: 0x%04x]", cr.raw)
}

func (cr *cmdRsp) ReqString() string {
	// See PCIe 6.4 Ch. 4.2.18.1 Receiver Number, Margin Type, Usage Model, and Margin Payload Fields
	// Table 4-75 Margin Commands and Corresponding Responses
	switch cr.typ {
	case MarginTypeNoCmd:
		return cr.reqNoCmdString()
	case MarginTypeReport:
		return cr.reqReportCmdString()
	case MarginTypeSet:
		return cr.reqSetCmdString()
	case MarginTypeTiming:
		return cr.reqStepTimingMarginCmdString()
	case MarginTypeVoltage:
		return cr.reqStepVoltageMarginCmdString()
	case MarginTypeVendorDefined:
		return cr.vendorDefinedCmdString()
	default:
		return fmt.Sprintf(
			"cmdRsp[rec: %s, typ: Unknown(0x%01x), usg: %d, pay: 0x%02x]",
			rxNumString(cr.rec), cr.typ, cr.usage, cr.payload)
	}
}

func (cr *cmdRsp) rspNoCmdString() string {
	if cr.rec != NoCmdRecNum {
		return fmt.Sprintf("NoCmd           [rec: Invalid(%s), pay: 0x%02x]",
			rxNumString(cr.rec), cr.payload)
	}
	if cr.payload != NoCmdPayload {
		return fmt.Sprintf("NoCmd           [pay: Invalid(0x%02x)]", cr.payload)
	}
	return "NoCmd"
}

func (cr *cmdRsp) rspReportCmdString(req *cmdRsp) string {
	if isAccessRetimerRegisterPayload(req.payload) {
		return fmt.Sprintf("AccessRetimerReg[rec: %s, regValue: 0x%02x]",
			rxNumString(cr.rec), cr.payload)
	}
	switch req.payload {
	case RptControlCapabilities:
		indErrSampler := (cr.payload & MskIndErrorSampler) != 0
		sampleMethod := "count"
		if (cr.payload & MskSampleReportingMethod) != 0 {
			sampleMethod = "rate"
		}
		indLRTiming := (cr.payload & MskIndLeftRightTiming) != 0
		indUDVoltage := (cr.payload & MskIndUpDownVoltage) != 0
		voltageSupported := (cr.payload & MskVoltageSupported) != 0
		return fmt.Sprintf("ReportCtrlCap   [rec: %s, indErrSampler: %t, sampleMethod: %s, indLRTiming: %t, indUDVoltage: %t, voltageSupported: %t]",
			rxNumString(cr.rec), indErrSampler, sampleMethod, indLRTiming, indUDVoltage, voltageSupported)
	case RptNumVoltageSteps:
		v := cr.payload & MskNumVoltageSteps
		return fmt.Sprintf("ReportNumVStep  [rec: %s, numSteps: %d]", rxNumString(cr.rec), v)
	case RptNumTimingSteps:
		v := cr.payload & MskNumTimingSteps
		return fmt.Sprintf("ReportNumTStep  [rec: %s, numSteps: %d]", rxNumString(cr.rec), v)
	case RptMaxTimingOffset:
		v := cr.payload & MskMaxTimingOffset
		return fmt.Sprintf("ReportMaxTOfst  [rec: %s, offset: %d]", rxNumString(cr.rec), v)
	case RptMaxVoltageOffset:
		v := cr.payload & MskMaxVoltageOffset
		return fmt.Sprintf("ReportMaxVOfst  [rec: %s, offset: %d]", rxNumString(cr.rec), v)
	case RptSamplingRateVoltage:
		v := cr.payload & MskSamplingRateVoltage
		return fmt.Sprintf("ReportSmplRateV [rec: %s, rate: %d]", rxNumString(cr.rec), v)
	case RptSamplingRateTiming:
		v := cr.payload & MskSamplingRateTiming
		return fmt.Sprintf("ReportSmplRateT [rec: %s, rate: %d]", rxNumString(cr.rec), v)
	case RptSampleCount:
		v := cr.payload & MskSampleCount
		return fmt.Sprintf("ReportSmplCount [rec: %s, count: %d]", rxNumString(cr.rec), v)
	case RptMaxLanes:
		v := cr.payload & MskMaxLanes
		return fmt.Sprintf("ReportMaxLanes  [rec: %s, lanes: %d]", rxNumString(cr.rec), v)
	default:
		// Only 91h-9Fh reach here: "Report Reserved" in PCIe 6.4 Table 4-75.
		return fmt.Sprintf("ReportReserved  [rec: %s, cmd: 0x%02x, pay: 0x%02x]",
			rxNumString(cr.rec), req.payload, cr.payload)
	}
}

func (cr *cmdRsp) rspSetCmdString() string {
	if isSetErrorCountLimit(cr.payload) {
		limit := cr.payload & 0b0011_1111
		return fmt.Sprintf("SetErrCntLimit  [rec: %s, limit: %d]", rxNumString(cr.rec), limit)
	}
	switch cr.payload {
	case SetGoToNormalSettings:
		return fmt.Sprintf("ToNrmlSetting   [rec: %s]", rxNumString(cr.rec))
	case SetClearErrorLog:
		return fmt.Sprintf("ClearErrorLog   [rec: %s]", rxNumString(cr.rec))
	default:
		return fmt.Sprintf("SetUnknown      [rec: %s, pay: Invalid(0x%02x)]",
			rxNumString(cr.rec), cr.payload)
	}
}

func stepMarginCmdRspStatusString(payload uint16) string {
	status := (payload & StepMarginExecutionStatusMask) >> StepMarginExecutionStatusPos
	switch status {
	case StepMarginExecutionStatusErrorOut:
		return "ErrorOut"
	case StepMarginExecutionStatusSettingUp:
		return "SettingUp"
	case StepMarginExecutionStatusMargining:
		return "Margining"
	case StepMarginExecutionStatusNak:
		return "NAK"
	default:
		return fmt.Sprintf("UnknownStatus(%d)", status)
	}
}

func (cr cmdRsp) rspStepTimingMarginCmdString() string {
	errcnt := uint32(cr.payload & StepMarginErrorCountMask)
	statusStr := stepMarginCmdRspStatusString(cr.payload)
	return fmt.Sprintf("StepTMargin     [rec: %s, status: %s, errCnt: %d]",
		rxNumString(cr.rec), statusStr, errcnt)
}

func (cr cmdRsp) rspStepVoltageMarginCmdString() string {
	errcnt := uint32(cr.payload & StepMarginErrorCountMask)
	statusStr := stepMarginCmdRspStatusString(cr.payload)
	return fmt.Sprintf("StepVMargin     [rec: %s, status: %s, errCnt: %d]",
		rxNumString(cr.rec), statusStr, errcnt)
}

func (cr *cmdRsp) RspString(req *cmdRsp) string {
	// See PCIe 6.4 Ch. 4.2.18.1 Receiver Number, Margin Type, Usage Model, and Margin Payload Fields
	// Table 4-75 Margin Commands and Corresponding Responses
	switch cr.typ {
	case MarginTypeNoCmd:
		return cr.rspNoCmdString()
	case MarginTypeReport:
		// A response carries the Margin Type and Receiver Number of the command
		// it answers (PCIe 6.4 Ch. 4.2.18.1). Decoding by req.payload is only
		// valid for such a response, so print other values field by field.
		if req.typ != MarginTypeReport || cr.rec != req.rec || cr.usage != UsageModel {
			return fmt.Sprintf("Unexpected (not a response to %s [rec: %s]): Report [rec: %s, usg: %d, pay: 0x%02x]",
				marginTypeName(req.typ), rxNumString(req.rec), rxNumString(cr.rec), cr.usage, cr.payload)
		}
		return cr.rspReportCmdString(req)
	case MarginTypeSet:
		return cr.rspSetCmdString()
	case MarginTypeTiming:
		return cr.rspStepTimingMarginCmdString()
	case MarginTypeVoltage:
		return cr.rspStepVoltageMarginCmdString()
	case MarginTypeVendorDefined:
		return cr.vendorDefinedCmdString()
	default:
		return fmt.Sprintf(
			"cmdRsp[rec: %s, typ: Unknown(0x%01x), usg: %d, pay: 0x%02x]",
			rxNumString(cr.rec), cr.typ, cr.usage, cr.payload)
	}
}
