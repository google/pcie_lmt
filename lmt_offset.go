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
	"strings"
	"time"

	log "github.com/golang/glog"
	lmtpb "lmt_go.proto"
)

const (
	// Margin status checking interval is 3ms. At Gen5 speed, 3ms * 32gbps ~= 1E8 samples.
	marginWait    = 3 * time.Millisecond
	marginTimeout = 1000 * time.Millisecond // Margining setup spec timeout is 200ms.
)

// prepareMarginCmdAndPoint composes the margin command at offset on ln, and
// creates its MarginPoint with the direction, the steps, and the voltage or
// timing offset. It appends the point to ln.vsteps or ln.tsteps.
func prepareMarginCmdAndPoint(ln *Lane, offset uint16, t *aspect) (
	cmdRsp, *lmtpb.LinkMargin_Lane_MarginPoint) {
	// Creates a new MarginPoint message.
	point := new(lmtpb.LinkMargin_Lane_MarginPoint)

	// Composes the margin command.
	var cmd cmdRsp
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.payload = offset

	// Identifies the up-down/left-right directions
	var dir lmtpb.LinkMargin_Lane_MarginPoint_DirectionEnum
	var steps uint16
	if t.VnotT {
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
	}
	point.Direction = dir
	point.Steps = uint32(steps)
	return cmd, point
}

// margin() conducts either a timing or a voltage margining at one offset on a
// receiver Lane. The result is logged in a LinkMargin_Lane_MarginPoint.
// The offset includes the direction bit, [6] for timing and [7] for voltage.
// The offset is used as the command payload as is.
func (ln *Lane) margin(offset uint16, t *aspect) (
	*lmtpb.LinkMargin_Lane_MarginPoint, error) {

	cmd, point := prepareMarginCmdAndPoint(ln, offset, t)
	var err error
	err = nil
	defer func() {
		if err != nil {
			e := err.Error()
			point.Error = &e
		}
	}()

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
			rsp.decode(readWord(ln.dev, ln.addr+2))
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
			if dwellActual >= t.dwell {
				// Exists loop when time is up.
				setSampleCount = true
				break looping
			}
			// Rereads the status after a fixed period
			time.Sleep(marginWait)
			rsp.decode(readWord(ln.dev, ln.addr+2))
		default:
			point.Status = lmtpb.LinkMargin_Lane_MarginPoint_S_UNKNOWN
			break looping
		}
	}
	bitCount := float64(point.ErrorCount) // bitCount is used to calculate BER
	if setSampleCount {
		// gets sample count
		if ln.param.GetSampleReportingMethod() || !ln.param.GetIndErrorSampler() {
			// The samples are counted by rate * dwell.
			// AMD CPU does not have error sampler. It also does not report a sample count.
			bitCount = dwellActual.Seconds() * t.sps // Actual dwell * samples per second
			// Refers to PCIe 5.0 spec 8.4.4: SampleCount = 3*log 2 (number of bits)
			if bitCount == 0 {
				samples := uint32(18) // 64 bits is for practical reason to differ from the default 0.
				point.SampleCount = &samples
				bitCount = math.Pow(2.0, float64(samples)/3.0)
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
			bitCount = math.Pow(2.0, float64(samples)/3.0)
			point.SampleCount = &samples
		}
	}
	fmt.Printf(
		"Point margin: BDF:%s Rx:%-9s Ln:%2d  Dir:%-8s Step:%3d  Status:%-13s ErrCnt:%2d  Samples:%3d\n",
		ln.cfg.GetUspBdf(), ln.rec.String(), ln.laneNumber, point.GetDirection().String(),
		point.GetSteps(), point.GetStatus().String(), point.GetErrorCount(), point.GetSampleCount())

	if point.GetStatus() != lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING {
		if point.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT {
			if !t.errOutOK {
				ln.Pass = false
			}
		} else {
			ln.Pass = false
		}
	}

	// Calls the margin hooks, e.g. to stream the OCP TestStepMeasurement artifacts.
	for _, h := range ln.rx.marginHooks {
		if err := h.PostMargin(ln, t, point, bitCount, dwellActual); err != nil {
			log.Warningf("%s: %s.PostMargin: %v", ln.laneRepr(), h.Name(), err)
		}
	}

	// Issues "Clear Error Log" and "Go to Normal Settings" commands
	return point, sendPostMarginCmds(ln)
}

// sendPostMarginCmds issues the "Clear Error Log" and "Go to Normal Settings"
// commands to the receiver of ln. It stops at the first command that fails.
func sendPostMarginCmds(ln *Lane) error {
	var cmd cmdRsp
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.typ = MarginTypeSet
	cmd.payload = SetClearErrorLog
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return err
	}
	cmd.payload = SetGoToNormalSettings
	return ln.lmrCmdRspEcho(&cmd)
}

// pointOcpName returns the signed offset of point for the OCP subcomponent
// location, e.g. "T:+0.250000UI" or "V:-0.100000V". The sign is omitted when
// the direction is not independent. It returns "" for an unknown direction.
func pointOcpName(point *lmtpb.LinkMargin_Lane_MarginPoint) string {
	switch point.GetDirection() {
	case lmtpb.LinkMargin_Lane_MarginPoint_D_UP:
		return fmt.Sprintf("V:+%fV", point.GetVoltage())
	case lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN:
		return fmt.Sprintf("V:-%fV", point.GetVoltage())
	case lmtpb.LinkMargin_Lane_MarginPoint_D_UD:
		return fmt.Sprintf("V:%fV", point.GetVoltage())
	case lmtpb.LinkMargin_Lane_MarginPoint_D_RIGHT:
		return fmt.Sprintf("T:+%fUI", point.GetPercentUi())
	case lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT:
		return fmt.Sprintf("T:-%fUI", point.GetPercentUi())
	case lmtpb.LinkMargin_Lane_MarginPoint_D_LR:
		return fmt.Sprintf("T:%fUI", point.GetPercentUi())
	default:
		return ""
	}
}

// unitName returns the OCP measurement unit of point, which holds its step,
// direction and voltage or timing offset, e.g.
// "Unit=UI;Step=005;Dir=RIGHT   ;Offset=0.250".
func unitName(point *lmtpb.LinkMargin_Lane_MarginPoint, t *aspect) string {
	if t.VnotT {
		return fmt.Sprintf("Unit=V;Step=%03d;Dir=%-8s;Offset=%6.4f",
			point.GetSteps(), strings.TrimPrefix(point.GetDirection().String(), "D_"), point.GetVoltage())
	}
	return fmt.Sprintf("Unit=UI;Step=%03d;Dir=%-8s;Offset=%5.3f",
		point.GetSteps(), strings.TrimPrefix(point.GetDirection().String(), "D_"), point.GetPercentUi())
}
