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

package pciutils

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBDF(t *testing.T) {
	testCases := []struct {
		name                 string
		in                   string
		domain, bus, dev, fn int
		wantErr              bool
	}{
		{name: "16-bit domain", in: "0000:15:01.0", domain: 0, bus: 0x15, dev: 1, fn: 0},
		{name: "non-zero function", in: "001a:ff:1f.7", domain: 0x1a, bus: 0xff, dev: 0x1f, fn: 7},
		{name: "domain wider than 16 bits, e.g. Intel VMD", in: "10000:e0:01.0", domain: 0x10000, bus: 0xe0, dev: 1, fn: 0},
		{name: "not a BDF", in: "pci0000:15", wantErr: true},
		{name: "missing function", in: "0000:15:01", wantErr: true},
		{name: "upper case", in: "0000:B8:00.0", domain: 0, bus: 0xb8, dev: 0, fn: 0},
		{name: "largest domain", in: "7fffffff:00:00.0", domain: 0x7fffffff, bus: 0, dev: 0, fn: 0},
		{name: "no domain", in: "16:00.0", wantErr: true},
		{name: "bus only", in: "16", wantErr: true},
		{name: "trailing garbage", in: "0000:16:00.0junk", wantErr: true},
		{name: "trailing space", in: "0000:16:00.0 ", wantErr: true},
		{name: "leading space", in: " 0000:16:00.0", wantErr: true},
		{name: "bus out of range", in: "0000:1ff:00.0", wantErr: true},
		{name: "device out of range", in: "0000:16:20.0", wantErr: true},
		{name: "function out of range", in: "0000:16:00.8", wantErr: true},
		{name: "negative domain", in: "-001:16:00.0", wantErr: true},
		{name: "domain out of range", in: "80000000:16:00.0", wantErr: true},
		{name: "domain too wide", in: "000000000:16:00.0", wantErr: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			domain, bus, dev, fn, err := ParseBDF(tc.in)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("ParseBDF(%q) error = %v, want error: %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if domain != tc.domain || bus != tc.bus || dev != tc.dev || fn != tc.fn {
				t.Errorf("ParseBDF(%q) = (%#x, %#x, %#x, %d), want (%#x, %#x, %#x, %d)",
					tc.in, domain, bus, dev, fn, tc.domain, tc.bus, tc.dev, tc.fn)
			}
		})
	}
}

func TestGetDevInfoAndBDFString(t *testing.T) {
	raw := _Ctype_struct_pci_dev{
		vendor_id: 0x1ae0,
		device_id: 0x0075,
		domain:    0x10000,
		bus:       0xe1,
		dev:       2,
		_func:     3,
		hdrtype:   1,
	}
	dev := Dev{cptr: &raw}
	wantInfo := PCIDevInfo{
		VendorID: 0x1ae0,
		DeviceID: 0x0075,
		Domain:   0x10000,
		Bus:      0xe1,
		Dev:      2,
		Func:     3,
		HdrType:  1,
	}
	if got := dev.GetDevInfo(); got != wantInfo {
		t.Errorf("GetDevInfo() = %+v, want %+v", got, wantInfo)
	}
	if got, want := dev.BDFString(), "10000:e1:02.3"; got != want {
		t.Errorf("BDFString() = %q, want %q", got, want)
	}
}

func TestFindDSP(t *testing.T) {
	Init()
	defer Cleanup()
	devs := ScanDevices()
	if !devs.Valid() {
		t.Skip("Skipping test: ScanDevices() found no devices.")
	}
	origDir := sysfsDevicesDir
	t.Cleanup(func() { sysfsDevicesDir = origDir })

	devBDF := devs.BDFString()
	wantInfo := devs.GetDevInfo()

	t.Run("missing sysfs symlink", func(t *testing.T) {
		sysfsDevicesDir = t.TempDir()
		if _, err := devs.FindDSP(); err == nil {
			t.Errorf("FindDSP() with missing symlink succeeded, want error")
		}
	})

	t.Run("parent directory is not a BDF", func(t *testing.T) {
		sysfsDevicesDir = t.TempDir()
		link := filepath.Join(sysfsDevicesDir, devBDF)
		target := filepath.Join("../../../devices/pci0000:00", devBDF)
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("os.Symlink(%q, %q) failed: %v", target, link, err)
		}
		if _, err := devs.FindDSP(); err == nil {
			t.Errorf("FindDSP() with non-BDF parent %q succeeded, want error", target)
		}
	})

	t.Run("populates DSP identity", func(t *testing.T) {
		sysfsDevicesDir = t.TempDir()
		link := filepath.Join(sysfsDevicesDir, devBDF)
		target := filepath.Join("../../../devices/pci0000:00", devBDF, devBDF)
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("os.Symlink(%q, %q) failed: %v", target, link, err)
		}
		dsp, err := devs.FindDSP()
		if err != nil {
			t.Fatalf("FindDSP() unexpected error: %v", err)
		}
		got := dsp.GetDevInfo()
		if got.BDFStr() != devBDF || got.VendorID != wantInfo.VendorID || got.DeviceID != wantInfo.DeviceID {
			t.Errorf("FindDSP().GetDevInfo() = %+v, want BDF %s with IDs %04x:%04x",
				got, devBDF, wantInfo.VendorID, wantInfo.DeviceID)
		}
	})
}

func TestBDFString(t *testing.T) {
	testCases := []struct {
		name string
		dev  Dev
		want string
	}{
		{name: "zero Dev", dev: Dev{}, want: "invalid"},
		{name: "valid Dev", dev: devAt(0, 0x15, 0x01, 0), want: "0000:15:01.0"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.dev.BDFString(); got != tc.want {
				t.Errorf("BDFString() = %q, want %q", got, tc.want)
			}
		})
	}
}
