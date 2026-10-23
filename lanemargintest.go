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
	"errors"
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
	gen           uint32
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
	// marginHooks are called after each margin point of the receiver lanes.
	marginHooks []MarginHook
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

	pciScanDevices = pci.ScanDevices
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
		if err := protojson.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	} else {
		if err := prototext.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// bdfToUnit32 converts a BDF string to a uint32 for comparing.
func bdf2u32(bdf string) uint32 {
	domain := uint16(0)
	b := uint8(0)
	d := uint8(0)
	f := uint8(0)
	if n, _ := fmt.Sscanf(bdf, "%04x:%02x:%02x.%d", &domain, &b, &d, &f); n == 4 {
		return uint32(domain)<<16 | uint32(b)<<8 | uint32(d)<<3 | uint32(f)
	} else {
		return 0
	}
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
	sort.SliceStable(lms, func(i, j int) bool { return bdf2u32(lms[i].GetUspBdf()) < bdf2u32(lms[j].GetUspBdf()) })
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
	if err := validateLinkTarget(cfg.GetLinkTarget()); err != nil {
		return err
	}

	pci.Init()
	defer pci.Cleanup()

	// Gets a list of PCI devices.
	devs := pciScanDevices()
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

// validateLinkTarget returns an error if the link target sets no filter field
// and does not set all_links, so that a spec that lost its filters fails the
// test instead of silently selecting every link, or if any BDF in the link
// target is malformed, so that a typo fails the test instead of silently
// matching nothing. A nil target sets no filter field.
func validateLinkTarget(target *lmtpb.PcieLinkTarget) error {
	hasFilter := target != nil &&
		(target.UspVendorId != nil || target.UspDeviceId != nil || len(target.GetUspBdfs()) > 0 ||
			target.DspVendorId != nil || target.DspDeviceId != nil || len(target.GetDspBdfs()) > 0)
	if !hasFilter && !target.GetAllLinks() {
		return errors.New("link_target sets no filter field (usp_vendor_id, usp_device_id, usp_bdfs, dsp_vendor_id, dsp_device_id or dsp_bdfs) and all_links is not true; refusing to test every link")
	}
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
func OverrideUSPBDFs(cfg *lmtpb.LinkMargin, bdfs []string) error {
	if cfg == nil {
		return errors.New("nil LinkMargin config")
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
		cfg.LinkTarget = &lmtpb.PcieLinkTarget{}
	}
	cfg.LinkTarget.UspBdfs = usp
	return nil
}

// matchLinkTarget reports whether the link between the given USP and DSP
// matches the link target. The USP BDFs are not checked here, and the USP IDs
// are checked again only for completeness, because getLinks checks both before
// looking up the DSP.
//
// A nil target, or an unset field within the target, is a wildcard.
func matchLinkTarget(uspInfo, dspInfo pci.PCIDevInfo, target *lmtpb.PcieLinkTarget) bool {
	if target == nil {
		return true
	}
	return matchID(target.UspVendorId, uspInfo.VendorID) &&
		matchID(target.UspDeviceId, uspInfo.DeviceID) &&
		matchID(target.DspVendorId, dspInfo.VendorID) &&
		matchID(target.DspDeviceId, dspInfo.DeviceID) &&
		matchBDF(target.GetDspBdfs(), dspInfo)
}

// getLinks gets a list of PCIe ports according to the proto param.
func getLinks(devs pci.Dev, cfg *lmtpb.LinkMargin) ([]*linktest, error) {
	var err error
	const numLinks = 8 // estimated array-initial-size of links to be tested.
	lts = make([]*linktest, 0, numLinks)
	// Filters devices by Vid, Did, and/or Bus. Only downstream dev is selected.
	// This assumes dev number == 0, and func = 0.
	for dev := devs; dev.Valid(); dev = dev.GetNext() {
		d := dev.GetDevInfo()
		// Only tests the USPs that match the USP IDs and BDFs of link_target, if
		// it is specified. A non-matching device is skipped here, before its
		// config space is read and its DSP is looked up, so that an unrelated
		// device cannot fail the run. The DSP fields are checked after the DSP is
		// found below.
		if t := cfg.GetLinkTarget(); t != nil &&
			(!matchID(t.UspVendorId, d.VendorID) || !matchID(t.UspDeviceId, d.DeviceID) || !matchBDF(t.GetUspBdfs(), d)) {
			continue
		}
		pf0Chk := (d.Dev == 0) && (d.Func == 0)
		if pf0Chk {
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

			// Dynamically filter out non-physical, internal Switch operational bridges.
			// Base Class 0x06 (Bridge) in byte 0x0B, Sub-Class 0x80 (Other Bridge) in byte 0x0A.
			if pci.ReadByte(lt.usp.dev, 0x0B) == 0x06 && pci.ReadByte(lt.usp.dev, 0x0A) == 0x80 {
				log.Infof("[Notice] Dynamically skipping non-physical internal Switch bridge BDF %s based on PCI Sub-Class (0x06:0x80).",
					dev.BDFString())
				continue
			}

			// Both ends of the link must match link_target, if it is specified. This
			// disambiguates links whose USPs share the same <vendor_id, device_id>.
			if !matchLinkTarget(d, lt.dsp.dev.GetDevInfo(), cfg.GetLinkTarget()) {
				log.Infof("BDF:[%s]: Skipping the link to DSP %s, which does not match link_target.",
					dev.BDFString(), lt.dsp.dev.BDFString())
				continue
			}

			// Clones a result protobuf for the test config protobuf.
			lt.pb = proto.Clone(cfg).(*lmtpb.LinkMargin)
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
					p.gen = uint32(val & C.PCI_EXP_LNKSTA_SPEED)
					msg.WriteString(fmt.Sprintf(
						"Info: %s: PCIEXP CAP offset=%x; PCI_EXP_LNKSTA_WIDTH=%d; PCI_EXP_LNKSTA_SPEED=%d  | ",
						bdf, p.pcieCapOffset, p.width, p.gen))
					switch p.gen {
					case Speed16G:
						p.speed = 16.0e9
					case Speed32G:
						p.speed = 32.0e9
					case Speed64G:
						p.speed = 64.0e9
					default:
						log.V(1).Infoln(bdf, " speed %d is not above gen4. Skipped.", p.gen)
						p.speed = 0.0
						p.testReady = false
						lt.testReady = false
					}
				}

				if p.lmrAddr, err = p.getLMRcapability(); err != nil {
					p.testReady = false
					// The LMR is required at gen4 and above. If it's not found, it's like not a real link.
					// However, currently CX8 does not have LMR capability. We still want to margin the DSP.
					// lt.testReady = false
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
		ConfigSpace     = 0x1000
		CapabilityStart = 0x100
		CapabilityMask  = 0xFFFF
		AddrMask        = 0x0FFC
		NextPos         = 20
	)
	var been [ConfigSpace]bool
	for addr := int32(CapabilityStart); addr != 0; {
		if addr&3 != 0 {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (unaligned pointer)", addr, p.dev.BDFString())
		}
		hdr := pci.ReadLong(p.dev, addr)
		if hdr == 0xFFFFFFFF {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (read 0xFFFFFFFF)", addr, p.dev.BDFString())
		}
		// An empty header (0x0) indicates no extended capabilities or end of the capability list.
		if hdr == 0 {
			break
		}
		if (hdr & CapabilityMask) == C.PCI_EXT_CAP_ID_LMR {
			return addr, nil
		}
		been[addr] = true
		addr = int32((hdr >> NextPos) & AddrMask)
		if been[addr] {
			return 0, fmt.Errorf("Capability chain loops at 0x%x on device %s", addr, p.dev.BDFString())
		}
	}
	return 0, fmt.Errorf("LMR capability header not found on device %s", p.dev.BDFString())
}

// getPcieCapOffset scans the PCI capability linked list for PCIe CAP.
// Refers to pciutils/ls-caps.c
func getPcieCapOffset(dev pci.Dev) (int32, error) {
	const (
		ConfigSpace     = 0x100 // The base config space is 256B.
		CapabilityStart = C.PCI_CAPABILITY_LIST
		CapabilityMask  = 0x00FF
		AddrMask        = 0x00FC
		NextPos         = 8
	)
	// Tracks if a loop occur in the linked list.
	var been [ConfigSpace]bool
	for addr := int32(pci.ReadByte(dev, CapabilityStart)); addr != 0; {
		if addr&3 != 0 {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (unaligned pointer)", addr, dev.BDFString())
		}
		hdr := pci.ReadWord(dev, addr)
		if hdr == 0xFFFF {
			return 0, fmt.Errorf("Capability chain broken at 0x%x on device %s (read 0xFFFF)", addr, dev.BDFString())
		}
		if (hdr & CapabilityMask) == C.PCI_CAP_ID_EXP {
			return addr, nil
		}
		been[addr] = true
		addr = int32((hdr >> NextPos) & AddrMask)
		if been[addr] {
			return 0, fmt.Errorf("Capability chain loops at 0x%x on device %s", addr, dev.BDFString())
		}
	}
	return 0, fmt.Errorf("PCIe capability header not found on device %s", dev.BDFString())
}
