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

// Link-level procedures, including parallel lane margining and synchronizaiton.

/*
// The Cgo import here is only for using pciutils constants.
#include "lib/header.h"
*/
import (
	"C"
)

import (
	"fmt"
	"slices"
	"sync"

	log "github.com/golang/glog"
	"google.golang.org/protobuf/proto"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

// /////////////////////////////////////////////////////////////////////////////////////////////////
// Disclaimer: The terms here are not strictly following the PCIe terminology for legacy and
//             implementation reasons.

// marginLink conducts USP and DSP lane margining in parallel according to the TestSpecs.
func (lt *linktest) marginLink() {
	lt.prepLink()
	defer lt.restoreLink()
	var err error
	// Creates and initializes lanes under each receiver port, only when it's specified and
	// ready to be tested.
	// Collects only those lanes to be tested in an array slice.
	cfg := lt.pb
	// Reads if retimer presents
	addr := lt.dsp.pcieCapOffset + C.PCI_EXP_LNKSTA2
	val := pci.ReadWord(lt.dsp.dev, addr)
	retimer0 := (val & C.PCI_EXP_LINKSTA2_RETIMER) != 0
	retimer1 := (val & C.PCI_EXP_LINKSTA2_2RETIMERS) != 0
	// Receiver ports and lanes initialization
	for i := range lt.allRx {
		// The index corresponds to the receiver number, where 0 is for broadcasting. not an actual
		// receiver point. Only 1 to 6 are valid
		if i == int(lmtpb.LinkMargin_R_BROADCAST0) ||
			i == int(lmtpb.LinkMargin_R_RESERVED) {
			lt.allRx[i] = nil
			continue
		}
		// Skips retimer receivers if they are not detected
		if (i == int(lmtpb.LinkMargin_R_RTU_B2) ||
			i == int(lmtpb.LinkMargin_R_RTD_C3)) && !retimer0 {
			lt.allRx[i] = nil
			continue
		}
		if (i == int(lmtpb.LinkMargin_R_RTU_D4) ||
			i == int(lmtpb.LinkMargin_R_RTD_E5)) && !retimer1 {
			lt.allRx[i] = nil
			continue
		}

		lt.allRx[i] = new(receiver)
		rxpt := lt.allRx[i]
		rxpt.testReady = false
		// Converts the index to the receiver enum,
		rxpt.rec = lmtpb.LinkMargin_ReceiverEnum(i)
		// Other than the USP, all retimer and usp receivers run from the DSP port.
		if rxpt.rec == lmtpb.LinkMargin_R_USP_F6 {
			rxpt.port = lt.usp
		} else {
			rxpt.port = lt.dsp
		}
		rxpt.hwinfo = "BDF=" + rxpt.port.dev.BDFString() + ";RX=" + rxpt.rec.String()[2:]
		rxpt.linkwg = lt.wg
		rxpt.rxwg = new(sync.WaitGroup)
		rxpt.lanes = make([]*Lane, rxpt.port.width, rxpt.port.width)
		for i := range rxpt.lanes {
			rxpt.lanes[i] = new(Lane)
			rxpt.lanes[i].Init(lt.pb, rxpt.port.dev, i, rxpt.port.lmrAddr,
				rxpt.rec, rxpt.port.speed, rxpt.rxwg, rxpt.linkwg, rxpt)
		}
		// Run lanes in parallel if the receiver lane has independent error sampler.
		if rxpt.parallel, err = rxpt.lanes[0].GetIndErrorSampler(); err != nil {
			message := lt.pb.GetMessage() + err.Error() + " | "
			lt.pb.Message = &message
		}
	}

	// Enlists the test specs from config.
	for _, spec := range cfg.GetTestSpecs() {
		if spec.GetReceiver() == lmtpb.LinkMargin_R_BROADCAST0 ||
			spec.GetReceiver() == lmtpb.LinkMargin_R_RESERVED {
			log.Warningf("Illegal test_specs receiver: %s. The test_spec is ignored.",
				spec.GetReceiver().String())
			continue
		}

		rxpt := lt.allRx[spec.GetReceiver()]
		if rxpt == nil {
			log.Warningf("The test_specs receiver: %s is not present on the link. The test_spec is ignored.",
				spec.GetReceiver().String())
			continue
		}

		rxpt.testReady = true
		if spec.GetAspect() != lmtpb.LinkMargin_M_VOLTAGE &&
			spec.GetAspect() != lmtpb.LinkMargin_M_TIMING {
			log.Warningf("The test_spec is missing the aspect (T or V). The test_spec is ignored.",
				spec.GetReceiver().String())
			rxpt.testReady = false
		} else {
			for n, lane := range rxpt.lanes {
				if len(spec.GetLaneNumber()) == 0 || slices.Contains(spec.GetLaneNumber(), uint32(n)) {
					if spec.GetAspect() == lmtpb.LinkMargin_M_VOLTAGE {
						lane.Vspec = proto.Clone(spec).(*lmtpb.LinkMargin_TestSpec)
					} else {
						lane.Tspec = proto.Clone(spec).(*lmtpb.LinkMargin_TestSpec)
					}
				}
			}
		}
	}

	const numLanes = 16 // estimated array-initial-size of lanes per port.
	lanes := make([]*lmtpb.LinkMargin_Lane, 0, maxRxPerLink*numLanes)
	// Tests upstream lanes in parallel, followed by downstream lanes in parallel,
	// with wait in between to avoid pcilib sysfs error
	var wg sync.WaitGroup
	for _, r := range lt.allRx {
		if r == nil {
			continue
		} // Skips R_BROADCAST0 = 0 and R_RESERVED = 7
		if !r.testReady {
			continue
		} // Skips receivers not tested
		log.V(1).Infoln("Margining lanes at receiver: ", r.rec.String())

		// OCP TestStepStart
		stepStart := &ocppb.TestStepStart{
			Name: "LMT@" + r.hwinfo,
		}
		stepArti := &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_TestStepStart{stepStart},
			TestStepId: r.hwinfo,
		}
		outArti := &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{stepArti},
		}
		outputArtifact(outArti)

		for _, ln := range r.lanes {
			if ln.Vspec == nil && ln.Tspec == nil {
				continue
			}
			// If runs in series, waits for the previous iteration to finish.
			if !r.parallel {
				wg.Wait()
			}
			wg.Add(1)
			// Some retimer cannot handle parameter reading overlapping with margining on another lane.
			r.rxwg.Add(1)
			go func(ln *Lane) {
				defer wg.Done()
				ln.MarginLane()
			}(ln)
		}
		wg.Wait()

		// Gather result protobuf
		lncnt := 0
		failcnt := 0
		for _, ln := range r.lanes {
			if ln.Vspec != nil || ln.Tspec != nil {
				if lanepb := ln.GatherResult(); lanepb != nil {
					lanes = append(lanes, lanepb)
				}
			}
			lncnt++
			if !ln.Pass {
				failcnt++
			}
		}

		diag := &ocppb.Diagnosis{
			Type:           ocppb.Diagnosis_UNKNOWN,
			HardwareInfoId: r.hwinfo,
		}
		if lncnt == 0 {
			diag.Verdict = "pcie_lmt-rx_ln-unknown"
			diag.Message = "0 Rx-lane tested."
		} else if failcnt == 0 {
			diag.Type = ocppb.Diagnosis_PASS
			diag.Verdict = "pcie_lmt-rx_ln-pass"
			diag.Message = fmt.Sprintf("%d Rx-lane tested. All passed.", lncnt)
		} else {
			diag.Type = ocppb.Diagnosis_FAIL
			diag.Verdict = "pcie_lmt-rx_ln-fail"
			diag.Message = fmt.Sprintf("%d Rx-lane tested; %d failed.", lncnt, failcnt)
		}

		stepArti = &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_Diagnosis{diag},
			TestStepId: r.hwinfo,
		}
		outArti = &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{stepArti},
		}
		outputArtifact(outArti)

		// OCP TestStepEnd
		stepEnd := &ocppb.TestStepEnd{
			Status: ocppb.TestRunEnd_COMPLETE,
		}
		stepArti = &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_TestStepEnd{stepEnd},
			TestStepId: r.hwinfo,
		}
		outArti = &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{stepArti},
		}
		outputArtifact(outArti)
	}

	lt.pb.ReceiverLanes = lanes
}

// GatherResult stuffs test results into proto messages.
func (ln *Lane) GatherResult() *lmtpb.LinkMargin_Lane {
	if ln.lane == nil {
		return nil
	}
	ln.lane.LaneNumber = ln.laneNumber
	ln.lane.Receiver = ln.rec
	if ln.Tspec != nil {
		ln.lane.Tspec = ln.Tspec
		ln.lane.TimingMargins = ln.tsteps
		ln.lane.EyeWidth = &ln.eyeWidth
	}
	if ln.Vspec != nil {
		ln.lane.Vspec = ln.Vspec
		ln.lane.VoltageMargins = ln.vsteps
		ln.lane.EyeHeight = &ln.eyeHeight
	}
	ln.lane.LaneParameter = ln.param
	ln.lane.ExtraInfo = &ln.msg
	ln.lane.Pass = &ln.Pass
	return ln.lane
}

// /////////////////////////////////////////////////////////////////////////////////////////////////
// The ASPM Control field of the Link Control register must be set to 00b
// (Disabled) in both the Downstream Port and Upstream Port.
// The state of the Hardware Autonomous Speed Disable bit of the Link Control 2
// register and the Hardware Autonomous Width Disable bit of the Link Control
// register must be saved to be restored later in this procedure.
// If writeable, the Hardware Autonomous Speed Disable bit of the Link Control 2
// register must be Set in both the Downstream Port and Upstream Port.
// If writeable, the Hardware Autonomous Width Disable bit of the Link Control
// register must be Set in both the	Downstream Port and Upstream Port.

// prepLink saves the state of the Hardware Autonomous Speed Disable and
// the Hardware Autonomous Width Disable, and clears the ASPM control.
func (lt *linktest) prepLink() {
	for _, p := range [2]*port{lt.dsp, lt.usp} {
		addr := p.pcieCapOffset + C.PCI_EXP_LNKCTL
		val := pci.ReadWord(p.dev, addr)
		p.hawd = (val & C.PCI_EXP_LNKCTL_HWAUTWD) != 0
		val = val | C.PCI_EXP_LNKCTL_HWAUTWD
		val = val &^ C.PCI_EXP_LNKCTL_ASPM
		pci.WriteWord(p.dev, addr, val)

		addr = p.pcieCapOffset + C.PCI_EXP_LNKCTL2
		val = pci.ReadWord(p.dev, addr)
		p.hasd = (val & C.PCI_EXP_LNKCTL2_SPEED_DIS) != 0
		val = val | C.PCI_EXP_LNKCTL2_SPEED_DIS
		pci.WriteWord(p.dev, addr, val)
	}
}

// restoreLink restores the state of the Hardware Autonomous Speed Disable and
// the Hardware Autonomous Width Disable.
// When the margin testing procedure is completed, the state of the
// Hardware Autonomous Speed Disable bit and the Hardware Autonomous Width
// Disable bit must be restored to the previously saved values.
func (lt *linktest) restoreLink() {
	for _, p := range [2]*port{lt.dsp, lt.usp} {
		addr := p.pcieCapOffset + C.PCI_EXP_LNKCTL
		val := pci.ReadWord(p.dev, addr)
		if p.hawd {
			val = val | C.PCI_EXP_LNKCTL_HWAUTWD
		} else {
			val = val &^ C.PCI_EXP_LNKCTL_HWAUTWD
		}
		pci.WriteWord(p.dev, addr, val)

		addr = p.pcieCapOffset + C.PCI_EXP_LNKCTL2
		val = pci.ReadWord(p.dev, addr)
		if p.hawd {
			val = val | C.PCI_EXP_LNKCTL2_SPEED_DIS
		} else {
			val = val &^ C.PCI_EXP_LNKCTL2_SPEED_DIS
		}
		pci.WriteWord(p.dev, addr, val)
	}
}
