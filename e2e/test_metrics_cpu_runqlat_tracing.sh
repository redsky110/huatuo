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
source "${ROOT_DIR}/integration/lib_cpu_runqlat.sh"

runqlat_snapshot() {
	local output=$1
	cpu_metrics_read "${output}.metrics.json" || return 1
	jq -e --arg host "$(hostname)" --argjson targets "$(runqlat_targets)" '
  def zones($family; $labels):
   $family as $f | [$f.samples[]? | select(.labels | contains($labels))] as $rows |
   if $f.type == "COUNTER" and ($f.help | length > 0)
    and ($rows | length) == 4 and ([$rows[].labels.zone] | sort) == ["0","1","2","3"]
    and all($rows[]; .labels == ($labels + {zone: .labels.zone})
     and (.value | type == "number" and . >= 0 and . <= 9007199254740992 and floor == .))
   then $rows | sort_by(.labels.zone) | map(.value)
   else error("missing or invalid runqlat counters for " + ($labels | tostring)) end;
  . as $metrics |
  [.huatuo_bamai_scrape_collector_success.samples[]? | select(.labels.collector == "runqlat")] as $success |
  if ($success | length) == 1 and $success[0].value == 1
   and (.huatuo_bamai_tracing_status_running.samples | length) == 1
   and .huatuo_bamai_tracing_status_running.samples[0].value == 1
   and ([.huatuo_bamai_tracing_status_hitcount.samples[]? | select(.labels.tracing == "runqlat")] |
    length == 1 and .[0].value == 0)
  then {Host: zones(.huatuo_bamai_runqlat_latency; {host: $host, region: "e2e"}),
   Containers: (reduce $targets[] as $t ({}; .[$t.Pod] =
    ($metrics | zones(.huatuo_bamai_runqlat_container_latency;
     {host: $host, region: "e2e", container_host: $t.Pod, container_name: $t.Container,
      container_hostnamespace: $t.Namespace, container_type: "normal", container_level: "burstable"}))))}
  else error("runqlat collector or tracing status is unhealthy") end
 ' "${output}.metrics.json" > "${output}.json"
}

runqlat_phase() {
	local phase=$1 name=$2 zone=${3:--1}
	runqlat_check_cpuset "${name}"
	log_info "runqlat phase: ${phase} (10-second measurement window)"
	wait_until 15 1 runqlat_snapshot "${RUNQLAT_DIR}/${phase}-before" || fatal "runqlat metrics did not become ready"
	sleep 10
	runqlat_check_cpuset "${name}"
	runqlat_snapshot "${RUNQLAT_DIR}/${phase}-after" || fatal "${phase}: invalid runqlat metrics"
	jq -en --slurpfile before "${RUNQLAT_DIR}/${phase}-before.json" --slurpfile after "${RUNQLAT_DIR}/${phase}-after.json" \
		--arg pod "${name}" --argjson zone "${zone}" '
  def delta($a; $b): [range(0; 4) as $i | $a[$i] - $b[$i]];
  {host: delta($after[0].Host; $before[0].Host),
   container: delta($after[0].Containers[$pod]; $before[0].Containers[$pod])} |
  if all(.[]; all(.[]; . >= 0) and add > 0 and (if $zone >= 0 then .[$zone] > 0 else true end))
  then . else error("host/container latency counters regressed or did not grow") end
 ' | tee "${RUNQLAT_DIR}/${phase}-growth.json"
}

runqlat_absent() {
	local pod=$1
	cpu_metrics_read "${RUNQLAT_DIR}/absent.metrics.json" || return 1
	jq -e --arg pod "${pod}" '
  [.huatuo_bamai_runqlat_container_latency.samples[]? | select(.labels.container_host == $pod)] | length == 0
 ' "${RUNQLAT_DIR}/absent.metrics.json" > /dev/null
}

runqlat_prepare
runqlat_restart
readonly a="${RUNQLAT_PREFIX}-a" b="${RUNQLAT_PREFIX}-b"
runqlat_create "${a}"
runqlat_create "${b}"
runqlat_phase wakeup "${a}" 0
runqlat_command "${a}" user 0 2
wait_until 10 1 runqlat_ready "${a}" "${runqlat_generation}" || fatal "busy workers did not initialize"
runqlat_phase preemption "${a}"

zones_seen=(false false false false)
for workers in 4 8 16 32 64 128; do
	runqlat_command "${a}" user 0 "${workers}"
	wait_until 10 1 runqlat_ready "${a}" "${runqlat_generation}" || fatal "${workers} workers did not initialize"
	phase="workers-${workers}"
	log_info "runqlat contention: Pod=${a} CPU=${RUNQLAT_CPU} workers=${workers} quota=1C"
	runqlat_phase "${phase}" "${a}"
	for zone in 1 2 3; do
		if jq -e --argjson zone "${zone}" '.host[$zone] > 0 and .container[$zone] > 0' \
			"${RUNQLAT_DIR}/${phase}-growth.json" > /dev/null; then zones_seen[zone]=true; fi
	done
done
for zone in 1 2 3; do
	[[ ${zones_seen[zone]} == true ]] || fatal "zone-${zone} did not grow during same-CPU contention"
done
runqlat_command "${a}" user 0 2
wait_until 10 1 runqlat_ready "${a}" "${runqlat_generation}" || fatal "workers did not reset"

if ((${#runqlat_cpus[@]} >= 3)); then
	runqlat_pin "${a}" "${runqlat_cpus[-2]},${RUNQLAT_CPU}"
	runqlat_phase multi-cpu "${a}"
	runqlat_pin "${a}" "${RUNQLAT_CPU}"
fi

pids=()
for query in 1 2 3 4; do
	(
		for round in 1 2 3 4 5 6 7 8; do
			runqlat_snapshot "${RUNQLAT_DIR}/concurrent-${query}-${round}" || exit 1
		done
	) > "${RUNQLAT_DIR}/concurrent-${query}.log" 2>&1 &
	pids+=("$!")
done
for pid in "${pids[@]}"; do wait "${pid}" || fatal "concurrent runqlat metrics queries failed"; done

runqlat_command host user 1000 1
taskset -c "${RUNQLAT_CPU}" "${RUNQLAT_LOAD}" host --control "${RUNQLAT_DIR}/host.command" > "${RUNQLAT_DIR}/host-worker.log" 2>&1 &
runqlat_host_pid=$!
runqlat_phase host-contention "${a}"
stop_and_wait_by_pid "${runqlat_host_pid}" || fatal "host competitor did not exit cleanly"
runqlat_host_pid=""
for signal in TERM KILL; do
	runqlat_restart "${signal}"
	runqlat_phase "after-${signal}" "${a}"
done

old_id=$(< "${RUNQLAT_DIR}/${a}.id")
kubectl delete pod -n "${RUNQLAT_NS}" "${a}" --timeout=30s
runqlat_pods=("${b}")
assert_huatuo_bamai_containers_absent "deleted runqlat Pod" "${old_id}"
wait_until 30 1 runqlat_absent "${a}" || fatal "deleted Pod still exports runqlat metrics"
runqlat_create "${a}"
runqlat_phase recreated "${a}" 0

RUNQLAT_DISABLED=true
runqlat_restart
cpu_metrics_read "${RUNQLAT_DIR}/disabled.metrics.json"
jq -e 'keys | all(.[]; startswith("huatuo_bamai_runqlat_") | not)' \
	"${RUNQLAT_DIR}/disabled.metrics.json" > /dev/null || fatal "blacklisted runqlat still exports metrics"
assert_cpu_tracing_status runqlat 0 || fatal "blacklisted runqlat is still running"
RUNQLAT_DISABLED=false
runqlat_restart
runqlat_phase reenabled "${a}" 0
runqlat_log_check
log_info "runqlat E2E passed; artifacts: ${RUNQLAT_DIR}"
