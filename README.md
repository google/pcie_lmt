# PCIe-LMT (PCIe Lane Margin Test)

huat@google.com

The LMT (Lane Margin Test) checks the signal quality of the PCIe link. The test
utilizes the PCIe Lane Margining at Receiver (LMR) feature. The LMR is specified
in PCIe Base Spec 5.0 sections 4.2.13, 7.7.7 and 8.4.4. The LMR samples bits at each offset away from
the eye center and checks against the expected value at the eye center. This
test explores the error rate both timing-wise and voltage-wise. Here's a
[PCIe LMT Overview presentation](https://docs.google.com/presentation/d/1a5xyykoV7n4HS6U9ag1mB2jeEvyaLT4BICTSh0fEqXA)

A [lmt.proto](lmt.proto) specifies all aspects of the test, as well as logging the test result. The
top-level proto message specifies the PCIe link(s) to test. Each receiver point
(upstream port, downstream port, retimers) has a timing spec and/or a voltage
spec. For production testing, the test spec sets a target offset, a sample count
(or in terms of a minimum dwell time), and an error limit. The margining passes
if the error count is within the limit after checking the specified number of
bits at the timing/voltage offset. In characterization usage, the test steps
through incremental offsets and records the error rates. A companion flow plots
the error rates in a Google Sheets.

<img src="lmt_plot_screenshot.png" align="center" />

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for details.

## License

Apache 2.0; see [`LICENSE`](LICENSE) for details.

## Disclaimer

This project is not an official Google project. It is not supported by
Google and Google specifically disclaims all warranties as to its quality,
merchantability, or fitness for a particular purpose.

## History
Version: 0.1 : demo

## Build and Run Commands:
```
bazel build -c opt :lmt
bazel run -c opt :lmt -- -h

bazel run -c opt :lmt -- -alsologtostderr -v=0 \
  -spec=dut_lmt_spec.pbtxt \
  -result=dut_lmt_result.pbtxt \
  -csv=dut_lmt_result.csv \
  -vendor_id=0x1000 -device_id=0xC030 \
  -bus=0x81,0xa1,0x63

bazel-bin/lmt_/lmt \
  -result2csv=dut_lmt_result.pbtxt \
  -csv=dut_lmt_result.csv
```

To plot the result in Google Sheets, make a copy of this [Google Sheet](https://docs.google.com/spreadsheets/d/1-do-2YzfelGlOP8fjFp2cVxNchu6K4X05dwvH-HCBaQ0)
Then import the dut_lmt_result.csv as a new sheet. Click the menu button `LMT
Plot` -> `Create Gradient Charts` to plot the charts.

## Feature Support
- Supports both timing and voltage margining
- Supports retimer receivers
- Works with or without independent error sampler
- Supports lane-parallel-margining when independent error sampler presents
- Supports link-parallel-margining
- Supports both sample count and sample rate reporting methods
- Supports offset sweep: Start|Target|Step
- Supports Linux only: LMT uses the pciutils lib, which is behind the lspci and setpci tools.
- Implemented in Golang tool engine; Protobuf test spec and result; Stand-alone executable main() or other integration layer; Google Sheet colorful plotting.

## Test Spec and Result Examples
Refer to [lmt.proto](lmt.proto).

Test spec for eye-corner-scan:

```
test_specs: {
  aspect: M_VOLTAGE
  receiver: R_RTD_C3
  samples: 110 # log2(bits/3)
  dwell: 10  # seconds
  error_limit: 10  # max 63
  start_offset: 4
  step: 1
  # target_offset: 20
  # lane_number: 0
}
```
Test spec for pass-fail:

```
test_specs: {
  aspect: M_TIMING
  receiver: R_USP_F6
  samples: 100  # 10^(samples/10) bits
  dwell: 3  # seconds
  error_limit: 1
  target_offset: 10  # 31/50%*15%=10
}
```

LMR parameters read from PHY hardware:

```
lane_parameter:  {
  num_timing_steps:  31
  max_timing_offset:  50
  ind_left_right_timing:  true
  voltage_supported:  true
  num_voltage_steps:  127
  max_voltage_offset:  49
  ind_up_down_voltage:  true
  ind_error_sampler:  true
  max_lanes:  15
}
```

Margin result:

```
timing_margins:  {
  direction:  D_RIGHT
  steps:  2
  status:  S_MARGINING
  sample_count:  112
  percent_ui:  0.057142857
}
voltage_margins:  {
  direction:  D_DOWN
  steps:  32
  status:  S_ERROR_OUT
  error_count:  12
  sample_count:  95
  voltage:  0.12346457
}
```

