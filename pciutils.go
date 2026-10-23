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

// Package pciutils wraps around the third_party/pciutils C library
// The cgo import is unique to the package. If two go packages both import the
// pciutils, they cannot pass pointers to pciutils structures to each other.
// Using interface{} results in panic: interface conversion: interface {} is
// *pciutils._Ctype_struct_pci_dev, not *linkmargin._Ctype_struct_pci_dev
// Therefore, this package serves as the single gateway to the pciutils cgo.
package pciutils

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
	"path"
	"regexp"
	"strconv"
	"sync"

	log "github.com/golang/glog"
)

// Dev exports the pciutils' device struct.
type Dev struct {
	cptr *C.struct_pci_dev
}

// PCIDevInfo struct is used to export C.struct_pci_dev members.
type PCIDevInfo struct {
	VendorID, DeviceID uint16
	// Domain is the full PCI domain, which can exceed 16 bits, e.g. Intel VMD
	// domains start at 0x10000.
	Domain         int32
	Bus, Dev, Func uint8
	HdrType        int32
}

// GetDevInfo fills a PCIDevInfo from a Dev, as Dev members are not exported.
// It must not lock m, because BDFString calls it while m is held.
func (dev Dev) GetDevInfo() PCIDevInfo {
	info := PCIDevInfo{
		VendorID: uint16(dev.cptr.vendor_id),
		DeviceID: uint16(dev.cptr.device_id),
		// domain_16 is clamped to 0xffff for domains wider than 16 bits.
		Domain:  int32(dev.cptr.domain),
		Bus:     uint8(dev.cptr.bus),
		Dev:     uint8(dev.cptr.dev),
		Func:    uint8(dev.cptr._func),
		HdrType: int32(dev.cptr.hdrtype),
	}
	return info
}

// BDFStr gets the device's BDF as a string, e.g. "0000:16:00.0".
func (d PCIDevInfo) BDFStr() string {
	return fmt.Sprintf("%04x:%02x:%02x.%d", d.Domain, d.Bus, d.Dev, d.Func)
}

// Access exports the pciutils' access instance.
var Access *C.struct_pci_access

// Only one goroutine can call pciutils at a time, because it's not thread-safe.
var m sync.Mutex

var sysfsDevicesDir = "/sys/bus/pci/devices"

// WriteByte exports pciutils.pci_write_byte().
func WriteByte(dev Dev, addr int32, val uint8) {
	m.Lock()
	defer m.Unlock()
	C.pci_write_byte(dev.cptr, C.int(addr), C.uchar(val))
}

// WriteWord exports pciutils.pci_write_word().
func WriteWord(dev Dev, addr int32, val uint16) {
	m.Lock()
	defer m.Unlock()
	C.pci_write_word(dev.cptr, C.int(addr), C.ushort(val))
}

// WriteLong exports pciutils.pci_write_long().
func WriteLong(dev Dev, addr int32, val uint32) {
	m.Lock()
	defer m.Unlock()
	C.pci_write_long(dev.cptr, C.int(addr), C.uint(val))
}

// ReadByte exports pciutils.pci_read_byte().
func ReadByte(dev Dev, addr int32) uint8 {
	m.Lock()
	defer m.Unlock()
	return uint8(C.pci_read_byte(dev.cptr, C.int(addr)))
}

// ReadWord exports pciutils.pci_read_word().
func ReadWord(dev Dev, addr int32) uint16 {
	m.Lock()
	defer m.Unlock()
	return uint16(C.pci_read_word(dev.cptr, C.int(addr)))
}

// ReadLong exports pciutils.pci_read_long().
func ReadLong(dev Dev, addr int32) uint32 {
	m.Lock()
	defer m.Unlock()
	return uint32(C.pci_read_long(dev.cptr, C.int(addr)))
}

// GetNext exports the next pointer of a Dev.
func (dev Dev) GetNext() Dev {
	return Dev{cptr: dev.cptr.next}
}

////////////////////////////////////////////////////////////////////////////////
// These are helper functions to access pciutils.

// Init allocates an Access instance and initializes it.
func Init() {
	m.Lock()
	defer m.Unlock()
	// This code follows pciutils/setpci.c
	Access = C.pci_alloc() // Get the pci_access structure
	C.pci_init(Access)     // Initialize the PCI library
}

// Cleanup tears down the Access.
func Cleanup() {
	m.Lock()
	defer m.Unlock()
	C.pci_cleanup(Access) // Closes everything at the end.
}

// ScanDevices gets a list of PCI devices.
func ScanDevices() Dev {
	m.Lock()
	defer m.Unlock()
	const HeaderLayoutMask = 0x7f
	C.pci_scan_bus(Access) // We want to get the list of devices.
	devs := Access.devices
	for dev := devs; dev != nil; dev = dev.next {
		// Fills in the header info we need.
		C.pci_fill_info(dev,
			C.PCI_FILL_IDENT|
				C.PCI_FILL_BASES|
				C.PCI_FILL_CLASS|
				C.PCI_FILL_DT_NODE|
				C.PCI_FILL_RESCAN)
		// The Header Type is not always filled. So, manually fills it.
		dev.hdrtype = C.int(C.pci_read_byte(dev, C.int(C.PCI_HEADER_TYPE)) & HeaderLayoutMask)
	}
	return Dev{cptr: devs}
}

// bdfRE matches a PCI device name in the sysfs format, e.g. "0000:15:01.0".
// The domain can be wider than 4 hex digits, e.g. Intel VMD domains start at
// 0x10000.
var bdfRE = regexp.MustCompile(`^([0-9a-fA-F]{1,8}):([0-9a-fA-F]{1,2}):([0-9a-fA-F]{1,2})\.([0-7])$`)

// ParseBDF parses a PCI device name in the sysfs format, e.g. "0000:15:01.0".
// It rejects trailing input and out-of-range fields: the domain must fit in an
// int32, and the device number must be at most 0x1f.
func ParseBDF(name string) (domain, bus, dev, fn int, err error) {
	m := bdfRE.FindStringSubmatch(name)
	if len(m) == 0 {
		return 0, 0, 0, 0, fmt.Errorf("%q is not a BDF in the DDDD:BB:dd.f format", name)
	}
	// The regexp bounds the bus to 0xff and the function to 7.
	d, err := strconv.ParseUint(m[1], 16, 31)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("domain of %q out of range: %w", name, err)
	}
	v, err := strconv.ParseUint(m[3], 16, 5)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("device of %q out of range: %w", name, err)
	}
	b, _ := strconv.ParseUint(m[2], 16, 8)
	f, _ := strconv.ParseUint(m[4], 10, 3)
	return int(d), int(b), int(v), int(f), nil
}

// FindDSP identifies the downstream port (DSP) of an upstream port (USP).
func (dev Dev) FindDSP() (Dev, error) {
	m.Lock()
	defer m.Unlock()
	devName := dev.BDFString()
	devPath := path.Join(sysfsDevicesDir, devName)
	dspPath, err := os.Readlink(devPath)
	if err != nil {
		log.Errorf("BDF[%s]: Failed accessing the device path: %s. Error: %s", devName, devPath, err.Error())
		return Dev{}, err
	}
	dspName := path.Base(path.Dir(dspPath))
	domain, b, d, f, err := ParseBDF(dspName)
	if err != nil {
		log.Errorf("BDF[%s]: Failed parsing DSP BDF: %s. Error: %v", devName, dspName, err)
		return Dev{}, err
	}
	dsp := C.pci_get_dev(Access, C.int(domain), C.int(b), C.int(d), C.int(f))
	// pci_get_dev() only allocates the device with its BDF. Without filling in
	// the identity, the vendor and device IDs read back as 0.
	C.pci_fill_info(dsp, C.PCI_FILL_IDENT)
	return Dev{cptr: dsp}, nil
}

// GetUSP identifies the USP of an DSP device.
func (dev Dev) GetUSP() Dev {
	m.Lock()
	defer m.Unlock()
	if dev.cptr.hdrtype != 1 {
		return Dev{}
	}
	bus := C.pci_read_byte(dev.cptr, C.PCI_SECONDARY_BUS)
	return Dev{cptr: C.pci_get_dev(Access, 0, C.int(bus), 0, 0)}
}

// BDFString gets a device's BDF as a string, or "invalid" if dev is not a valid device.
func (dev Dev) BDFString() string {
	if dev.cptr == nil {
		return "invalid"
	}
	return fmt.Sprintf("%04x:%02x:%02x.%d", dev.cptr.domain, dev.cptr.bus, dev.cptr.dev, dev.cptr._func)
}

// devAt returns a Dev with only its BDF filled in, for tests. It has no access to the config space.
func devAt(domain int, bus, dev, fn uint8) Dev {
	return Dev{cptr: &C.struct_pci_dev{domain: C.int(domain), bus: C.u8(bus), dev: C.u8(dev), _func: C.u8(fn)}}
}

// Valid returns whether Dev represents a valid device.
func (dev Dev) Valid() bool {
	return dev.cptr != nil
}

// FindCapability searches the PCI device's capability list for a capability with the given ID.
// capType specifies whether it is a conventional PCI capability (C.PCI_CAP_NORMAL) or a PCIe extended capability (C.PCI_CAP_EXTENDED).
// It returns the offset of the capability structure.
func FindCapability(dev Dev, capID uint16, capType uint32) (int32, error) {
	m.Lock()
	defer m.Unlock()

	capPtr := C.pci_find_cap(dev.cptr, C.uint(capID), C.uint(capType))
	if capPtr == nil {
		return 0, fmt.Errorf("PCI capability 0x%x (type %d) not found on device %s", capID, capType, dev.BDFString())
	}
	return int32(capPtr.addr), nil
}
