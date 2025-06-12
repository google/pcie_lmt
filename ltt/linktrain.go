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
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/golang/glog"
	structpb "google.golang.org/protobuf/types/known/structpb"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
	pb "ltt_go.proto"
	pci "pciutils"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	ocppb "ocpdiag/results_go_proto"
)

const (
	numLinks = 8 // Estimated number of links to be tested. 8 is usually enough.
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

// A Linktest has everything needed to test a link.
type Linktest struct {
	usp, dsp         pci.Dev
	dspPCIeCapOffset int32
	waitTime         time.Duration // Wait time per training iteration
	info             string
	Cfg              *pb.LinkTrain
	chks             []*pb.LinkTrain_PciConfigField
	logs             []*pb.LinkTrain_PciConfigField
	recs             []*pb.LinkTrain_PciConfigField
	Pass             bool
	// PciLocation      *pci.PCIDevInfo
	hwinfo           string // OCP hardware_info_id
	seriesID         []string
	seriesCnt        []int32
}

var (
	cfgfn string // linktrain pbtxt filename
	// Lts is link training test results
	Lts []*Linktest
	// Teardownwait is the wait time before restarting the link training.
	// The default 200ms is used by the AMDXIO tool.
	Teardownwait = 200 * time.Millisecond
)

// //////////////////////////////////////////////////////////////////////////////
// Issues a Link Retrain.
func (lt Linktest) retrain() {
	dsp := lt.dsp
	addr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKCTL
	val := pci.ReadWord(dsp, addr)
	pci.WriteWord(dsp, addr, val|C.PCI_EXP_LNKCTL_RETRAIN)
	time.Sleep(lt.waitTime)
}

// Issues a Secondary Bus Reset on a link.
func (lt Linktest) secondaryBusReset() {
	dsp := lt.dsp

	// Sets and clears the Secondary Bus Reset bit.
	const sbrmsk = 0x0040
	val := pci.ReadWord(dsp, C.PCI_BRIDGE_CONTROL)
	pci.WriteWord(dsp, C.PCI_BRIDGE_CONTROL, val|sbrmsk)
	// readback to ensure effective write.
	rdbk := pci.ReadWord(dsp, C.PCI_BRIDGE_CONTROL)
	log.V(2).Infoln(fmt.Sprintf("%s PCI_BRIDGE_CONTROL=0x%04x", lt.Cfg.GetUspBdf(), rdbk))
	time.Sleep(Teardownwait)
	pci.WriteWord(dsp, C.PCI_BRIDGE_CONTROL, val)
	// readback to ensure effective write.
	rdbk = pci.ReadWord(dsp, C.PCI_BRIDGE_CONTROL)
	log.V(2).Infoln(fmt.Sprintf("%s PCI_BRIDGE_CONTROL=0x%04x", lt.Cfg.GetUspBdf(), rdbk))
	time.Sleep(lt.waitTime)
}

// Disables and re-enables a link.
func (lt Linktest) reenable() {
	dsp := lt.dsp
	addr := lt.dspPCIeCapOffset + C.PCI_EXP_LNKCTL
	val := pci.ReadWord(dsp, addr)
	pci.WriteWord(dsp, addr, val|C.PCI_EXP_LNKCTL_DISABLE)
	// readback to ensure effective write.
	lnkctl := pci.ReadWord(dsp, addr)
	log.V(2).Infoln(fmt.Sprintf("%s PCI_EXP_LNKCTL=0x%04x", lt.Cfg.GetUspBdf(), lnkctl))
	time.Sleep(Teardownwait)
	pci.WriteWord(dsp, addr, val)
	// readback to ensure effective write.
	lnkctl = pci.ReadWord(dsp, addr)
	log.V(2).Infoln(fmt.Sprintf("%s PCI_EXP_LNKCTL=0x%04x", lt.Cfg.GetUspBdf(), lnkctl))
	time.Sleep(lt.waitTime)
}

// getPCIeCapOffset scans the PCI capability linked list for PCIe CAP.
// Refers to google3/third_party/pciutils/ls-caps.c;rcl=357071835;l=1675
var getPCIeCapOffset = func(dev pci.Dev) (int32, error) {
	const (
		configSpace     = int32(0x100) // The base config space is 256B
		capabilityStart = int32(C.PCI_CAPABILITY_LIST)
		capabilityMask  = int32(0x00FF)
		addrMask        = int32(0x0FFC)
		nextPos         = int(8)
	)
	// Tracks if a loop occur in the linked list.
	var been [configSpace]bool
	for addr := int32(pci.ReadByte(dev, capabilityStart)); addr != 0; {
		hdr := int32(pci.ReadWord(dev, addr))
		if (hdr & capabilityMask) == int32(C.PCI_CAP_ID_EXP) {
			return addr, nil
		}
		been[addr] = true
		addr = (hdr >> nextPos) & addrMask
		if been[addr] {
			return 0, fmt.Errorf("Capability chain loops at 0x%x", addr)
		}
	}
	return 0, fmt.Errorf("PCIe capability header not found")
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

// Gets a list of links according to the proto spec.
func getLinks(devs pci.Dev, cfg *pb.LinkTrain) ([]*Linktest, error) {
	linktests := make([]*Linktest, 0, numLinks)
	for dev := devs; dev.Valid(); dev = dev.GetNext() {
		d := dev.GetDevInfo()
		vidChk := cfg.VendorId == nil || uint32(d.VendorID) == cfg.GetVendorId()
		didChk := cfg.DeviceId == nil || uint32(d.DeviceID) == cfg.GetDeviceId()
		bdfChk := len(cfg.GetBdf()) == 0 || slices.Contains(cfg.GetBdf(), fmt.Sprintf("%04x:%02x:%02x.%d", d.Domain, d.Bus, d.Dev, d.Func))
		pf0Chk := (d.Dev == 0) && (d.Func == 0)
		if vidChk && didChk && bdfChk && pf0Chk {
			// Checks the PCIe port type. Only an endpoint or a switch upstream port
			// are eligible for training test.
			if offset, err := getPCIeCapOffset(dev); err != nil {
				// If there's any error getting the PCIe capability offset, the device
				// is to be excluded from testing.
				log.Warningf("A matching device failed to get the PCIe Capability offset: %v. Error: %s", dev, err.Error())
				continue
			} else {
				portType := pci.ReadWord(dev, offset+C.PCI_EXP_FLAGS) & C.PCI_EXP_FLAGS_TYPE
				portType = portType >> 4
				if portType != C.PCI_EXP_TYPE_ENDPOINT && portType != C.PCI_EXP_TYPE_UPSTREAM {
					continue
				}
			}

			var lt Linktest
			var dsp pci.Dev
			var offset int32
			var err error
			if dsp, err = dev.FindDSP(); err != nil {
				return linktests, err
			}
			if offset, err = getPCIeCapOffset(dsp); err != nil {
				return linktests, err
			}
			lt.usp = dev
			lt.dsp = dsp
			lt.dspPCIeCapOffset = offset
			lt.Cfg = proto.Clone(cfg).(*pb.LinkTrain)
			lt.chks = filterFields(lt.Cfg.GetField(), pb.LinkTrain_PciConfigField_S_CHECK)
			lt.logs = filterFields(lt.Cfg.GetField(), pb.LinkTrain_PciConfigField_S_LOG)
			lt.recs = filterFields(lt.Cfg.GetField(), pb.LinkTrain_PciConfigField_S_RECOVER)
			vendorID := uint32(d.VendorID)
			lt.Cfg.VendorId = &vendorID
			deviceID := uint32(d.DeviceID)
			lt.Cfg.DeviceId = &deviceID
			lt.Cfg.Bdf = ([]string{dev.BDFString()})
			uspBdf := lt.usp.BDFString()
			lt.Cfg.UspBdf = &uspBdf
			dspBdf := lt.dsp.BDFString()
			lt.Cfg.DspBdf = &dspBdf
			passCnt := int32(0)
			lt.Cfg.PassCount = &passCnt
			failCnt := int32(0)
			lt.Cfg.FailCount = &failCnt
			lt.waitTime = time.Duration(lt.Cfg.GetTrainingWaitMs()) * time.Millisecond
			lt.Pass = false
			// lt.PciLocation = pci.PCIDevInfo{}.Build()
			// lt.PciLocation.SetDomain(int32(d.Domain))
			// lt.PciLocation.SetBus(int32(d.Bus))
			// lt.PciLocation.SetDev(int32(d.Dev))
			// lt.PciLocation.SetFunc(int32(d.Func))
			linktests = append(linktests, &lt)
		}
	}
	return linktests, nil
}

// Checks config registers of an USP against the proto spec.
func (lt *Linktest) check() bool {
	usp := lt.usp
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
				log.Errorf("The %s checking does not have an expected value.", f.GetName())
			}
			f.State = pb.LinkTrain_PciConfigField_S_ERROR
			pass = false
		} else {
			if (f.GetMask() & f.GetExpected()) != (f.GetMask() & v) {
				pass = false
			}
			if f.GetState() == pb.LinkTrain_PciConfigField_S_CHECK || f.GetState() == pb.LinkTrain_PciConfigField_S_PASS {
				v = f.GetMask() & v
				f.Val = &v
				if pass {
					f.State = pb.LinkTrain_PciConfigField_S_PASS
				} else {
					f.State = pb.LinkTrain_PciConfigField_S_ERROR
				}
			}
		}
	}
	return pass
}

// Logs config register values of an USP according to the proto spec.
func (lt *Linktest) log() {
	usp := lt.usp
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

// record records config register values of an USP according to the proto spec.
func (lt *Linktest) record() {
	usp := lt.usp
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
	usp := lt.usp
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

// Trains one link iteratively.
func trainLoop(lt *Linktest) {
	cfg := lt.Cfg
	itr := int(cfg.GetIterations())

	// Records the fields to be restored.
	lt.record()

	// OCP TestStepStart
	lt.hwinfo = lt.usp.BDFString()
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
				log.Errorf("The %s checking does not have an expected value.", f.GetName())
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
	if !lt.check() {
		lt.log()
		failCnt := cfg.GetFailCount() + 1
		cfg.FailCount = &failCnt
		lt.Pass = false
		log.Warningf("Initial checking failed. %v", lt)

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
		switch lt.Cfg.GetMethod() {
		case pb.LinkTrain_M_RETRAIN_DEFAULT:
			lt.retrain()
		case pb.LinkTrain_M_SBR:
			lt.secondaryBusReset()
		case pb.LinkTrain_M_REENABLE:
			lt.reenable()
		}
		if !lt.check() {
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
		log.V(1).Infoln(fmt.Sprintf("BDF:%s: Iteration:%d; Pass:%d; Fail:%d",
			cfg.GetUspBdf(), i, cfg.GetPassCount(), cfg.GetFailCount()))
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
func ocpTestRunStart(cfg *pb.LinkTrain) {
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
			HardwareInfoId: lt.dsp.BDFString(),
			Name:           "DSP",
		}
		dutInfo.HardwareInfos = append(dutInfo.HardwareInfos, hwInfo)

		hwInfo = &ocppb.HardwareInfo{
			HardwareInfoId: lt.usp.BDFString(),
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
	pci.Init()
	defer pci.Cleanup()

	var err error

	// Gets a list of PCI devices.
	devs := pci.ScanDevices()
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
