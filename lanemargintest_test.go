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

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	lmtpb "lmt_go.proto"
	pci "pciutils"
)

func TestReadLinkMargin(t *testing.T) {
	want := &lmtpb.LinkMargin{
		LinkTarget: &lmtpb.PcieLinkTarget{
			UspVendorId: proto.Uint32(0x1ae0),
			UspDeviceId: proto.Uint32(0x0060),
			UspBdfs:     []string{"0000:1a:00.0", "0000:39:00.0"},
		},
		TestSpecs: []*lmtpb.LinkMargin_TestSpec{
			{
				Receiver:   lmtpb.LinkMargin_R_USP_F6.Enum(),
				Aspect:     lmtpb.LinkMargin_M_TIMING.Enum(),
				Samples:    100,
				ErrorLimit: 4,
			},
		},
	}

	t.Run("pbtxt", func(t *testing.T) {
		content := `
link_target: {
	usp_vendor_id: 0x1ae0
	usp_device_id: 0x0060
	usp_bdfs: ["0000:1a:00.0", "0000:39:00.0"]
}
test_specs: {
	receiver: R_USP_F6
	aspect: M_TIMING
	samples: 100
	error_limit: 4
}
`
		fn := filepath.Join(t.TempDir(), "test.pbtxt")
		if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
		got, err := ReadLinkMargin(fn, false)
		if err != nil {
			t.Errorf("ReadLinkMargin(%q, false) failed: %v", fn, err)
		}
		if !proto.Equal(got, want) {
			t.Errorf("ReadLinkMargin(%q, false) = %v, want %v", fn, got, want)
		}
	})

	t.Run("json", func(t *testing.T) {
		content := `{
  "link_target": {
    "usp_vendor_id": 6880,
    "usp_device_id": 96,
    "usp_bdfs": ["0000:1a:00.0", "0000:39:00.0"]
  },
  "test_specs": [
    {
      "receiver": "R_USP_F6",
      "aspect": "M_TIMING",
      "samples": 100,
      "error_limit": 4
    }
  ]
}`
		fn := filepath.Join(t.TempDir(), "test.json")
		if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
		got, err := ReadLinkMargin(fn, true)
		if err != nil {
			t.Errorf("ReadLinkMargin(%q, true) failed: %v", fn, err)
		}
		if !proto.Equal(got, want) {
			t.Errorf("ReadLinkMargin(%q, true) = %v, want %v", fn, got, want)
		}
	})
}

func TestReadLinkMarginErrors(t *testing.T) {
	t.Run("FileNotFound", func(t *testing.T) {
		_, err := ReadLinkMargin(filepath.Join(t.TempDir(), "nonexistent.pbtxt"), false)
		if err == nil {
			t.Errorf("ReadLinkMargin() for nonexistent file succeeded, want error")
		}
	})

	t.Run("MalformedPbtxt", func(t *testing.T) {
		fn := filepath.Join(t.TempDir(), "malformed.pbtxt")
		if err := os.WriteFile(fn, []byte(`test_specs: { samples: 10a }`), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
		if _, err := ReadLinkMargin(fn, false); err == nil {
			t.Errorf("ReadLinkMargin(%q, false) succeeded with malformed content, want error", fn)
		}
	})

	t.Run("MalformedJSON", func(t *testing.T) {
		fn := filepath.Join(t.TempDir(), "malformed.json")
		if err := os.WriteFile(fn, []byte(`{"test_specs": [}`), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}
		if _, err := ReadLinkMargin(fn, true); err == nil {
			t.Errorf("ReadLinkMargin(%q, true) succeeded with malformed content, want error", fn)
		}
	})

	// The removed USP filters must fail to parse rather than be silently
	// dropped, which would select every link.
	for field, content := range map[string]string{
		"vendor_id": `vendor_id: 0x1ae0`,
		"device_id": `device_id: 0x0075`,
		"bdf":       `bdf: "0000:16:00.0"`,
	} {
		t.Run("RemovedPbtxtField_"+field, func(t *testing.T) {
			fn := filepath.Join(t.TempDir(), "legacy.pbtxt")
			if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
				t.Fatalf("Failed to write test file: %v", err)
			}
			if got, err := ReadLinkMargin(fn, false); err == nil {
				t.Errorf("ReadLinkMargin(pbtxt) with %q = %v, want error", content, got)
			}
		})
	}
	for field, content := range map[string]string{
		"vendor_id": `{"vendor_id": 6880}`,
		"device_id": `{"device_id": 117}`,
		"bdf":       `{"bdf": ["0000:16:00.0"]}`,
	} {
		t.Run("RemovedJSONField_"+field, func(t *testing.T) {
			fn := filepath.Join(t.TempDir(), "legacy.json")
			if err := os.WriteFile(fn, []byte(content), 0644); err != nil {
				t.Fatalf("Failed to write test file: %v", err)
			}
			if got, err := ReadLinkMargin(fn, true); err == nil {
				t.Errorf("ReadLinkMargin(json) with %q = %v, want error", content, got)
			}
		})
	}
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
		target *lmtpb.PcieLinkTarget
		want   bool
	}{
		{
			name:   "no link target matches any link",
			target: nil,
			want:   true,
		},
		{
			name:   "empty link target matches any link",
			target: &lmtpb.PcieLinkTarget{},
			want:   true,
		},
		{
			name: "usp only matches",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0075),
			},
			want: true,
		},
		{
			name: "dsp only matches",
			target: &lmtpb.PcieLinkTarget{
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
			},
			want: true,
		},
		{
			name: "both ends match",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0075),
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
			},
			want: true,
		},
		{
			name: "vendor only wildcard matches both ends",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				DspVendorId: proto.Uint32(0x8086),
			},
			want: true,
		},
		{
			name: "usp mismatches",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0076),
				DspVendorId: proto.Uint32(0x8086),
				DspDeviceId: proto.Uint32(0x352a),
			},
			want: false,
		},
		{
			name: "dsp mismatches",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				UspDeviceId: proto.Uint32(0x0075),
				DspVendorId: proto.Uint32(0x1ae0),
				DspDeviceId: proto.Uint32(0x0075),
			},
			want: false,
		},
		{
			name: "both ends mismatch",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1000),
				DspVendorId: proto.Uint32(0x1000),
			},
			want: false,
		},
		{
			name: "field set to zero is not a wildcard",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				DspDeviceId: proto.Uint32(0),
			},
			want: false,
		},
		{
			name: "dsp bdf and ids match",
			target: &lmtpb.PcieLinkTarget{
				UspVendorId: proto.Uint32(0x1ae0),
				DspVendorId: proto.Uint32(0x8086),
				DspBdfs:     []string{"0000:37:01.0", "0000:15:01.0"},
			},
			want: true,
		},
		{
			name: "dsp ids match but dsp bdf mismatches",
			target: &lmtpb.PcieLinkTarget{
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
		target  *lmtpb.PcieLinkTarget
		wantErr bool
	}{
		{name: "nil target", target: nil},
		{name: "no bdfs", target: &lmtpb.PcieLinkTarget{UspVendorId: proto.Uint32(0x1ae0)}},
		{
			name: "valid bdfs",
			target: &lmtpb.PcieLinkTarget{
				UspBdfs: []string{"0000:16:00.0", "10000:e1:00.0"},
				DspBdfs: []string{"0000:15:01.0"},
			},
		},
		{
			name:    "usp bdf without domain",
			target:  &lmtpb.PcieLinkTarget{UspBdfs: []string{"0000:16:00.0", "16:00.0"}},
			wantErr: true,
		},
		{
			name:    "dsp bdf without function",
			target:  &lmtpb.PcieLinkTarget{DspBdfs: []string{"0000:15:01"}},
			wantErr: true,
		},
		{
			name:    "bus number only",
			target:  &lmtpb.PcieLinkTarget{UspBdfs: []string{"16"}},
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

func TestMarginLinksRejectsMalformedBDF(t *testing.T) {
	oldPciScanDevices := pciScanDevices
	defer func() { pciScanDevices = oldPciScanDevices }()
	pciScanDevices = func() pci.Dev {
		t.Error("MarginLinks() scanned the PCI devices despite a malformed BDF")
		return pci.Dev{}
	}
	cfg := &lmtpb.LinkMargin{
		LinkTarget: &lmtpb.PcieLinkTarget{DspBdfs: []string{"15:01.0"}},
	}
	err := MarginLinks(cfg)
	if err == nil || !strings.Contains(err.Error(), "malformed BDF") {
		t.Errorf("MarginLinks(%v) = %v, want a malformed BDF error", cfg, err)
	}
}

func TestOverrideUSPBDFs(t *testing.T) {
	spec := func() *lmtpb.LinkMargin {
		return &lmtpb.LinkMargin{
			LinkTarget: &lmtpb.PcieLinkTarget{UspBdfs: []string{"0000:16:00.0"}},
		}
	}
	overridden := func(bdfs ...string) *lmtpb.LinkMargin {
		return &lmtpb.LinkMargin{LinkTarget: &lmtpb.PcieLinkTarget{UspBdfs: bdfs}}
	}
	testCases := []struct {
		name    string
		cfg     *lmtpb.LinkMargin
		bdfs    []string
		want    *lmtpb.LinkMargin
		wantErr string // A substring of the error; empty means no error.
	}{
		{
			name: "creates the link target",
			cfg:  &lmtpb.LinkMargin{Message: proto.String("info")},
			bdfs: []string{"0000:38:00.0"},
			want: &lmtpb.LinkMargin{
				Message:    proto.String("info"),
				LinkTarget: &lmtpb.PcieLinkTarget{UspBdfs: []string{"0000:38:00.0"}},
			},
		},
		{
			name: "replaces only the usp bdfs of the link target",
			cfg: &lmtpb.LinkMargin{
				LinkTarget: &lmtpb.PcieLinkTarget{
					UspVendorId: proto.Uint32(0x1ae0),
					UspBdfs:     []string{"0000:16:00.0"},
					DspBdfs:     []string{"0000:37:01.0"},
				},
			},
			bdfs: []string{"0000:38:00.0"},
			want: &lmtpb.LinkMargin{
				LinkTarget: &lmtpb.PcieLinkTarget{
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
			wantErr: "nil LinkMargin config",
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
			if !proto.Equal(tc.cfg, tc.want) {
				t.Errorf("OverrideUSPBDFs(%q) cfg = %v, want %v", tc.bdfs, tc.cfg, tc.want)
			}
		})
	}
}

func TestOverrideUSPBDFsCopiesInput(t *testing.T) {
	cfg := &lmtpb.LinkMargin{}
	bdfs := []string{"0000:38:00.0"}
	if err := OverrideUSPBDFs(cfg, bdfs); err != nil {
		t.Fatalf("OverrideUSPBDFs(%q) = %v, want nil", bdfs, err)
	}
	bdfs[0] = "0000:b8:00.0"
	if got := cfg.GetLinkTarget().GetUspBdfs(); !slices.Equal(got, []string{"0000:38:00.0"}) {
		t.Errorf("UspBdfs = %q after changing the input, want [\"0000:38:00.0\"]", got)
	}
}

func TestGetLinksSkipsUnlistedUSP(t *testing.T) {
	pci.Init()
	defer pci.Cleanup()
	devs := pci.ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: pci.ScanDevices() found no devices.")
	}
	// No real device has this BDF or these IDs, so every device must be skipped
	// before its DSP is looked up.
	for _, target := range []*lmtpb.PcieLinkTarget{
		{UspBdfs: []string{"ffff:ff:1f.7"}},
		{UspVendorId: proto.Uint32(0xffff)},
		{UspDeviceId: proto.Uint32(0xffff)},
	} {
		cfg := &lmtpb.LinkMargin{LinkTarget: target}
		got, err := getLinks(devs, cfg)
		if len(got) != 0 || err != nil {
			t.Errorf("getLinks(%v) = (%v, %v), want ([], nil)", cfg, got, err)
		}
	}
}
