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

package linktrain

/*
 #include <stdlib.h>
 #include "lib/pci.h"
 #include "lib/header.h"
*/
import "C"

import (
	"fmt"
	"strings"
	"time"

	log "github.com/golang/glog"
)

const (
	// lnkdownUspLnkSta is the value we will get from the Upstream Port (Device Side) Link Status
	// Register when the device is unreachable.
	lnkdownUspLnkSta = uint16(0xffff)
)

var (
	// Polling timeouts and intervals for link training. Declared as vars so unit tests can override
	// timeout and delay durations deterministically.
	linkStatusPollInterval = 1 * time.Millisecond
	linkUpTimeout          = 5 * time.Second
	linkUpStableDelay      = 100 * time.Millisecond
	linkDownTimeout        = 1 * time.Second
)

// linkUpState holds the PCIe config register values read during link-up polling.
type linkUpState struct {
	dsplnksta uint16
	usplnksta uint16
}

// linkDisableState holds the PCIe config register values read during link-disable polling.
type linkDisableState struct {
	dsplnkctl uint16 // Downstream Port (CPU Side) Link Control Register
	dsplnksta uint16 // Downstream Port (CPU Side) Link Status Register
	usplnksta uint16 // Upstream Port (Device Side) Link Status Register
}

func (s linkUpState) String() string {
	return fmt.Sprintf("{dsplnksta: 0x%04x, usplnksta: 0x%04x}", s.dsplnksta, s.usplnksta)
}

func (s linkDisableState) String() string {
	return fmt.Sprintf("{dsplnkctl: 0x%04x, dsplnksta: 0x%04x, usplnksta: 0x%04x}",
		s.dsplnkctl, s.dsplnksta, s.usplnksta)
}

// pollUntil polls PCI configuration registers via readWords, until the condition in until is met,
// or the timeout is reached. The until callback returns an empty string when the condition is met,
// or a non-empty "notFinishedReason" describing why the condition is not yet met.
// On timeout, the last notFinishedReason is returned.
func pollUntil[T any](lt *Linktest, readWords func() T, until func(T) string, timeout time.Duration, pollInterval time.Duration) (notFinishedReason string) {
	startTime := time.Now()
	for {
		words := readWords()
		notFinishedReason = until(words)
		if notFinishedReason == "" {
			log.V(2).Infof("BDF:[%s]: Polling finished, lastRead: %+v", lt.Cfg.GetUspBdf(), words)
			return ""
		}
		if time.Since(startTime) > timeout {
			log.Warningf("BDF:[%s]: Polling timeout after %v: %s", lt.Cfg.GetUspBdf(), timeout, notFinishedReason)
			return notFinishedReason
		}
		time.Sleep(pollInterval)
	}
}

func (lt *Linktest) getLinkUpState() linkUpState {
	dspLnksta := pciReadWord(lt.Dsp, lt.dspPCIeCapOffset+C.PCI_EXP_LNKSTA)
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkUp: read dsplnksta=0x%04x", lt.Cfg.GetUspBdf(), dspLnksta)
	uspLnksta := pciReadWord(lt.Usp, lt.uspPCIeCapOffset+C.PCI_EXP_LNKSTA)
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkUp: read usplnksta=0x%04x", lt.Cfg.GetUspBdf(), uspLnksta)
	return linkUpState{
		dsplnksta: dspLnksta,
		usplnksta: uspLnksta,
	}
}

// linkNotUpReasons returns the individual reasons why the link is not yet up, or nil if it is up.
func linkNotUpReasons(s linkUpState, preResetSpeed uint16) []string {
	trainingBit := (s.dsplnksta & C.PCI_EXP_LNKSTA_TRAIN) != 0
	dlActBit := (s.dsplnksta & C.PCI_EXP_LNKSTA_DL_ACT) != 0
	dspCurrentSpeed := s.dsplnksta & C.PCI_EXP_LNKSTA_SPEED
	uspCurrentSpeed := s.usplnksta & C.PCI_EXP_LNKSTA_SPEED

	var reasons []string
	if trainingBit {
		reasons = append(reasons, "stuck in training")
	}
	if !dlActBit {
		reasons = append(reasons, "data link not active")
	}
	if s.usplnksta == lnkdownUspLnkSta {
		reasons = append(reasons, fmt.Sprintf("USP Link Status is 0x%04x", s.usplnksta))
	} else {
		// Only check link speed when USP link status is meaningful (i.e. not 0xffff).
		if dspCurrentSpeed < preResetSpeed {
			reasons = append(reasons, fmt.Sprintf(
				"speed downgrade (0x%x < 0x%x)", dspCurrentSpeed, preResetSpeed))
		}
		if uspCurrentSpeed < preResetSpeed {
			reasons = append(reasons, fmt.Sprintf(
				"USP speed downgrade (0x%x < 0x%x)", uspCurrentSpeed, preResetSpeed))
		}
	}
	return reasons
}

// linkNotUpReason returns an empty string if the link is up.
// Otherwise, returns a non-empty string describing why the link is still not up.
func linkNotUpReason(s linkUpState, preResetSpeed uint16, uspBdf string) string {
	reasons := linkNotUpReasons(s, preResetSpeed)
	if len(reasons) == 0 {
		return ""
	}
	ret := fmt.Sprintf("%s (dsplnksta: 0x%04x, usplnksta: 0x%04x)",
		strings.Join(reasons, ", "), s.dsplnksta, s.usplnksta)
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkUp still not finished: %s", uspBdf, ret)
	return ret
}

// pollNotTraining waits until the Downstream Port Link Training bit (PCI_EXP_LNKSTA_TRAIN) is 0b
// before software writes Perform Equalization or Retrain Link (PCIe Base Spec Sec 7.5.3.7).
func (lt *Linktest) pollNotTraining() error {
	dspLnkstaAddr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKSTA
	readWords := func() uint16 {
		return pciReadWord(lt.Dsp, dspLnkstaAddr)
	}
	notTraining := func(dsplnksta uint16) string {
		if (dsplnksta & C.PCI_EXP_LNKSTA_TRAIN) != 0 {
			return fmt.Sprintf("stuck in training before retrain (dsplnksta: 0x%04x)", dsplnksta)
		}
		return ""
	}
	if reason := pollUntil(lt, readWords, notTraining, linkUpTimeout, linkStatusPollInterval); reason != "" {
		return &TrainError{Code: ErrLinkUpTimeout, Message: reason}
	}
	return nil
}

// pollRetrainComplete polls until a Link Retrain (with or without Equalization) has transitioned
// through Recovery and returned to L0 without requiring a post-DL_Active reset settle sleep:
//   - PCI_EXP_LNKSTA_BWMGMT == 1b, proving hardware completed the requested Link Retrain Recovery
//     cycle rather than returning a stale pre-Recovery L0 read.
//   - PCI_EXP_LNKSTA_TRAIN == 0b, PCI_EXP_LNKSTA_DL_ACT == 1b, USP Link Status != 0xffff, and both
//     DSP and USP Current Link Speeds >= preResetSpeed.
func (lt *Linktest) pollRetrainComplete(preResetSpeed uint16) error {
	readWords := func() linkUpState {
		return lt.getLinkUpState()
	}
	retrainDone := func(s linkUpState) string {
		var reasons []string
		if (s.dsplnksta & C.PCI_EXP_LNKSTA_BWMGMT) == 0 {
			reasons = append(reasons, "retrain recovery not yet completed (BWMGMT=0)")
		}
		reasons = append(reasons, linkNotUpReasons(s, preResetSpeed)...)
		if len(reasons) == 0 {
			return ""
		}
		ret := fmt.Sprintf("%s (dsplnksta: 0x%04x, usplnksta: 0x%04x)",
			strings.Join(reasons, ", "), s.dsplnksta, s.usplnksta)
		log.V(3).Infof("BDF:[%s]: pollRetrainComplete still not finished: %s", lt.Cfg.GetUspBdf(), ret)
		return ret
	}
	if reason := pollUntil(lt, readWords, retrainDone, linkUpTimeout, linkStatusPollInterval); reason != "" {
		return &TrainError{Code: ErrLinkUpTimeout, Message: reason}
	}
	return nil
}

// pollLinkUp polls the Link Training, Data Link Layer Link Active, and Current Link Speed bits in
// the Link Status Register to determine if the link is up, then waits linkUpStableDelay (100ms per
// PCIe Base Spec Sec 6.6.1 post-DL_Active settle after DL_Down -> DL_Active link re-enable).
// It returns nil if the link comes up at or above preResetSpeed within the timeout, and
// a TrainError otherwise.
func (lt *Linktest) pollLinkUp(preResetSpeed uint16) error {
	readWords := func() linkUpState {
		return lt.getLinkUpState()
	}
	linkUp := func(s linkUpState) string {
		return linkNotUpReason(s, preResetSpeed, lt.Cfg.GetUspBdf())
	}
	if reason := pollUntil(lt, readWords, linkUp, linkUpTimeout, linkStatusPollInterval); reason != "" {
		return &TrainError{Code: ErrLinkUpTimeout, Message: reason}
	}
	// Wait for a short time to ensure the link is stable.
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkUp: sleep for %v", lt.Cfg.GetUspBdf(), linkUpStableDelay)
	time.Sleep(linkUpStableDelay)
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkUp: finished", lt.Cfg.GetUspBdf())
	return nil
}

// pollLinkDisabled polls the following conditions to determine if the link is down:
//   - DSP Link Control Register: Disable bit is 1.
//   - DSP Link Status Register: Data Link Layer Link Active bit is 0.
//   - USP Link Status Register: Link Status is 0xffff.
func (lt *Linktest) pollLinkDisabled() error {
	dspLnkctlAddr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKCTL
	dspLnkstaAddr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKSTA
	uspLnkstaAddr := lt.uspPCIeCapOffset + C.PCI_EXP_LNKSTA
	readWords := func() linkDisableState {
		dsplnkctl := pciReadWord(lt.Dsp, dspLnkctlAddr)
		// Debug only log (v=3).
		log.V(3).Infof("BDF:[%s]: pollLinkDisabled: read dsplnkctl=0x%04x", lt.Cfg.GetUspBdf(), dsplnkctl)
		dsplnksta := pciReadWord(lt.Dsp, dspLnkstaAddr)
		// Debug only log (v=3).
		log.V(3).Infof("BDF:[%s]: pollLinkDisabled: read dsplnksta=0x%04x", lt.Cfg.GetUspBdf(), dsplnksta)
		usplnksta := pciReadWord(lt.Usp, uspLnkstaAddr)
		// Debug only log (v=3).
		log.V(3).Infof("BDF:[%s]: pollLinkDisabled: read usplnksta=0x%04x", lt.Cfg.GetUspBdf(), usplnksta)
		return linkDisableState{
			dsplnkctl: dsplnkctl,
			dsplnksta: dsplnksta,
			usplnksta: usplnksta,
		}
	}
	linkDown := func(s linkDisableState) string {
		disableBit := (s.dsplnkctl & C.PCI_EXP_LNKCTL_DISABLE) != 0
		dlActBit := (s.dsplnksta & C.PCI_EXP_LNKSTA_DL_ACT) != 0

		var reasons []string
		if !disableBit {
			reasons = append(reasons, "disable bit not set")
		}
		if dlActBit {
			reasons = append(reasons, "data link still active")
		}
		if s.usplnksta != lnkdownUspLnkSta {
			reasons = append(reasons, fmt.Sprintf("USP Link Status is not 0x%04x (got: 0x%04x)",
				lnkdownUspLnkSta, s.usplnksta))
		}
		if len(reasons) == 0 {
			return ""
		}
		ret := fmt.Sprintf("%s (dsplnkctl: 0x%04x, dsplnksta: 0x%04x, usplnksta: 0x%04x)",
			strings.Join(reasons, ", "), s.dsplnkctl, s.dsplnksta, s.usplnksta)
		// Debug only log (v=3).
		log.V(3).Infof("BDF:[%s]: pollLinkDisabled still not finished: %s", lt.Cfg.GetUspBdf(), ret)
		return ret
	}
	if reason := pollUntil(lt, readWords, linkDown, linkDownTimeout, linkStatusPollInterval); reason != "" {
		return &TrainError{Code: ErrDisableTimeout, Message: reason}
	}
	// Even SW sees the link is down, it still take some time for HW to destruct internal resources
	// and stabilize at Gen 1 speed at Detect.
	// Will need to sleep for a short time to let HW stabilize.
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkDisabled: hold for %v", lt.Cfg.GetUspBdf(), lt.reenableWaitAfterDisable)
	time.Sleep(lt.reenableWaitAfterDisable)
	// Debug only log (v=3).
	log.V(3).Infof("BDF:[%s]: pollLinkDisabled: finished", lt.Cfg.GetUspBdf())
	return nil
}
