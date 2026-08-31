#!/usr/bin/env bash

# Copyright 2026 The HuaTuo Authors.
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

# Verify the config-to-storage irqtracing path with deterministic proc/stat
# samples and the real CLI/BPF subprocess.

set -euo pipefail

source "${ROOT_DIR}/integration/lib.sh"
source "${ROOT_DIR}/integration/config.sh"

command -v jq > /dev/null || skip "jq command is not installed"
command -v ss > /dev/null || skip "ss command is not installed"
tracepoint_available irq softirq_raise || skip "irq/softirq_raise is unavailable"
tracepoint_available irq softirq_entry || skip "irq/softirq_entry is unavailable"
[[ -x "${HUATUO_BAMAI_BIN}" ]] \
	|| fatal "huatuo-bamai binary missing: ${HUATUO_BAMAI_BIN}"
[[ -x "${ROOT_DIR}/_output/bin/irqtracing" ]] \
	|| fatal "irqtracing binary missing: ${ROOT_DIR}/_output/bin/irqtracing"
[[ -r "${ROOT_DIR}/_output/bpf/irq_tracing.o" ]] \
	|| fatal "irqtracing BPF object missing: ${ROOT_DIR}/_output/bpf/irq_tracing.o"

IRQTRACING_API_PORT=$(allocate_available_port) \
	|| fatal "failed to allocate a huatuo-bamai API port"
readonly IRQTRACING_API_PORT
HUATUO_BAMAI_ADDR="http://127.0.0.1:${IRQTRACING_API_PORT}"
HUATUO_BAMAI_METRICS_API="${HUATUO_BAMAI_ADDR}/metrics"
export HUATUO_BAMAI_ADDR HUATUO_BAMAI_METRICS_API

readonly IRQTRACING_FIXTURE_ROOT="${HUATUO_BAMAI_TEST_TMPDIR}/irqtracing-fixture"
readonly IRQTRACING_STAT="${IRQTRACING_FIXTURE_ROOT}/proc/stat"
readonly IRQTRACING_EVENT="${HUATUO_BAMAI_TEST_TMPDIR}/events/irq_tracing"

mkdir -p \
	"${IRQTRACING_FIXTURE_ROOT}/bin" \
	"${IRQTRACING_FIXTURE_ROOT}/bpf" \
	"${IRQTRACING_FIXTURE_ROOT}/proc" \
	"${IRQTRACING_FIXTURE_ROOT}/sys" \
	"${IRQTRACING_FIXTURE_ROOT}/dev"

cp "${HUATUO_BAMAI_BIN}" "${IRQTRACING_FIXTURE_ROOT}/bin/huatuo-bamai"
cp "${ROOT_DIR}/_output/bin/irqtracing" "${IRQTRACING_FIXTURE_ROOT}/bin/irqtracing"
cp "${ROOT_DIR}/_output/bpf/irq_tracing.o" "${IRQTRACING_FIXTURE_ROOT}/bpf/irq_tracing.o"

write_stat() {
	local user=$1 irq=$2
	local tmp="${IRQTRACING_STAT}.tmp"
	printf 'cpu0 %d 0 0 900 0 %d 0 0\n' "${user}" "${irq}" > "${tmp}"
	mv "${tmp}" "${IRQTRACING_STAT}"
}

irqtracing_event_is_valid() {
	[[ -s "${IRQTRACING_EVENT}" ]] || return 1
	jq -e '
		.tracer_name == "irq_tracing"
		and .tracer_type == "autotracing"
		and .tracer_data.rule == "rule_cpu_pct_spike"
		and .tracer_data.trigger_cpu == 0
		and .tracer_data.trace_duration == 1
		and (.tracer_data.hit_cpus | length) == 1
		and .tracer_data.hit_cpus[0].cpu == 0
		and (.tracer_data.nmissed | type) == "number"
	' "${IRQTRACING_EVENT}" > /dev/null
}

write_stat 100 0

HUATUO_BAMAI_BIN="${IRQTRACING_FIXTURE_ROOT}/bin/huatuo-bamai"
integration_huatuo_bamai_start \
	write_irqtracing_autotracing_config \
	--region dev \
	--procfs-prefix "${IRQTRACING_FIXTURE_ROOT}" \
	--disable-kubelet \
	--log-debug

# Establish two low-utilization samples, then raise irq utilization sharply.
user=100
for _ in {1..4}; do
	sleep 0.5
	user=$((user + 50))
	write_stat "${user}" 0
done

irq=0
for _ in {1..6}; do
	sleep 0.5
	irq=$((irq + 50))
	write_stat "${user}" "${irq}"
done

wait_until 15 1 irqtracing_event_is_valid \
	|| fatal "irqtracing did not persist the expected autotracing event"
