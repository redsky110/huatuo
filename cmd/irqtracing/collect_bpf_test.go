// Copyright 2026 The HuaTuo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/ccfos/huatuo/internal/bpf"
	bpfabi "github.com/ccfos/huatuo/internal/bpf/abi"
)

// irqTracingBPFObjectPath is the committed BPF object next to bpf/irq_tracing.c.
// The test package runs with cmd/irqtracing as its working directory, so the
// repository bpf dir is two levels up.
const irqTracingBPFObjectPath = "../../bpf/irq_tracing.o"

// loadTestBPF reads the compiled irq_tracing.o and loads it with the given
// constants. Tests that need a real kernel skip when the object is missing
// from the tree (e.g. builds without a BPF toolchain).
func loadTestBPF(t *testing.T, consts map[string]any) (bpf.BPF, error) {
	t.Helper()

	raw, err := os.ReadFile(irqTracingBPFObjectPath)
	if err != nil {
		return nil, err
	}
	return bpf.LoadBPFFromBytes("irq_tracing_test.o", raw, consts)
}

// requireBPFPermission skips the test when the process lacks BPF
// capabilities, so unprivileged containers keep passing the package tests.
func requireBPFPermission(t *testing.T) {
	t.Helper()

	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.Hash,
		KeySize:    4,
		ValueSize:  8,
		MaxEntries: 1,
	})
	if err != nil {
		if errors.Is(err, ebpf.ErrNotSupported) ||
			errors.Is(err, unix.EPERM) ||
			errors.Is(err, unix.EACCES) {
			t.Skipf("insufficient permissions for bpf: %v", err)
		}
		t.Fatalf("ebpf.NewMap() = %v, want nil", err)
	}
	_ = m.Close()
}

// testTargetCPU picks a cpu the test process is allowed to run on,
// preferring a non-zero one (cpu 0 often runs container housekeeping).
func testTargetCPU(t *testing.T) int {
	t.Helper()

	data, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)

	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "Cpus_allowed_list:") {
			line = strings.TrimSpace(strings.TrimPrefix(l, "Cpus_allowed_list:"))
			break
		}
	}
	require.NotEmpty(t, line, "no Cpus_allowed_list in /proc/self/status")

	var cpus []int
	for _, part := range strings.Split(line, ",") {
		if strings.Contains(part, "-") {
			r := strings.SplitN(part, "-", 2)
			lo, err1 := strconv.Atoi(r[0])
			hi, err2 := strconv.Atoi(r[1])
			require.NoError(t, err1)
			require.NoError(t, err2)
			for c := lo; c <= hi; c++ {
				cpus = append(cpus, c)
			}
		} else {
			c, err := strconv.Atoi(part)
			require.NoError(t, err)
			cpus = append(cpus, c)
		}
	}
	require.NotEmpty(t, cpus)

	for _, c := range cpus {
		if c != 0 {
			return c
		}
	}
	return cpus[0]
}

// attachFailedForEnvironment reports whether the attach error comes from a
// restricted environment (permissions, unsupported feature) rather than a
// real problem with the object. Only precisely identifiable errors skip:
// anything else, including EINVAL, must fail the test so object or attach
// regressions cannot hide behind a SKIP.
func attachFailedForEnvironment(err error) bool {
	return errors.Is(err, unix.EPERM) ||
		errors.Is(err, unix.EACCES) ||
		errors.Is(err, ebpf.ErrNotSupported)
}

func TestAllCPUsCollectSoftirqSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	requireBPFPermission(t)

	obj, err := loadTestBPF(t, map[string]any{"target_cpu": allCPUsTarget})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("skipping: BPF object %s not built: %v", irqTracingBPFObjectPath, err)
		}
		if errors.Is(err, ebpf.ErrNotSupported) {
			t.Skipf("skipping: load bpf: %v", err)
		}
		require.NoError(t, err)
	}
	defer obj.Close()

	if err := obj.AttachWithOptions([]bpf.AttachOption{
		{ProgramName: "probe_softirq_raise", Symbol: "irq/softirq_raise"},
	}); err != nil {
		if attachFailedForEnvironment(err) {
			t.Skipf("skipping: attach: %v", err)
		}
		require.NoError(t, err)
	}

	conn, err := net.Dial("udp", "127.0.0.1:9")
	require.NoError(t, err)
	defer conn.Close()
	for i := 0; i < 64; i++ {
		_, _ = conn.Write([]byte("x"))
	}

	assert.Eventually(t, func() bool {
		items, err := obj.DumpMapByName("source_counts")
		return err == nil && len(items) > 0
	}, 5*time.Second, 50*time.Millisecond,
		"target_cpu=-1 must admit softirq sources")

	nmissed, err := readDroppedSamples(obj)
	require.NoError(t, err)
	assert.Zero(t, nmissed, "omitting the runtime rate limit must leave collection unlimited")
}

func TestConfiguredRateLimitDropsSoftirqSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	requireBPFPermission(t)

	// A total budget of two gives source and victim one event per second each.
	obj, err := loadTestBPF(t, irqTracingBPFConstants(allCPUsTarget, 2))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("skipping: BPF object %s not built: %v", irqTracingBPFObjectPath, err)
		}
		if errors.Is(err, ebpf.ErrNotSupported) {
			t.Skipf("skipping: load bpf: %v", err)
		}
		require.NoError(t, err)
	}
	defer obj.Close()

	if err := obj.AttachWithOptions([]bpf.AttachOption{
		{ProgramName: "probe_softirq_raise", Symbol: "irq/softirq_raise"},
	}); err != nil {
		if attachFailedForEnvironment(err) {
			t.Skipf("skipping: attach: %v", err)
		}
		require.NoError(t, err)
	}

	conn, err := net.Dial("udp", "127.0.0.1:9")
	require.NoError(t, err)
	defer conn.Close()
	for i := 0; i < 64; i++ {
		_, _ = conn.Write([]byte("x"))
	}

	assert.Eventually(t, func() bool {
		nmissed, err := readDroppedSamples(obj)
		return err == nil && nmissed > 0
	}, 5*time.Second, 50*time.Millisecond,
		"configured source budget must reject excess softirq raises")
}

// TestMapFullDropCounted locks the map-full regression on a real kernel:
// when a count map is full, an admitted sample whose insertion fails must
// increment dropped_samples, otherwise nmissed would claim a complete
// profile even though stacks are missing. Deleting the count_drop call in
// account_stack makes this test fail.
func TestMapFullDropCounted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires linux")
	}
	requireBPFPermission(t)

	cpu := testTargetCPU(t)

	// Pin this thread to the target cpu so the softirqs it raises fire the
	// probe on that cpu.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var mask unix.CPUSet
	mask.Set(cpu)
	if err := unix.SchedSetaffinity(0, &mask); err != nil {
		t.Skipf("skipping: cannot pin to cpu %d: %v", cpu, err)
	}

	// Runtime rate-limit constants are intentionally omitted, which leaves the
	// limiter disabled. The full map is then the only possible drop source.
	obj, err := loadTestBPF(t, map[string]any{
		"target_cpu": int32(cpu),
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("skipping: BPF object %s not built: %v", irqTracingBPFObjectPath, err)
		}
		if errors.Is(err, ebpf.ErrNotSupported) {
			t.Skipf("skipping: load bpf: %v", err)
		}
		require.NoError(t, err)
	}
	defer obj.Close()

	sourceMapID := obj.MapIDByName("source_counts")
	require.NotZero(t, sourceMapID)
	sourceMap, err := ebpf.NewMapFromID(ebpf.MapID(sourceMapID))
	require.NoError(t, err)
	defer sourceMap.Close()

	// Fill source_counts with 1024 synthetic keys that cannot collide with
	// real events: real pids are small and 0xF0000000+ never occurs.
	possibleCPUs, err := ebpf.PossibleCPU()
	require.NoError(t, err)
	perCPUValue := make([]uint64, possibleCPUs)
	for cpu := range perCPUValue {
		perCPUValue[cpu] = 1
	}
	for i := 0; i < 1024; i++ {
		err := sourceMap.Put(
			stackKeyBytes(&bpfabi.IrqTracingStackKey{
				PID: 0xF0000000 + uint32(i),
				Vec: 99,
			}),
			perCPUValue,
		)
		require.NoError(t, err)
	}

	if err := obj.AttachWithOptions([]bpf.AttachOption{
		{ProgramName: "probe_softirq_raise", Symbol: "irq/softirq_raise"},
	}); err != nil {
		if attachFailedForEnvironment(err) {
			t.Skipf("skipping: attach: %v", err)
		}
		require.NoError(t, err)
	}

	// Raise softirqs on the pinned cpu: NET_RX from UDP writes to a closed
	// loopback port plus the natural timer ticks. Every admitted sample now
	// fails to insert into the full source_counts map and must be counted in
	// dropped_samples.
	conn, err := net.Dial("udp", "127.0.0.1:9")
	if err == nil {
		defer conn.Close()
		for i := 0; i < 64; i++ {
			_, _ = conn.Write([]byte("x"))
		}
	}

	assert.Eventually(t, func() bool {
		nmissed, err := readDroppedSamples(obj)
		return err == nil && nmissed > 0
	}, 5*time.Second, 50*time.Millisecond, "map-full insertion failures must be counted in dropped_samples")
}
