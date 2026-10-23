#!/usr/bin/env bash

# Copyright 2026 The HuaTuo Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Verify the runqlat BPF ABI and map-to-metric aggregation independently of E2E.
set -euo pipefail
source "${ROOT_DIR}/integration/lib_cpu_runqlat.sh"

runqlat_map_targets() {
	local name css cpu tids=tasks
	[[ ${RUNQLAT_V2} != true ]] || tids=cgroup.threads
	for name in "${runqlat_pods[@]}"; do
		css=$(curl -sf "${CURL_TIMEOUT[@]}" "${HUATUO_BAMAI_ADDR}/v1/containers/$(< "${RUNQLAT_DIR}/${name}.id")" | jq -er '.data.cgroup_css.cpu') || return 1
		cpu=$(< "${RUNQLAT_DIR}/${name}.cgroup")
		runqlat_target "${name}" | jq --arg css "${css}" --rawfile tids "${cpu}/${tids}" \
			'. + {CSS: $css, TIDs: ($tids | split("\n") | map(select(length > 0) | tonumber))}'
	done | jq -s '.' > "${RUNQLAT_DIR}/targets.json"
}

runqlat_map_check() {
	local phase=$1
	wait_until 20 1 runqlat_map_targets || fatal "${phase}: container CSS metadata did not become ready"
	runqlat_pause
	wait_until 5 1 "${RUNQLAT_BIN}" check --pid "$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")" \
		--url "${HUATUO_BAMAI_METRICS_API}" --targets "${RUNQLAT_DIR}/targets.json" --stable \
		--output "${RUNQLAT_DIR}/${phase}-settled" || fatal "${phase}: stopped map/metric values differ"
	runqlat_resume
}

runqlat_map_phase() {
	local phase=$1 cpus=${2:-1} pid
	pid=$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")
	runqlat_check_cpuset "${a}"
	runqlat_map_targets
	"${RUNQLAT_BIN}" snapshot --pid "${pid}" --targets "${RUNQLAT_DIR}/targets.json" --output "${RUNQLAT_DIR}/${phase}-before.json"
	"${RUNQLAT_BIN}" check --pid "${pid}" --url "${HUATUO_BAMAI_METRICS_API}" --targets "${RUNQLAT_DIR}/targets.json" \
		--output "${RUNQLAT_DIR}/${phase}-active"
	# Keep workload growth separate from SIGSTOP-generated scheduling events.
	sleep 5
	runqlat_check_cpuset "${a}"
	"${RUNQLAT_BIN}" snapshot --pid "${pid}" --targets "${RUNQLAT_DIR}/targets.json" --output "${RUNQLAT_DIR}/${phase}-after.json"
	jq --arg pod "${a}" '[.[] | select(.Pod == $pod)]' "${RUNQLAT_DIR}/targets.json" > "${RUNQLAT_DIR}/active-target.json"
	"${RUNQLAT_BIN}" growth --before "${RUNQLAT_DIR}/${phase}-before.json" --after "${RUNQLAT_DIR}/${phase}-after.json" \
		--targets "${RUNQLAT_DIR}/active-target.json" --zone -1 --switches involuntary --cpus "${cpus}"
	runqlat_map_check "${phase}"
}

runqlat_prepare
RUNQLAT_BIN="${RUNQLAT_DIR}/maps"
if [[ -n ${RUNQLAT_HELPER_BIN:-} ]]; then
	cp "${RUNQLAT_HELPER_BIN}" "${RUNQLAT_BIN}"
else
	require_commands go
	CGO_ENABLED=0 go build -mod=vendor -o "${RUNQLAT_BIN}" \
		"${ROOT_DIR}/integration/testdata/test_cpu_runqlat.go" "${ROOT_DIR}/integration/testdata/test_cpu_runqlat_maps.go" \
		"${ROOT_DIR}/integration/testdata/metrics_helpers.go" "${ROOT_DIR}/integration/testdata/fixture_helpers.go"
fi
runqlat_restart
wait_until 20 1 "${RUNQLAT_BIN}" inspect --pid "$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")" \
	--output "${RUNQLAT_DIR}/objects.json" || fatal "runqlat BPF objects did not become ready"
readonly a="${RUNQLAT_PREFIX}-a" b="${RUNQLAT_PREFIX}-b"
runqlat_create "${a}"
runqlat_create "${b}"
runqlat_map_check idle
runqlat_command "${a}" user 0 4
wait_until 10 1 runqlat_ready "${a}" "${runqlat_generation}" || fatal "busy workers did not initialize"
runqlat_map_phase single-cpu
if ((${#runqlat_cpus[@]} >= 3)); then
	runqlat_pin "${a}" "${runqlat_cpus[-2]},${RUNQLAT_CPU}"
	runqlat_map_phase multi-cpu 2
fi
runqlat_map_targets
"${RUNQLAT_BIN}" concurrent --pid "$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")" \
	--url "${HUATUO_BAMAI_METRICS_API}" --targets "${RUNQLAT_DIR}/targets.json"
jq --arg pod "${a}" '[.[] | select(.Pod == $pod)]' "${RUNQLAT_DIR}/targets.json" > "${RUNQLAT_DIR}/deleted-target.json"
old_id=$(< "${RUNQLAT_DIR}/${a}.id")
kubectl delete pod -n "${RUNQLAT_NS}" "${a}" --timeout=30s
runqlat_pods=("${b}")
assert_huatuo_bamai_containers_absent "deleted runqlat Pod" "${old_id}"
wait_until 60 2 "${RUNQLAT_BIN}" deleted --pid "$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")" \
	--url "${HUATUO_BAMAI_METRICS_API}" --targets "${RUNQLAT_DIR}/deleted-target.json" || fatal "deleted Pod remains in BPF maps or metrics"
for signal in TERM KILL; do
	"${RUNQLAT_BIN}" inspect --pid "$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")" --output "${RUNQLAT_DIR}/before-${signal}.json"
	runqlat_restart "${signal}"
	wait_until 15 1 "${RUNQLAT_BIN}" released --before "${RUNQLAT_DIR}/before-${signal}.json" || fatal "stopped daemon leaked BPF objects"
	runqlat_map_check "after-${signal}"
done
RUNQLAT_DISABLED=true
runqlat_restart
"${RUNQLAT_BIN}" disabled --pid "$(< "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid")" --url "${HUATUO_BAMAI_METRICS_API}"
runqlat_log_check
log_info "runqlat map integration passed; artifacts: ${RUNQLAT_DIR}"
