# LTT (PCIe Link Training Test)

The Link Training Test ensures that a PCIe link can be established reliably
every time. The test repeats link training a given number of iterations. Each
iteration is 🥪sandwiched🥪 between link status checking. \
The test can be run as a standalone binary for bring-up/troubleshooting usage.
The test can also be integrated into Voodoo. \
A [ltt.proto](ltt.proto) specifies all aspects of the test. The same proto
format is used to log the test result. The proto specifies the PCIe links under
test through `link_target` (`PcieLinkTarget`), which matches the vendor IDs,
device IDs, and BDFs of both the upstream port (`usp_vendor_id`,
`usp_device_id`, `usp_bdfs`) and downstream port (`dsp_vendor_id`,
`dsp_device_id`, `dsp_bdfs`) ends of each link. All set fields in `link_target`
are ANDed, and unset fields (or empty BDF lists) act as wildcards. At least one
of these six filter fields must be set, either in the spec or through the `-bdf`
or `-bus` flags: a spec without any filter fails before any link is touched,
unless `link_target` sets `all_links: true` to explicitly train every eligible
link. PCIe config
space register fields are specified in the proto for checking and logging. The
proto also includes the number of link training iterations and pass/fail counts.

## Build and Run Commands:
```shell
# Not yet ported to the bzlmod required by bazel v8.0.1
# So build with v7.5.0 by using bazelisk
sudo npm install -g @bazel/bazelisk

USE_BAZEL_VERSION=7.5.0 bazelisk build -c opt ltt:ltt

USE_BAZEL_VERSION=7.5.0 bazelisk build -c opt ltt:ltt \
  --platforms=@io_bazel_rules_go//go/toolchain:linux_arm64_cgo  # ARM support

USE_BAZEL_VERSION=7.5.0 bazelisk run -c opt ltt:ltt -- -h

bazel-bin/ltt/ltt_/ltt \
  -cfgpb=ltt/example_ltt.pbtxt \
  -outpb=result.pbtxt \
  -bdf=0000:81:00.0,0000:a1:00.0 \
  -alsologtostderr=true \
  -v=1 \
  -csv=dut_lmt_result.csv \
  -ocp_pipe=dut_lmt_ocp.json
```

The result is logged in `result.pbtxt`.

## Test Spec Example
Refer to [ltt.proto](ltt.proto) and [example_ltt.pbtxt](example_ltt.pbtxt).

```
link_target {
  usp_vendor_id: 0x1000
  usp_device_id: 0xc030
  usp_bdfs: "0000:81:00.0"
  usp_bdfs: "0000:a1:00.0"
  dsp_vendor_id: 0x8086
  dsp_device_id: 0x352a
  dsp_bdfs: "0000:80:01.0"
  dsp_bdfs: "0000:a0:01.0"
}
# To deliberately train every eligible link instead, replace the filters above
# with:
#   link_target { all_links: true }

iterations: 100
method: M_REENABLE  # Other options are M_SBR or M_RETRAIN_DEFAULT
training_wait_ms: 1000

continue: true
parallel: true

recover_pci_config_space_header: true
recover_pcie_cap_ctrl_regs: true
recover_sriov_cap_regs: true

field: {
  name: "Speed"
  state: S_CHECK
  addr: 0x92
  size: UINT16
  mask: 0x000f
  expected: 0x0005
}

field: {
  name: "Width"
  state: S_CHECK
  addr: 0x92
  size: UINT16
  mask: 0x03f0
  expected: 0x0080
}
```
