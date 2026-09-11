#!/usr/bin/env bash
set -euo pipefail

tokenizers_commit="f678a7768d5479d9d5a5161c4fc45c8a5ba46146"
rust_version="1.95.0"

# The output directory is not a free choice: it is where the cgo directives in
# internal/embed/onnx.go already look. Linux carries the architecture because
# only amd64 is built; darwin does not, because upstream ONNX Runtime ships one
# macOS asset and it is arm64, so there is no second darwin target to name.
host_os="$(uname -s)"
host_arch="$(uname -m)"
case "${host_os}/${host_arch}" in
  Linux/x86_64)
    host_triple="x86_64-unknown-linux-gnu"
    rust_target="x86_64-unknown-linux-gnu"
    lib_dir="linux-amd64"
    ;;
  Darwin/arm64)
    host_triple="aarch64-apple-darwin"
    rust_target="aarch64-apple-darwin"
    lib_dir="darwin"
    ;;
  Darwin/x86_64)
    echo "Intel macOS is not supported: ONNX Runtime publishes no x86_64 darwin build" >&2
    exit 1
    ;;
  *)
    echo "unsupported build host: ${host_os}/${host_arch}" >&2
    exit 1
    ;;
esac

rust_toolchain="${rust_version}-${host_triple}"
build_revision="${tokenizers_commit} rust=${rust_toolchain} target=${rust_target}"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
output_path="${1:-${repo_root}/libs/${lib_dir}/libtokenizers.a}"
revision_path="${output_path}.revision"
target_dir="${CARGO_TARGET_DIR:-${repo_root}/.task/tokenizers-target/${lib_dir}}"

if [[ "${FORCE:-0}" != "1" && -f "${output_path}" && -f "${revision_path}" ]]; then
  built_revision="$(tr -d '[:space:]' < "${revision_path}")"
  if [[ "${built_revision}" == "${build_revision}" ]]; then
    echo "libtokenizers.a already matches ${build_revision}"
    exit 0
  fi
fi

# cc rather than gcc: macOS has clang under that name and no gcc.
for command_name in git cargo rustup cc; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "required command is missing: ${command_name}" >&2
    exit 1
  fi
done

work_dir="$(mktemp -d -t contextmaxxer-tokenizers-XXXXXXXX)"
cleanup() {
  case "${work_dir}" in
    "${TMPDIR:-/tmp}"/contextmaxxer-tokenizers-*) rm -rf -- "${work_dir}" ;;
    *) echo "refusing to clean unexpected build directory: ${work_dir}" >&2 ;;
  esac
}
trap cleanup EXIT

git -C "${work_dir}" init --quiet
git -C "${work_dir}" remote add origin https://github.com/daulet/tokenizers
git -C "${work_dir}" fetch --depth 1 origin "${tokenizers_commit}"
git -C "${work_dir}" checkout --quiet --detach FETCH_HEAD

echo "Building daulet/tokenizers ${tokenizers_commit} for ${rust_target}..."
# rustup prints "<toolchain> (active, default)", so the name is the first field
# and never has a trailing dash. Matching one reinstalled the toolchain on every
# build.
if ! rustup toolchain list | awk '{print $1}' | grep -qxF "${rust_toolchain}"; then
  rustup toolchain install "${rust_toolchain}" --profile minimal
fi
if ! rustup target list --installed --toolchain "${rust_toolchain}" | grep -qx "${rust_target}"; then
  rustup target add "${rust_target}" --toolchain "${rust_toolchain}"
fi
(
  cd -- "${work_dir}"
  export CARGO_TARGET_DIR="${target_dir}"
  cargo "+${rust_toolchain}" build --release
)

source_path="${target_dir}/release/libtokenizers_ffi.a"
if [[ ! -f "${source_path}" ]]; then
  echo "build succeeded but ${source_path} was not produced" >&2
  exit 1
fi

mkdir -p -- "$(dirname -- "${output_path}")"
install -m 0644 "${source_path}" "${output_path}"
printf '%s\n' "${build_revision}" > "${revision_path}"
echo "libtokenizers.a built at ${output_path}"
