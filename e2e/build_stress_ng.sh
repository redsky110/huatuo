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

readonly repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
readonly version=0.19.06
readonly archive_sha256=054953145d7de36725a54636ea68975bb9ada06b4769878d25e556fab5804513
readonly directory="${repo}/_output/stress-ng-${version}"
readonly archive="${directory}/source.tar.gz"
export CC=${CC:-gcc}
command -v "${CC}" > /dev/null || {
	echo "stress-ng build: ${CC} is missing; install gcc and the static C libraries" >&2
	exit 1
}
mkdir -p "${directory}/source" "${repo}/_output/bin"
if [[ ! -f ${archive} ]]; then
	curl -fL --retry 2 "https://codeload.github.com/ColinIanKing/stress-ng/tar.gz/refs/tags/V${version}" -o "${archive}"
fi
printf '%s  %s\n' "${archive_sha256}" "${archive}" | sha256sum -c -
if [[ ! -f ${directory}/source/Makefile ]]; then
	tar -xzf "${archive}" --strip-components=1 -C "${directory}/source"
fi
# Static glibc's gethostid can load incompatible NSS libraries in BusyBox.
# Regenerate cached configuration before disabling its probe.
make -C "${directory}/source" cleanconfig
make -C "${directory}/source" -f Makefile.config -j"${BUILD_JOBS:-2}" \
	STATIC=1 VERSION="${version}" -o GETHOSTID
make -C "${directory}/source" -j"${BUILD_JOBS:-2}" STATIC=1
cp "${directory}/source/stress-ng" "${repo}/_output/bin/stress-ng"
"${repo}/_output/bin/stress-ng" --version
