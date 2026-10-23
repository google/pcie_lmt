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

// Package linktrain is a PCIe link Training Test
// This test conducts repeated PCIe link reset on specified End Points,
// and checks/logs specified PCI config registers.
// The upstream port (USP) and config registers are specified in a linktrain.proto
// The results are also logged as a linktrain.proto per USP.
// Currently this test only supports Secondary Bus Reset (Hot Reset).
// //third_party/pciutils:libpci is used for PCI config access.
package linktrain

/*
 #include <stdlib.h>
 #include "lib/pci.h"
 #include "lib/header.h"
*/
import (
	"C"
)

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	pci "pciutils"

	structpb "google.golang.org/protobuf/types/known/structpb"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
	ocppb "ocpdiag/results_go_proto"
	pb "ltt_go.proto"
)

const (
	numLinks = 8 // Estimated number of links to be tested. 8 is usually enough.

	// Wait time related constants.

	defaultReenableWaitAfterDisable time.Duration = 150 * time.Millisecond
	defaultSbrWaitTrainTime         time.Duration = 2000 * time.Millisecond

	// PCIe extended capability traversal constants.

	// PcieExtCapStart is the starting offset of the PCIe Extended Capabilities in Config Space.
	//
	// See PCIe Spec 6.4 Ch. 7.6.1 Extended Capabilities in Configuration Space.
	PcieExtCapStart    = 0x100
	configSpaceSize    = 0x100
	extConfigSpaceSize = 0x1000
)

var (
	// ocpPipe points to the ocp artifact streaming pipe shared by all lm go routines.
	ocpPipe     *os.File
	ocpPipeLock sync.Mutex
	// seqNum is a shared monotonically increasing counter for determining if any artifact is lost.
	seqNum atomic.Int32
	// testRunStart is the OCP starting message
	testRunStart *ocppb.TestRunStart
	// testRunEnd tracks the OCP TestStatus and TestResult
	testRunEnd *ocppb.TestRunEnd

	// linktrain pbtxt filename
	cfgfn string
	// Lts is link training test results
	Lts []*Linktest
	// Teardownwait is the wait time before restarting the link training.
	// The default 200ms is used by the AMDXIO tool.
	Teardownwait = 200 * time.Millisecond

	pciScanDevices = pci.ScanDevices
)

// OcpInit initializes the OCP output headers.
func OcpInit(f *os.File, name string, version string, cmdline string) {
	ocpPipe = f
	testRunStart = &ocppb.TestRunStart{
		Name:        name,
		Version:     version,
		CommandLine: cmdline,
	}
	// Initialize the OCP output artifact's sequence number. Default is 0.
	seqNum.Store(int32(0)) // 0  because of the atomicity of the counter.Add(1)
}

// outputArtifact streams artiOut to the OcpPipe.
func outputArtifact(artiOut *ocppb.OutputArtifact) {
	artiOut.SequenceNumber = int32(seqNum.Add(1))
	artiOut.Timestamp = timestamppb.Now()
	opt := &protojson.MarshalOptions{
		UseProtoNames:   false,
		UseEnumNumbers:  false,
		EmitUnpopulated: false,
		Multiline:       false,
		Indent:          "",
		AllowPartial:    false,
	}
	if data, err := opt.Marshal(artiOut); err != nil {
		log.Errorf("pbj.Marshal(%v) failed: %v", data, err)
	} else {
		ocpPipeLock.Lock()
		ocpPipe.Write(data)
		ocpPipe.WriteString("\n")
		ocpPipeLock.Unlock()
	}
}

// A Linktest has everything needed to test a link.
type Linktest struct {
	Usp, Dsp         pci.Dev
	uspPCIeCapOffset int32
	dspPCIeCapOffset int32

	// Wait time related settings.
	reenableWaitAfterDisable time.Duration
	sbrWaitTrainTime         time.Duration // Wait time for SBR to complete training.

	info      string
	Cfg       *pb.LinkTrain
	chks      []*pb.LinkTrain_PciConfigField
	logs      []*pb.LinkTrain_PciConfigField
	recs      []*pb.LinkTrain_PciConfigField
	Pass      bool
	hwinfo    string // OCP hardware_info_id
	seriesID  []string
	seriesCnt []int32
}

// //////////////////////////////////////////////////////////////////////////////
// retrain issues a Link Retrain. It returns true if the link comes up within the timeout, and
// false otherwise.
func (lt *Linktest) retrain() error {
	dsp := lt.Dsp
	addr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKCTL
	preResetSpeed := lt.dspLinkSpeed()
	val := pci.ReadWord(dsp, addr)
	pci.WriteWord(dsp, addr, val|C.PCI_EXP_LNKCTL_RETRAIN)
	return lt.pollLinkUp(preResetSpeed)
}

// retrainWithEQ sets the Perform Equalization bit in the Link Control 3 Register,
// then issues a Link Retrain. It returns true if the link comes up within the timeout, and
// false otherwise.
func (lt *Linktest) retrainWithEQ() error {
	dsp := lt.Dsp

	// Find the Secondary PCI Express Extended Capability.
	secOffset, err := pci.FindCapability(dsp, uint16(C.PCI_EXT_CAP_ID_SECPCI), uint32(C.PCI_CAP_EXTENDED))
	if err != nil {
		log.Errorf("BDF:[%s]: failed to find Secondary PCIe Extended Cap: %v", dsp.BDFString(), err)
		return &TrainError{Code: ErrSecPCIeExtCapNotFound, Message: err.Error()}
	}

	// Read Link Control 3 Register.
	lnkctl3Addr := secOffset + C.PCI_SEC_LNKCTL3
	lnkctl3Val := pci.ReadWord(dsp, lnkctl3Addr)

	// Set Perform Equalization bit (Bit 0).
	pci.WriteWord(dsp, lnkctl3Addr, lnkctl3Val|C.PCI_SEC_LNKCTL3_PERFORM_LINK_EQU)
	log.V(2).Infof("BDF:[%s]: Set Perform Equalization bit (addr 0x%x, val 0x%x)", dsp.BDFString(), lnkctl3Addr, lnkctl3Val|C.PCI_SEC_LNKCTL3_PERFORM_LINK_EQU)

	// Perform standard retrain by calling retrain().
	return lt.retrain()
}

// secondaryBusReset issues a Secondary Bus Reset on a link. It returns true if the link comes up
// within the timeout, and false otherwise.
func (lt *Linktest) secondaryBusReset() error {
	// Record the original state so it can be checked and restored later.
	uspPresetSpeed, err := lt.uspLinkSpeed()
	if err != nil {
		log.Errorf("BDF:[%s]: Failed to read USP link speed, skip this iteration: %v",
			lt.Cfg.GetUspBdf(), err)
		return err
	}
	dspPresetSpeed := lt.dspLinkSpeed()
	if dspPresetSpeed != uspPresetSpeed {
		log.Errorf("BDF:[%s]: Link speed mismatch, skip this iteration (dsp: 0x%x, usp: 0x%x)",
			lt.Cfg.GetUspBdf(), dspPresetSpeed, uspPresetSpeed)
		return &TrainError{
			Code: ErrDspUspSpeedMismatch,
			Message: fmt.Sprintf(
				"Link speed mismatch (dsp: 0x%x, usp: 0x%x)", dspPresetSpeed, uspPresetSpeed),
		}
	}
	origDspBridgeCtrl := pci.ReadWord(lt.Dsp, C.PCI_BRIDGE_CONTROL)
	if origDspBridgeCtrl&C.PCI_BRIDGE_CTL_BUS_RESET != 0 {
		log.Warningf("BDF:[%s]: SBR bit is already set (origDspBridgeCtrl: 0x%x)",
			lt.Cfg.GetUspBdf(), origDspBridgeCtrl)
	}

	// Sets and clears the Secondary Bus Reset bit.
	log.V(2).Infof("BDF:[%s]: Write SBR=1 to PCI_BRIDGE_CONTROL", lt.Cfg.GetUspBdf())
	pci.WriteWord(lt.Dsp, C.PCI_BRIDGE_CONTROL, origDspBridgeCtrl|C.PCI_BRIDGE_CTL_BUS_RESET)
	newDspBridgeCtrl := pci.ReadWord(lt.Dsp, C.PCI_BRIDGE_CONTROL) // Read-after-write to ensure effective write.
	if newDspBridgeCtrl&C.PCI_BRIDGE_CTL_BUS_RESET == 0 {
		log.Errorf("BDF:[%s]: Failed to set SBR bit (newDspBridgeCtrl: 0x%x, origDspBridgeCtrl: 0x%x)",
			lt.Cfg.GetUspBdf(), newDspBridgeCtrl, origDspBridgeCtrl)
		return &TrainError{
			Code: ErrSetSBRFailed,
			Message: fmt.Sprintf(
				"Failed to set SBR bit (newDspBridgeCtrl: 0x%x, origDspBridgeCtrl: 0x%x)",
				newDspBridgeCtrl, origDspBridgeCtrl),
		}
	}
	log.V(2).Infof("BDF:[%s]: Finished setting SBR bit (newDspBridgeCtrl: 0x%x, origDspBridgeCtrl: 0x%x)",
		lt.Cfg.GetUspBdf(), newDspBridgeCtrl, origDspBridgeCtrl)

	// Wait for a teardown time and clears the Secondary Bus Reset bit.
	time.Sleep(Teardownwait)
	pci.WriteWord(lt.Dsp, C.PCI_BRIDGE_CONTROL, origDspBridgeCtrl&^C.PCI_BRIDGE_CTL_BUS_RESET)
	newDspBridgeCtrl = pci.ReadWord(lt.Dsp, C.PCI_BRIDGE_CONTROL) // Read-after-write to ensure effective write.
	if newDspBridgeCtrl&C.PCI_BRIDGE_CTL_BUS_RESET != 0 {
		log.Errorf("BDF:[%s]: Failed to clear SBR bit (newDspBridgeCtrl: 0x%x, origDspBridgeCtrl: 0x%x)",
			lt.Cfg.GetUspBdf(), newDspBridgeCtrl, origDspBridgeCtrl)
		return &TrainError{
			Code: ErrSetSBRFailed,
			Message: fmt.Sprintf(
				"Failed to clear SBR bit (newDspBridgeCtrl: 0x%x, origDspBridgeCtrl: 0x%x)",
				newDspBridgeCtrl, origDspBridgeCtrl),
		}
	}
	log.V(2).Infof("BDF:[%s]: Finished clearing SBR bit (newDspBridgeCtrl: 0x%x, origDspBridgeCtrl: 0x%x)",
		lt.Cfg.GetUspBdf(), newDspBridgeCtrl, origDspBridgeCtrl)

	// Wait for the link to come up.
	time.Sleep(lt.sbrWaitTrainTime)

	// Check if the link is already up.
	linkUpState := lt.getLinkUpState()
	if linkUpState.usplnksta == lnkdownUspLnkSta {
		return &TrainError{
			Code:    ErrLinkDown,
			Message: fmt.Sprintf("USP responds link status 0x%04x after SBR", linkUpState.usplnksta),
		}
	}
	notUpReason := linkNotUpReason(linkUpState, uspPresetSpeed, lt.Cfg.GetUspBdf())
	if notUpReason != "" {
		return &TrainError{
			Code:    ErrLinkUpTimeout,
			Message: fmt.Sprintf("Link is not up after SBR: %s", notUpReason),
		}
	}
	return nil
}

// reenable disables and re-enables a link. It returns true if the link is successfully disabled
// and re-enabled, and false otherwise.
func (lt *Linktest) reenable() error {
	lnkctlAddr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKCTL

	// Record the original state so it can be checked and restored later.
	uspPresetSpeed, err := lt.uspLinkSpeed()
	if err != nil {
		log.Errorf("BDF:[%s]: Failed to read USP link speed, skip this iteration: %v",
			lt.Cfg.GetUspBdf(), err)
		return err
	}
	dspPresetSpeed := lt.dspLinkSpeed()
	if dspPresetSpeed != uspPresetSpeed {
		log.Errorf("BDF:[%s]: Link speed mismatch, skip this iteration (dsp: 0x%x, usp: 0x%x)",
			lt.Cfg.GetUspBdf(), dspPresetSpeed, uspPresetSpeed)
		return &TrainError{
			Code: ErrDspUspSpeedMismatch,
			Message: fmt.Sprintf(
				"Link speed mismatch (dsp: 0x%x, usp: 0x%x)", dspPresetSpeed, uspPresetSpeed),
		}
	}
	origLnkCtl := pci.ReadWord(lt.Dsp, lnkctlAddr)
	if origLnkCtl&C.PCI_EXP_LNKCTL_DISABLE != 0 {
		log.Warningf("BDF:[%s]: Link is already disabled, (origLnkCtl: 0x%x)",
			lt.Cfg.GetUspBdf(), origLnkCtl)
	}

	// Disable the link and poll until the link is down.
	log.V(2).Infof("BDF:[%s]: Write Disable=1 to Link Control Register", lt.Cfg.GetUspBdf())
	pci.WriteWord(lt.Dsp, lnkctlAddr, origLnkCtl|C.PCI_EXP_LNKCTL_DISABLE)
	log.V(2).Infof("BDF:[%s]: Start polling until link is down", lt.Cfg.GetUspBdf())
	lnkDownErr := lt.pollLinkDisabled()
	if lnkDownErr != nil {
		log.Errorf("BDF:[%s]: Link disable failed: %v", lt.Cfg.GetUspBdf(), lnkDownErr)
	} else {
		log.V(2).Infof("BDF:[%s]: Link disabled", lt.Cfg.GetUspBdf())
	}

	// Re-enable the link and poll until the link is up.
	// This should always be done no matter if the link down was successful or not.
	log.V(2).Infof("BDF:[%s]: Write Disable=0 to Link Control Register", lt.Cfg.GetUspBdf())
	pci.WriteWord(lt.Dsp, lnkctlAddr, origLnkCtl)
	log.V(2).Infof("BDF:[%s]: Start polling until link is up", lt.Cfg.GetUspBdf())
	lnkUpErr := lt.pollLinkUp(uspPresetSpeed)
	if lnkUpErr != nil {
		log.Errorf("BDF:[%s]: Link re-enable failed (uspPresetSpeed: 0x%x): %v",
			lt.Cfg.GetUspBdf(), uspPresetSpeed, lnkUpErr)
	} else {
		log.V(2).Infof("BDF:[%s]: Link re-enabled (uspPresetSpeed: 0x%x)",
			lt.Cfg.GetUspBdf(), uspPresetSpeed)
	}

	if lnkDownErr != nil {
		return lnkDownErr
	}
	return lnkUpErr
}

// dspLinkSpeed returns the current link speed of the DSP.
func (lt *Linktest) dspLinkSpeed() uint16 {
	staVal := pci.ReadWord(lt.Dsp, lt.dspPCIeCapOffset+C.PCI_EXP_LNKSTA)
	speed := staVal & C.PCI_EXP_LNKSTA_SPEED
	return speed
}

// uspLinkSpeed returns the current link speed of the USP.
// Returns ErrLinkDown if the link is down (USP link status value is 0xffff).
func (lt *Linktest) uspLinkSpeed() (uint16, error) {
	staVal := pci.ReadWord(lt.Usp, lt.uspPCIeCapOffset+C.PCI_EXP_LNKSTA)
	if staVal == lnkdownUspLnkSta {
		return 0, &TrainError{Code: ErrLinkDown, Message: "Failed to read USP link speed due to 0xffff"}
	}
	return (staVal & C.PCI_EXP_LNKSTA_SPEED), nil
}

var (
	pciReadByte = pci.ReadByte
	pciReadWord = pci.ReadWord
	pciReadLong = pci.ReadLong
	findDSP     = (pci.Dev).FindDSP
)

func devBDF(dev pci.Dev) string {
	if !dev.Valid() {
		return "none"
	}
	return dev.BDFString()
}

// getPCIeCapOffset scans the PCI capability linked list for PCIe CAP.
// Refers to pciutils/ls-caps.c
var getPCIeCapOffset = func(dev pci.Dev) (int32, *TrainError) {
	const (
		capabilityStart = C.PCI_CAPABILITY_LIST
		capabilityMask  = 0x00FF
		addrMask        = 0x00FC
		nextPos         = 8
	)
	// Tracks if a loop occur in the linked list.
	var been [configSpaceSize]bool
	for addr := int32(pciReadByte(dev, capabilityStart)); addr != 0; {
		if addr&3 != 0 {
			return 0, &TrainError{Code: ErrCapChainUnaligned,
				Message: fmt.Sprintf("Capability chain broken at 0x%x on device %s (unaligned pointer)", addr, devBDF(dev))}
		}
		hdr := pciReadWord(dev, addr)
		if hdr == 0xFFFF {
			return 0, &TrainError{Code: ErrLinkDown,
				Message: fmt.Sprintf("Capability chain broken at 0x%x on device %s (read 0xFFFF)", addr, devBDF(dev))}
		}
		if (hdr & capabilityMask) == C.PCI_CAP_ID_EXP {
			return addr, nil
		}
		been[addr] = true
		addr = int32((hdr >> nextPos) & addrMask)
		if been[addr] {
			return 0, &TrainError{Code: ErrCapChainLoop,
				Message: fmt.Sprintf("Capability chain loops at 0x%x on device %s", addr, devBDF(dev))}
		}
	}
	return 0, &TrainError{Code: ErrPCIeCapNotFound,
		Message: fmt.Sprintf("PCIe capability header not found on device %s", devBDF(dev))}
}

// getPCIeExtCapOffset scans the PCIe extended capability linked list for the specified Extended Capability ID.
// Refers to PCIe Base Specification 6.4 Chapter 9.4.
//
// Returns address 0 to indicate the ext capability is not in the linked list.
var getPCIeExtCapOffset = func(dev pci.Dev, wantExtCapID uint16) (int32, error) {
	var been [extConfigSpaceSize]bool
	// PCIe Extended Capability header is 32 bits (4 bytes).
	//   - Bits 15:0: PCIe Extended Capability ID.
	//   - Bits 19:16: Capability Version.
	//   - Bits 31:20: Next PCIe Extended Capability Pointer.
	for addr := int32(PcieExtCapStart); addr != 0; {
		if addr&0b11 != 0 {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (unaligned pointer)", addr, devBDF(dev))
		}
		hdr := pciReadLong(dev, addr)
		if hdr == 0xFFFFFFFF {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (read 0xFFFFFFFF)", addr, devBDF(dev))
		}
		gotExtCapID := uint16(hdr & 0xFFFF)
		if gotExtCapID == wantExtCapID {
			return addr, nil
		}
		been[addr] = true
		nextOffset := int32((hdr >> 20) & 0xFFF)
		if nextOffset == 0 {
			break // End of the extended capability linked list.
		}
		if nextOffset < PcieExtCapStart || nextOffset >= extConfigSpaceSize {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (offset out of range [0x%x, 0x%x))",
				nextOffset, devBDF(dev), PcieExtCapStart, extConfigSpaceSize)
		}
		if been[nextOffset] {
			return 0, fmt.Errorf("Capability chain loops at 0x%x on device %s", nextOffset, devBDF(dev))
		}
		addr = nextOffset
	}
	return 0, nil // Return address 0 to indicate the ext capability is not in the linked list.
}

// getSriovCapOffset scans the PCIe extended capability linked list for the SR-IOV Extended Capability.
// Refers to PCIe Base Specification 6.4 Chapter 9.4.
var getSriovCapOffset = func(dev pci.Dev) (int32, error) {
	return getPCIeExtCapOffset(dev, C.PCI_EXT_CAP_ID_SRIOV)
}

// ReadLinkTrainProto reads in the linktrain.proto in text format.
func ReadLinkTrainProto(fn string) (*pb.LinkTrain, error) {
	cfgfn = fn
	// os.ReadFile returns a byte slice of the file contents and an error (which may be nil).
	data, err := os.ReadFile(fn)
	if err != nil {
		return nil, err
	}
	// Allocates a new LinkTrain to hold the deserialized proto.
	cfgpb := &pb.LinkTrain{}
	if err := prototext.Unmarshal(data, cfgpb); err != nil {
		return nil, err
	}
	return cfgpb, nil
}

// Filters a list of PciConfigField by its State. It's used to split the list
// into checkers and loggers.
func filterFields(fields []*pb.LinkTrain_PciConfigField,
	e pb.LinkTrain_PciConfigField_StateEnum) []*pb.LinkTrain_PciConfigField {
	subset := make([]*pb.LinkTrain_PciConfigField, len(fields))[:0]
	for _, f := range fields {
		if f.GetState() == e {
			subset = append(subset, f)
		}
	}
	return subset
}

func extendLinkTrainFields(dev pci.Dev, cfg *pb.LinkTrain, uspPCIeCapOffset int32) error {
	if cfg.GetRecoverPciConfigSpaceHeader() {
		// The PCI-Compatible Config Space Header (Spec 7.5.1.1) is always the first 64 (0x40) bytes.
		for offset := uint32(0); offset < 0x40; offset += 4 {
			cfg.Field = append(cfg.Field, &pb.LinkTrain_PciConfigField{
				Name:  fmt.Sprintf("BaseConfigDword%02X", offset),
				State: pb.LinkTrain_PciConfigField_S_RECOVER,
				Addr:  offset,
				Size:  pb.LinkTrain_PciConfigField_UINT32,
				Mask:  0xFFFFFFFF,
			})
		}
	}

	if cfg.GetRecoverPcieCapCtrlRegs() {
		ctrlOffsets := []struct {
			name   string
			offset int32
		}{
			{"PcieDeviceControl", C.PCI_EXP_DEVCTL},
			{"PcieLinkControl", C.PCI_EXP_LNKCTL},
			{"PcieSlotControl", C.PCI_EXP_SLTCTL},
			{"PcieRootControl", C.PCI_EXP_RTCTL},
			{"PcieDeviceControl2", C.PCI_EXP_DEVCTL2},
			{"PcieLinkControl2", C.PCI_EXP_LNKCTL2},
			{"PcieSlotControl2", C.PCI_EXP_SLTCTL2},
		}
		for _, reg := range ctrlOffsets {
			cfg.Field = append(cfg.Field, &pb.LinkTrain_PciConfigField{
				Name:  reg.name,
				State: pb.LinkTrain_PciConfigField_S_RECOVER,
				Addr:  uint32(uspPCIeCapOffset + reg.offset),
				Size:  pb.LinkTrain_PciConfigField_UINT16,
				Mask:  0xFFFF,
			})
		}
	}

	if cfg.GetRecoverSriovCapRegs() {
		sriovOffset, err := getSriovCapOffset(dev)
		if err != nil {
			return fmt.Errorf("failed to get SR-IOV Extended Capability Structure's start offset on device %s: %w", devBDF(dev), err)
		}
		if sriovOffset != 0 {
			// PCIe Base Spec 6.4 Ch 9.4: SR-IOV Extended Capability has 16 DWORDs (64 bytes, 0x00 to 0x3C).
			for i := uint32(0); i < 16; i++ {
				offset := i * 4
				cfg.Field = append(cfg.Field, &pb.LinkTrain_PciConfigField{
					Name:  fmt.Sprintf("SRIOVCapDword%02X", offset),
					State: pb.LinkTrain_PciConfigField_S_RECOVER,
					Addr:  uint32(sriovOffset) + offset,
					Size:  pb.LinkTrain_PciConfigField_UINT32,
					Mask:  0xFFFFFFFF,
				})
			}
		}
	}

	return nil
}

// matchID reports whether a PCI ID matches want. A nil want is a wildcard.
func matchID(want *uint32, got uint16) bool {
	return want == nil || *want == uint32(got)
}

// matchBDF reports whether the device is one of bdfs. An empty bdfs is a
// wildcard. BDFs are compared numerically, so e.g. "0000:B8:00.0" matches the
// device 0000:b8:00.0. Malformed BDFs, which validateLinkTarget rejects before
// the scan, never match.
func matchBDF(bdfs []string, d pci.PCIDevInfo) bool {
	if len(bdfs) == 0 {
		return true
	}
	for _, bdf := range bdfs {
		domain, bus, dev, fn, err := pci.ParseBDF(bdf)
		if err != nil {
			log.Warningf("Failed to parse BDF %q: %v", bdf, err)
			continue
		}
		if domain == int(d.Domain) && bus == int(d.Bus) && dev == int(d.Dev) && fn == int(d.Func) {
			return true
		}
	}
	return false
}

// validateLinkTarget returns an error if any BDF in the link target is
// malformed, so that a typo fails the test instead of silently matching
// nothing.
func validateLinkTarget(target *pb.PcieLinkTarget) error {
	for _, bdf := range slices.Concat(target.GetUspBdfs(), target.GetDspBdfs()) {
		if _, _, _, _, err := pci.ParseBDF(bdf); err != nil {
			return fmt.Errorf("malformed BDF %q in link_target: %w", bdf, err)
		}
	}
	return nil
}

// OverrideUSPBDFs replaces the USP BDF filter of cfg with bdfs, e.g. from a
// command line flag.
//
// Surrounding whitespace is trimmed from each entry and empty entries are
// dropped, so e.g. strings.Split("0000:16:00.0,", ",") selects one USP. It
// returns an error and leaves cfg unchanged if cfg is nil, if any entry is
// malformed, or if no entry is left, because an empty filter would select every
// link.
func OverrideUSPBDFs(cfg *pb.LinkTrain, bdfs []string) error {
	if cfg == nil {
		return errors.New("nil LinkTrain config")
	}
	var usp []string
	for _, bdf := range bdfs {
		bdf = strings.TrimSpace(bdf)
		if bdf == "" {
			continue
		}
		if _, _, _, _, err := pci.ParseBDF(bdf); err != nil {
			return fmt.Errorf("malformed USP BDF: %w", err)
		}
		usp = append(usp, bdf)
	}
	if len(usp) == 0 {
		return fmt.Errorf("no valid USP BDF given (input: %q)", bdfs)
	}
	if cfg.LinkTarget == nil {
		cfg.LinkTarget = &pb.PcieLinkTarget{}
	}
	cfg.LinkTarget.UspBdfs = usp
	return nil
}

// matchLinkTarget reports whether the link between the given USP and DSP
// matches the link target. The USP BDFs are not checked here, and the USP IDs
// are checked again only for completeness, because matchTestTarget checks both
// before looking up the DSP.
//
// A nil target, or an unset field within the target, is a wildcard.
func matchLinkTarget(uspInfo, dspInfo pci.PCIDevInfo, target *pb.PcieLinkTarget) bool {
	if target == nil {
		return true
	}
	return matchID(target.UspVendorId, uspInfo.VendorID) &&
		matchID(target.UspDeviceId, uspInfo.DeviceID) &&
		matchID(target.DspVendorId, dspInfo.VendorID) &&
		matchID(target.DspDeviceId, dspInfo.DeviceID) &&
		matchBDF(target.GetDspBdfs(), dspInfo)
}

// testTarget is a link selected for testing, with everything matchTestTarget
// already had to resolve in order to select it.
type testTarget struct {
	usp pci.Dev
	dsp pci.Dev
	// PCIe Capability offsets of the USP and the DSP.
	uspCapOffset int32
	dspCapOffset int32
}

// matchTestTarget checks if the device matches the test target specified in the
// LinkTrain proto. All the filters in the proto are ANDed.
//
// If any error occurs, returns (nil, error).
// If the device does not match the test target, returns (nil, nil).
// If the device matches the test target, returns the resolved link, nil.
var matchTestTarget = func(dev pci.Dev, cfg *pb.LinkTrain) (*testTarget, error) {
	d := dev.GetDevInfo()

	// Only tests the USPs that match the USP IDs and BDFs of link_target, if
	// it is specified. A non-matching device is skipped here, before its
	// config space is read and its DSP is looked up, so that an unrelated
	// device cannot fail the run. The DSP fields are checked after the DSP is
	// found below.
	if t := cfg.GetLinkTarget(); t != nil &&
		(!matchID(t.UspVendorId, d.VendorID) || !matchID(t.UspDeviceId, d.DeviceID) || !matchBDF(t.GetUspBdfs(), d)) {
		return nil, nil
	}

	// Only the first physical function of a device represents the link.
	if d.Dev != 0 || d.Func != 0 {
		return nil, nil
	}

	// Checks the PCIe port type. Only an endpoint or a switch upstream port
	// are eligible for training test.
	offset, terr := getPCIeCapOffset(dev)
	if terr != nil {
		// If there's any error getting the PCIe capability offset, the device
		// is to be excluded from testing.
		log.Warningf("BDF:[%s]: Failed to get PCIe Capability offset: %v", dev.BDFString(), terr)
		return nil, nil
	}
	portType := pciReadWord(dev, offset+C.PCI_EXP_FLAGS) & C.PCI_EXP_FLAGS_TYPE
	portType = portType >> 4
	if portType != C.PCI_EXP_TYPE_ENDPOINT && portType != C.PCI_EXP_TYPE_UPSTREAM {
		return nil, nil
	}

	// Dynamically filter out non-physical, internal Switch operational bridges.
	// Base Class 0x06 (Bridge) in byte 0x0B, Sub-Class 0x80 (Other Bridge) in byte 0x0A.
	if pciReadByte(dev, 0x0B) == 0x06 && pciReadByte(dev, 0x0A) == 0x80 {
		log.Infof("BDF:[%s]: [Notice] Dynamically skipping non-physical internal Switch bridge based on PCI Sub-Class (0x06:0x80).",
			dev.BDFString())
		return nil, nil
	}

	dsp, err := findDSP(dev)
	if err != nil {
		return nil, fmt.Errorf("failed to find the DSP of USP %s: %w", d.BDFStr(), err)
	}

	// Both ends of the link must match link_target, if it is specified. This
	// disambiguates links whose USPs share the same <vendor_id, device_id>.
	if !matchLinkTarget(d, dsp.GetDevInfo(), cfg.GetLinkTarget()) {
		log.Infof("BDF:[%s]: Skipping the link to DSP %s, which does not match link_target.",
			dev.BDFString(), devBDF(dsp))
		return nil, nil
	}

	dspOffset, terr := getPCIeCapOffset(dsp)
	if terr != nil {
		// As with the USP above, a DSP whose PCIe Capability cannot be read is
		// excluded from testing rather than failing the whole run, so that one
		// down link does not hide the results of every other link.
		log.Warningf("BDF:[%s]: Skipping the link to DSP %s: failed to get the DSP's PCIe Capability offset: %v",
			dev.BDFString(), devBDF(dsp), terr)
		return nil, nil
	}

	return &testTarget{usp: dev, dsp: dsp, uspCapOffset: offset, dspCapOffset: dspOffset}, nil
}

// getLinks gets a list of links according to the proto spec.
var getLinks = func(devs pci.Dev, cfg *pb.LinkTrain) ([]*Linktest, error) {
	linktests := make([]*Linktest, 0, numLinks)
	for dev := devs; dev.Valid(); dev = dev.GetNext() {
		tt, err := matchTestTarget(dev, cfg)
		if err != nil {
			return linktests, fmt.Errorf("failed to match the test target for device %s: %w", devBDF(dev), err)
		}
		if tt == nil {
			// The device is not a test target.
			continue
		}
		d := tt.usp.GetDevInfo()

		var lt Linktest
		lt.Usp = tt.usp
		lt.Dsp = tt.dsp
		lt.uspPCIeCapOffset = tt.uspCapOffset
		lt.dspPCIeCapOffset = tt.dspCapOffset
		lt.Cfg = proto.Clone(cfg).(*pb.LinkTrain)
		if err := extendLinkTrainFields(tt.usp, lt.Cfg, lt.uspPCIeCapOffset); err != nil {
			return linktests, fmt.Errorf("failed to extend the link train fields of USP %s (DSP %s): %w",
				d.BDFStr(), tt.dsp.BDFString(), err)
		}
		lt.chks = filterFields(lt.Cfg.GetField(), pb.LinkTrain_PciConfigField_S_CHECK)
		lt.logs = filterFields(lt.Cfg.GetField(), pb.LinkTrain_PciConfigField_S_LOG)
		lt.recs = filterFields(lt.Cfg.GetField(), pb.LinkTrain_PciConfigField_S_RECOVER)
		uspBdf := lt.Usp.BDFString()
		lt.Cfg.UspBdf = &uspBdf
		dspBdf := lt.Dsp.BDFString()
		lt.Cfg.DspBdf = &dspBdf
		passCnt := int32(0)
		lt.Cfg.PassCount = &passCnt
		failCnt := int32(0)
		lt.Cfg.FailCount = &failCnt
		if lt.Cfg.TrainingWaitMs == 0 {
			// Set to default value.
			lt.Cfg.TrainingWaitMs = uint32(defaultSbrWaitTrainTime.Milliseconds())
		}
		lt.sbrWaitTrainTime = time.Duration(lt.Cfg.TrainingWaitMs) * time.Millisecond
		lt.Pass = false

		// Wait time related.
		if lt.Cfg.ReenableWaitMsAfterDisable == 0 {
			lt.Cfg.ReenableWaitMsAfterDisable = uint32(defaultReenableWaitAfterDisable.Milliseconds())
		}
		lt.reenableWaitAfterDisable = time.Duration(lt.Cfg.GetReenableWaitMsAfterDisable()) * time.Millisecond

		linktests = append(linktests, &lt)
	}
	return linktests, nil
}

// check checks config registers of an USP against the proto spec.
func (lt *Linktest) check(iteration int) bool {
	usp := lt.Usp
	chks := lt.chks
	pass := true

	for i, f := range chks {
		var v uint32
		var valStr string
		switch f.GetSize() {
		case pb.LinkTrain_PciConfigField_UINT8:
			v = uint32(pci.ReadByte(usp, int32(f.GetAddr())))
			valStr = fmt.Sprintf("%2x", v)
		case pb.LinkTrain_PciConfigField_UINT16:
			v = uint32(pci.ReadWord(usp, int32(f.GetAddr())))
			valStr = fmt.Sprintf("%4x", v)
		case pb.LinkTrain_PciConfigField_UINT32:
			v = uint32(pci.ReadLong(usp, int32(f.GetAddr())))
			valStr = fmt.Sprintf("%8x", v)
		}

		mSeries := &ocppb.MeasurementSeriesElement{
			Index:               lt.seriesCnt[i],
			MeasurementSeriesId: lt.seriesID[i],
			Value:               structpb.NewStringValue(valStr),
			Timestamp:           timestamppb.Now(),
		}
		stepArti := &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_MeasurementSeriesElement{MeasurementSeriesElement: mSeries},
			TestStepId: lt.hwinfo,
		}
		outArti := &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
		}
		outputArtifact(outArti)
		lt.seriesCnt[i]++

		if f.Expected == nil {
			if f.GetState() != pb.LinkTrain_PciConfigField_S_ERROR {
				log.Errorf("BDF:[%s]: [Iteration %4d][Field %-25s] Field check missing expected value", lt.Cfg.GetUspBdf(), iteration, f.GetName())
			}
			f.State = pb.LinkTrain_PciConfigField_S_ERROR
			pass = false
		} else {
			fieldPass := (f.GetMask() & f.GetExpected()) == (f.GetMask() & v)
			if !fieldPass {
				pass = false
				hexWidth := (1 << f.GetSize()) * 2
				log.Errorf("BDF:[%s]: [Iteration %4d][Field %-25s] Field check mismatch: actual 0x%0*x, expected 0x%0*x (state %s)",
					lt.Cfg.GetUspBdf(), iteration, f.GetName(), hexWidth, f.GetMask()&v, hexWidth, f.GetMask()&f.GetExpected(), f.GetState())
			}
			if f.GetState() == pb.LinkTrain_PciConfigField_S_CHECK || f.GetState() == pb.LinkTrain_PciConfigField_S_PASS {
				v = f.GetMask() & v
				f.Val = &v
				if fieldPass {
					f.State = pb.LinkTrain_PciConfigField_S_PASS
				} else {
					f.State = pb.LinkTrain_PciConfigField_S_ERROR
				}
			}
		}
	}
	return pass
}

// log logs config register values of an USP according to the proto spec.
func (lt *Linktest) log() {
	usp := lt.Usp
	for _, f := range lt.logs {
		var v uint32
		switch f.GetSize() {
		case pb.LinkTrain_PciConfigField_UINT8:
			v = uint32(pci.ReadByte(usp, int32(f.GetAddr())))
		case pb.LinkTrain_PciConfigField_UINT16:
			v = uint32(pci.ReadWord(usp, int32(f.GetAddr())))
		case pb.LinkTrain_PciConfigField_UINT32:
			v = uint32(pci.ReadLong(usp, int32(f.GetAddr())))
		}
		v = v & f.GetMask()
		f.Val = &v
		f.State = pb.LinkTrain_PciConfigField_S_LOGGED
	}
}

// record records config register values of an USP that will be restored later.
func (lt *Linktest) record() {
	usp := lt.Usp
	for _, f := range lt.recs {
		var v uint32
		switch f.GetSize() {
		case pb.LinkTrain_PciConfigField_UINT8:
			v = uint32(pci.ReadByte(usp, int32(f.GetAddr())))
		case pb.LinkTrain_PciConfigField_UINT16:
			v = uint32(pci.ReadWord(usp, int32(f.GetAddr())))
		case pb.LinkTrain_PciConfigField_UINT32:
			v = uint32(pci.ReadLong(usp, int32(f.GetAddr())))
		}
		v = v & f.GetMask()
		f.Val = &v
	}
}

// restore restores the recorded config register values.
func (lt *Linktest) restore() {
	usp := lt.Usp
	for _, f := range lt.recs {
		var v uint32
		// Conducts read-modify-write
		switch f.GetSize() {
		case pb.LinkTrain_PciConfigField_UINT8:
			v = uint32(pci.ReadByte(usp, int32(f.GetAddr()))) & (0xff ^ f.GetMask())
		case pb.LinkTrain_PciConfigField_UINT16:
			v = uint32(pci.ReadWord(usp, int32(f.GetAddr()))) & (0xffff ^ f.GetMask())
		case pb.LinkTrain_PciConfigField_UINT32:
			v = uint32(pci.ReadLong(usp, int32(f.GetAddr()))) & (0xffffffff ^ f.GetMask())
		}
		v = v | (f.GetVal() & f.GetMask())
		switch f.GetSize() {
		case pb.LinkTrain_PciConfigField_UINT8:
			pci.WriteByte(usp, int32(f.GetAddr()), uint8(v))
		case pb.LinkTrain_PciConfigField_UINT16:
			pci.WriteWord(usp, int32(f.GetAddr()), uint16(v))
		case pb.LinkTrain_PciConfigField_UINT32:
			pci.WriteLong(usp, int32(f.GetAddr()), uint32(v))
		}
	}
}

// trainLoop trains one link iteratively.
var trainLoop = func(lt *Linktest) {
	cfg := lt.Cfg
	itr := int(cfg.GetIterations())

	// Records the fields to be restored.
	lt.record()

	// OCP TestStepStart
	lt.hwinfo = lt.Usp.BDFString()
	stepStart := &ocppb.TestStepStart{
		Name: strings.TrimPrefix(cfg.GetMethod().String(), "M_") + "@" + lt.hwinfo,
	}
	stepArti := &ocppb.TestStepArtifact{
		Artifact:   &ocppb.TestStepArtifact_TestStepStart{TestStepStart: stepStart},
		TestStepId: lt.hwinfo,
	}
	outArti := &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
	}
	outputArtifact(outArti)

	// Starts one MeasurementSeries per checker.
	for i, f := range lt.chks {
		lt.seriesCnt = append(lt.seriesCnt, 0)

		if f.Expected == nil {
			if f.GetState() != pb.LinkTrain_PciConfigField_S_ERROR {
				log.Errorf("BDF:[%s]: [Field %-25s] Field check missing expected value", lt.Cfg.GetUspBdf(), f.GetName())
			}
			state := pb.LinkTrain_PciConfigField_S_ERROR
			f.State = state
			lt.seriesID = append(lt.seriesID, "")
			continue
		}

		var seriesFmt string
		var valFmt string
		switch f.GetSize() {
		case pb.LinkTrain_PciConfigField_UINT8:
			seriesFmt = "%s:%04X.%s==%02x:%02x"
			valFmt = "%02x"
		case pb.LinkTrain_PciConfigField_UINT16:
			seriesFmt = "%s:%04X.%s==%04x:%04x"
			valFmt = "%04x"
		case pb.LinkTrain_PciConfigField_UINT32:
			seriesFmt = "%s:%04X.%s==%08x:%08x"
			valFmt = "%08x"
		}
		lt.seriesID = append(lt.seriesID,
			fmt.Sprintf(seriesFmt, f.GetName(), f.GetAddr(), f.GetSize().String(), f.GetExpected(), f.GetMask()))
		val := &ocppb.Validator{
			Name:  lt.seriesID[i],
			Type:  ocppb.Validator_EQUAL,
			Value: structpb.NewStringValue(fmt.Sprintf(valFmt, (f.GetMask() & f.GetExpected()))),
		}
		mSeries := &ocppb.MeasurementSeriesStart{
			Name:                strings.ToLower(fmt.Sprintf("%s-%04x-"+valFmt, f.GetName(), f.GetAddr(), f.GetExpected())),
			MeasurementSeriesId: lt.seriesID[i],
			HardwareInfoId:      lt.hwinfo,
			Validators:          []*ocppb.Validator{val},
		}
		stepArti = &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_MeasurementSeriesStart{MeasurementSeriesStart: mSeries},
			TestStepId: lt.hwinfo,
		}
		outArti = &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
		}
		outputArtifact(outArti)
	}

	diag := &ocppb.Diagnosis{
		Type:           ocppb.Diagnosis_UNKNOWN,
		HardwareInfoId: lt.hwinfo,
	}

	// Initial checking: If fails, quit testing.
	if !lt.check(-1) {
		lt.log()
		failCnt := cfg.GetFailCount() + 1
		cfg.FailCount = &failCnt
		lt.Pass = false
		log.Warningf("BDF:[%s]: Initial checking failed: %v", lt.Cfg.GetUspBdf(), lt)

		diag.Type = ocppb.Diagnosis_FAIL
		diag.Verdict = "ltt-initial-checking-failed"
		diag.Message = fmt.Sprintf("%s link failed LTT initial checking: pass_count=%d; fail_count=%d",
			lt.hwinfo, cfg.GetPassCount(), cfg.GetFailCount())
		stepArti = &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_Diagnosis{Diagnosis: diag},
			TestStepId: lt.hwinfo,
		}
		outArti = &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
		}
		outputArtifact(outArti)
		return
	}

	for i := 0; i < itr; i++ {
		timeBeforeIteration := time.Now()
		var trainErr error
		switch lt.Cfg.GetMethod() {
		case pb.LinkTrain_M_RETRAIN_DEFAULT:
			trainErr = lt.retrain()
		case pb.LinkTrain_M_RETRAIN_WITH_EQ:
			trainErr = lt.retrainWithEQ()
		case pb.LinkTrain_M_SBR:
			trainErr = lt.secondaryBusReset()
		case pb.LinkTrain_M_REENABLE:
			trainErr = lt.reenable()
		}
		iterationDuration := time.Now().Sub(timeBeforeIteration)
		if trainErr != nil {
			log.Errorf("BDF:[%s]: Link training failed (time spent: %v): %v",
				lt.Cfg.GetUspBdf(), iterationDuration, trainErr)
		}
		checkPassed := true
		if trainErr == nil || needFieldCheck(trainErr) {
			checkPassed = lt.check(i)
		}
		if trainErr != nil || !checkPassed {
			// Always logs the first failure.
			if cfg.GetFailCount() == 0 {
				lt.log()
			}
			failCnt := cfg.GetFailCount() + 1
			cfg.FailCount = &failCnt
			// By default, continues testing after the first failure, unless
			// continue is set to false.
			if cfg.Continue != nil && !cfg.GetContinue() {
				break
			}
		} else {
			passCnt := cfg.GetPassCount() + 1
			cfg.PassCount = &passCnt
		}
		log.V(1).Infof("BDF:[%s]: Iteration:%d; Pass:%d; Fail:%d; TrainingTimeSpent: %v; TrainErr: %v",
			cfg.GetUspBdf(), i, cfg.GetPassCount(), cfg.GetFailCount(), iterationDuration, trainErr)
	}

	// Logs at the end if no failure.
	if cfg.GetFailCount() == 0 && cfg.GetPassCount() > 0 {
		lt.log()
		lt.Pass = true
		diag.Type = ocppb.Diagnosis_PASS
		diag.Verdict = "ltt-passed"
		diag.Message = fmt.Sprintf("%s link passed LTT: pass_count=%d; fail_count=%d",
			lt.hwinfo, cfg.GetPassCount(), cfg.GetFailCount())
	} else {
		diag.Type = ocppb.Diagnosis_FAIL
		diag.Verdict = "ltt-failed"
		diag.Message = fmt.Sprintf("%s link failed LTT: pass_count=%d; fail_count=%d",
			lt.hwinfo, cfg.GetPassCount(), cfg.GetFailCount())
	}

	stepArti = &ocppb.TestStepArtifact{
		Artifact:   &ocppb.TestStepArtifact_Diagnosis{Diagnosis: diag},
		TestStepId: lt.hwinfo,
	}
	outArti = &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
	}
	outputArtifact(outArti)

	// Restores the recorded fields.
	lt.restore()

	// Ends MeasurementSeries per checker.
	for i, id := range lt.seriesID {
		mSeries := &ocppb.MeasurementSeriesEnd{
			MeasurementSeriesId: id,
			TotalCount:          int32(lt.seriesCnt[i]),
		}
		stepArti = &ocppb.TestStepArtifact{
			Artifact:   &ocppb.TestStepArtifact_MeasurementSeriesEnd{MeasurementSeriesEnd: mSeries},
			TestStepId: lt.hwinfo,
		}
		outArti = &ocppb.OutputArtifact{
			Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
		}
		outputArtifact(outArti)
	}

	// OCP TestStepEnd
	stepEnd := &ocppb.TestStepEnd{
		Status: ocppb.TestRunEnd_COMPLETE,
	}
	stepArti = &ocppb.TestStepArtifact{
		Artifact:   &ocppb.TestStepArtifact_TestStepEnd{TestStepEnd: stepEnd},
		TestStepId: lt.hwinfo,
	}
	outArti = &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestStepArtifact{TestStepArtifact: stepArti},
	}
	outputArtifact(outArti)
}

// ocpTestRunStart starts an OCP TestRun
var ocpTestRunStart = func(cfg *pb.LinkTrain) {
	if testRunStart == nil {
		if f, err := os.OpenFile("/dev/null", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0777); err != nil {
			log.Fatalf("error opening /dev/null : %v", err)
		} else {
			OcpInit(f, fmt.Sprintf("pcie_ltt_%s", strings.TrimPrefix(cfg.GetMethod().String(), "M_")), "undefined", fmt.Sprint(os.Args))
		}
	}

	ver := &ocppb.SchemaVersion{
		Major: 2,
		Minor: 0,
	}
	artiOut := &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_SchemaVersion{SchemaVersion: ver},
	}
	outputArtifact(artiOut)

	dutInfo := &ocppb.DutInfo{
		DutInfoId:     "this_pcie",
		Name:          "pcie_lmt_dut_info",
		SoftwareInfos: []*ocppb.SoftwareInfo{},
	}

	var hwInfo *ocppb.HardwareInfo
	for _, lt := range Lts {
		hwInfo = &ocppb.HardwareInfo{
			HardwareInfoId: lt.Dsp.BDFString(),
			Name:           "DSP",
		}
		dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)

		hwInfo = &ocppb.HardwareInfo{
			HardwareInfoId: lt.Usp.BDFString(),
			Name:           "USP",
		}
		dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)
	}

	testRunStart.DutInfo = dutInfo
	runArti := &ocppb.TestRunArtifact{
		Artifact: &ocppb.TestRunArtifact_TestRunStart{TestRunStart: testRunStart},
	}
	artiOut = &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestRunArtifact{TestRunArtifact: runArti},
	}
	outputArtifact(artiOut)

	testRunEnd = &ocppb.TestRunEnd{
		Status: ocppb.TestRunEnd_UNKNOWN,
		Result: ocppb.TestRunEnd_NOT_APPLICABLE,
	}
}

// LinkTrain is the top-level function.
// It identifies links to be tested, tests them in parallel, and writes out the result.
func LinkTrain(cfg *pb.LinkTrain) (pass bool, _ error) {
	if err := validateLinkTarget(cfg.GetLinkTarget()); err != nil {
		return false, err
	}

	pci.Init()
	defer pci.Cleanup()

	var err error

	// Gets a list of PCI devices.
	devs := pciScanDevices()
	if !devs.Valid() {
		err = fmt.Errorf("no pcie devices found")
		return false, err
	}

	if Lts, err = getLinks(devs, cfg); err != nil {
		return false, err
	}

	if len(Lts) == 0 {
		err = fmt.Errorf("no downstream device matches the proto spec")
		return false, err
	}

	// Starts OCP TestRun
	ocpTestRunStart(cfg)

	// Trains all links in parallel. Waits for all links to finish testing.
	var wg sync.WaitGroup
	for _, lt := range Lts {
		// If runs in series, waits for the previous iteration to finish.
		if lt.Cfg.Parallel == nil || !lt.Cfg.GetParallel() {
			wg.Wait()
		}
		wg.Add(1)
		go func(lt *Linktest) {
			defer wg.Done()
			trainLoop(lt)
		}(lt)
	}
	wg.Wait()

	// OCP TestRunEnd
	testRunEnd.Status = ocppb.TestRunEnd_COMPLETE
	testRunEnd.Result = ocppb.TestRunEnd_PASS
	for _, lt := range Lts {
		if !lt.Pass {
			testRunEnd.Result = ocppb.TestRunEnd_FAIL
		}
	}
	runArti := &ocppb.TestRunArtifact{
		Artifact: &ocppb.TestRunArtifact_TestRunEnd{TestRunEnd: testRunEnd},
	}
	artiOut := &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestRunArtifact{TestRunArtifact: runArti},
	}
	outputArtifact(artiOut)
	ocpPipeLock.Lock()
	ocpPipe.Close()
	ocpPipeLock.Unlock()

	for _, lt := range Lts {
		if !lt.Pass {
			return false, nil
		}
	}
	return true, nil
}

// WriteResultPbtxt writes out the result in a pb.txt.
func WriteResultPbtxt(outfn string) error {
	// Marshals test resuLts into pbtxt bytes per link.
	cfgs := make([]*pb.LinkTrain, len(Lts))
	for i, lt := range Lts {
		cfgs[i] = lt.Cfg
	}
	ltt := new(pb.LinkTrainTests)
	ltt.LinkTrain = cfgs

	opt := &prototext.MarshalOptions{Multiline: true, Indent: "  "}
	data, err := opt.Marshal(ltt)
	if err != nil {
		return err
	}
	if err := os.WriteFile(outfn, data, 0600); err != nil {
		return err
	}
	return nil
}
