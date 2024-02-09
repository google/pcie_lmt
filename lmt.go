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

// PCIe LMT (Lane Margin Test) main()
// This file handles the CLI, and the spec/result pbtxt I/O.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flag"
	
	
	log "github.com/golang/glog"
	pbj "google.golang.org/protobuf/encoding/protojson"
	lmt "local/lanemargintest"
)

var (
	// git_hash := $(git rev-parse --short HEAD || echo 'development')
	// current_time = $(date +"%Y-%m-%d:T%H:%M:%S")
	// go -ldflags "-X main.version=$git_hash -X main.buildTime=$current_time" lmt.go
	// The init value here is stamped by the coder. The binary builder is expected to overwrite them.
	version   = "2024-02-04"
	buildTime = "unknown"
)

func main() {
	var (
		getVer   = flag.Bool("version", false, "Return the version number.")
		vid      = flag.Int("vendor_id", -1, "The 16-bit Vendor ID of the USP (such as the EP).")
		did      = flag.Int("device_id", -1, "The 16-bit Device ID of the USP (such as the EP).")
		bus      = flag.String("bus", "", "A comma-separted list of bus numbers.")
		spec     = flag.String("spec", "", "The test spec .pbtxt file.")
		specJSON = flag.String("spec_json", "", "The test spec .json file.")
		result   = flag.String("result", "result.pbtxt", "The result pbtxt file name.")
		csv      = flag.String("csv", "", "Dumps a csv file for plotting.")
		pb2csv   = flag.Bool("result2csv", false, "Converts the [result] to a [csv] file for plotting.")
	)
	
	flag.Parse()

	if *getVer {
		fmt.Printf("Version:\t%s\n", version)
		fmt.Printf("BuildTime:\t%s\n", buildTime)
		os.Exit(0)
	}

	if *pb2csv {
		if *csv == "" || *result == "" {
			log.Exit("Error: With -result2csv, both -result and -csv must be specified.")
		}
		lmt.ReadResult(*result)
		lmt.ConvertToCsv(*csv)

		os.Exit(0)
	}

	// The test spec or spec_json is required.
	var fn string
	var isJSON bool
	if *spec != "" {
		fn = *spec
		isJSON = false
	} else if *specJSON != "" {
		fn = *specJSON
		isJSON = true
	} else {
		log.Exit("Error: Either -spec or -spec_json must be specified.")
	}
	// Reads the test spec
	cfg, err := lmt.ReadLinkMargin(fn, isJSON)
	if err != nil {
		log.Exit(err)
	}

	// Overrides Vendor ID from command line flags.
	if *vid != -1 {
		if *vid < 0 || *vid > 0xFFFF {
			log.Exit("The vendor_id = ", fmt.Sprintf("%04x", *vid), " option is out of range [0:0xFFFF].")
		}
		vendorID := uint32(*vid)
		cfg.VendorId = &vendorID
	}
	// Overrides Vendor ID from command line flags.
	if *did != -1 {
		if *did < 0 || *did > 0xFFFF {
			log.Exit("The device_id = ", fmt.Sprintf("%04x", *did), " option is out of range [0:0xFFFF].")
		}
		deviceID := uint32(*did)
		cfg.DeviceId = &deviceID
	}

	// Overrides BDF from command line flags.
	if *bus != "" {
		busList := strings.Split(*bus, ",")
		if len(busList) != 0 {
			cfg.Bus = ([]uint32{})
			for _, busstr := range busList {
				if bus, err := strconv.ParseUint(busstr, 0, 32); err != nil {
					log.Error(busstr, " is not a valid bus number format.")
				} else {
					cfg.Bus = append(cfg.GetBus(), uint32(bus))
				}
			}
		}
	}

	// Automatically dump the test spec to a spec.dump.json.
	opt := pbj.MarshalOptions{
		UseProtoNames:   true,
		UseEnumNumbers:  false,
		EmitUnpopulated: false,
		Multiline:       true,
		Indent:          "  ",
		AllowPartial:    false,
	}
	fn = strings.TrimSuffix(fn, filepath.Ext(fn)) + ".dump.json"
	if data, err := opt.Marshal(cfg); err != nil {
		log.Exit(err)
	} else if err := os.WriteFile(fn, data, 0600); err != nil {
		log.Exit(err)
	}

	// Runs lane margin test.
	t := time.Now()
	log.Infoln("Starting LMT: t = ", t.String())
	if err := lmt.MarginLinks(cfg); err != nil {
		log.Exit(err)
	}
	duration := time.Since(t)
	log.Infoln("Finished lane margining: duration = ", duration.String())

	if err := lmt.WriteResultPbtxt(*result); err != nil {
		log.Exit(err)
	}
	if *csv != "" {
		lmt.ConvertToCsv(*csv)
	}
}
