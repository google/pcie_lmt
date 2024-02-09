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

// The margining procedure at a single offset.

import (
	"fmt"
	"math"
	"time"

	lmtpb "lmt_go.proto"
	pci "pciutils"
)

const (
	// Margin status checking interval is 3ms. At Gen5 speed, 3ms * 32gbps ~= 1E8 samples.
	marginWait    = 3 * time.Millisecond
	marginTimeout = 1000 * time.Millisecond // Margining setup spec timeout is 200ms.
)

// margin() conducts either a timing or a voltage margining at one offset on a
// receiver Lane. The result is logged in a LinkMargin_Lane_MarginPoint.
// The offset includes the direction bit, [6] for timing and [7] for voltage.
// The offset is used as the command payload as is.
func (ln *Lane) margin(offset uint16, vNotT bool, sps float64) (
	*lmtpb.LinkMargin_Lane_MarginPoint, error) {

	// Creates a new MarginPoint message.
	point := new(lmtpb.LinkMargin_Lane_MarginPoint)
	var err error
	err = nil
	defer func() {
		if err != nil {
			e := err.Error()
			point.Error = &e
		}
	}()

	// Composes the margin command.
	var cmd cmdRsp
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.payload = offset

	// Identifies the up-down/left-right directions
	var dir lmtpb.LinkMargin_Lane_MarginPoint_DirectionEnum
	var steps uint16
	var dwell time.Duration
	if vNotT {
		cmd.typ = MarginTypeVoltage
		steps = offset &^ VoltageDirMask
		vv := float32(steps) *
			float32(ln.param.GetMaxVoltageOffset()) / 100.0 / float32(ln.param.GetNumVoltageSteps())
		point.Voltage = &vv
		ln.vsteps = append(ln.vsteps, point)
		if ln.param.GetIndUpDownVoltage() {
			if (offset & VoltageDirMask) == 0 {
				dir = lmtpb.LinkMargin_Lane_MarginPoint_D_UP
			} else {
				dir = lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN
			}
		} else {
			dir = lmtpb.LinkMargin_Lane_MarginPoint_D_UD
		}
		dwell = time.Duration(ln.Vspec.GetDwell()) * time.Second
	} else {
		cmd.typ = MarginTypeTiming
		steps = offset &^ TimingDirMask
		ui := float32(steps) *
			float32(ln.param.GetMaxTimingOffset()) / 100.0 / float32(ln.param.GetNumTimingSteps())
		point.PercentUi = &ui
		ln.tsteps = append(ln.tsteps, point)
		if ln.param.GetIndLeftRightTiming() {
			if (offset & TimingDirMask) == 0 {
				dir = lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT
			} else {
				dir = lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT
			}
		} else {
			dir = lmtpb.LinkMargin_Lane_MarginPoint_D_LR
		}
		dwell = time.Duration(ln.Tspec.GetDwell()) * time.Second
	}
	point.Direction = dir
	point.Steps = uint32(steps)

	// Executes the command
	var rsp *cmdRsp
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return point, err
	}
	// Starts tracking time after the command is executed.
	t0 := time.Now()
	var tDwellStart time.Time // The dwell starting time.
	var dwellActual time.Duration
	dwellActual = 0
	setSampleCount := false

	// Loops status reads
looping:
	for {
		// Always records the latest response
		// TODO(huat): log this point.SetRawResponse(uint32(rsp.raw))
		// Extracts and logs the status fields from the response
		status := (rsp.payload & StepMarginExecutionStatusMask) >>
			StepMarginExecutionStatusPos

		// Extracts and logs the error count
		errcnt := uint32(rsp.payload & StepMarginErrorCountMask)
		point.ErrorCount = errcnt

		switch status {
		case StepMarginExecutionStatusNak:
			// NAK. Indicates that an unsupported Lane Margining command was issued.
			// Most likely the offset is out of bound
			point.Status = lmtpb.LinkMargin_Lane_MarginPoint_S_NAK
			break looping
		case StepMarginExecutionStatusErrorOut:
			// Get the actual dwell time.
			if !tDwellStart.IsZero() {
				dwellActual = time.Since(tDwellStart)
			}
			// Margining is able to determine the set error limit is exceeded.
			point.Status = lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT
			setSampleCount = true
			break looping
		case StepMarginExecutionStatusSettingUp:
			// The Receiver is getting ready but has not yet started margining
			point.Status = lmtpb.LinkMargin_Lane_MarginPoint_S_SETTING_UP
			if time.Since(t0) > marginTimeout {
				break looping
			}
			// Rereads the status after a fixed period.
			time.Sleep(marginWait)
			rsp.decode(uint16(pci.ReadWord(ln.dev, ln.addr+2)))
		case StepMarginExecutionStatusMargining:
			// Starts the actual dwell clock.
			if tDwellStart.IsZero() {
				tDwellStart = time.Now()
			}
			// Get the actual dwell time.
			dwellActual = time.Since(tDwellStart)
			// This is the case pb.LinkMargin_Lane_MarginPoint_S_MARGINING.
			// Margining is in progress.
			point.Status = lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING
			if time.Since(t0) >= dwell {
				// Exists loop when time is up.
				setSampleCount = true
				break looping
			}
			// Rereads the status after a fixed period
			time.Sleep(marginWait)
			rsp.decode(uint16(pci.ReadWord(ln.dev, ln.addr+2)))
		default:
			point.Status = lmtpb.LinkMargin_Lane_MarginPoint_S_UNKNOWN
			break looping
		}
	}
	if setSampleCount {
		// gets sample count
		if ln.param.GetSampleReportingMethod() || !ln.param.GetIndErrorSampler() {
			// The samples are counted by rate * dwell.
			// AMD CPU does not have error sampler. It also does not report a sample count.
			bitCount := dwellActual.Seconds() * sps // Actual dwell * samples per second
			// Refers to PCIe 5.0 spec 8.4.4: SampleCount = 3*log 2 (number of bits)
			if bitCount == 0 {
				samples := uint32(18) // 64 bits is for practical reason to differ from the default 0.
				point.SampleCount = &samples
			} else {
				samples := uint32(math.Round(math.Log2(bitCount) * 3))
				point.SampleCount = &samples
			}
		} else {
			cmd.typ = MarginTypeReport
			cmd.payload = RptSampleCount
			if rsp, err = ln.lmrCmdRspBase(&cmd, false); err != nil {
				return point, err
			}
			samples := uint32(rsp.payload & MskSampleCount)
			point.SampleCount = &samples
		}
	}
	fmt.Printf(
		"Point margin: Bus:%02x Rx:%-9s Ln:%2d  Dir:%-8s Step:%3d  Status:%-13s ErrCnt:%2d  Samples:%3d\n",
		ln.cfg.GetBus()[0], ln.rec.String(), ln.laneNumber, point.GetDirection().String(),
		point.GetSteps(), point.GetStatus().String(), point.GetErrorCount(), point.GetSampleCount())

	// Issues "Clear Error Log" and "Go to Normal Settings" commands
	cmd.typ = MarginTypeSet
	cmd.payload = SetClearErrorLog
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return point, err
	}
	cmd.payload = SetGoToNormalSettings
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return point, err
	}
	return point, nil
}
