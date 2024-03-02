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
load("@io_bazel_rules_go//go:def.bzl", "go_binary", "go_library")
load("@io_bazel_rules_go//proto:def.bzl", "go_proto_library")
load("@com_google_protobuf//:protobuf.bzl", "py_proto_library")

# gazelle:prefix local/pcie_lmt
gazelle(name = "gazelle")

proto_library(
    name = "lmt_proto",
    srcs = ["lmt.proto"],
)

go_proto_library(
    name = "lmt_go_proto",
    importpath = "lmt_go.proto",
    proto = ":lmt_proto",
)

py_proto_library(
    name = "lmt_py_proto",
    srcs = ["lmt.proto"],
)

go_library(
    name = "lanemargintest",
    srcs = [
        "lanemargintest.go",
        "lmt_cmdrsp.go",
        "lmt_lane.go",
        "lmt_link.go",
        "lmt_offset.go",
        "lmt_result2csv.go",
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
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//encoding/prototext",
        "@org_golang_google_protobuf//proto",
    ],
)

# bazel build //:lmt 
# TODO(anyone?): --platforms=@io_bazel_rules_go//go/toolchain:linux_arm_cgo gives error:
#       Unable to find a CC toolchain
go_binary(
    name = "lmt",
    srcs = ["lmt.go"],
    x_defs = {"main.version":"{VERSION}", "main.buildTime":"{BUILD_TIME}"},
    deps = [
        ":lanemargintest",
        "@com_github_golang_glog//:go_default_library",
        "@org_golang_google_protobuf//encoding/protojson",
    ],
)

go_library(
    name = "pciutils",
    srcs = ["pciutils.go"],
    cdeps = [
        "@pciutils//:libpci",
    ],
    cgo = 1,
    importpath = "pciutils",
    deps = [
        "@com_github_golang_glog//:go_default_library",
    ],
)
