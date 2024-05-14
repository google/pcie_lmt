# Copyright 2023 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# The data model for the results library, including functions for converting back and forth between
# protobuf and struct representations.

load("@io_bazel_rules_go//proto:def.bzl", "go_proto_library")

licenses(["notice"])

package(default_visibility = ["//visibility:public"])

go_proto_library(
    name = "results_go_proto",
    importpath = "ocpdiag/results_go_proto",
    proto = "@ocpdiag//ocpdiag/core/results/data_model:results_proto",
)
