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

// Package lanemargintest conducts PCIe Lane Margining at Receiver (LMR) Test on multiple
// homogenous PCIe links.
package lanemargintest

// This file includes the main exported functions:
// ReadLinkMargin() ingests the test spec.
// MarginLinks() enlists the PCIe links, and tests each link independently and in parallel.
// WriteResultPbtxt() dumps out all the readings.

/*
// The Cgo import here is only for using pciutils constants.
#include "lib/header.h"
*/
import (
	"C"
)

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	log "github.com/golang/glog"
	structpb "google.golang.org/protobuf/types/known/structpb"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	ocppb "ocpdiag/results_go_proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

// /////////////////////////////////////////////////////////////////////////////////////////////////
// Disclaimer: The terms here are not strictly following the PCIe terminology for legacy and
//             implementation reasons.
// /////////////////////////////////////////////////////////////////////////////////////////////////

// lts (LinkTests) is the overall storage object.
var lts []*linktest

// lmts is the result proto
var lmts *lmtpb.LinkMarginTest

// A linktest has everything needed to test a link. It corresponds to a
// LinkMargin proto message.
type linktest struct {
	usp, dsp  *port
	pb        *lmtpb.LinkMargin
	testReady bool                        // The link is capable of LMR testing.
	allRx     [ReceiverEnumSize]*receiver // all Rx ordered by the receiver number. 0 & 7 are nil .
	wg        *sync.WaitGroup             // Sometimes, links need to sync.
}

// A port is a PCIe device that contains a bunch of lanes. It can be a USP or a DSP.
type port struct {
	dev           pci.Dev // PCI config access for the port.
	isUSP         bool
	width         uint32
	pcieCapOffset int32 // PCI EXP CAPABILITIES offset
	lmrAddr       int32 // LMR capability address
	testReady     bool  // The port is capable of LMR testing.
	hasd          bool  // saved Hardware Autonomous Speed Disable state
	hawd          bool  // saved Hardware Autonomous Width Disable state
	speed         float64
}

// A receiver is a port or a pseudo port on a retimer, where lanes are margined.
type receiver struct {
	testReady bool // The receiver is included for testing.
	rec       lmtpb.LinkMargin_ReceiverEnum
	port      *port // Other than the USP, all retimer and usp receivers run from the DSP port.
	parallel  bool  // whether to test all lanes in parallel
	lanes     []*Lane
	rxwg      *sync.WaitGroup // To sync the receiver port.
	linkwg    *sync.WaitGroup // Sometimes, the receiver needs to wait for other links.
	hwinfo    string          // OCP hardware_info_id
}

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
)

// OcpInit initializes the OCP output headers.
func OcpInit(f *os.File, name string, version string, cmdline string, cfg *lmtpb.LinkMargin) {
	ocpPipe = f
	testRunStart = &ocppb.TestRunStart{
		Name:        name,
		Version:     version,
		CommandLine: cmdline,
	}
	// Initialize the OCP output artifact's sequence number. Default is 0.
	seqNum.Store(int32(0)) // 0  because of the atomicity of the counter.Add(1)

	opt := protojson.MarshalOptions{
		UseProtoNames:   true,
		UseEnumNumbers:  false,
		EmitUnpopulated: false,
		Multiline:       true,
		Indent:          "  ",
		AllowPartial:    false,
	}
	if data, err := opt.Marshal(cfg); err != nil {
		log.Exit(err)
	} else {
		var v structpb.Struct
		if err := json.Unmarshal(data, &v); err != nil {
			log.Exit(err)
		}
		testRunStart.Parameters = &v
	}
}

// /////////////////////////////////////////////////////////////////////////////////////////////////

// ReadLinkMargin reads in the test spec (cfg) from a linkmargin pbtxt or JSON.
func ReadLinkMargin(fn string, isJSON bool) (*lmtpb.LinkMargin, error) {
	if _, err := os.Stat(fn); os.IsNotExist(err) {
		return nil, err
	}
	// os.ReadFile returns a byte slice of the file contents
	// and an error (which may be nil).
	data, err := os.ReadFile(fn)
	if err != nil {
		return nil, err
	}
	// Allocate a new PciConfigField to hold the deserialized proto.
	cfg := &lmtpb.LinkMargin{}
	if isJSON {
		opt := protojson.UnmarshalOptions{
			AllowPartial:   false,
			DiscardUnknown: true,
		}
		if err := opt.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else {
		opt := prototext.UnmarshalOptions{
			AllowPartial:   false,
			DiscardUnknown: true,
		}
		if err := opt.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// WriteResultPbtxt writes out the test results from the lts to a textproto.
func WriteResultPbtxt(outfn string) error {
	// Marshal test results into pbtxt bytes per lk.
	lmts = new(lmtpb.LinkMarginTest)
	lms := make([]*lmtpb.LinkMargin, 0, 8)
	for _, lt := range lts {
		lms = append(lms, lt.pb)
	}
	// Sorts the result pb message by bus number.
	sort.SliceStable(lms, func(i, j int) bool { return lms[i].GetBus()[0] < lms[j].GetBus()[0] })
	lmts.LinkMargin = lms
	opt := &prototext.MarshalOptions{
		Multiline:    true,
		Indent:       "  ",
		EmitUnknown:  false,
		AllowPartial: false,
	}
	data, err := opt.Marshal(lmts)
	if err != nil {
		return err
	}
	err = os.WriteFile(outfn, data, 0600)
	if err != nil {
		return err
	}
	return nil
}

// MarginLinks is the top-level test function.
// It identifies links to be tested and tests them in parallel.
func MarginLinks(cfg *lmtpb.LinkMargin) error {
	pci.Init()
	defer pci.Cleanup()

	// Gets a list of PCI devices.
	devs := pci.ScanDevices()
	if !devs.Valid() {
		err := fmt.Errorf("no pcie devices found")
		return err
	}

	// Gets a list of links matching the test configuration.
	lts, err := getLinks(devs, cfg)
	if err != nil {
		return err
	}
	if len(lts) == 0 {
		err := fmt.Errorf("no port matches the proto param")
		return err
	}

	// Starts OCP TestRun
	ocpTestRunStart(cfg)

	// Tests all links in parallel. Waits for all links to finish testing.
	var wg sync.WaitGroup
	for _, lt := range lts {
		log.V(1).Infoln(lt.usp.dev.BDFString(), " test ready? ", lt.testReady)
		if lt.testReady {
			wg.Add(1)
			go func(lt *linktest) {
				defer wg.Done()
				lt.marginLink()
			}(lt)
		}
	}
	wg.Wait()

	// OCP TestRunEnd
	testRunEnd.Status = ocppb.TestRunEnd_COMPLETE
	testRunEnd.Result = ocppb.TestRunEnd_NOT_APPLICABLE
	for _, lt := range lts {
		if lt.testReady {
			for _, l := range lt.pb.ReceiverLanes {
				if l.Pass != nil {
					if *l.Pass && testRunEnd.Result != ocppb.TestRunEnd_FAIL {
						testRunEnd.Result = ocppb.TestRunEnd_PASS
					} else {
						testRunEnd.Result = ocppb.TestRunEnd_FAIL
					}
				}
			}
		}
	}
	runArti := &ocppb.TestRunArtifact{
		Artifact: &ocppb.TestRunArtifact_TestRunEnd{testRunEnd},
	}
	artiOut := &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestRunArtifact{runArti},
	}
	outputArtifact(artiOut)
	ocpPipeLock.Lock()
	ocpPipe.Close()
	ocpPipeLock.Unlock()

	return nil
}

// outputArtifact() streams artiOut to the OcpPipe.
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

// ocpTestRunStart starts an OCP TestRun
func ocpTestRunStart(cfg *lmtpb.LinkMargin) {
	if testRunStart == nil {
		if f, err := os.OpenFile("/dev/null", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0777); err != nil {
			log.Fatalf("error opening /dev/null : %v", err)
		} else {
			OcpInit(f, "pcie_lmt", "undefined", fmt.Sprint(os.Args), cfg)
		}
	}

	ver := &ocppb.SchemaVersion{
		Major: 2,
		Minor: 0,
	}
	artiOut := &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_SchemaVersion{ver},
	}
	outputArtifact(artiOut)

	dutInfo := &ocppb.DutInfo{
		DutInfoId:     "this_pcie",
		Name:          "pcie_lmt_dut_info",
		SoftwareInfos: []*ocppb.SoftwareInfo{},
	}

	var hwInfo *ocppb.HardwareInfo
	for _, lt := range lts {
		hwInfo = &ocppb.HardwareInfo{
			HardwareInfoId: "BDF=" + lt.dsp.dev.BDFString() + ";RX=" + strings.TrimPrefix(lmtpb.LinkMargin_ReceiverEnum(1).String(), "R_"),
			Name:           "DSP",
		}
		dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)

		hwInfo = &ocppb.HardwareInfo{
			HardwareInfoId: "BDF=" + lt.usp.dev.BDFString() + ";RX=" + strings.TrimPrefix(lmtpb.LinkMargin_ReceiverEnum(6).String(), "R_"),
			Name:           "USP",
		}
		dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)

		// Reads if retimer presents
		addr := lt.dsp.pcieCapOffset + C.PCI_EXP_LNKSTA2
		val := pci.ReadWord(lt.dsp.dev, addr)
		retimer0 := (val & C.PCI_EXP_LINKSTA2_RETIMER) != 0
		retimer1 := (val & C.PCI_EXP_LINKSTA2_2RETIMERS) != 0
		if retimer0 {
			hwInfo = &ocppb.HardwareInfo{
				HardwareInfoId: "BDF=" + lt.dsp.dev.BDFString() + ";RX=" + strings.TrimPrefix(lmtpb.LinkMargin_ReceiverEnum(2).String(), "R_"),
				Name:           "Retimer0-USP",
			}
			dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)
			hwInfo = &ocppb.HardwareInfo{
				HardwareInfoId: "BDF=" + lt.dsp.dev.BDFString() + ";RX=" + strings.TrimPrefix(lmtpb.LinkMargin_ReceiverEnum(3).String(), "R_"),
				Name:           "Retimer0-DSP",
			}
			dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)
		}
		if retimer1 {
			hwInfo = &ocppb.HardwareInfo{
				HardwareInfoId: "BDF=" + lt.dsp.dev.BDFString() + ";RX=" + strings.TrimPrefix(lmtpb.LinkMargin_ReceiverEnum(4).String(), "R_"),
				Name:           "Retimer1-USP",
			}
			dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)
			hwInfo = &ocppb.HardwareInfo{
				HardwareInfoId: "BDF=" + lt.dsp.dev.BDFString() + ";RX=" + strings.TrimPrefix(lmtpb.LinkMargin_ReceiverEnum(5).String(), "R_"),
				Name:           "Retimer1-DSP",
			}
			dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)
		}
	}

	testRunStart.DutInfo = dutInfo
	runArti := &ocppb.TestRunArtifact{
		Artifact: &ocppb.TestRunArtifact_TestRunStart{testRunStart},
	}
	artiOut = &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_TestRunArtifact{runArti},
	}
	outputArtifact(artiOut)

	testRunEnd = &ocppb.TestRunEnd{
		Status: ocppb.TestRunEnd_UNKNOWN,
		Result: ocppb.TestRunEnd_NOT_APPLICABLE,
	}
}

// /////////////////////////////////////////////////////////////////////////////////////////////////

// A global synchronizer to avoid overlapping lane parameter reading and margining.
// This is global, rather than per link, because of bifurcation consideration. All links are synced.
var linkwg sync.WaitGroup

// getLinks gets a list of PCIe ports according to the proto param.
func getLinks(devs pci.Dev, cfg *lmtpb.LinkMargin) ([]*linktest, error) {
	var err error
	const numLinks = 8 // estimated array-initial-size of links to be tested.
	lts = make([]*linktest, 0, numLinks)
	buses := cfg.GetBus()
	// Filters devices by Vid, Did, and/or Bus. Only downstream dev is selected.
	// This assumes dev number == 0, and func = 0.
	for dev := devs; dev.Valid(); dev = dev.GetNext() {
		d := dev.GetDevInfo()
		vidChk := cfg.VendorId == nil || uint32(d.VendorID) == cfg.GetVendorId()
		didChk := cfg.DeviceId == nil || uint32(d.DeviceID) == cfg.GetDeviceId()
		busChk := len(buses) == 0 || slices.Contains(buses, uint32(d.Bus))
		pf0Chk := (d.Dev == 0) && (d.Func == 0)
		if vidChk && didChk && busChk && pf0Chk {
			// Checks the PCIe port type. Only an endpoint or a switch upstream port
			// are eligible for margining.
			if offset, err := getPcieCapOffset(dev); err != nil {
				// If there's any error getting the PCIe capability offset, the device
				// is to be excluded from testing.
				continue
			} else {
				portType := pci.ReadWord(dev, offset+C.PCI_EXP_FLAGS) & C.PCI_EXP_FLAGS_TYPE
				portType = portType >> 4
				if portType != C.PCI_EXP_TYPE_ENDPOINT && portType != C.PCI_EXP_TYPE_UPSTREAM {
					continue
				}
			}

			log.V(2).Infoln("Found dev: ", dev.BDFString())
			lt := new(linktest)
			lt.usp = new(port)
			lt.dsp = new(port)
			lt.wg = &linkwg

			// Then gets the link partner.
			lt.usp.dev = dev
			lt.dsp.dev, err = dev.FindDSP()
			if err != nil {
				return nil, err
			}

			lt.dsp.isUSP = false
			lt.usp.isUSP = true

			// Clones a result protobuf for the test config protobuf.
			lt.pb = proto.Clone(cfg).(*lmtpb.LinkMargin)
			vendorID := uint32(d.VendorID)
			lt.pb.VendorId = &vendorID
			deviceID := uint32(d.DeviceID)
			lt.pb.DeviceId = &deviceID
			lt.pb.Bus = ([]uint32{uint32(d.Bus)})
			uspBdf := dev.BDFString()
			lt.pb.UspBdf = &uspBdf
			dspBdf := lt.dsp.dev.BDFString()
			lt.pb.DspBdf = &dspBdf
			lts = append(lts, lt)

			// Gets port capability offsets.
			var msg strings.Builder
			lt.testReady = true
			for _, p := range [2]*port{lt.dsp, lt.usp} {
				p.testReady = true
				bdf := p.dev.BDFString()
				if p.pcieCapOffset, err = getPcieCapOffset(p.dev); err != nil {
					p.testReady = false
					lt.testReady = false
					msg.WriteString(fmt.Sprintf("Error: %s: %s | ", bdf, err.Error()))
				} else {
					addr := p.pcieCapOffset + C.PCI_EXP_LNKSTA
					val := pci.ReadWord(p.dev, addr)
					p.width = uint32((val & C.PCI_EXP_LNKSTA_WIDTH) >> LinkStatusWidthPos)
					speed := pci.ReadWord(p.dev, addr) & C.PCI_EXP_LNKSTA_SPEED
					msg.WriteString(fmt.Sprintf(
						"Info: %s: PCIEXP CAP offset=%x; PCI_EXP_LNKSTA_WIDTH=%d; PCI_EXP_LNKSTA_SPEED=%d  | ",
						bdf, p.pcieCapOffset, p.width, speed))
					switch speed {
					case Speed16G:
						p.speed = 16.0e9
					case Speed32G:
						p.speed = 32.0e9
					default:
						log.V(1).Infoln(bdf, " speed %d is not gen4 nor gen5. Skipped.", speed)
						p.speed = 0.0
						p.testReady = false
						lt.testReady = false
					}
				}

				if p.lmrAddr, err = p.getLMRcapability(); err != nil {
					p.testReady = false
					// The LMR is required at gen4 and above. If it's not found, it's like not a real link.
					lt.testReady = false
					msg.WriteString(fmt.Sprintf("Error: %s: %s | ", bdf, err.Error()))
				} else {
					msg.WriteString(fmt.Sprintf("Info: %s: LMR CAP offset=%x | ", bdf, p.lmrAddr))
				}
			}
			message := msg.String()
			lt.pb.Message = &message
		}
	}
	return lts, nil
}

// getLMRcapability scans the PCI capability linked list for LMR capability.
// pciutils/ls-ecaps.c
func (p *port) getLMRcapability() (int32, error) {
	const (
		ConfigSpace     = int32(0x1000)
		CapabilityStart = int32(0x100)
		CapabilityMask  = int32(0x00FF)
		AddrMask        = int32(0x0FFC)
		NextPos         = int(20)
	)
	var been [ConfigSpace]bool
	for addr := CapabilityStart; addr != 0; {
		hdr := int32(pci.ReadLong(p.dev, addr))
		if (hdr & CapabilityMask) == C.PCI_EXT_CAP_ID_LMR {
			return addr, nil
		}
		been[addr] = true
		addr = (hdr >> NextPos) & AddrMask
		if been[addr] {
			return 0, fmt.Errorf("Capability chain loops at 0x%x", addr)
		}
	}
	return 0, fmt.Errorf("LMR capability header not found")
}

// getPcieCapOffset scans the PCI capability linked list for PCIe CAP.
// Refers to pciutils/ls-caps.c
func getPcieCapOffset(dev pci.Dev) (int32, error) {
	const (
		ConfigSpace     = int32(0x100) // The base config space is 256B.
		CapabilityStart = C.PCI_CAPABILITY_LIST
		CapabilityMask  = int32(0x00FF)
		AddrMask        = int32(0x0FFC)
		NextPos         = int(8)
	)
	// Tracks if a loop occur in the linked list.
	var been [ConfigSpace]bool
	for addr := pci.ReadByte(dev, CapabilityStart); addr != 0; {
		hdr := int32(pci.ReadWord(dev, int32(addr)))
		if (hdr & CapabilityMask) == int32(C.PCI_CAP_ID_EXP) {
			return int32(addr), nil
		}
		been[addr] = true
		addr = uint8((hdr >> NextPos) & AddrMask)
		if been[addr] {
			return 0, fmt.Errorf("Capability chain loops at 0x%x", addr)
		}
	}
	return 0, fmt.Errorf("PCIe capability header not found")
}
