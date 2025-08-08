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

// Pass/Fail enum
const (
	pass = iota // 0
	fail        // 1
)

// Pos/Neg enum
const (
	pos = iota // 0
	neg        // 1
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
	param     *lmtpb.LinkMargin_Lane_Parameters
	Vspec     *lmtpb.LinkMargin_TestSpec
	Tspec     *lmtpb.LinkMargin_TestSpec
	tsteps    []*lmtpb.LinkMargin_Lane_MarginPoint
	vsteps    []*lmtpb.LinkMargin_Lane_MarginPoint
	msg       string
	Pass      bool            // This member exports the pass/fail status of the lane.
	rxwg      *sync.WaitGroup // Wait for the receiver port.
	linkwg    *sync.WaitGroup // Wait for all links.
	eyeWidth  float32
	eyeHeight float32
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

// This is a test config to distinguish between timing and voltage margining.
type aspect struct {
	VnotT       bool
	spec        *lmtpb.LinkMargin_TestSpec
	steps       uint32
	maxOffset   float32
	rate        uint32
	indDir      bool // independent left/right or up/down
	dirmask     uint16
	mp          [2][2]*lmtpb.LinkMargin_Lane_MarginPoint // [Pos/Neg][Pass/Fail]
	dwell       time.Duration
	bitCount    float64
	sps         float64
	berThresh   float64
	start       uint16
	target      uint16
	step        uint16
	eyeScanMode bool
	targetMode  bool
	eyeSizeMode bool
	untilFail   bool
	errOutOK    bool
}

// MarginLane performs series of margining at steps.
func (ln *Lane) MarginLane() error {
	var msg strings.Builder
	defer func() {
		ln.msg = msg.String()
	}()

	// Reads Lane parameters
	if err := ln.readLaneParameters(); err != nil {
		log.Errorf("Failed to read lane parameters for lane %d: %v", ln.laneNumber, err)
		ln.rxwg.Done()
		return err
	}

	ln.rxwg.Done()
	// Waits for all the lanes (if in parallel) to finish reading the parameter.
	ln.rxwg.Wait()
	ln.linkwg.Wait()

	aspects := ln.prepareMarginTests(&msg)

	// Executes the prepared test specs.
	for i := range aspects {
		if err := ln.testAspect(&aspects[i], &msg); err != nil {
			// Log error and continue to next test type if desired, or return error to stop all tests.
			log.Errorf("Error executing test for lane %d: %v", ln.laneNumber, err)
			return err
		}
	}

	// ln.lane != nil is an indication that the margining is done maturely.
	ln.lane = new(lmtpb.LinkMargin_Lane)

	return nil
}

// readLaneParameters() reads the Lane margining capability parameters from each
// Lane.
func (ln *Lane) readLaneParameters() error {
	param := new(lmtpb.LinkMargin_Lane_Parameters)
	ln.param = param

	var rsp *cmdRsp
	var err error
	var cmd cmdRsp

	// clears error logs and sets the lane to normal settings.
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.typ = MarginTypeSet
	cmd.payload = SetClearErrorLog
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return fmt.Errorf("failed to clear error log: %w", err)
	}
	cmd.payload = SetGoToNormalSettings
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		return fmt.Errorf("failed to set to normal settings: %w", err)
	}

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

// prepareMarginTests creates the list of timing and/or voltage tests to run.
func (ln *Lane) prepareMarginTests(msg *strings.Builder) []aspect {
	aspects := make([]aspect, 0, 2)
	param := ln.param

	// Margins timing if specified
	if ln.Tspec != nil {
		aspects = append(aspects, aspect{
			VnotT:     false,
			spec:      ln.Tspec,
			steps:     param.GetNumTimingSteps(),
			maxOffset: float32(param.GetMaxTimingOffset()) / 100.0, // in UI, 50 = 50%UI
			rate:      param.GetSamplingRateTiming(),
			indDir:    param.GetIndLeftRightTiming(),
			dirmask:   TimingDirMask,
		})
	} else {
		msg.WriteString("Timing margining not specified. | ")
	}

	// Margins voltage if supported and specified
	if ln.Vspec != nil {
		if param.GetVoltageSupported() {
			aspects = append(aspects, aspect{
				VnotT:     true,
				spec:      ln.Vspec,
				steps:     param.GetNumVoltageSteps(),
				maxOffset: float32(param.GetMaxVoltageOffset()) / 100.0, // in Volts 50 = 0.5V
				rate:      param.GetSamplingRateVoltage(),
				indDir:    param.GetIndUpDownVoltage(),
				dirmask:   VoltageDirMask,
			})
		} else {
			msg.WriteString("Voltage margining specified but voltage not supported. | ")
			ln.Vspec = nil // Ensure Vspec is nil if not supported
		}
	} else {
		msg.WriteString("Voltage margining not specified. | ")
	}
	return aspects
}

// testAspect executes one test from the list.
func (ln *Lane) testAspect(t *aspect, msg *strings.Builder) error {
	if t.spec.StartOffset == nil && t.spec.TargetOffset == nil && t.spec.EyeSize == nil {
		log.Warningf("Lane %d: Test spec is empty, skipping", ln.laneNumber)
		return nil
	}

	ln.calculateDwellTime(t)
	ln.setupLaneValidators(t)
	err := ln.setErrLimit(t, msg)
	if err != nil {
		return err
	}

	ln.determineMarginRange(t)

	if t.eyeSizeMode {
		ln.testEyeSize(t, msg)
	} else {
		ln.scanEye(t, msg)
	}

	ln.outputEyeMeasurement(t)
	return nil
}

// setErrLimit sets up the test parameters like error limit and dwell time.
func (ln *Lane) setErrLimit(t *aspect, msg *strings.Builder) error {
	var cmd cmdRsp
	cmd.rec = uint16(ln.rec)
	cmd.usage = UsageModel
	cmd.typ = MarginTypeSet
	errlimit := uint16(t.spec.GetErrorLimit())
	cmd.payload = SetErrorCountLimit | errlimit
	if err := ln.lmrCmdRspEcho(&cmd); err != nil {
		msg.WriteString(err.Error() + " | ")
		return fmt.Errorf("failed to set error count limit: %w", err)
	}

	return nil
}

// calculateDwellTime calculates the samples per second and updates the dwell time in the test spec.
func (ln *Lane) calculateDwellTime(t *aspect) {
	// Assumes max sampling rate if it reads 0, and independent error sampler is
	// not supported or reporting method is count not rate.
	if t.rate == 0 && (!ln.param.GetIndErrorSampler() || !ln.param.GetSampleReportingMethod()) {
		t.rate = 63
	}
	// Calculates minimum dwell time based on bits to be sampled.
	// Refers to PCIe 5.0 spec 8.4.4: SampleCount = 3*log 2 (number of bits)
	t.bitCount = math.Pow(2.0, float64(t.spec.GetSamples())/3.0)
	// Samples per second. t.rate is define as the # of bits checked out of 64 bits, - 1.
	t.sps = (float64(t.rate+1) / 64.0) * ln.speed
	// Expected dwell is bitCount / sps
	t.dwell = time.Duration(t.bitCount / t.sps * float64(time.Second))

	log.V(1).Infof("Lane %d: Sample bit count = %f, SPS = %f, Calculated dwell = %s",
		ln.laneNumber, t.bitCount, t.sps, t.dwell)

	// If greater dwell is specified, use that.
	if t.spec.Dwell == nil || t.dwell > time.Duration(float64(*t.spec.Dwell)*float64(time.Second)) {
		dwellSeconds := float32(t.dwell.Seconds())
		t.spec.Dwell = &dwellSeconds
		log.V(1).Infof("Lane %d: Using calculated dwell: %f seconds", ln.laneNumber, dwellSeconds)
	} else {
		log.V(1).Infof("Lane %d: Using specified dwell: %f seconds", ln.laneNumber, *t.spec.Dwell)
	}
}

// determineMarginRange sets the starting, target, and step for margining.
func (ln *Lane) determineMarginRange(t *aspect) {
	t.eyeSizeMode = false
	t.targetMode = false
	t.eyeScanMode = false
	t.untilFail = false
	t.errOutOK = false
	t.target = 0
	t.start = 0
	t.step = 1

	if t.spec.Step != nil {
		t.step = uint16(t.spec.GetStep())
		if t.step == 0 {
			log.Warningf("Lane %d: step cannot be 0, adjusting to 1", ln.laneNumber)
			t.step = 1
		}
	}

	startWithoutTarget := t.spec.TargetOffset == nil && t.spec.StartOffset != nil
	targetSmallerThanEye := t.spec.TargetOffset == nil ||
		(float32(t.spec.GetTargetOffset())*2.0*t.maxOffset < t.spec.GetEyeSize()*float32(t.steps))

	// eyeSizeMode when the eye size is specified and is greater than the target offset if specified,
	// except StartOffset without TargetOffset.
	if t.spec.EyeSize != nil && !startWithoutTarget && targetSmallerThanEye {
		t.eyeSizeMode = true
		t.errOutOK = true
		t.start = uint16(math.Ceil(float64(t.spec.GetEyeSize() / t.maxOffset * float32(t.steps))))
		if t.spec.TargetOffset != nil {
			t.target = uint16(t.spec.GetTargetOffset())
		} else {
			t.target = 0
		}
		log.V(1).Infof("Lane %d: EyeSizeMode: size=%d steps, target=%d, step=%d", ln.laneNumber, t.start, t.target, t.step)
		return
	}

	if t.spec.TargetOffset != nil {
		// Set the target max offset to be no greater than the Lane's capability.
		if t.spec.GetTargetOffset() > t.steps {
			log.Warningf("Lane %d: target_offset %d exceeds capability %d, adjusting", ln.laneNumber, *t.spec.TargetOffset, t.steps)
			*t.spec.TargetOffset = t.steps
		}
		t.target = uint16(t.spec.GetTargetOffset())
		t.targetMode = t.spec.StartOffset == nil
	} else if t.spec.StartOffset != nil {
		// Explore until fail mode
		t.untilFail = true
		t.target = uint16(t.steps)
	}

	if t.spec.StartOffset != nil {
		t.eyeScanMode = true
		t.errOutOK = true
		t.start = uint16(*t.spec.StartOffset)
		// Step, if specified, must be no greater than the target.
		if t.start > t.target {
			log.Warningf("Lane %d: start_offset %d > target_offset %d, adjusting start = target", ln.laneNumber, t.start, t.target)
			t.start = t.target
		}
	} else {
		t.start = t.target
	}
	log.V(1).Infof("Lane %d: Margin range: start=%d, target=%d, step=%d, untilFail=%v", ln.laneNumber, t.start, t.target, t.step, t.untilFail)
}

// setupLaneValidators sets up OCP validators for status and BER.
func (ln *Lane) setupLaneValidators(t *aspect) {
	ln.statusVal = &ocppb.Validator{
		Name: "Status Check",
		Type: ocppb.Validator_IN_SET,
	}
	var expectedStatus []any
	if t.errOutOK {
		expectedStatus = []any{
			strings.TrimPrefix(lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING.String(), "S_"),
			strings.TrimPrefix(lmtpb.LinkMargin_Lane_MarginPoint_S_ERROR_OUT.String(), "S_"),
		}
	} else {
		expectedStatus = []any{
			strings.TrimPrefix(lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING.String(), "S_"),
		}
	}
	lv, _ := structpb.NewList(expectedStatus)
	ln.statusVal.Value = structpb.NewListValue(lv)

	// BER threshold is calculated as the error_limit / (dwell_time * sample_rate).
	// Additional 0.5 is added to the error_limit to avoid false failure
	// when the actual error count is the same as the error_limit,
	// while the berThresh is text-truncated in meltan artifact.
	errlimit := uint16(t.spec.GetErrorLimit())
	t.berThresh = (float64(errlimit) + 0.5) / (float64(*t.spec.Dwell) * t.sps)
	ln.berVal = &ocppb.Validator{
		Name:  "Max BER Check",
		Type:  ocppb.Validator_LESS_THAN_OR_EQUAL,
		Value: structpb.NewNumberValue(t.berThresh),
	}
}

// scanEye iterates through margin offsets to find the pass/fail boundaries.
func (ln *Lane) scanEye(t *aspect, msg *strings.Builder) {
	var err error
	for offset := t.start; ; {
		// Steps until either target offset is reached or neither side is passing
		passPos := false
		passNeg := false
		var mp *lmtpb.LinkMargin_Lane_MarginPoint

		// If independent error sampler is not supported and the positive offset is already failed, stop
		// margining the positive offset, because too many errors may break the link.
		if ln.param.IndErrorSampler || t.mp[pos][fail] == nil {
			if mp, err = ln.margin(offset, t); err != nil {
				msg.WriteString(err.Error() + " | ")
			}

			passPos = mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING
			if passPos {
				if t.mp[pos][fail] == nil { // if not yet found a fail point, store the pass point
					t.mp[pos][pass] = mp
				}
			} else {
				if t.mp[pos][fail] == nil {
					t.mp[pos][fail] = mp
				}
			}
		}

		passNeg = passPos
		// if independent left/right or up/down, tests the negative side.
		if t.indDir {
			// If independent error sampler is not supported and the negative offset is already failed, stop
			// margining the negative offset, because too many errors may break the link.
			if ln.param.IndErrorSampler || t.mp[neg][fail] == nil {

				if mp, err = ln.margin(offset|t.dirmask, t); err != nil {
					msg.WriteString(err.Error() + " | ")
				}

				passNeg = mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING
			}
		}
		if passNeg {
			if t.mp[neg][fail] == nil { // if not yet found a fail point, store the pass point
				t.mp[neg][pass] = mp
			}
		} else {
			if t.mp[neg][fail] == nil {
				t.mp[neg][fail] = mp
			}
		}

		if offset >= t.target {
			break
		} else if t.untilFail && !passPos && !passNeg {
			break
		} else {
			offset += uint16(t.step)
			// Makes sure target is margined in the end.
			if offset > t.target {
				offset = t.target
			}
		}
	}
}

// testEyeSize checks if the eye size meets the spec even if it's off-centered.
func (ln *Lane) testEyeSize(t *aspect, msg *strings.Builder) {
	var err error

	passPos := false
	passNeg := false

	var mp *lmtpb.LinkMargin_Lane_MarginPoint
	offset := t.start / 2 // Under the checkEyeSize mode, start is the required eye size in steps.

RedoPos: // Redo the positive offset if the eye center is more positive.
	for offset = t.start - offset; ; { // Start from positive half of the eye size rounding up.
		// margin at offset downwards towards the positive target, or if passing.
		if mp, err = ln.margin(offset, t); err != nil {
			msg.WriteString(err.Error() + " | ")
		}
		passPos = mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING

		// If independent left/right or up/down not supported, the test is done, as no off-centeredness.
		if !t.indDir {
			passNeg = passPos
			if passPos {
				t.mp[pos][pass] = mp
				t.mp[neg][pass] = mp
			} else {
				t.mp[pos][fail] = mp
				t.mp[neg][fail] = mp
			}
			return
		}

		if passPos {
			t.mp[pos][pass] = mp
			break // move to the negative half if the positive passing offset is found.
		}

		t.mp[pos][fail] = mp // record the smallest failing offset.

		if offset <= t.target {
			break // when all failing, end the scan at the target offset.
		}
		offset -= uint16(t.step)
		if offset < t.target {
			offset = t.target // when all failing, always test the target offset.
		}
	}

	if passNeg { // Only when RedoPos, passNeg is true.
		return
	}

	// Even when the positive target fails, still get some measurement on the negative side, even
	// though the eye size check already failed.
	for offset = t.start - offset; ; { // Start from eye_size - positive passing offset.
		// margin at negative offset upwards towards the negative target, or if passing.
		if mp, err = ln.margin(offset|t.dirmask, t); err != nil {
			msg.WriteString(err.Error() + " | ")
		}
		passNeg = mp.GetStatus() == lmtpb.LinkMargin_Lane_MarginPoint_S_MARGINING

		if passNeg {
			t.mp[neg][pass] = mp
			break
		}

		t.mp[neg][fail] = mp // record the smallest failing offset.

		if offset <= t.target {
			break // when all failing, end the scan at the target offset.
		}
		offset -= uint16(t.step)
		if offset < t.target {
			offset = t.target // when all failing, always test the target offset.
		}
	}

	// If positive scan never fails, and the negative scan is smaller the eye_size/2,
	// the eye center may be more positive. Redo the positive scan starting from bigger offset.
	if passPos && passNeg && t.mp[pos][fail] == nil && t.mp[neg][fail] != nil {
		goto RedoPos
	}
}

// outputEyeMeasurement processes margin results, printing to console and generating OCP artifacts.
func (ln *Lane) outputEyeMeasurement(t *aspect) {
	m := &ocppb.Measurement{HardwareInfoId: ln.rx.hwinfo}
	ln.mStepArti.Artifact = &ocppb.TestStepArtifact_Measurement{Measurement: m}

	vt := 1
	if t.VnotT {
		vt = 0
	}

	MeasDir := [2][2]string{
		{"TOP", "BOT"},
		{"RIGHT", "LEFT"},
	}
	MeasUnit := [2]string{"V", "UI"}
	MeasPF := [2]string{"MAX-PASSING", "MIN-FAILING"}

	for pn := pos; pn <= neg; pn++ { // 0: Pos, 1: Neg
		for pf := pass; pf <= fail; pf++ {
			mp := t.mp[pn][pf]
			if mp == nil {
				continue
			}

			name := fmt.Sprintf("EYE CORNER %s %-5s", MeasPF[pf], MeasDir[vt][pn])
			mp.Info = &name

			var value float64
			if t.VnotT {
				value = float64(mp.GetVoltage())
			} else {
				value = float64(mp.GetPercentUi())
			}
			fmt.Println(ln.rx.hwinfo, ";LN=", ln.laneNumber, ";", name, ":", value, MeasUnit[vt], ";Step=", mp.GetSteps())

			if t.eyeScanMode {
				m.Name = fmt.Sprintf("LN=%02d;%s-%s-%s", ln.laneNumber, MeasPF[pf], MeasUnit[vt], MeasDir[vt][pn])
				m.Unit = fmt.Sprintf("Unit=%s;BER=%.2E", MeasUnit[vt], t.berThresh)
				m.Value = structpb.NewNumberValue(value)
				m.Validators = nil // Clear validators from any previous artifact.
				outputArtifact(ln.stepArtiOut)
			}
		}
	}

	if t.eyeScanMode || t.eyeSizeMode {
		ln.outputEyeSizeArtifact(m, t)
	}
}

// outputEyeSizeArtifact streams an OCP artifact for the total eye width or height.
func (ln *Lane) outputEyeSizeArtifact(m *ocppb.Measurement, t *aspect) {
	var totalSize float32
	if t.VnotT {
		m.Name = fmt.Sprintf("LN=%02d;Eye-Height", ln.laneNumber)
		m.Unit = fmt.Sprintf("Unit=V;BER=%.2E", t.berThresh)
		if t.mp[pos][pass] != nil {
			totalSize += t.mp[pos][pass].GetVoltage()
		}
		if t.mp[neg][pass] != nil {
			totalSize += t.mp[neg][pass].GetVoltage()
		}
		ln.eyeHeight = totalSize
	} else {
		m.Name = fmt.Sprintf("LN=%02d;Eye-Width", ln.laneNumber)
		m.Unit = fmt.Sprintf("Unit=UI;BER=%.2E", t.berThresh)
		if t.mp[pos][pass] != nil {
			totalSize += t.mp[pos][pass].GetPercentUi()
		}
		if t.mp[neg][pass] != nil {
			totalSize += t.mp[neg][pass].GetPercentUi()
		}
		ln.eyeWidth = totalSize
	}

	m.Value = structpb.NewNumberValue(float64(totalSize))
	if t.spec.EyeSize != nil {
		if totalSize < t.spec.GetEyeSize() {
			ln.Pass = false
		}
		m.Validators = []*ocppb.Validator{{
			Name:  "Eye Size Check",
			Type:  ocppb.Validator_GREATER_THAN_OR_EQUAL,
			Value: structpb.NewNumberValue(float64(t.spec.GetEyeSize())),
		}}
	} else {
		m.Validators = nil
	}
	outputArtifact(ln.stepArtiOut)
}
