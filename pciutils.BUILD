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

load("@bazel_skylib//rules:write_file.bzl", "write_file")

genrule(
    name = "gen_pci_pci_h",
    srcs = ["lib/pci.h"],
    outs = ["pci/pci.h"],
    cmd = "cp -f $<  $@",
)

write_file(
    name = "gen_lib_config_h",
    out = "lib/config.h",
    content = [
        "#if defined(__x86_64__)",
        "#define PCI_ARCH_X86_64",
        "#define PCI_HAVE_PM_INTEL_CONF",
        "#elif defined(__powerpc__)",
        "#define PCI_ARCH_POWERPC",
        "#elif defined(__aarch64__)",
        "#define PCI_ARCH_AARCH64",
        "#else",
        "#error UNSUPPORTED_ARCHITECTURE",
        "#endif",
        "",
        "#define PCI_CONFIG_H",
        "#define PCI_OS_LINUX",
        "#define PCI_HAVE_PM_LINUX_SYSFS",
        "#define PCI_HAVE_PM_LINUX_PROC",
        "#define PCI_HAVE_LINUX_BYTEORDER_H",
        '#define PCI_PATH_PROC_BUS_PCI "/proc/bus/pci"',
        '#define PCI_PATH_SYS_BUS_PCI "/sys/bus/pci"',
        "#define PCI_HAVE_64BIT_ADDRESS",
        "#define PCI_HAVE_PM_DUMP",
        '#define PCI_IDS "pci.ids"',
        '#define PCI_PATH_IDS_DIR "/usr/share/misc"',
        "#define PCILIB_VERSION 3.9.0",
    ],
)

cc_library(
    name = "libpci",
    srcs = [
        "lib/access.c",
        "lib/caps.c",
        "lib/dump.c",
        "lib/filter.c",
        "lib/generic.c",
        "lib/header.h",
        "lib/init.c",
        "lib/internal.h",
        "lib/names.c",
        "lib/names.h",
        "lib/names-cache.c",
        "lib/names-hash.c",
        "lib/names-hwdb.c",
        "lib/names-net.c",
        "lib/names-parse.c",
        "lib/params.c",
        "lib/pci.h",
        "lib/pread.h",
        "lib/proc.c",
        "lib/sysdep.h",
        "lib/sysfs.c",
        "lib/types.h",
        ":lib/config.h",
    ] + select({
        "@platforms//cpu:x86_64": [
            "lib/i386-io-linux.h",
            "lib/i386-ports.c",
        ],
        "//conditions:default": [],
    }),
    hdrs = ["pci/pci.h"],
    copts = [
        "-Wno-error",
        "-w",
    ],
    hdrs_check = "strict",
    includes = [
        ".",
        "lib",
    ],
    visibility = ["//visibility:public"],
    deps = ["@zlib"],
)
