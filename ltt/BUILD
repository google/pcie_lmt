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
load("@rules_proto//proto:defs.bzl", "proto_library")

# gazelle:prefix local/pcie_lmt
gazelle(name = "gazelle")

proto_library(
    name = "ltt_proto",
    srcs = ["ltt.proto"],
    visibility = ["//visibility:public"],
)

go_proto_library(
    name = "ltt_go_proto",
    importpath = "ltt_go.proto",
    proto = ":ltt_proto",
)

go_library(
    name = "linktrain",
    srcs = [
        "linktrain.go",
    ],
    cdeps = [
        "@pciutils//:libpci",
    ],
    cgo = 1,
    importpath = "local/linktrain",
    deps = [
        ":ltt_go_proto",
        "//:pciutils",
        "@com_github_golang_glog//:go_default_library",
        "@ocpdiag//:results_go_proto",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//encoding/prototext",
        "@org_golang_google_protobuf//proto",
        "@org_golang_google_protobuf//types/known/structpb",
        "@org_golang_google_protobuf//types/known/timestamppb",
    ],
)

# bazel build //:ltt \
#   --platforms=@io_bazel_rules_go//go/toolchain:linux_arm64_cgo
go_binary(
    name = "ltt",
    srcs = ["ltt.go"],
    x_defs = {
        "main.version": "{VERSION}",
        "main.buildTime": "{BUILD_TIME}",
    },
    deps = [
        ":linktrain",
        ":ltt_go_proto",
        "@com_github_golang_glog//:go_default_library",
        "@ocpdiag//:results_go_proto",
        "@org_golang_google_protobuf//encoding/protojson",
        "@org_golang_google_protobuf//types/known/structpb",
    ],
)