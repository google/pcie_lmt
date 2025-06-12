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

// This is a standalone PCIe link training test
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"flag"
	
	
	log "github.com/golang/glog"
	lt "local/linktrain"
	pb "ltt_go.proto"
)

var (
	// git_hash := $(git rev-parse --short HEAD || echo 'development')
	// current_time = $(date +"%Y-%m-%d:T%H:%M:%S")
	// go -ldflags "-X main.version=$git_hash -X main.buildTime=$current_time" lmt.go
	// The init value here is stamped by the coder. The binary builder is expected to overwrite them.
	version   = "2024-05-17"
	buildTime = "unknown"

	getVer         = flag.Bool("version", false, "Return the version number.")
	cfgfn          = flag.String("cfgpb", "", "Config pbtxt file name")
	resfn          = flag.String("outpb", "result.pbtxt", "Result pbtxt file name")
	bus            = flag.String("bus", "", "Deprecated. Use -bdf instead. A comma-separted list of bus numbers.")
	bdf            = flag.String("bdf", "", "A comma-separted list of DDDD:BB:dd:f numbers.")
	method         = flag.String("method", "", "Link training method: retrain, sbr, or reenable")
	iterations     = flag.Int("iterations", 0, "The number of link training iterations.")
	parallel       = flag.Bool("parallel", true, "If true, tests multiple links in parallel.")
	teardownwaitms = flag.Int("teardownwaitms", -1, "Wait in milliseconds after teardown.")
	ocpPipe        = flag.String("ocp_pipe", "/dev/null", "Named pipe or file to stream the OCP Artifacts.")
)

func main() {
	
	flag.Parse()

	if *getVer {
		fmt.Printf("Version:\t%s\n", version)
		fmt.Printf("BuildTime:\t%s\n", buildTime)
		os.Exit(0)
	}

	path, err := os.Getwd()
	if err != nil {
		log.Error(err)
	}
	log.V(0).Infoln("The current working directory is ", path)

	// The config proto is required.
	if *cfgfn == "" {
		log.Exit("Error: -cfgpb flag missing.")
	}
	// Checks that the config proto exists.
	if _, err := os.Stat(*cfgfn); os.IsNotExist(err) {
		log.Exit(err)
	}

	// Reads the PCI config protobuf.
	cfg, err := lt.ReadLinkTrainProto(*cfgfn)
	if err != nil {
		log.Exit(err)
	}

	// Overrides BDF from command line flags.
	if *bus != "" {
		busList := strings.Split(*bus, ",")
		if len(busList) != 0 {
			cfg.Bdf = ([]string{})
			for _, busstr := range busList {
				if bus, err := strconv.ParseUint(busstr, 0, 32); err != nil {
					log.Error(busstr, " is not a valid bus number format.")
				} else {
					cfg.Bdf = append(cfg.GetBdf(), fmt.Sprintf("%04x:%02x:%02x.%d", 0, bus, 0, 0) )
				}
			}
		}
	}

	if *bdf != "" {
		bdfList := strings.Split(*bdf, ",")
		if len(bdfList) != 0 {
			cfg.Bdf = ([]string{})
			for _, bdfstr := range bdfList {
				domain := uint16(0)
				b := uint8(0)
				d := uint8(0)
				f := uint8(0)
				if n, _ := fmt.Sscanf(bdfstr, "%04x:%02x:%02x.%d", &domain, &b, &d, &f); n == 4 {
					cfg.Bdf = append(cfg.GetBdf(), fmt.Sprintf("%04x:%02x:%02x.%d", domain, b, d, f))
					continue
				} else if n, _ := fmt.Sscanf(bdfstr, "%02x", &b); n == 1 {
					cfg.Bdf = append(cfg.GetBdf(), fmt.Sprintf("%04x:%02x:%02x.%d", 0, b, 0, 0))
					continue
				} else {
					log.Error(bdfstr, " is not a valid BDF in DDDD:BB:dd:f format.")
				}
			}
		}
	}

	// Overrides method from command line flags.
	switch *method {
	case "retrain":
		cfg.Method = pb.LinkTrain_M_RETRAIN_DEFAULT
		log.V(0).Infoln("cfgpb.method overridden to ", pb.LinkTrain_M_RETRAIN_DEFAULT.String())
	case "sbr":
		cfg.Method = pb.LinkTrain_M_SBR
		log.V(0).Infoln("cfgpb.method overridden to ", pb.LinkTrain_M_SBR.String())
	case "reenable":
		cfg.Method = pb.LinkTrain_M_REENABLE
		log.V(0).Infoln("cfgpb.method overridden to ", pb.LinkTrain_M_REENABLE.String())
	case "": // The method flag is not set.
	default:
		log.Exit("Unknown method: ", *method, "; expecting retrain, sbr, or reenable.")
	}

	// Overrides iterations from command line flags.
	if *iterations > 0 {
		iteration := int32(*iterations)
		cfg.Iterations = iteration
	}

	// Sets parallel to the flag value only when it's specified.
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "parallel" {
			cfg.Parallel = parallel
		}
	})

	// Sets Teardownwaitms to the flag value only when it's specified.
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "teardownwaitms" {
			lt.Teardownwait = time.Duration(*teardownwaitms) * time.Millisecond
		}
	})

	// If the file exists, it's assumed to be a named pipe to append in. Otherwise, it's a file to
	// create and dump into.
	if f, err := os.OpenFile(*ocpPipe, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0777); err != nil {
		log.Fatalf("error opening the ocp_pipe: %s %v", *ocpPipe, err)
	} else {
		lt.OcpInit(f, fmt.Sprintf("pcie_ltt_%s", strings.TrimPrefix(cfg.GetMethod().String(), "M_")),
			version, fmt.Sprint(os.Args))
	}

	// Runs link training test.
	t := time.Now()
	log.V(0).Infoln("Starting LinkTrain: t = ", t.String())
	if _, err := lt.LinkTrain(cfg); err != nil {
		log.Exit(err)
	}
	duration := time.Since(t)
	log.V(0).Infoln("LinkTrain Done: duration = ", duration.String())

	if err := lt.WriteResultPbtxt(*resfn); err != nil {
		log.Exit(err)
	}
}
