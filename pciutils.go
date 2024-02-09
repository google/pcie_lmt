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
	"sync"

	log "github.com/golang/glog"
)

// Dev exports the pciutils' device struct.
type Dev = C.struct_pci_dev

// PCIDevInfo struct is used to export C.struct_pci_dev members.
type PCIDevInfo struct {
	VendorID, DeviceID, Domain uint16
	Bus, Dev, Func             uint8
	HdrType                    int32
}

// GetDevInfo fills a PCIDevInfo from a Dev, as Dev members are not exported.
func (dev *Dev) GetDevInfo() PCIDevInfo {
	info := PCIDevInfo{
		VendorID: uint16(dev.vendor_id),
		DeviceID: uint16(dev.device_id),
		Domain:   uint16(dev.domain_16),
		Bus:      uint8(dev.bus),
		Dev:      uint8(dev.dev),
		Func:     uint8(dev._func),
		HdrType:  int32(dev.hdrtype),
	}
	return info
}

// Access exports the pciutils' access instance.
var Access *C.struct_pci_access

// Only one goroutine can call pciutils at a time, because it's not thread-safe.
var m sync.Mutex

// WriteByte exports pciutils.pci_write_byte().
func WriteByte(dev *Dev, addr int32, val uint8) {
	m.Lock()
	defer m.Unlock()
	C.pci_write_byte(dev, C.int(addr), C.uchar(val))
}

// WriteWord exports pciutils.pci_write_word().
func WriteWord(dev *Dev, addr int32, val uint16) {
	m.Lock()
	defer m.Unlock()
	C.pci_write_word(dev, C.int(addr), C.ushort(val))
}

// WriteLong exports pciutils.pci_write_long().
func WriteLong(dev *Dev, addr int32, val uint32) {
	m.Lock()
	defer m.Unlock()
	C.pci_write_long(dev, C.int(addr), C.uint(val))
}

// ReadByte exports pciutils.pci_read_byte().
func ReadByte(dev *Dev, addr int32) uint8 {
	m.Lock()
	defer m.Unlock()
	return uint8(C.pci_read_byte(dev, C.int(addr)))
}

// ReadWord exports pciutils.pci_read_word().
func ReadWord(dev *Dev, addr int32) uint16 {
	m.Lock()
	defer m.Unlock()
	return uint16(C.pci_read_word(dev, C.int(addr)))
}

// ReadLong exports pciutils.pci_read_long().
func ReadLong(dev *Dev, addr int32) uint32 {
	m.Lock()
	defer m.Unlock()
	return uint32(C.pci_read_long(dev, C.int(addr)))
}

// GetNext exports the next pointer of a Deva.
func (dev *Dev) GetNext() *Dev {
	return dev.next
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
func ScanDevices() *Dev {
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
	return devs
}

// FindDSP identifies the downstream port (DSP) of an upstream port (USP).
func (dev *Dev) FindDSP() (*Dev, error) {
	m.Lock()
	defer m.Unlock()
	devName := dev.BDFString()
	devPath := path.Join("/sys/bus/pci/devices", devName)
	dspPath, err := os.Readlink(devPath)
	if err != nil {
		log.Errorf("Failed accessing the device path: %s. Error: %s", devPath, err.Error())
		return nil, err
	}
	dspName := path.Base(path.Dir(dspPath))
	var domain, b, d, f C.int
	if n, err := fmt.Sscanf(dspName, "%04x:%02x:%02x.%d", &domain, &b, &d, &f); err != nil || n != 4 {
		log.Errorf("Failed parsing DSP BDF: %s. Error: %s", dspName, err.Error())
		return nil, err
	}
	dsp := C.pci_get_dev(Access, domain, b, d, f)
	return dsp, nil
}

// GetUSP identifies the USP of an DSP device.
func (dev *Dev) GetUSP() *Dev {
	m.Lock()
	defer m.Unlock()
	if dev.hdrtype != 1 {
		return nil
	}
	bus := C.pci_read_byte(dev, C.PCI_SECONDARY_BUS)
	return C.pci_get_dev(Access, 0, C.int(bus), 0, 0)
}

// BDFString gets a device's BDF as a string.
func (dev *Dev) BDFString() string {
	return fmt.Sprintf("%04x:%02x:%02x.%d", dev.domain, dev.bus, dev.dev, dev._func)
}
