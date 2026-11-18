#!/usr/bin/env bash
set -euo pipefail

KNO_BIN="${KNO_BIN:-./bin/kno}"
KNO_ENDPOINT="${KNO_ENDPOINT:-http://127.0.0.1:18080}"
M11_DURATION="${M11_DURATION:-1s}"
M11_LIMIT="${M11_LIMIT:-20}"
M11_STRICT_WARNINGS="${M11_STRICT_WARNINGS:-false}"
M11_REQUIRE_GUEST="${M11_REQUIRE_GUEST:-false}"

for setting in M11_STRICT_WARNINGS M11_REQUIRE_GUEST; do
  value="${!setting}"
  if [[ "$value" != "true" && "$value" != "false" ]]; then
    echo "$setting must be true or false" >&2
    exit 2
  fi
done

if [[ -z "${M11_RUNC_NAMESPACE:-}" || -z "${M11_RUNC_POD:-}" ]]; then
  echo "M11_RUNC_NAMESPACE and M11_RUNC_POD are required" >&2
  exit 2
fi
if [[ -n "${M11_KATA_NAMESPACE:-}" || -n "${M11_KATA_POD:-}" ]]; then
  if [[ -z "${M11_KATA_NAMESPACE:-}" || -z "${M11_KATA_POD:-}" ]]; then
    echo "M11_KATA_NAMESPACE and M11_KATA_POD must be set together" >&2
    exit 2
  fi
fi
if [[ "$M11_REQUIRE_GUEST" == "true" && -z "${M11_KATA_NAMESPACE:-}" ]]; then
  echo "M11_REQUIRE_GUEST=true requires a Kata namespace and Pod" >&2
  exit 2
fi
if [[ ! -x "$KNO_BIN" ]]; then
  echo "kno binary is not executable: $KNO_BIN" >&2
  exit 2
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
output_dir="${M11_OUTPUT_DIR:-artifacts/m11-acceptance-${stamp}}"
if [[ -e "$output_dir" ]]; then
  echo "refusing to overwrite existing evidence directory: $output_dir" >&2
  exit 2
fi
mkdir -p "$output_dir"

git_head="unknown"
git_dirty="unknown"
if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git_head="$(git rev-parse HEAD)"
  if [[ -n "$(git status --porcelain)" ]]; then git_dirty="true"; else git_dirty="false"; fi
fi

python3 - "$output_dir/manifest.json" "$stamp" "$git_head" "$git_dirty" "$KNO_ENDPOINT" <<'PY'
import json
import sys

path, stamp, git_head, git_dirty, endpoint = sys.argv[1:]
manifest = {
    "schemaVersion": "kno.m11-evidence/v1",
    "startedAt": stamp,
    "source": {"gitCommit": git_head, "dirty": git_dirty},
    "endpoint": endpoint,
}
with open(path, "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, indent=2)
    handle.write("\n")
PY

run_capture() {
  local stdout_path="$1"
  local stderr_path="$2"
  shift 2
  set +e
  "$@" >"$stdout_path" 2>"$stderr_path"
  local status=$?
  set -e
  printf '%s' "$status"
}

doctor_exit="$(run_capture "$output_dir/doctor.json" "$output_dir/doctor.stderr" \
  "$KNO_BIN" --endpoint "$KNO_ENDPOINT" doctor -o json)"
runc_exit="$(run_capture "$output_dir/inspect-runc.json" "$output_dir/inspect-runc.stderr" \
  "$KNO_BIN" --endpoint "$KNO_ENDPOINT" inspect pod "$M11_RUNC_POD" \
  -n "$M11_RUNC_NAMESPACE" --duration "$M11_DURATION" --limit "$M11_LIMIT" -o json)"

validator=(
  python3 hack/m11_acceptance.py
  --doctor "$output_dir/doctor.json"
  --doctor-exit "$doctor_exit"
  --runc "$output_dir/inspect-runc.json"
  --runc-exit "$runc_exit"
  --runc-namespace "$M11_RUNC_NAMESPACE"
  --runc-pod "$M11_RUNC_POD"
  --output "$output_dir/summary.json"
)

if [[ -n "${M11_KATA_NAMESPACE:-}" ]]; then
  kata_exit="$(run_capture "$output_dir/inspect-kata.json" "$output_dir/inspect-kata.stderr" \
    "$KNO_BIN" --endpoint "$KNO_ENDPOINT" inspect pod "$M11_KATA_POD" \
    -n "$M11_KATA_NAMESPACE" --duration "$M11_DURATION" --limit "$M11_LIMIT" -o json)"
  validator+=(
    --kata "$output_dir/inspect-kata.json"
    --kata-exit "$kata_exit"
    --kata-namespace "$M11_KATA_NAMESPACE"
    --kata-pod "$M11_KATA_POD"
  )
fi
if [[ "$M11_STRICT_WARNINGS" == "true" ]]; then validator+=(--strict-warnings); fi
if [[ "$M11_REQUIRE_GUEST" == "true" ]]; then validator+=(--require-guest); fi

set +e
"${validator[@]}" | tee "$output_dir/validation.stdout"
validation_exit=${PIPESTATUS[0]}
set -e
printf '%s\n' "$validation_exit" >"$output_dir/validation.exit"

echo "M11 evidence: $output_dir"
exit "$validation_exit"
