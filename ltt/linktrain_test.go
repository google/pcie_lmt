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

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	ocppb "ocpdiag/results_go_proto"

	pb "ltt_go.proto"
	pci "pciutils"
)

func TestFilterFields(t *testing.T) {
	fields := []*pb.LinkTrain_PciConfigField{
		{Name: "field1", State: pb.LinkTrain_PciConfigField_S_CHECK},
		{Name: "field2", State: pb.LinkTrain_PciConfigField_S_LOG},
		{Name: "field3", State: pb.LinkTrain_PciConfigField_S_CHECK},
		{Name: "field4", State: pb.LinkTrain_PciConfigField_S_RECOVER},
	}
	testCases := []struct {
		name  string
		state pb.LinkTrain_PciConfigField_StateEnum
		want  []*pb.LinkTrain_PciConfigField
	}{
		{
			name:  "filter for check",
			state: pb.LinkTrain_PciConfigField_S_CHECK,
			want: []*pb.LinkTrain_PciConfigField{
				{Name: "field1", State: pb.LinkTrain_PciConfigField_S_CHECK},
				{Name: "field3", State: pb.LinkTrain_PciConfigField_S_CHECK},
			},
		},
		{
			name:  "filter for log",
			state: pb.LinkTrain_PciConfigField_S_LOG,
			want: []*pb.LinkTrain_PciConfigField{
				{Name: "field2", State: pb.LinkTrain_PciConfigField_S_LOG},
			},
		},
		{
			name:  "filter for recover",
			state: pb.LinkTrain_PciConfigField_S_RECOVER,
			want: []*pb.LinkTrain_PciConfigField{
				{Name: "field4", State: pb.LinkTrain_PciConfigField_S_RECOVER},
			},
		},
		{
			name:  "filter for pass",
			state: pb.LinkTrain_PciConfigField_S_PASS,
			want:  []*pb.LinkTrain_PciConfigField{},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterFields(fields, tc.state)
			if diff := cmp.Diff(tc.want, got, protocmp.Transform()); diff != "" {
				t.Errorf("filterFields(%v, %v) returned diff (-want +got):\n%s", fields, tc.state, diff)
			}
		})
	}
}

func TestReadLinkTrainProto(t *testing.T) {
	content := `
iterations: 10
training_wait_ms: 100
link_target: {
	usp_vendor_id: 0x1ae0
	usp_device_id: 0x0060
	usp_bdfs: ["0000:1a:00.0", "0000:39:00.0", "0000:58:00.0", "0000:77:00.0"]
}
field: {
	name: "LnkSta"
	addr: 0x1a2
	size: UINT16
	mask: 0x000f
	expected: 4
	state: S_CHECK
}
`
	dir := t.TempDir()
	fn := filepath.Join(dir, "test.pbtxt")
	if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}
	want := &pb.LinkTrain{
		Iterations:     10,
		TrainingWaitMs: 100,
		LinkTarget: &pb.PcieLinkTarget{
			UspVendorId: proto.Uint32(0x1ae0),
			UspDeviceId: proto.Uint32(0x0060),
			UspBdfs:     []string{"0000:1a:00.0", "0000:39:00.0", "0000:58:00.0", "0000:77:00.0"},
		},
		Field: []*pb.LinkTrain_PciConfigField{
			{
				Name:     "LnkSta",
				Addr:     0x1a2,
				Size:     pb.LinkTrain_PciConfigField_UINT16,
				Mask:     0x000f,
				Expected: proto.Uint32(4),
				State:    pb.LinkTrain_PciConfigField_S_CHECK,
			},
		},
	}
	got, err := ReadLinkTrainProto(fn)
	if err != nil {
		t.Errorf("ReadLinkTrainProto(%q) failed: %v", fn, err)
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("ReadLinkTrainProto(%q) returned diff (-want +got):\n%s", fn, diff)
	}
}

func TestReadLinkTrainProtoErrors(t *testing.T) {
	t.Run("FileNotFound", func(t *testing.T) {
		_, err := ReadLinkTrainProto(filepath.Join(t.TempDir(), "nonexistent.pbtxt"))
		if err == nil {
			t.Errorf("ReadLinkTrainProto() for nonexistent file succeeded, want error")
		}
	})

	t.Run("MalformedContent", func(t *testing.T) {
		content := `iterations: 10a` // malformed pbtxt
		dir := t.TempDir()
		fn := filepath.Join(dir, "malformed.pbtxt")
		if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
		_, err := ReadLinkTrainProto(fn)
		if err == nil {
			t.Errorf("ReadLinkTrainProto(%q) succeeded with malformed content, want error", fn)
		}
	})

	// The removed USP filters must fail to parse rather than be silently
	// dropped, which would select every link.
	for field, content := range map[string]string{
		"vendor_id": `vendor_id: 0x1ae0`,
		"device_id": `device_id: 0x0075`,
		"bdf":       `bdf: "0000:16:00.0"`,
	} {
		t.Run("RemovedField_"+field, func(t *testing.T) {
			fn := filepath.Join(t.TempDir(), "legacy.pbtxt")
			if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
				t.Fatalf("Failed to write test file: %v", err)
			}
			if got, err := ReadLinkTrainProto(fn); err == nil {
				t.Errorf("ReadLinkTrainProto() with %q = %v, want error", content, got)
			}
		})
	}
}

func TestWriteResultPbtxt(t *testing.T) {
	Lts = []*Linktest{
		{
			Cfg: &pb.LinkTrain{
				UspBdf: proto.String("0000:01:00.0"),
				DspBdf: proto.String("0000:02:00.0"),
			},
		},
		{
			Cfg: &pb.LinkTrain{
				UspBdf: proto.String("0000:03:00.0"),
				DspBdf: proto.String("0000:04:00.0"),
			},
		},
	}
	dir := t.TempDir()
	outfn := filepath.Join(dir, "output.pbtxt")
	err := WriteResultPbtxt(outfn)
	if err != nil {
		t.Errorf("WriteResultPbtxt(%q) failed: %v", outfn, err)
	}
	data, err := os.ReadFile(outfn)
	if err != nil {
		t.Fatalf("Failed to read output file: %v", err)
	}
	got := &pb.LinkTrainTests{}
	if err := prototext.Unmarshal(data, got); err != nil {
		t.Fatalf("Failed to unmarshal output file: %v", err)
	}
	want := &pb.LinkTrainTests{
		LinkTrain: []*pb.LinkTrain{
			{
				UspBdf: proto.String("0000:01:00.0"),
				DspBdf: proto.String("0000:02:00.0"),
			},
			{
				UspBdf: proto.String("0000:03:00.0"),
				DspBdf: proto.String("0000:04:00.0"),
			},
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("WriteResultPbtxt(%q) produced diff (-want +got):\n%s", outfn, diff)
	}
}

func TestWriteResultPbtxtErrors(t *testing.T) {
	t.Run("write to invalid path", func(t *testing.T) {
		Lts = []*Linktest{}
		err := WriteResultPbtxt("/no/such/dir/output.pbtxt")
		if err == nil {
			t.Errorf("WriteResultPbtxt() with bad path succeeded, want error")
		}
	})

	t.Run("marshal error", func(t *testing.T) {
		Lts = []*Linktest{
			{
				Cfg: &pb.LinkTrain{
					UspBdf: proto.String(string([]byte{0xff, 0xfe, 0xfd})),
				},
			},
		}
		dir := t.TempDir()
		outfn := filepath.Join(dir, "output.pbtxt")
		err := WriteResultPbtxt(outfn)
		if err == nil {
			t.Errorf("WriteResultPbtxt() with data that causes marshal error succeeded, want error")
		}
	})
}

func TestOcpInit(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()

	OcpInit(w, "test_name", "1.0", "test_cmd")
	if ocpPipe != w {
		t.Errorf("OcpInit() failed to set ocpPipe")
	}
	if testRunStart.GetName() != "test_name" || testRunStart.GetVersion() != "1.0" || testRunStart.GetCommandLine() != "test_cmd" {
		t.Errorf("OcpInit() failed to set testRunStart: got %v", testRunStart)
	}
	if seqNum.Load() != 0 {
		t.Errorf("OcpInit() failed to reset seqNum: got %d", seqNum.Load())
	}
}

func TestOutputArtifact(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}

	OcpInit(w, "test_name", "1.0", "test_cmd")
	artiOut := &ocppb.OutputArtifact{
		Artifact: &ocppb.OutputArtifact_SchemaVersion{SchemaVersion: &ocppb.SchemaVersion{Major: 1, Minor: 2}},
	}

	done := make(chan struct{})
	var readData []byte
	var readErr error
	go func() {
		readData, readErr = io.ReadAll(r)
		close(done)
	}()

	outputArtifact(artiOut)
	w.Close() // Close writer to unblock ReadAll

	<-done
	r.Close()

	if readErr != nil {
		t.Fatalf("Failed to read from pipe: %v", readErr)
	}
	if len(readData) == 0 {
		t.Errorf("outputArtifact() wrote no data")
	}
	if !strings.Contains(string(readData), `"schemaVersion"`) {
		t.Errorf("outputArtifact() didn't write expected data: %s", string(readData))
	}
	if artiOut.SequenceNumber != 1 {
		t.Errorf("outputArtifact() failed to set sequence number: got %d, want 1", artiOut.SequenceNumber)
	}
}

func TestLinkTrainNoPciDevices(t *testing.T) {
	oldPciScanDevices := pciScanDevices
	pciScanDevices = func() pci.Dev { return pci.Dev{} }
	defer func() { pciScanDevices = oldPciScanDevices }()
	_, err := LinkTrain(&pb.LinkTrain{LinkTarget: &pb.PcieLinkTarget{AllLinks: proto.Bool(true)}})
	if err == nil || err.Error() != "no pcie devices found" {
		t.Errorf("LinkTrain() with no pci devices returned %v, want error 'no pcie devices found'", err)
	}
}

func TestLinkTrainNoLinksFound(t *testing.T) {
	cfg := &pb.LinkTrain{
		LinkTarget: &pb.PcieLinkTarget{
			UspVendorId: proto.Uint32(0xffff),
			UspDeviceId: proto.Uint32(0xffff),
		},
	}
	_, err := LinkTrain(cfg)
	if err == nil || err.Error() != "no downstream device matches the proto spec" {
		// If error is "no pcie devices found", it means test couldn't work because no devices were scanned.
		if err != nil && err.Error() == "no pcie devices found" {
			t.Skipf("Skipping test: pci.ScanDevices() found no devices, cannot test 'no links found' condition.")
		}
		t.Errorf("LinkTrain() with VID/DID 0xFFFF returned err: %v, want 'no downstream device matches the proto spec'", err)
	}
}

func TestLinkTrainRuns(t *testing.T) {
	oldGetLinks := getLinks
	oldTrainLoop := trainLoop
	oldOcpTestRunStart := ocpTestRunStart
	trainLoopCalled := false
	getLinks = func(devs pci.Dev, cfg *pb.LinkTrain) ([]*Linktest, error) {
		return []*Linktest{{
			Cfg: &pb.LinkTrain{Parallel: proto.Bool(true)},
		}}, nil
	}
	trainLoop = func(lt *Linktest) {
		trainLoopCalled = true
		lt.Pass = true
	}
	ocpTestRunStart = func(cfg *pb.LinkTrain) { testRunEnd = &ocppb.TestRunEnd{} } // Mock to prevent panic
	defer func() {
		getLinks = oldGetLinks
		trainLoop = oldTrainLoop
		ocpTestRunStart = oldOcpTestRunStart
		testRunEnd = nil
	}()

	pass, err := LinkTrain(&pb.LinkTrain{LinkTarget: &pb.PcieLinkTarget{AllLinks: proto.Bool(true)}})
	if err != nil && err.Error() == "no pcie devices found" {
		t.Skipf("Skipping test: pci.ScanDevices() found no devices, cannot test full LinkTrain run.")
	}
	if err != nil {
		t.Errorf("LinkTrain() returned unexpected error: %v", err)
	}
	if !pass {
		t.Errorf("LinkTrain() returned pass=false, want true")
	}
	if !trainLoopCalled {
		t.Errorf("LinkTrain() did not call trainLoop")
	}
}

func TestGetSriovCapOffset(t *testing.T) {
	origReadLong := pciReadLong
	defer func() { pciReadLong = origReadLong }()

	testCases := []struct {
		name       string
		configMem  map[int32]uint32
		wantOffset int32
		wantErrSub string
	}{
		{
			name: "SR-IOV found at 0x100 (first ext cap)",
			configMem: map[int32]uint32{
				PcieExtCapStart: 0x00000010, // SR-IOV ID (0x0010), Next = 0
			},
			wantOffset: PcieExtCapStart,
		},
		{
			name: "SR-IOV found at 0x180 via chain traversal",
			configMem: map[int32]uint32{
				PcieExtCapStart: (0x180 << 20) | 0x0001, // AER ID (0x0001), Next = 0x180
				0x180:           0x00000010,             // SR-IOV ID (0x0010), Next = 0
			},
			wantOffset: 0x180,
		},
		{
			name: "SR-IOV not present, clean termination",
			configMem: map[int32]uint32{
				PcieExtCapStart: 0x00000001, // AER ID (0x0001), Next = 0
			},
			wantOffset: 0,
		},
		{
			name: "Capability chain loop detected",
			configMem: map[int32]uint32{
				PcieExtCapStart: (0x140 << 20) | 0x0001, // AER ID, Next = 0x140
				0x140:           (0x100 << 20) | 0x0002, // VC ID, Next = 0x100 (loop!)
			},
			wantErrSub: "Capability chain loops at 0x100",
		},
		{
			name: "Link down 0xFFFFFFFF on Cap Header",
			configMem: map[int32]uint32{
				PcieExtCapStart: 0xFFFFFFFF,
			},
			wantErrSub: "read 0xFFFFFFFF",
		},
		{
			name: "Unaligned pointer in chain",
			configMem: map[int32]uint32{
				PcieExtCapStart: (0x141 << 20) | 0x0001, // Next = 0x141 (unaligned)
			},
			wantErrSub: "unaligned pointer",
		},
		{
			name: "Offset out of range below 0x100",
			configMem: map[int32]uint32{
				PcieExtCapStart: (0x080 << 20) | 0x0001, // Next = 0x080 (< 0x100)
			},
			wantErrSub: "offset out of range",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pciReadLong = func(dev pci.Dev, addr int32) uint32 {
				if val, ok := tc.configMem[addr]; ok {
					return val
				}
				return 0
			}

			gotOffset, err := getSriovCapOffset(pci.Dev{})
			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("getSriovCapOffset() expected error containing %q, got nil", tc.wantErrSub)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Errorf("getSriovCapOffset() error = %v, want substring %q", err, tc.wantErrSub)
				}
			} else {
				if err != nil {
					t.Fatalf("getSriovCapOffset() unexpected error: %v", err)
				}
				if gotOffset != tc.wantOffset {
					t.Errorf("getSriovCapOffset() = 0x%x, want 0x%x", gotOffset, tc.wantOffset)
				}
			}
		})
	}
}

func TestGetPCIeExtCapOffset(t *testing.T) {
	origReadLong := pciReadLong
	defer func() { pciReadLong = origReadLong }()

	configMem := map[int32]uint32{
		PcieExtCapStart: (0x140 << 20) | 0x0001, // AER ID, Next = 0x140
		0x140:           (0x180 << 20) | 0x0010, // SR-IOV ID, Next = 0x180
		0x180:           (0x000 << 20) | 0x0019, // Secondary PCIe ID, Next = 0
	}
	pciReadLong = func(dev pci.Dev, addr int32) uint32 {
		if val, ok := configMem[addr]; ok {
			return val
		}
		return 0
	}

	testCases := []struct {
		name       string
		extCapID   uint16
		wantOffset int32
	}{
		{"Find AER at 0x100", 0x0001, 0x100},
		{"Find SR-IOV at 0x140", 0x0010, 0x140},
		{"Find Secondary PCIe at 0x180", 0x0019, 0x180},
		{"Not found DSN (0x0003)", 0x0003, 0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := getPCIeExtCapOffset(pci.Dev{}, tc.extCapID)
			if err != nil {
				t.Fatalf("getPCIeExtCapOffset(0x%x) unexpected error: %v", tc.extCapID, err)
			}
			if got != tc.wantOffset {
				t.Errorf("getPCIeExtCapOffset(0x%x) = 0x%x, want 0x%x", tc.extCapID, got, tc.wantOffset)
			}
		})
	}
}

func TestExtendLinkTrainFields(t *testing.T) {
	origGetSriovCapOffset := getSriovCapOffset
	defer func() { getSriovCapOffset = origGetSriovCapOffset }()

	t.Run("SriovRecoveryEnabled_Found", func(t *testing.T) {
		getSriovCapOffset = func(dev pci.Dev) (int32, error) {
			return 0x200, nil
		}

		cfg := &pb.LinkTrain{
			RecoverSriovCapRegs: true,
		}
		if err := extendLinkTrainFields(pci.Dev{}, cfg, 0x40); err != nil {
			t.Fatalf("extendLinkTrainFields() unexpected error: %v", err)
		}

		if len(cfg.GetField()) != 16 {
			t.Fatalf("len(cfg.GetField()) = %d, want 16", len(cfg.GetField()))
		}

		for i := 0; i < 16; i++ {
			offset := uint32(i * 4)
			field := cfg.GetField()[i]
			wantName := fmt.Sprintf("SRIOVCapDword%02X", offset)
			if field.GetName() != wantName {
				t.Errorf("field[%d].Name = %q, want %q", i, field.GetName(), wantName)
			}
			if field.GetAddr() != 0x200+offset {
				t.Errorf("field[%d].Addr = 0x%x, want 0x%x", i, field.GetAddr(), 0x200+offset)
			}
			if field.GetState() != pb.LinkTrain_PciConfigField_S_RECOVER {
				t.Errorf("field[%d].State = %v, want S_RECOVER", i, field.GetState())
			}
			if field.GetSize() != pb.LinkTrain_PciConfigField_UINT32 {
				t.Errorf("field[%d].Size = %v, want UINT32", i, field.GetSize())
			}
			if field.GetMask() != 0xFFFFFFFF {
				t.Errorf("field[%d].Mask = 0x%x, want 0xFFFFFFFF", i, field.GetMask())
			}
		}
	})

	t.Run("SriovRecoveryEnabled_NotFound", func(t *testing.T) {
		getSriovCapOffset = func(dev pci.Dev) (int32, error) {
			return 0, nil
		}

		cfg := &pb.LinkTrain{
			RecoverSriovCapRegs: true,
		}
		if err := extendLinkTrainFields(pci.Dev{}, cfg, 0x40); err != nil {
			t.Fatalf("extendLinkTrainFields() unexpected error: %v", err)
		}

		if len(cfg.GetField()) != 0 {
			t.Errorf("len(cfg.GetField()) = %d, want 0 for clean no-op", len(cfg.GetField()))
		}
	})

	t.Run("SriovRecoveryDisabled", func(t *testing.T) {
		called := false
		getSriovCapOffset = func(dev pci.Dev) (int32, error) {
			called = true
			return 0x200, nil
		}

		cfg := &pb.LinkTrain{
			RecoverSriovCapRegs: false,
		}
		if err := extendLinkTrainFields(pci.Dev{}, cfg, 0x40); err != nil {
			t.Fatalf("extendLinkTrainFields() unexpected error: %v", err)
		}

		if called {
			t.Errorf("getSriovCapOffset() was called when recover_sriov_cap_regs is false")
		}
		if len(cfg.GetField()) != 0 {
			t.Errorf("len(cfg.GetField()) = %d, want 0", len(cfg.GetField()))
		}
	})

	t.Run("SriovRecoveryEnabled_ErrorPropagated", func(t *testing.T) {
		getSriovCapOffset = func(dev pci.Dev) (int32, error) {
			return 0, fmt.Errorf("simulated capability loop error")
		}

		cfg := &pb.LinkTrain{
			RecoverSriovCapRegs: true,
		}
		err := extendLinkTrainFields(pci.Dev{}, cfg, 0x40)
		if err == nil {
			t.Fatal("extendLinkTrainFields() expected error, got nil")
		}
		if !strings.Contains(err.Error(), "simulated capability loop error") {
			t.Errorf("extendLinkTrainFields() error = %v, want simulated error", err)
		}
		if !strings.Contains(err.Error(), "failed to get SR-IOV Extended Capability Structure's start offset") {
			t.Errorf("extendLinkTrainFields() error = %v, want context 'failed to get SR-IOV Extended Capability Structure's start offset'", err)
		}
	})

	t.Run("MultiFlagRecovery", func(t *testing.T) {
		getSriovCapOffset = func(dev pci.Dev) (int32, error) {
			return 0x180, nil
		}

		cfg := &pb.LinkTrain{
			RecoverPciConfigSpaceHeader: true,
			RecoverPcieCapCtrlRegs:      true,
			RecoverSriovCapRegs:         true,
		}
		if err := extendLinkTrainFields(pci.Dev{}, cfg, 0x40); err != nil {
			t.Fatalf("extendLinkTrainFields() unexpected error: %v", err)
		}

		// 16 DWORDs for base header (0x40 / 4) + 7 ctrl regs + 16 DWORDs for SR-IOV = 39 fields
		const wantTotal = 16 + 7 + 16
		if len(cfg.GetField()) != wantTotal {
			t.Fatalf("len(cfg.GetField()) = %d, want %d", len(cfg.GetField()), wantTotal)
		}

		// First 16 are base config
		for i := 0; i < 16; i++ {
			if !strings.HasPrefix(cfg.GetField()[i].GetName(), "BaseConfigDword") {
				t.Errorf("field[%d] = %s, want BaseConfigDword prefix", i, cfg.GetField()[i].GetName())
			}
		}
		// Next 7 are PCIe ctrl regs
		for i := 16; i < 23; i++ {
			if !strings.HasPrefix(cfg.GetField()[i].GetName(), "Pcie") {
				t.Errorf("field[%d] = %s, want Pcie prefix", i, cfg.GetField()[i].GetName())
			}
		}
		// Last 16 are SR-IOV
		for i := 23; i < 39; i++ {
			if !strings.HasPrefix(cfg.GetField()[i].GetName(), "SRIOVCapDword") {
				t.Errorf("field[%d] = %s, want SRIOVCapDword prefix", i, cfg.GetField()[i].GetName())
			}
		}
	})
}

func TestMatchID(t *testing.T) {
	testCases := []struct {
		name string
		want *uint32
		got  uint16
		ok   bool
	}{
		{name: "unset is a wildcard", want: nil, got: 0x1ae0, ok: true},
		{name: "match", want: proto.Uint32(0x1ae0), got: 0x1ae0, ok: true},
		{name: "mismatch", want: proto.Uint32(0x8086), got: 0x1ae0, ok: false},
		{name: "set to zero matches only zero", want: proto.Uint32(0), got: 0x1ae0, ok: false},
		{name: "out of 16-bit range never matches", want: proto.Uint32(0x11ae0), got: 0x1ae0, ok: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchID(tc.want, tc.got); got != tc.ok {
				t.Errorf("matchID(%v, %#x) = %v, want %v", tc.want, tc.got, got, tc.ok)
			}
		})
	}
}

func TestMatchLinkTarget(t *testing.T) {
	// An example link: an endpoint USP 1ae0:0075 behind a CPU root port DSP
	// 8086:352a at 0000:15:01.0.
	usp := pci.PCIDevInfo{VendorID: 0x1ae0, DeviceID: 0x0075}
	dsp := pci.PCIDevInfo{VendorID: 0x8086, DeviceID: 0x352a, Bus: 0x15, Dev: 1}
	testCases := []struct {
		name   string
		target *pb.PcieLinkTarget
		want   bool
	}{
		{
			name:   "no link target matches any link",
			target: nil,
			want:   true,
		},
		{
			name:   "empty link target matches any link",
			target: &pb.PcieLinkTarget{},
			want:   true,
		},
		{
			name: "usp only matches",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0075),
			},
			want: true,
		},
		{
			name: "dsp only matches",
			target: &pb.PcieLinkTarget{
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
			},
			want: true,
		},
		{
			name: "both ends match",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0075),
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
			},
			want: true,
		},
		{
			name: "vendor only wildcard matches both ends",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				DspVendorId: proto.Uint32(0x8086),
			},
			want: true,
		},
		{
			name: "usp mismatches",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0076),
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
			},
			want: false,
		},
		{
			name: "dsp mismatches",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0075),
				DspVendorId: proto.Uint32(0x1ae0),
				DspDeviceId: proto.Uint32(0x0075),
			},
			want: false,
		},
		{
			name: "both ends mismatch",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1000),
				DspVendorId: proto.Uint32(0x1000),
			},
			want: false,
		},
		{
			name: "field set to zero is not a wildcard",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				DspDeviceId: proto.Uint32(0),
			},
			want: false,
		},
		{
			name: "dsp bdf and ids match",
			target: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				DspVendorId: proto.Uint32(0x8086),
				DspBdfs:     []string{"0000:37:01.0", "0000:15:01.0"},
			},
			want: true,
		},
		{
			name: "dsp ids match but dsp bdf mismatches",
			target: &pb.PcieLinkTarget{
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
				DspBdfs:     []string{"0000:37:01.0"},
			},
			want: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchLinkTarget(usp, dsp, tc.target); got != tc.want {
				t.Errorf("matchLinkTarget(%v, %v, %v) = %v, want %v", usp, dsp, tc.target, got, tc.want)
			}
		})
	}
}

func TestMatchBDF(t *testing.T) {
	dev := pci.PCIDevInfo{Domain: 0, Bus: 0xb8, Dev: 0, Func: 0}
	testCases := []struct {
		name string
		bdfs []string
		want bool
	}{
		{name: "empty list is a wildcard", bdfs: nil, want: true},
		{name: "exact match", bdfs: []string{"0000:b8:00.0"}, want: true},
		{name: "upper case hex matches", bdfs: []string{"0000:B8:00.0"}, want: true},
		{name: "any entry matches", bdfs: []string{"0000:16:00.0", "0000:b8:00.0"}, want: true},
		{name: "different bus", bdfs: []string{"0000:98:00.0"}, want: false},
		{name: "different domain", bdfs: []string{"0001:b8:00.0"}, want: false},
		{name: "different function", bdfs: []string{"0000:b8:00.1"}, want: false},
		{name: "malformed entry never matches", bdfs: []string{"b8:00.0"}, want: false},
		{name: "malformed entry is skipped", bdfs: []string{"b8:00.0", "0000:b8:00.0"}, want: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchBDF(tc.bdfs, dev); got != tc.want {
				t.Errorf("matchBDF(%q, %v) = %v, want %v", tc.bdfs, dev, got, tc.want)
			}
		})
	}
}

func TestValidateLinkTarget(t *testing.T) {
	testCases := []struct {
		name    string
		target  *pb.PcieLinkTarget
		wantErr bool
	}{
		{name: "nil target", target: nil, wantErr: true},
		{name: "empty target", target: &pb.PcieLinkTarget{}, wantErr: true},
		{name: "all_links false", target: &pb.PcieLinkTarget{AllLinks: proto.Bool(false)}, wantErr: true},
		{name: "all_links true", target: &pb.PcieLinkTarget{AllLinks: proto.Bool(true)}},
		{name: "usp_vendor_id only", target: &pb.PcieLinkTarget{UspVendorId: proto.Uint32(0x1ae0)}},
		{name: "usp_device_id only", target: &pb.PcieLinkTarget{UspDeviceId: proto.Uint32(0x0075)}},
		{name: "usp_bdfs only", target: &pb.PcieLinkTarget{UspBdfs: []string{"0000:16:00.0"}}},
		{name: "dsp_vendor_id only", target: &pb.PcieLinkTarget{DspVendorId: proto.Uint32(0x8086)}},
		{name: "dsp_device_id only", target: &pb.PcieLinkTarget{DspDeviceId: proto.Uint32(0x352a)}},
		{name: "dsp_bdfs only", target: &pb.PcieLinkTarget{DspBdfs: []string{"0000:15:01.0"}}},
		{name: "usp_vendor_id zero is a filter", target: &pb.PcieLinkTarget{UspVendorId: proto.Uint32(0)}},
		{
			name: "valid bdfs",
			target: &pb.PcieLinkTarget{
				UspBdfs: []string{"0000:16:00.0", "10000:e1:00.0"},
				DspBdfs: []string{"0000:15:01.0"},
			},
		},
		{
			name:    "usp bdf without domain",
			target:  &pb.PcieLinkTarget{UspBdfs: []string{"0000:16:00.0", "16:00.0"}},
			wantErr: true,
		},
		{
			name:    "dsp bdf without function",
			target:  &pb.PcieLinkTarget{DspBdfs: []string{"0000:15:01"}},
			wantErr: true,
		},
		{
			name:    "bus number only",
			target:  &pb.PcieLinkTarget{UspBdfs: []string{"16"}},
			wantErr: true,
		},
		{
			name:    "all_links true with malformed bdf",
			target:  &pb.PcieLinkTarget{AllLinks: proto.Bool(true), DspBdfs: []string{"15:01.0"}},
			wantErr: true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLinkTarget(tc.target)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("validateLinkTarget(%v) = %v, want error: %v", tc.target, err, tc.wantErr)
			}
		})
	}
}

func TestLinkTrainRejectsMalformedBDF(t *testing.T) {
	oldPciScanDevices := pciScanDevices
	defer func() { pciScanDevices = oldPciScanDevices }()
	pciScanDevices = func() pci.Dev {
		t.Error("LinkTrain() scanned the PCI devices despite a malformed BDF")
		return pci.Dev{}
	}
	cfg := &pb.LinkTrain{
		LinkTarget: &pb.PcieLinkTarget{DspBdfs: []string{"15:01.0"}},
	}
	_, err := LinkTrain(cfg)
	if err == nil || !strings.Contains(err.Error(), "malformed BDF") {
		t.Errorf("LinkTrain(%v) = %v, want a malformed BDF error", cfg, err)
	}
}

func TestLinkTrainRejectsEmptyLinkTarget(t *testing.T) {
	oldPciScanDevices := pciScanDevices
	defer func() { pciScanDevices = oldPciScanDevices }()
	pciScanDevices = func() pci.Dev {
		t.Error("LinkTrain() scanned the PCI devices despite an empty link_target")
		return pci.Dev{}
	}
	for name, cfg := range map[string]*pb.LinkTrain{
		"no link_target":    {},
		"empty link_target": {LinkTarget: &pb.PcieLinkTarget{}},
		"all_links false":   {LinkTarget: &pb.PcieLinkTarget{AllLinks: proto.Bool(false)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LinkTrain(cfg)
			if err == nil || !strings.Contains(err.Error(), "all_links") {
				t.Errorf("LinkTrain(%v) = %v, want an error about the missing filter and all_links", cfg, err)
			}
		})
	}
}

func TestOverrideUSPBDFs(t *testing.T) {
	spec := func() *pb.LinkTrain {
		return &pb.LinkTrain{
			LinkTarget: &pb.PcieLinkTarget{UspBdfs: []string{"0000:16:00.0"}},
		}
	}
	overridden := func(bdfs ...string) *pb.LinkTrain {
		return &pb.LinkTrain{LinkTarget: &pb.PcieLinkTarget{UspBdfs: bdfs}}
	}
	testCases := []struct {
		name    string
		cfg     *pb.LinkTrain
		bdfs    []string
		want    *pb.LinkTrain
		wantErr string // A substring of the error; empty means no error.
	}{
		{
			name: "creates the link target",
			cfg:  &pb.LinkTrain{Iterations: 10},
			bdfs: []string{"0000:38:00.0"},
			want: &pb.LinkTrain{
				Iterations: 10,
				LinkTarget: &pb.PcieLinkTarget{UspBdfs: []string{"0000:38:00.0"}},
			},
		},
		{
			name: "replaces only the usp bdfs of the link target",
			cfg: &pb.LinkTrain{
				LinkTarget: &pb.PcieLinkTarget{
					UspVendorId: proto.Uint32(0x1ae0),
					UspBdfs:     []string{"0000:16:00.0"},
					DspBdfs:     []string{"0000:37:01.0"},
				},
			},
			bdfs: []string{"0000:38:00.0"},
			want: &pb.LinkTrain{
				LinkTarget: &pb.PcieLinkTarget{
					UspVendorId: proto.Uint32(0x1ae0),
					UspBdfs:     []string{"0000:38:00.0"},
					DspBdfs:     []string{"0000:37:01.0"},
				},
			},
		},
		{
			name: "multiple bdfs",
			cfg:  spec(),
			bdfs: strings.Split("0000:38:00.0,10000:e1:00.0", ","),
			want: overridden("0000:38:00.0", "10000:e1:00.0"),
		},
		{
			name: "trailing comma",
			cfg:  spec(),
			bdfs: strings.Split("0000:38:00.0,", ","),
			want: overridden("0000:38:00.0"),
		},
		{
			name: "leading comma",
			cfg:  spec(),
			bdfs: strings.Split(",0000:38:00.0", ","),
			want: overridden("0000:38:00.0"),
		},
		{
			name: "whitespace around entries",
			cfg:  spec(),
			bdfs: strings.Split("0000:38:00.0, 0000:b8:00.0 ,\t", ","),
			want: overridden("0000:38:00.0", "0000:b8:00.0"),
		},
		{
			name: "uppercase hex",
			cfg:  spec(),
			bdfs: []string{"0000:B8:00.0"},
			want: overridden("0000:B8:00.0"),
		},
		{
			name:    "empty input",
			cfg:     spec(),
			bdfs:    strings.Split("", ","),
			want:    spec(),
			wantErr: "no valid USP BDF given",
		},
		{
			name:    "all blank entries",
			cfg:     spec(),
			bdfs:    strings.Split(" , ,", ","),
			want:    spec(),
			wantErr: "no valid USP BDF given",
		},
		{
			name:    "nil bdfs",
			cfg:     spec(),
			want:    spec(),
			wantErr: "no valid USP BDF given",
		},
		{
			name:    "malformed entry",
			cfg:     spec(),
			bdfs:    []string{"0000:38:00.0", "zz"},
			want:    spec(),
			wantErr: "malformed USP BDF",
		},
		{
			name:    "bus number only",
			cfg:     spec(),
			bdfs:    []string{"38"},
			want:    spec(),
			wantErr: "malformed USP BDF",
		},
		{
			name:    "bus out of range",
			cfg:     spec(),
			bdfs:    []string{"0000:100:00.0"},
			want:    spec(),
			wantErr: "malformed USP BDF",
		},
		{
			name:    "nil cfg",
			bdfs:    []string{"0000:38:00.0"},
			wantErr: "nil LinkTrain config",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := OverrideUSPBDFs(tc.cfg, tc.bdfs)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("OverrideUSPBDFs(%q) = %v, want nil", tc.bdfs, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("OverrideUSPBDFs(%q) = %v, want an error containing %q", tc.bdfs, err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, tc.cfg, protocmp.Transform()); diff != "" {
				t.Errorf("OverrideUSPBDFs(%q) returned diff (-want +got):\n%s", tc.bdfs, diff)
			}
		})
	}
}

func TestOverrideUSPBDFsCopiesInput(t *testing.T) {
	cfg := &pb.LinkTrain{}
	bdfs := []string{"0000:38:00.0"}
	if err := OverrideUSPBDFs(cfg, bdfs); err != nil {
		t.Fatalf("OverrideUSPBDFs(%q) = %v, want nil", bdfs, err)
	}
	bdfs[0] = "0000:b8:00.0"
	if got := cfg.GetLinkTarget().GetUspBdfs(); !slices.Equal(got, []string{"0000:38:00.0"}) {
		t.Errorf("UspBdfs = %q after changing the input, want [\"0000:38:00.0\"]", got)
	}
}

func TestMatchTestTargetSkipsUnlistedUSP(t *testing.T) {
	pci.Init()
	defer pci.Cleanup()
	devs := pci.ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: pci.ScanDevices() found no devices.")
	}
	// No real device has this BDF or these IDs, so every device must be skipped
	// before its DSP is looked up.
	for _, target := range []*pb.PcieLinkTarget{
		{UspBdfs: []string{"ffff:ff:1f.7"}},
		{UspVendorId: proto.Uint32(0xffff)},
		{UspDeviceId: proto.Uint32(0xffff)},
	} {
		cfg := &pb.LinkTrain{LinkTarget: target}
		for dev := devs; dev.Valid(); dev = dev.GetNext() {
			tt, err := matchTestTarget(dev, cfg)
			if tt != nil || err != nil {
				t.Errorf("matchTestTarget(%s, %v) = (%v, %v), want (nil, nil)", dev.BDFString(), cfg, tt, err)
			}
		}
	}
}

func TestGetLinksSkipsUnmatchedDevices(t *testing.T) {
	oldMatchTestTarget := matchTestTarget
	defer func() { matchTestTarget = oldMatchTestTarget }()
	called := 0
	// A skipped device must not stop the scan, so every device is expected to
	// be offered to matchTestTarget.
	matchTestTarget = func(dev pci.Dev, cfg *pb.LinkTrain) (*testTarget, error) {
		called++
		return nil, nil
	}
	pci.Init()
	defer pci.Cleanup()
	devs := pci.ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: pci.ScanDevices() found no devices.")
	}
	wantCalled := 0
	for dev := devs; dev.Valid(); dev = dev.GetNext() {
		wantCalled++
	}
	linktests, err := getLinks(devs, &pb.LinkTrain{})
	if err != nil {
		t.Errorf("getLinks() returned unexpected error: %v", err)
	}
	if len(linktests) != 0 {
		t.Errorf("getLinks() returned %d link tests, want 0", len(linktests))
	}
	if called != wantCalled {
		t.Errorf("getLinks() called matchTestTarget %d times, want %d (one per device)", called, wantCalled)
	}
}

func TestGetLinksWrapsMatchTestTargetError(t *testing.T) {
	oldMatchTestTarget := matchTestTarget
	defer func() { matchTestTarget = oldMatchTestTarget }()
	wantErr := &TrainError{Code: ErrLinkDown, Message: "injected"}
	matchTestTarget = func(dev pci.Dev, cfg *pb.LinkTrain) (*testTarget, error) {
		return nil, wantErr
	}
	pci.Init()
	defer pci.Cleanup()
	devs := pci.ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: pci.ScanDevices() found no devices.")
	}
	bdf := devs.BDFString()

	_, err := getLinks(devs, &pb.LinkTrain{})
	if err == nil {
		t.Fatalf("getLinks() returned nil error, want the injected error")
	}
	// The error must name the device it happened on.
	if !strings.Contains(err.Error(), bdf) {
		t.Errorf("getLinks() error = %q, want it to contain the BDF %q", err.Error(), bdf)
	}
	// Wrapping must not hide the underlying TrainError.
	var te *TrainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(%v, *TrainError) = false, want true", err)
	}
	if te.Code != ErrLinkDown {
		t.Errorf("unwrapped error code = %v, want %v", te.Code, ErrLinkDown)
	}
	if !errors.Is(err, error(wantErr)) {
		t.Errorf("errors.Is(%v, wantErr) = false, want true", err)
	}
}

func TestBDFStr(t *testing.T) {
	testCases := []struct {
		name string
		info pci.PCIDevInfo
		want string
	}{
		{
			name: "root port",
			info: pci.PCIDevInfo{Domain: 0, Bus: 0x15, Dev: 0x01, Func: 0},
			want: "0000:15:01.0",
		},
		{
			name: "endpoint",
			info: pci.PCIDevInfo{Domain: 0, Bus: 0xb8, Dev: 0, Func: 0},
			want: "0000:b8:00.0",
		},
		{
			name: "non-zero domain and function",
			info: pci.PCIDevInfo{Domain: 0x1a, Bus: 0xff, Dev: 0x1f, Func: 7},
			want: "001a:ff:1f.7",
		},
		{
			name: "domain wider than 16 bits, e.g. Intel VMD",
			info: pci.PCIDevInfo{Domain: 0x10000, Bus: 0xe1, Dev: 0, Func: 0},
			want: "10000:e1:00.0",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.BDFStr(); got != tc.want {
				t.Errorf("BDFStr() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGetPCIeCapOffset(t *testing.T) {
	origReadByte := pciReadByte
	origReadWord := pciReadWord
	defer func() {
		pciReadByte = origReadByte
		pciReadWord = origReadWord
	}()

	const (
		capListAddr = 0x34 // C.PCI_CAPABILITY_LIST
		capIDExp    = 0x10 // C.PCI_CAP_ID_EXP
	)
	testCases := []struct {
		name string
		// capPtr is the first capability pointer, read from capListAddr.
		capPtr uint8
		// configMem maps a capability offset to its 16-bit header.
		configMem  map[int32]uint16
		wantOffset int32
		// wantCode is ErrNone when no error is expected.
		wantCode TrainErrorCode
	}{
		{
			name:       "PCIe capability found",
			capPtr:     0x40,
			configMem:  map[int32]uint16{0x40: capIDExp},
			wantOffset: 0x40,
		},
		{
			name:      "link down reads 0xffff",
			capPtr:    0x40,
			configMem: map[int32]uint16{0x40: 0xFFFF},
			wantCode:  ErrLinkDown,
		},
		{
			name:     "unaligned capability pointer",
			capPtr:   0x41,
			wantCode: ErrCapChainUnaligned,
		},
		{
			name:      "capability chain loops",
			capPtr:    0x40,
			configMem: map[int32]uint16{0x40: 0x4005}, // cap ID 0x05, next pointer 0x40.
			wantCode:  ErrCapChainLoop,
		},
		{
			name:      "PCIe capability not found",
			capPtr:    0x40,
			configMem: map[int32]uint16{0x40: 0x0005}, // cap ID 0x05, end of the chain.
			wantCode:  ErrPCIeCapNotFound,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pciReadByte = func(dev pci.Dev, addr int32) uint8 {
				if addr == capListAddr {
					return tc.capPtr
				}
				return 0
			}
			pciReadWord = func(dev pci.Dev, addr int32) uint16 { return tc.configMem[addr] }

			offset, err := getPCIeCapOffset(pci.Dev{})
			if tc.wantCode == ErrNone {
				if err != nil {
					t.Fatalf("getPCIeCapOffset() returned unexpected error: %v", err)
				}
				if offset != tc.wantOffset {
					t.Errorf("getPCIeCapOffset() = 0x%x, want 0x%x", offset, tc.wantOffset)
				}
				return
			}
			if err == nil {
				t.Fatalf("getPCIeCapOffset() = (0x%x, nil), want error code %v", offset, tc.wantCode)
			}
			if err.Code != tc.wantCode {
				t.Errorf("getPCIeCapOffset() returned error %v (code %v), want code %v", err, err.Code, tc.wantCode)
			}
			if !strings.Contains(err.Error(), tc.wantCode.String()) {
				t.Errorf("err.Error() = %q, want substring %q", err.Error(), tc.wantCode.String())
			}
		})
	}
	if got := ErrNone.String(); got != "no error" {
		t.Errorf("ErrNone.String() = %q, want %q", got, "no error")
	}
}

func TestGetPCIeCapOffsetLinkDownIsTrainError(t *testing.T) {
	origReadByte := pciReadByte
	origReadWord := pciReadWord
	defer func() {
		pciReadByte = origReadByte
		pciReadWord = origReadWord
	}()
	pciReadByte = func(dev pci.Dev, addr int32) uint8 { return 0x40 }
	pciReadWord = func(dev pci.Dev, addr int32) uint16 { return 0xFFFF }

	// The caller propagates the result as a plain error, so it must still be
	// recoverable with errors.As.
	var err error = mustCapOffsetError(t)
	var te *TrainError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As(%v, *TrainError) = false, want true", err)
	}
	if te.Code != ErrLinkDown {
		t.Errorf("error code = %v, want %v", te.Code, ErrLinkDown)
	}
	if needFieldCheck(err) {
		t.Errorf("needFieldCheck(%v) = true, want false", err)
	}
}

// mustCapOffsetError returns the error from getPCIeCapOffset, failing the test
// if there is none.
func mustCapOffsetError(t *testing.T) *TrainError {
	t.Helper()
	offset, err := getPCIeCapOffset(pci.Dev{})
	if err == nil {
		t.Fatalf("getPCIeCapOffset() = (0x%x, nil), want an error", offset)
	}
	return err
}

func TestMatchTestTarget(t *testing.T) {
	pci.Init()
	defer pci.Cleanup()
	devs := pci.ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: pci.ScanDevices() found no devices.")
	}
	var pf0Dev pci.Dev
	for dev := devs; dev.Valid(); dev = dev.GetNext() {
		if d := dev.GetDevInfo(); d.Dev == 0 && d.Func == 0 {
			pf0Dev = dev
			break
		}
	}
	if !pf0Dev.Valid() {
		t.Skip("Skipping test: no device with dev=0, func=0 found.")
	}
	d := pf0Dev.GetDevInfo()

	origGetCap := getPCIeCapOffset
	origReadWord := pciReadWord
	origReadByte := pciReadByte
	origFindDSP := findDSP
	t.Cleanup(func() {
		getPCIeCapOffset = origGetCap
		pciReadWord = origReadWord
		pciReadByte = origReadByte
		findDSP = origFindDSP
	})

	t.Run("non-zero device or function is skipped", func(t *testing.T) {
		for dev := devs; dev.Valid(); dev = dev.GetNext() {
			if d := dev.GetDevInfo(); d.Dev != 0 || d.Func != 0 {
				if got, err := matchTestTarget(dev, &pb.LinkTrain{}); got != nil || err != nil {
					t.Errorf("matchTestTarget(%s) = (%v, %v), want (nil, nil)", dev.BDFString(), got, err)
				}
				return
			}
		}
	})

	t.Run("USP PCIe capability error skips device", func(t *testing.T) {
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) {
			return 0, &TrainError{Code: ErrPCIeCapNotFound, Message: "no cap"}
		}
		if got, err := matchTestTarget(pf0Dev, &pb.LinkTrain{}); got != nil || err != nil {
			t.Errorf("matchTestTarget() = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("root port type is skipped", func(t *testing.T) {
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) { return 0x40, nil }
		pciReadWord = func(pci.Dev, int32) uint16 { return 0x4 << 4 } // PCI_EXP_TYPE_ROOT_PORT
		if got, err := matchTestTarget(pf0Dev, &pb.LinkTrain{}); got != nil || err != nil {
			t.Errorf("matchTestTarget() = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("internal switch bridge 0x06:0x80 is skipped", func(t *testing.T) {
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) { return 0x40, nil }
		pciReadWord = func(pci.Dev, int32) uint16 { return 0x5 << 4 } // PCI_EXP_TYPE_UPSTREAM
		pciReadByte = func(_ pci.Dev, addr int32) uint8 {
			if addr == 0x0B {
				return 0x06
			}
			return 0x80
		}
		if got, err := matchTestTarget(pf0Dev, &pb.LinkTrain{}); got != nil || err != nil {
			t.Errorf("matchTestTarget() = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("FindDSP error is propagated", func(t *testing.T) {
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) { return 0x40, nil }
		pciReadWord = func(pci.Dev, int32) uint16 { return 0 } // PCI_EXP_TYPE_ENDPOINT
		pciReadByte = func(pci.Dev, int32) uint8 { return 0 }
		findDSP = func(pci.Dev) (pci.Dev, error) { return pci.Dev{}, errors.New("no dsp") }
		if got, err := matchTestTarget(pf0Dev, &pb.LinkTrain{}); err == nil || got != nil {
			t.Errorf("matchTestTarget() = (%v, %v), want (nil, error)", got, err)
		}
	})

	t.Run("link_target mismatch skips link", func(t *testing.T) {
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) { return 0x40, nil }
		pciReadWord = func(pci.Dev, int32) uint16 { return 0 }
		pciReadByte = func(pci.Dev, int32) uint8 { return 0 }
		findDSP = func(pci.Dev) (pci.Dev, error) { return pf0Dev, nil }
		cfg := &pb.LinkTrain{
			LinkTarget: &pb.PcieLinkTarget{
				DspVendorId: proto.Uint32(uint32(d.VendorID) ^ 1),
			},
		}
		if got, err := matchTestTarget(pf0Dev, cfg); got != nil || err != nil {
			t.Errorf("matchTestTarget() = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("DSP PCIe capability error skips link", func(t *testing.T) {
		calls := 0
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) {
			calls++
			if calls == 1 {
				return 0x40, nil
			}
			return 0, &TrainError{Code: ErrLinkDown, Message: "dsp down"}
		}
		pciReadWord = func(pci.Dev, int32) uint16 { return 0 }
		pciReadByte = func(pci.Dev, int32) uint8 { return 0 }
		findDSP = func(pci.Dev) (pci.Dev, error) { return pf0Dev, nil }
		if got, err := matchTestTarget(pf0Dev, &pb.LinkTrain{}); got != nil || err != nil {
			t.Errorf("matchTestTarget() = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("matching link returns testTarget", func(t *testing.T) {
		calls := 0
		getPCIeCapOffset = func(pci.Dev) (int32, *TrainError) {
			calls++
			if calls == 1 {
				return 0x40, nil
			}
			return 0x80, nil
		}
		pciReadWord = func(pci.Dev, int32) uint16 { return 0 }
		pciReadByte = func(pci.Dev, int32) uint8 { return 0 }
		findDSP = func(pci.Dev) (pci.Dev, error) { return pf0Dev, nil }
		cfg := &pb.LinkTrain{
			LinkTarget: &pb.PcieLinkTarget{
				UspVendorId: proto.Uint32(uint32(d.VendorID)),
				DspVendorId: proto.Uint32(uint32(d.VendorID)),
			},
		}
		got, err := matchTestTarget(pf0Dev, cfg)
		if err != nil || got == nil {
			t.Fatalf("matchTestTarget() = (%v, %v), want non-nil target", got, err)
		}
		if got.uspCapOffset != 0x40 || got.dspCapOffset != 0x80 {
			t.Errorf("matchTestTarget() offsets = (%#x, %#x), want (0x40, 0x80)", got.uspCapOffset, got.dspCapOffset)
		}
	})
}

func TestGetLinksBuildsLinktest(t *testing.T) {
	pci.Init()
	defer pci.Cleanup()
	devs := pci.ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: pci.ScanDevices() found no devices.")
	}

	origMatch := matchTestTarget
	origSriov := getSriovCapOffset
	t.Cleanup(func() {
		matchTestTarget = origMatch
		getSriovCapOffset = origSriov
	})

	matched := false
	matchTestTarget = func(dev pci.Dev, _ *pb.LinkTrain) (*testTarget, error) {
		if matched {
			return nil, nil
		}
		matched = true
		return &testTarget{usp: dev, dsp: dev, uspCapOffset: 0x40, dspCapOffset: 0x80}, nil
	}

	t.Run("populates Linktest and defaults", func(t *testing.T) {
		matched = false
		lts, err := getLinks(devs, &pb.LinkTrain{})
		if err != nil {
			t.Fatalf("getLinks() unexpected error: %v", err)
		}
		if len(lts) != 1 {
			t.Fatalf("len(getLinks()) = %d, want 1", len(lts))
		}
		lt := lts[0]
		if lt.uspPCIeCapOffset != 0x40 || lt.dspPCIeCapOffset != 0x80 {
			t.Errorf("cap offsets = (%#x, %#x), want (0x40, 0x80)", lt.uspPCIeCapOffset, lt.dspPCIeCapOffset)
		}
		if lt.Cfg.GetUspBdf() != devs.BDFString() || lt.Cfg.GetDspBdf() != devs.BDFString() {
			t.Errorf("BDFs = (%q, %q), want %q", lt.Cfg.GetUspBdf(), lt.Cfg.GetDspBdf(), devs.BDFString())
		}
	})

	t.Run("propagates extendLinkTrainFields error", func(t *testing.T) {
		matched = false
		getSriovCapOffset = func(pci.Dev) (int32, error) {
			return 0, errors.New("sriov broken")
		}
		_, err := getLinks(devs, &pb.LinkTrain{RecoverSriovCapRegs: true})
		if err == nil || !strings.Contains(err.Error(), "sriov broken") {
			t.Errorf("getLinks() error = %v, want sriov broken error", err)
		}
	})
}
