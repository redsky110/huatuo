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

set -euo pipefail

source "${ROOT_DIR}/integration/lib.sh"
source "${ROOT_DIR}/integration/config.sh"
source "${ROOT_DIR}/integration/lib_cgroup.sh"
source "${ROOT_DIR}/e2e/lib.sh"
source "${ROOT_DIR}/e2e/lib_cpu_load.sh"
source "${ROOT_DIR}/e2e/lib_cpu_metrics.sh"

runqlat_cleanup() {
	local status=$?
	trap - EXIT
	stop_and_wait_by_pid "${runqlat_host_pid}" || status=1
	if [[ ${status} -ne 0 && ${status} -ne 77 ]]; then
		kubectl --request-timeout=10s describe pod -n "${RUNQLAT_NS}" -l "${RUNQLAT_LABEL}" \
			> "${RUNQLAT_DIR}/pods-failure.txt" 2>&1 || true
	fi
	kubectl --request-timeout=30s delete pod -n "${RUNQLAT_NS}" -l "${RUNQLAT_LABEL}" \
		--ignore-not-found --timeout=30s || status=1
	huatuo_bamai_stop || status=1
	exit "${status}"
}

runqlat_prepare() {
	require_commands kubectl jq findmnt ss taskset
	require_readable "${KUBELET_CERT}" "${KUBELET_KEY}"
	kprobe_available free_fair_sched_group || skip "free_fair_sched_group kprobe is unavailable"
	for event in sched_wakeup sched_wakeup_new sched_switch sched_process_exit; do
		tracepoint_available sched "${event}" || skip "sched/${event} tracepoint is unavailable"
	done

	RUNQLAT_DIR="${HUATUO_BAMAI_TEST_TMPDIR}/runqlat"
	RUNQLAT_LOAD="${RUNQLAT_DIR}/cpu-load"
	RUNQLAT_NS="${BUSINESS_POD_NS}"
	RUNQLAT_PREFIX="runqlat-${BASHPID}-${RANDOM}"
	RUNQLAT_LABEL="app=${RUNQLAT_PREFIX}"
	RUNQLAT_DISABLED=false
	runqlat_generation=0
	runqlat_daemon_generation=0
	runqlat_pods=()
	runqlat_host_pid=""
	trap runqlat_cleanup EXIT
	mkdir -p "${RUNQLAT_DIR}"
	chmod 755 "${RUNQLAT_DIR}"
	uname -a > "${RUNQLAT_DIR}/kernel.txt"
	"${HUATUO_BAMAI_BIN}" --version > "${RUNQLAT_DIR}/bamai-version.txt"

	cpu_load_prepare "${RUNQLAT_DIR}"
	cpu_metrics_prepare "${RUNQLAT_DIR}"

	RUNQLAT_NODE=$(kubelet_pods_json | jq -er '[.items[].spec.nodeName] | unique | if length == 1 then .[0] else error("cannot identify local node") end')
	mapfile -t runqlat_cpus < <(awk '/Cpus_allowed_list/ {
	n=split($2, ranges, ",")
	for (i=1; i<=n; i++) {
		m=split(ranges[i], ends, "-"); last=(m==2 ? ends[2] : ends[1])
		for (cpu=ends[1]; cpu<=last; cpu++) print cpu
	}
}' /proc/self/status)
	RUNQLAT_CPU=${runqlat_cpus[-1]}
	RUNQLAT_CPU_ROOT=$(findmnt -rn -t cgroup -O cpu -o TARGET || true)
	RUNQLAT_V2=false
	if [[ -n ${RUNQLAT_CPU_ROOT} ]]; then
		RUNQLAT_CPUSET_ROOT=$(findmnt -rn -t cgroup -O cpuset -o TARGET)
	else
		RUNQLAT_V2=true
		RUNQLAT_CPU_ROOT=$(findmnt -rn -t cgroup2 -o TARGET) || skip "CPU cgroup controller is not mounted"
		RUNQLAT_CPUSET_ROOT=${RUNQLAT_CPU_ROOT}
	fi
	RUNQLAT_PORT=$(allocate_available_port) || fatal "cannot allocate runqlat metrics port"
	RUNQLAT_KUBELET_PORT=${KUBELET_PODS_API##*:}
	RUNQLAT_KUBELET_PORT=${RUNQLAT_KUBELET_PORT%%/*}
	log_info "runqlat: cgroup_v2=${RUNQLAT_V2} workload_cpu=${RUNQLAT_CPU}"

	if [[ -f ${HUATUO_BAMAI_TEST_TMPDIR}/huatuo.log ]]; then
		huatuo_bamai_stop
		huatuo_bamai_log_check || fatal "runner baseline daemon has unexpected errors"
		cp "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo.log" "${RUNQLAT_DIR}/baseline.log"
	fi
	HUATUO_BAMAI_ADDR="http://127.0.0.1:${RUNQLAT_PORT}"
	HUATUO_BAMAI_METRICS_API="${HUATUO_BAMAI_ADDR}/metrics"

}

runqlat_restart() {
	local signal=${1:-TERM}
	if [[ -f ${HUATUO_BAMAI_TEST_TMPDIR}/huatuo-bamai.pid ]]; then
		huatuo_bamai_stop_with_signal "${signal}"
		runqlat_log_check
		cp "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo.log" "${RUNQLAT_DIR}/daemon-${runqlat_daemon_generation}.log"
	fi
	runqlat_daemon_generation=$((runqlat_daemon_generation + 1))
	integration_huatuo_bamai_start write_runqlat_config --region e2e --disable-storage
}

runqlat_log_check() {
	huatuo_bamai_log_check || fatal "runqlat daemon has unexpected errors"
	if grep -q 'runqlat: update container cache' "${HUATUO_BAMAI_TEST_TMPDIR}/huatuo.log"; then
		fatal "runqlat failed to refresh container counters; inspect huatuo.log"
	fi
}

runqlat_ready() {
	kubectl --request-timeout=5s logs -n "${RUNQLAT_NS}" "$1" -c worker --tail=5 | grep -Fxq "ready $2"
}

runqlat_command() {
	local name=$1 mode=$2 milli=$3 workers=${4:-1}
	runqlat_generation=$((runqlat_generation + 1))
	printf '%s %s %s %s\n' "${mode}" "${milli}" "${workers}" "${runqlat_generation}" > "${RUNQLAT_DIR}/${name}.command.next"
	mv "${RUNQLAT_DIR}/${name}.command.next" "${RUNQLAT_DIR}/${name}.command"
}

runqlat_create() {
	local name=$1 id cpuset
	runqlat_command "${name}" idle 0
	jq -n --arg name "${name}" --arg ns "${RUNQLAT_NS}" --arg app "${RUNQLAT_PREFIX}" \
		--arg node "${RUNQLAT_NODE}" --arg image "${CPU_LOAD_IMAGE}" --arg dir "${RUNQLAT_DIR}" '
		{apiVersion: "v1", kind: "Pod", metadata: {name: $name, namespace: $ns, labels: {app: $app}},
		 spec: {nodeName: $node, terminationGracePeriodSeconds: 2,
		   containers: [{name: "worker", image: $image, imagePullPolicy: "Never",
		     command: ["/work/cpu-load", "load", "--control", ("/work/" + $name + ".command")],
		     resources: {requests: {cpu: "100m", memory: "16Mi"}, limits: {cpu: "1", memory: "512Mi"}},
		     volumeMounts: [{name: "work", mountPath: "/work", readOnly: true}]}],
		   volumes: [{name: "work", hostPath: {path: $dir, type: "Directory"}}]}}
	' > "${RUNQLAT_DIR}/${name}.json"
	kubectl --request-timeout=20s create -f "${RUNQLAT_DIR}/${name}.json"
	runqlat_pods+=("${name}")
	kubectl wait --for=condition=Ready pod -n "${RUNQLAT_NS}" "${name}" --timeout=90s
	assert_huatuo_bamai_containers_present "${RUNQLAT_NS}" "^${name}$" 1
	id=$(kubelet_container_ids "${RUNQLAT_NS}" "^${name}$")
	printf '%s\n' "${id}" > "${RUNQLAT_DIR}/${name}.id"
	cgroup_find_container "${RUNQLAT_CPU_ROOT}" "${id}" > "${RUNQLAT_DIR}/${name}.cgroup"
	cpuset=$(cgroup_find_container "${RUNQLAT_CPUSET_ROOT}" "${id}")
	printf '%s\n' "${cpuset}" > "${RUNQLAT_DIR}/${name}.cpuset"
	runqlat_pin "${name}" "${RUNQLAT_CPU}"
	wait_until 10 1 runqlat_ready "${name}" "${runqlat_generation}" || fatal "Pod workload did not initialize"
}

runqlat_pin() {
	local name=$1 cpus=$2 path
	path=$(< "${RUNQLAT_DIR}/${name}.cpuset")
	echo "${cpus}" > "${path}/cpuset.cpus"
	# Save the kernel's canonical list, which may render adjacent CPUs as a range.
	cp "${path}/cpuset.cpus" "${RUNQLAT_DIR}/${name}.pinned-cpus"
	runqlat_check_cpuset "${name}"
}

runqlat_check_cpuset() {
	local name=$1 path file=cpuset.cpus actual expected
	path=$(< "${RUNQLAT_DIR}/${name}.cpuset")
	[[ ${RUNQLAT_V2} != true ]] || file=cpuset.cpus.effective
	actual=$(< "${path}/${file}")
	expected=$(< "${RUNQLAT_DIR}/${name}.pinned-cpus")
	[[ ${actual} == "${expected}" ]] || fatal "Pod ${name} cpuset changed: expected ${expected}, actual ${actual}"
}

runqlat_target() {
	jq -n --arg pod "$1" --arg ns "${RUNQLAT_NS}" '{Pod: $pod, Container: "worker", Namespace: $ns}'
}

runqlat_targets() {
	local name
	for name in "${runqlat_pods[@]}"; do runqlat_target "${name}"; done | jq -s '.'
}
