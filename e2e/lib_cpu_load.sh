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

cpu_load_prepare() {
	local directory=$1 binary=${STRESS_NG_BIN:-${ROOT_DIR}/_output/bin/stress-ng}
	if [[ ! -x ${binary} && -z ${STRESS_NG_BIN:-} ]]; then
		log_info "building static stress-ng"
		bash "${ROOT_DIR}/e2e/build_stress_ng.sh" \
			|| fatal "stress-ng build failed; check compiler output and static C library availability"
	fi
	[[ -x ${binary} ]] || fatal "static stress-ng is not executable: ${binary}; check STRESS_NG_BIN"
	"${binary}" --version | tee "${directory}/stress-ng-version.txt"
	cp "${binary}" "${directory}/stress-ng"
	cp "${ROOT_DIR}/e2e/testdata/test_cpu_load.sh" "${directory}/cpu-load"
	chmod 755 "${directory}/cpu-load" "${directory}/stress-ng"
}

cpu_load_ready() {
	grep -Fxq "ready $2" "$1"
}

cpu_load_worker_pid() {
	awk -v generation="$2" '
		$1 == "workers" && $2 == generation {
			if (NF != 3) exit 1
			print $3
			found=1
			exit
		}
		END { if (!found) exit 1 }
	' "$1"
}
