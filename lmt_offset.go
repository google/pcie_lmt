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

	structpb "google.golang.org/protobuf/types/known/structpb"
	ocppb "ocpdiag/results_go_proto"
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
func (ln *Lane) margin(offset uint16, t *aspect) (
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
	var ocpName string
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
				ocpName = fmt.Sprintf("V:+%fV", vv)
			} else {
				dir = lmtpb.LinkMargin_Lane_MarginPoint_D_DOWN
				ocpName = fmt.Sprintf("V:-%fV", vv)
			}
		} else {
			dir = lmtpb.LinkMargin_Lane_MarginPoint_D_UD
			ocpName = fmt.Sprintf("V:%fV", vv)
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
				ocpName = fmt.Sprintf("T:+%fUI", ui)
			} else {
				dir = lmtpb.LinkMargin_Lane_MarginPoint_D_LEFT
				ocpName = fmt.Sprintf("T:-%fUI", ui)
			}
		} else {
			dir = lmtpb.LinkMargin_Lane_MarginPoint_D_LR
			ocpName = fmt.Sprintf("T:%fUI", ui)
		}
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
			if dwellActual >= t.dwell {
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
		ln.cfg.GetBdf()[0], ln.rec.String(), ln.laneNumber, point.GetDirection().String(),
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

	// Stream OCP TestStepMeasurement artifact
	var unit string
	if t.VnotT {
		unit = fmt.Sprintf("Unit=V;Step=%03d;Dir=%-8s;Offset=%6.4f",
			point.GetSteps(), strings.TrimPrefix(point.GetDirection().String(), "D_"), point.GetVoltage())
	} else {
		unit = fmt.Sprintf("Unit=UI;Step=%03d;Dir=%-8s;Offset=%5.3f",
			point.GetSteps(), strings.TrimPrefix(point.GetDirection().String(), "D_"), point.GetPercentUi())
	}
	subcomp := &ocppb.Subcomponent{
		Type: ocppb.Subcomponent_BUS,
		Name: "PCIELMT-MARGINPOINT-PCI",
		Location: fmt.Sprintf("BDF=%s;RX=%1d;LN=%02d;Offset=%s",
			ln.cfg.GetBdf()[0], ln.rec.Number(), ln.laneNumber, ocpName),
	}

	if !t.eyeScanMode || point.GetStatus() != lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING {
		m := &ocppb.Measurement{
			Name:           fmt.Sprintf("LN=%02d;Step-Status", ln.laneNumber),
			Value:          structpb.NewStringValue(strings.TrimPrefix(point.GetStatus().String(), "S_")),
			Unit:           unit,
			HardwareInfoId: ln.rx.hwinfo,
			Subcomponent:   subcomp,
			Validators:     []*ocppb.Validator{ln.statusVal},
		}
		ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}
		outputArtifact(ln.stepArtiOut)
	}

	if (point.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING ||
		point.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT) && (!t.eyeScanMode ||
		point.ErrorCount != 0) {
		m := &ocppb.Measurement{
			Name:           fmt.Sprintf("LN=%02d;Step-BER", ln.laneNumber),
			Value:          structpb.NewNumberValue(float64(point.ErrorCount) / bitCount),
			Unit:           unit,
			HardwareInfoId: ln.rx.hwinfo,
			Subcomponent:   subcomp,
		}
		if !t.errOutOK {
			m.Validators = []*ocppb.Validator{ln.berVal}
		}
		ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}
		outputArtifact(ln.stepArtiOut)
	}

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
