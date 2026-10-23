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

package collector

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ccfos/huatuo/internal/bpf"
	"github.com/ccfos/huatuo/pkg/metric"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/require"
)

func TestRunqlatAggregatePerCPUValue(t *testing.T) {
	for _, test := range []struct {
		name   string
		values []uint64
		want   latencyBpfData
	}{
		{name: "empty"},
		{name: "one CPU", values: []uint64{1, 2, 3, 4, 5, 6}, want: latencyBpfData{1, 2, 3, 4, 5, 6}},
		{
			name:   "two CPUs",
			values: []uint64{1, 2, 3, 4, 5, 6, 10, 20, 30, 40, 50, 60},
			want:   latencyBpfData{11, 22, 33, 44, 55, 66},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw bytes.Buffer
			require.NoError(t, binary.Write(&raw, binary.LittleEndian, test.values))
			got := latencyBpfData{99, 99, 99, 99, 99, 99}
			require.NoError(t, aggregatePerCPUValue(raw.Bytes(), &got))
			require.Equal(t, test.want, got)
		})
	}
	t.Run("partial record", func(t *testing.T) {
		var got latencyBpfData
		require.Error(t, aggregatePerCPUValue(make([]byte, binary.Size(got)-1), &got))
	})
}

func TestRunqlatKernelAggregationAndRelease(t *testing.T) {
	if os.Getenv("TEST_INTEGRATION") != "true" {
		t.Skip("set TEST_INTEGRATION=true to load runqlat BPF programs")
	}
	require.NoError(t, bpf.Init(&bpf.Option{}))
	t.Cleanup(bpf.Shutdown)
	oldDir := bpf.DefaultObjDir
	bpf.DefaultObjDir = filepath.Join("..", "..", "bpf")
	t.Cleanup(func() { bpf.DefaultObjDir = oldDir })

	ctx, cancel := context.WithCancel(t.Context())
	collector := &runqlatCollector{}
	done := make(chan error, 1)
	go func() { done <- collector.Start(ctx) }()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Error("runqlat collector did not stop after cancellation")
			}
		}
	})

	var lease bpf.Lease
	deadline := time.Now().Add(15 * time.Second)
	for {
		var ok bool
		lease, ok = collector.bpf.Acquire()
		if ok {
			break
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("runqlat collector stopped before publishing BPF: %v", err)
		default:
		}
		require.True(t, time.Now().Before(deadline), "runqlat BPF was not published")
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(lease.Release)
	info, err := lease.Info()
	require.NoError(t, err)

	// Stop kernel updates so known per-CPU values can verify exact aggregation.
	require.NoError(t, lease.Detach())
	possibleCPUs, err := ebpf.PossibleCPU()
	require.NoError(t, err)
	values := make([]latencyBpfData, possibleCPUs)
	for cpu := range values {
		n := uint64(cpu + 1)
		values[cpu] = latencyBpfData{n, 2 * n, 3 * n, 4 * n, 5 * n, 6 * n}
	}
	sum := uint64(possibleCPUs) * uint64(possibleCPUs+1) / 2
	want := latencyBpfData{sum, 2 * sum, 3 * sum, 4 * sum, 5 * sum, 6 * sum}
	for _, name := range []string{"cpu_host_metric", "cpu_tg_metric"} {
		t.Run(name, func(t *testing.T) {
			m, err := ebpf.NewMapFromID(ebpf.MapID(lease.MapIDByName(name)))
			require.NoError(t, err)
			defer m.Close()
			var key any = uint32(0)
			if name == "cpu_tg_metric" {
				key = uint64(1)
			}
			require.NoError(t, m.Put(key, values))
			if name == "cpu_host_metric" {
				data := collector.fetchHostRunqlat(lease.BPF)
				require.Len(t, data, 4)
				for zone, sample := range data {
					require.Equal(t, "latency", sample.Name())
					require.Equal(t, metric.MetricTypeCounter, sample.Type())
					require.Equal(t, strconv.Itoa(zone), sample.Labels()["zone"])
					require.Equal(t, float64(uint64(zone+3)*sum), sample.Value)
				}
				require.Equal(t, want, collector.runqlatHost)
			} else {
				items, err := lease.DumpMapByName(name)
				require.NoError(t, err)
				found := false
				for _, item := range items {
					if binary.LittleEndian.Uint64(item.Key) != 1 {
						continue
					}
					found = true
					var got latencyBpfData
					require.NoError(t, aggregatePerCPUValue(item.Value, &got))
					require.Equal(t, want, got)
				}
				require.True(t, found, "seeded container key was absent from the map dump")
			}
		})
	}

	lease.Release()
	cancel()
	select {
	case err := <-done:
		stopped = true
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("runqlat collector did not stop after cancellation")
	}
	_, ok := collector.bpf.Acquire()
	require.False(t, ok, "stopped collector still publishes its BPF object")
	// Kernel reclamation can outlive the last userspace file descriptor.
	for _, metadata := range info.MapsInfo {
		require.Eventually(t, func() bool {
			m, err := ebpf.NewMapFromID(ebpf.MapID(metadata.ID))
			if err == nil {
				m.Close()
			}
			return errors.Is(err, os.ErrNotExist)
		}, 2*time.Second, 10*time.Millisecond,
			"runqlat map %s ID %d survived collector shutdown", metadata.Name, metadata.ID)
	}
	for _, metadata := range info.ProgramsInfo {
		require.Eventually(t, func() bool {
			p, err := ebpf.NewProgramFromID(ebpf.ProgramID(metadata.ID))
			if err == nil {
				p.Close()
			}
			return errors.Is(err, os.ErrNotExist)
		}, 2*time.Second, 10*time.Millisecond,
			"runqlat program %s ID %d survived collector shutdown", metadata.Name, metadata.ID)
	}
}
