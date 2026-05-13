"""Module extensions for non-module dependencies."""

load("@bazel_tools//tools/build_defs/repo:git.bzl", "git_repository")
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

def _non_module_deps_impl():
    # ocpdiag
    git_repository(
        name = "ocpdiag",
        build_file = "@pcie_lmt//:result_proto.BUILD",
        commit = "7f38c4d",
        remote = "https://github.com/opencomputeproject/ocp-diag-core-cpp",
        patch_cmds = [
            "rm -f ocpdiag/core/results/data_model/BUILD",
            "rm -f ocpdiag/core/results/BUILD",
            "rm -f ocpdiag/core/BUILD",
            "rm -f ocpdiag/BUILD",
        ],
    )

    # pciutils
    http_archive(
        name = "pciutils",
        build_file = "@pcie_lmt//:pciutils.BUILD",
        sha256 = "e579d87f1afe2196db7db648857023f80adb500e8194c4488c8b47f9a238c1c6",
        strip_prefix = "pciutils-3.10.0",
        url = "https://github.com/pciutils/pciutils/archive/refs/tags/v3.10.0.tar.gz",
    )

    # zlib (needed by pciutils)
    http_archive(
        name = "zlib",
        build_file = "@pcie_lmt//:zlib.BUILD",
        sha256 = "b5b06d60ce49c8ba700e0ba517fa07de80b5d4628a037f4be8ad16955be7a7c0",
        strip_prefix = "zlib-1.3",
        url = "https://github.com/madler/zlib/archive/refs/tags/v1.3.tar.gz",
    )

non_module_deps = module_extension(
    implementation = _non_module_deps_impl,
)
