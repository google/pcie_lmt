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

// Lane-level margining operations.

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	log "github.com/golang/glog"
	structpb "google.golang.org/protobuf/types/known/structpb"
	pbj "google.golang.org/protobuf/encoding/protojson"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

// /////////////////////////////////////////////////////////////////////////////////////////////////

// A Lane conducts a series of timing/voltage margining points
type Lane struct {
	cfg        *lmtpb.LinkMargin // The test configuration protobuf.
	dev        pci.Dev           // The PCI config access for the port.
	laneNumber uint32
	addr       int32                         // The lane address in the LMR config space.
	rec        lmtpb.LinkMargin_ReceiverEnum // the enumerated receiver number at the 6 Rx points on a link.
	rx         *receiver
	speed      float64                // bps: Gen4:16E9, Gen5:32E9
	lane       *lmtpb.LinkMargin_Lane // The lane result protobuf.
	// The following are result messages under the lane protobuf.
	param        *lmtpb.LinkMargin_Lane_Parameters
	Vspec        *lmtpb.LinkMargin_TestSpec
	Tspec        *lmtpb.LinkMargin_TestSpec
	tsteps       []*lmtpb.LinkMargin_Lane_MarginPoint
	vsteps       []*lmtpb.LinkMargin_Lane_MarginPoint
	msg          string
	Pass         bool            // This member exports the pass/fail status of the lane.
	rxwg         *sync.WaitGroup // Wait for the receiver port.
	linkwg       *sync.WaitGroup // Wait for all links.
	eyeWidth     float32
	eyeHeight    float32
	eyeScanMode  bool // When the target_offset is not set, the test spec is in eye-scan mode.
	eyeSizeCheck bool // When the eye_size is set, the test spec check for size rather than margin.
	// OCP JSON message output
	stepArtiOut *ocppb.OutputArtifact
	mStepArti   *ocppb.TestStepArtifact
	statusVal   *ocppb.Validator
	berVal      *ocppb.Validator
}

// Init initialized a new Lane instance with the test setup.
func (ln *Lane) Init(
	cfg *lmtpb.LinkMargin,
	dev pci.Dev,
	laneNumber int,
	addr int32,
	rec lmtpb.LinkMargin_ReceiverEnum,
	speed float64,
	rxwg *sync.WaitGroup,
	linkwg *sync.WaitGroup,
	rx *receiver) {

	ln.cfg = cfg
	ln.dev = dev
	ln.laneNumber = uint32(laneNumber)
	ln.addr = addr + 8 + int32(laneNumber)*4 // 4B per Lane start with 8B offset
	ln.speed = speed
	ln.rec = rec
	ln.Pass = true
	ln.rxwg = rxwg
	ln.linkwg = linkwg
	ln.lane = nil
	ln.param = nil
	ln.Vspec = nil
	ln.Tspec = nil
	ln.tsteps = nil
	ln.vsteps = nil
	ln.rx = rx

	// OCP JSON message output
	ln.mStepArti = &ocppb.TestStepArtifact{
		Artifact:   &ocppb.TestStepArtifact_Measurement{Measurement: nil},
		TestStepId: ln.rx.hwinfo,
	}

	ln.stepArtiOut = &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: ln.mStepArti},
	}
}

const (
	// MaxTimingOffset is between 20 and 50; default to the max.
	defaultMaxTimingOffset = 50
	// MaxVoltageOffset is between 5 and 50; default to the max.
	defaultMaxVoltageOffset = 50
)

// readLaneParameters() reads the Lane margining capability parameters from each
// Lane.
func (ln *Lane) readLaneParameters() error {
	param := new(lmtpb.LinkMargin_Lane_Parameters)
	ln.param = param

	var rsp *cmdRsp
	var err error
	var cmd cmdRsp
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.typ = MarginTypeReport

	cmd.payload = RptControlCapabilities
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.IndErrorSampler = (rsp.payload & MskIndErrorSampler) != 0
	param.SampleReportingMethod = (rsp.payload & MskSampleReportingMethod) != 0
	param.IndLeftRightTiming = (rsp.payload & MskIndLeftRightTiming) != 0
	param.IndUpDownVoltage = (rsp.payload & MskIndUpDownVoltage) != 0
	param.VoltageSupported = (rsp.payload & MskVoltageSupported) != 0

	cmd.payload = RptNumVoltageSteps
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.NumVoltageSteps = uint32(rsp.payload & MskNumVoltageSteps)

	cmd.payload = RptNumTimingSteps
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.NumTimingSteps = uint32(rsp.payload & MskNumTimingSteps)

	cmd.payload = RptMaxTimingOffset
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.MaxTimingOffset = uint32(rsp.payload & MskMaxTimingOffset)
	// 0 may be reported if the vendor chooses not to report the offset. Then default to the max.
	if param.MaxTimingOffset == 0 {
		param.MaxTimingOffset = defaultMaxTimingOffset
	}

	cmd.payload = RptMaxVoltageOffset
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.MaxVoltageOffset = uint32(rsp.payload & MskMaxVoltageOffset)
	// 0 may be reported if the vendor chooses not to report the offset. Then default to the max.
	if param.MaxVoltageOffset == 0 {
		param.MaxVoltageOffset = defaultMaxVoltageOffset
	}

	cmd.payload = RptSamplingRateVoltage
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.SamplingRateVoltage = uint32(rsp.payload & MskSamplingRateVoltage)

	cmd.payload = RptSamplingRateTiming
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.SamplingRateTiming = uint32(rsp.payload & MskSamplingRateTiming)

	cmd.payload = RptMaxLanes
	if rsp, err = ln.lmrCmdRsp(&cmd); err != nil {
		return err
	}
	param.MaxLanes = uint32(rsp.payload & MskMaxLanes)

	opt := &pbj.MarshalOptions{
		UseProtoNames:   false,
		UseEnumNumbers:  false,
		EmitUnpopulated: false,
		Multiline:       false,
		Indent:          "",
		AllowPartial:    false,
	}
	data, err := opt.Marshal(param)
	if err != nil {
		log.Errorf("pbj.Marshal(%v) failed: %v", data, err)
		return err
	}

	m := &ocppb.Measurement{
		Name:           fmt.Sprintf("LN=%02d;Lane-Parameters", ln.laneNumber),
		Value:          structpb.NewStringValue(string(data)),
		HardwareInfoId: ln.rx.hwinfo,
	}
	ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}
	outputArtifact(ln.stepArtiOut)

	return nil
}

// MarginLane performs series of margining at steps.
func (ln *Lane) MarginLane() error {
	var msg strings.Builder
	defer func() {
		ln.msg = msg.String()
	}()
	var err error
	var cmd cmdRsp

	// Clears error log and goes to normal settings.
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.typ = MarginTypeSet
	cmd.payload = SetClearErrorLog
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return err
	}
	cmd.payload = SetGoToNormalSettings
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return err
	}

	// Reads Lane parameters
	ln.readLaneParameters()
	param := ln.param

	ln.rxwg.Done()
	// Waits for all the lanes (if in parallel) to finish reading the parameter.
	ln.rxwg.Wait()
	ln.linkwg.Wait()

	// This is a test config to distinguish between timing and voltage margining.
	type test struct {
		VnotT   bool
		spec    *lmtpb.LinkMargin_TestSpec
		steps   uint32
		rate    uint32
		indDir  bool // independent left/right or up/down
		dirmask uint16
	}
	tests := make([]test, 0, 2)

	// Margins timing if specified
	tt := test{
		VnotT:   false,
		spec:    ln.Tspec,
		steps:   param.GetNumTimingSteps(),
		rate:    param.GetSamplingRateTiming(),
		indDir:  param.GetIndLeftRightTiming(),
		dirmask: TimingDirMask}

	if ln.Tspec != nil {
		tt.spec = ln.Tspec
		tests = append(tests, tt)
	} else {
		msg.WriteString("Timing margining not specified. | ")
	}

	// Margins voltage if supported and specified
	vt := test{
		VnotT:   true,
		spec:    ln.Vspec,
		steps:   param.GetNumVoltageSteps(),
		rate:    param.GetSamplingRateVoltage(),
		indDir:  param.GetIndUpDownVoltage(),
		dirmask: VoltageDirMask}

	if !param.GetVoltageSupported() {
		ln.Vspec = nil
		vt.spec = ln.Vspec
		msg.WriteString("Voltage margining not supported. | ")
	} else if ln.Vspec != nil {
		vt.spec = ln.Vspec
		tests = append(tests, vt)
	} else {
		msg.WriteString("Voltage margining not specified. | ")
	}

	// Executes the prepared test specs.
	for _, t := range tests {
		errlimit := uint16(t.spec.GetErrorLimit())
		cmd.typ = MarginTypeSet
		cmd.payload = SetErrorCountLimit | errlimit
		if err = ln.lmrCmdRspEcho(&cmd); err != nil {
			msg.WriteString(err.Error() + " | ")
			return err
		}

		// Assumes max sampling rate if it reads 0, and independent error sampler is
		// not supported or reporting method is count not rate.
		if t.rate == 0 && (!ln.param.GetIndErrorSampler() || !ln.param.GetSampleReportingMethod()) {
			t.rate = 63
		}
		// Calculates minimum dwell time based on bits to be sampled.
		// Refers to PCIe 5.0 spec 8.4.4: SampleCount = 3*log 2 (number of bits)
		bitCount := math.Pow(2.0, float64(t.spec.GetSamples())/3.0)
		log.V(1).Infoln("Sample bit count = ", bitCount)
		// Samples per second. t.rate is define as the # of bits checked out of 64 bits, - 1.
		sps := (float64(t.rate+1) / 64.0) * ln.speed
		log.V(1).Infoln("Sample per second = ", sps)
		// Expected dwell is bitCount / sampleRate / speed
		dwell := time.Duration(math.Ceil(bitCount/sps)) * time.Second
		log.V(1).Infoln("Calculated dwell = ", dwell)

		// If greater dwell is specified, use that.
		if t.spec.Dwell == nil || dwell > time.Duration(*t.spec.Dwell)*time.Second {
			dwellSeconds := (float32(dwell.Seconds()))
			t.spec.Dwell = &dwellSeconds
		}

		if t.spec.StartOffset == nil && t.spec.TargetOffset == nil {
			log.Error("Either the start_offset or the target_offset must be specified. Margining skipped.")
			continue
		}
		// Sets the starting and ending steps.
		var untilFail bool
		var target, start uint16
		if t.spec.TargetOffset != nil {
			untilFail = false
			// Set the target max offset to be no greater than the Lane's capability.
			if *t.spec.TargetOffset > t.steps {
				t.spec.TargetOffset = &t.steps
			}
			target = uint16(t.spec.GetTargetOffset())
		} else {
			// Explore until fail mode
			untilFail = true
			target = uint16(t.steps)
		}
		if t.spec.StartOffset != nil {
			start = uint16(*t.spec.StartOffset)
			// Step, if specified, must be no greater than the target.
			if start > target {
				log.Warningf("start_offset %d cannot be greater than target_offset %d; adjusting it to be equal.", start, target)
				start = target
			}
		} else {
			start = target
		}

		var step uint16
		if t.spec.Step != nil {
			step = uint16(t.spec.GetStep())
			if step == 0 {
				log.Warning("step cannot be 0; adjusting it to be 1.")
				step = 1
			}
		} else {
			step = 1
		}

		// eye scan mode is when the target_offset is not specified. Then ERROR_OUT is expected when the
		// eye boundary is detected. Therefore, they are not reported as failure. Also, error_count==0
		// are not reported to reduce cluttering.
		ln.eyeScanMode = untilFail
		// eye_size enables width and height checking, and also assumes the eye does not need to be
		// centered. ERROR_OUT and BER checking are not enforced.
		ln.eyeSizeCheck = t.spec.EyeSize != nil
		// For eye size pass-fail testing, specify a range of steps (both starting_offset and
		// target_offset), and also specify the eye_size in the test_spec. Then even the eye is off-
		// center, the test can still pass.

		ln.statusVal = &ocppb.Validator{
			Name: "Status Check",
			Type: ocppb.Validator_IN_SET,
		}
		if ln.eyeScanMode || ln.eyeSizeCheck {
			lv, _ := structpb.NewList([]any{
				strings.TrimPrefix(lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING.String(), "S_"),
				strings.TrimPrefix(lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT.String(), "S_")})
			ln.statusVal.Value = structpb.NewListValue(lv)
		} else {
			lv, _ := structpb.NewList([]any{
				strings.TrimPrefix(lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING.String(), "S_")})
			ln.statusVal.Value = structpb.NewListValue(lv)
		}

		// BER threshold is calculated as the error_limit / (dwell_time * sample_rate).
		// Additional 0.5 is added to the error_limit to avoid false failure
		// when the actual error count is the same as the error_limit,
		// while the berThresh is text-truncated in meltan artifact.
		berThresh := (float64(errlimit) + 0.5) / (float64(*t.spec.Dwell) * sps)
		ln.berVal = &ocppb.Validator{
			Name:  "Max BER Check",
			Type:  ocppb.Validator_LESS_THAN_OR_EQUAL,
			Value: structpb.NewNumberValue(berThresh),
		}

		var mpPassPos, mpPassNeg, mpFailPos, mpFailNeg *lmtpb.LinkMargin_Lane_MarginPoint
		for offset := start; ; {
			// Steps until either target offset is reached or neither side is passing
			passPos := false
			passNeg := false
			var mp *lmtpb.LinkMargin_Lane_MarginPoint
			if mp, err = ln.margin(offset, t.VnotT, sps); err != nil {
				msg.WriteString(err.Error() + " | ")
			}
			passPos = mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING
			if passPos && mpFailPos == nil {
				mpPassPos = mp
			} else if mpFailPos == nil {
				mpFailPos = mp
			}

			passNeg = passPos
			// if independent left/right or up/down, tests the negative side.
			if t.indDir {
				if mp, err = ln.margin(offset|t.dirmask, t.VnotT, sps); err != nil {
					msg.WriteString(err.Error() + " | ")
				}
				passNeg = mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING
			}
			if passNeg && mpFailNeg == nil {
				mpPassNeg = mp
			} else if mpFailNeg == nil {
				mpFailNeg = mp
			}

			if offset >= target {
				break
			} else if untilFail && !passPos && !passNeg {
				break
			} else {
				offset += uint16(step)
				// Makes sure target is margined in the end.
				if offset > target {
					offset = target
				}
			}
		}

		// Output the pass/fail boundary.
		m := &ocppb.Measurement{
			HardwareInfoId: ln.rx.hwinfo,
		}
		ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}
		for i, mp := range []*lmtpb.LinkMargin_Lane_MarginPoint{
			mpPassPos, mpFailPos, mpPassNeg, mpFailNeg} {
			var dir string
			var pf string
			var unit string
			var v float64
			if mp != nil {
				if i == 0 || i == 2 {
					pf = "MAX-PASSING"
				} else {
					pf = "MIN-FAILING"
				}
				if t.VnotT {
					if i == 0 || i == 1 {
						dir = "TOP"
					} else {
						dir = "BOT"
					}
					v = float64(mp.GetVoltage())
					unit = "V"
				} else {
					if i == 0 || i == 1 {
						dir = "RIGHT"
					} else {
						dir = "LEFT"
					}
					v = float64(mp.GetPercentUi())
					unit = "UI"
				}
				name := fmt.Sprintf("EYE CORNER %s %-5s", pf, dir)
				mp.Info = &name
				fmt.Println(ln.rx.hwinfo, ";LN=", ln.laneNumber, ";", name, ":", v, unit, ";Step=", mp.GetSteps())

				if ln.eyeScanMode {
					// Stream OCP TestStepMeasurement artifact
					mName := fmt.Sprintf("LN=%02d;%s-%s-%s", ln.laneNumber, pf, unit, dir)
					m.Name = mName
					m.Unit = fmt.Sprintf("Unit=%s;BER=%.2E", unit, berThresh)
					m.Value = structpb.NewNumberValue(v)
					outputArtifact(ln.stepArtiOut)
				}
			}
		}

		if ln.eyeScanMode || ln.eyeSizeCheck {
			v := float32(0.0)
			if t.VnotT {
				m.Name = fmt.Sprintf("LN=%02d;Eye-Height", ln.laneNumber)
				m.Unit = fmt.Sprintf("Unit=V;BER=%.2E", berThresh)
				if mpPassPos != nil {
					v += mpPassPos.GetVoltage()
				}
				if mpPassNeg != nil {
					v += mpPassNeg.GetVoltage()
				}
				ln.eyeHeight = float32(v)
			} else {
				m.Name = fmt.Sprintf("LN=%02d;Eye-Width", ln.laneNumber)
				m.Unit = fmt.Sprintf("Unit=UI;BER=%.2E", berThresh)
				if mpPassPos != nil {
					v += mpPassPos.GetPercentUi()
				}
				if mpPassNeg != nil {
					v += mpPassNeg.GetPercentUi()
				}
				ln.eyeWidth = float32(v)
			}
			m.Value = structpb.NewNumberValue(float64(v))
			if ln.eyeSizeCheck {
				if v < t.spec.GetEyeSize() {
					ln.Pass = false
				}
				val := &ocppb.Validator{
					Name:  "Eye Size Check",
					Type:  ocppb.Validator_GREATER_THAN_OR_EQUAL,
					Value: structpb.NewNumberValue(float64(t.spec.GetEyeSize())),
				}
				m.Validators = []*ocppb.Validator{val}
			}
			outputArtifact(ln.stepArtiOut)
		}
	}

	// ln.lane != nil is an indication that the margining is done maturely.
	ln.lane = new(lmtpb.LinkMargin_Lane)

	return nil
}
