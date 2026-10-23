# Copyright 2023 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load("@bazel_gazelle//:def.bzl", "gazelle")
load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library", "go_test")
load("@io_bazel_rules_go//proto:def.bzl", "go_proto_library")
load("@rules_proto//proto:defs.bzl", "proto_library")

# gazelle:prefix local/pcie_lmt
gazelle(name = "gazelle")

config_setting(
    name = "dynamic_libpci",
    define_values = {"pci_link": "dynamic"},
    visibility = ["//visibility:public"],
)

proto_library(
    name = "lmt_proto",
    srcs = ["lmt.proto"],
    visibility = ["//visibility:public"],
)

go_proto_library(
    name = "lmt_go_proto",
    importpath = "lmt_go.proto",
    proto = ":lmt_proto",
)

go_library(
    name = "lanemargintest",
    srcs = [
        "lanemargintest.go",
        "lmt_cmdrsp.go",
        "lmt_lane.go",
        "lmt_link.go",
        "lmt_margin_hook.go",
        "lmt_ocp_writer.go",
        "lmt_offset.go",
        "lmt_result2csv.go",
        "lmt_result2html.go",
        "lmt_scheduler.go",
        "lmt_tally.go",
    ],
    cdeps = [
        "@pciutils//:libpci",
    ],
    cgo = 1,
    importpath = "local/lanemargintest",
    deps = [
        ":lmt_go_proto",
        ":pciutils",
        "@com_github_golang_glog//:go_default_library",
        "@ocpdiag//:results_go_proto",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//encoding/prototext",
        "@org_golang_google_protobuf//proto",
        "@org_golang_google_protobuf//types/known/structpb",
        "@org_golang_google_protobuf//types/known/timestamppb",
    ],
)

go_test(
    name = "lanemargintest_test",
    srcs = [
        "fake_lmr_test.go",
        "lanemargintest_test.go",
        "lmt_cmdrsp_test.go",
        "lmt_lane_test.go",
        "lmt_link_test.go",
        "lmt_margin_hook_test.go",
        "lmt_ocp_writer_test.go",
        "lmt_offset_test.go",
        "lmt_scheduler_test.go",
        "log_capture_test.go",
    ],
    embed = [":lanemargintest"],
)

# USE_BAZEL_VERSION=7.5.0 bazelisk \
# bazel build //:lmt \
#   --define pci_link=dynamic \
#   --platforms=@io_bazel_rules_go//go/toolchain:linux_arm64_cgo
go_binary(
    name = "lmt",
    srcs = ["lmt.go"],
    x_defs = {
        "main.version": "{VERSION}",
        "main.buildTime": "{BUILD_TIME}",
    },
    deps = [
        ":lanemargintest",
        ":lmt_go_proto",
        "@com_github_golang_glog//:go_default_library",
        "@ocpdiag//:results_go_proto",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//types/known/structpb",
    ],
)

go_library(
    name = "pciutils",
    srcs = ["pciutils.go"],
    cdeps = [
        "@pciutils//:libpci",
    ],
    cgo = 1,
    clinkopts = select({
        "@//:dynamic_libpci": [],
        "//conditions:default": ["-static"],
    }),
    importpath = "pciutils",
    visibility = ["//visibility:public"],
    deps = [
        "@com_github_golang_glog//:go_default_library",
    ],
)

go_test(
    name = "pciutils_test",
    srcs = ["pciutils_test.go"],
    embed = [":pciutils"],
)
