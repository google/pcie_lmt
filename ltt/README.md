# LTT (PCIe Link Training Test)

The Link Training Test ensures that a PCIe link can be established reliably
every time. The test repeats link training a given number of iterations. Each
iteration is 🥪sandwiched🥪 between link status checking. \
The test can be run as a standalone binary for bring-up/troubleshooting usage.
The test can also be integrated into Voodoo. \
A
[ltt.proto](ltt.proto))
specifies all aspects of the test. The same proto format is used to log the test
result. The proto specifies the PCIe endpoints under test through vendor-device
IDs. PCIe config space register fields are specified in the proto for checking
and logging. The proto also includes the number of link training iterations and
pass/fail counts.

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
  -cfgpb=viperlite_ltt.pbtxt \
  -alsologtostderr=true \
  -v=1

  -- -alsologtostderr -v=0 \
  -spec=dut_lmt_spec.pbtxt \
  -result=dut_lmt_result.pbtxt \
  -csv=dut_lmt_result.csv \
  -ocp_pipe=dut_lmt_ocp.json \
  -vendor_id=0x1000 -device_id=0xC030 \
  -bus=0x81,0xa1,0x63
```

The result is logged in `result.pbtxt`
