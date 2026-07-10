#!/usr/bin/env bash
# GoReleaser build post-hook (ADR 16 decision 12): Apple Silicon's AMFI
# SIGKILLs a darwin binary that lacks a valid signature, and Go binaries
# cross-compiled on a linux runner carry only a linker signature macOS 26
# can treat as corrupt. Ad-hoc signing (no certificate) fixes this for free
# and must run on every darwin artifact — silently skipping it here
# resurfaces as a SIGKILL on someone's Mac. Runs for every build target
# (darwin and linux); no-ops on non-darwin so one hook definition covers all.
set -euo pipefail

binary_path="$1"
target_os="$2"

if [[ "${target_os}" != "darwin" ]]; then
  exit 0
fi

if ! command -v quill >/dev/null 2>&1; then
  echo "adhoc-sign: quill not found on PATH — refusing to ship an unsigned darwin binary (AMFI would SIGKILL it on Apple Silicon)" >&2
  exit 1
fi

quill sign --ad-hoc "${binary_path}"
