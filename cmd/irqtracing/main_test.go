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
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/ccfos/huatuo/internal/toolstream"
	"github.com/ccfos/huatuo/internal/version"
)

func TestValidateOutputFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "local output"},
		{
			name: "Toolstream output",
			args: []string{"--output-storage", "/run/toolstream.sock", "--task-id", "task-1"},
		},
		{
			name:    "both outputs",
			args:    []string{"--output-path", "/tmp", "--output-storage", "/run/toolstream.sock", "--task-id", "task-1"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "missing task ID",
			args:    []string{"--output-storage", "/run/toolstream.sock"},
			wantErr: "--task-id is required",
		},
		{
			name:    "task ID without Toolstream",
			args:    []string{"--task-id", "task-1"},
			wantErr: "--task-id requires --output-storage",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := newOutputFlagContext(t, test.args...)
			err := validateOutputFlags(ctx)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateOutputFlags() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateOutputFlags() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateTargetCPU(t *testing.T) {
	tests := []struct {
		name    string
		cpu     int64
		wantErr bool
	}{
		{name: "all CPUs", cpu: -1},
		{name: "CPU zero", cpu: 0},
		{name: "largest supported CPU", cpu: 1<<31 - 1},
		{name: "invalid negative CPU", cpu: -2, wantErr: true},
		{name: "CPU exceeds BPF value", cpu: 1 << 31, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTargetCPU(test.cpu)
			if test.wantErr && err == nil {
				t.Fatal("validateTargetCPU() error = nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateTargetCPU() error = %v", err)
			}
		})
	}
}

func TestValidateDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration int
		wantErr  bool
	}{
		{name: "one second", duration: 1},
		{name: "largest supported duration", duration: int(maxDurationSeconds)},
		{name: "zero", duration: 0, wantErr: true},
		{name: "negative", duration: -1, wantErr: true},
		{name: "overflow", duration: int(maxDurationSeconds + 1), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDuration(test.duration)
			if test.wantErr && err == nil {
				t.Fatal("validateDuration() error = nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateDuration() error = %v", err)
			}
		})
	}
}

func TestIrqTracingBPFConstants(t *testing.T) {
	t.Run("unlimited", func(t *testing.T) {
		constants := irqTracingBPFConstants(allCPUsTarget, 0)
		if len(constants) != 1 || constants["target_cpu"] != allCPUsTarget {
			t.Fatalf("constants = %#v, want only target_cpu", constants)
		}
	})

	t.Run("splits total limit", func(t *testing.T) {
		constants := irqTracingBPFConstants(3, 1000)
		if got := constants["bpf_rlimit_burst_source_rate"]; got != uint64(500) {
			t.Fatalf("source rate = %v, want 500", got)
		}
		if got := constants["bpf_rlimit_burst_victim_rate"]; got != uint64(500) {
			t.Fatalf("victim rate = %v, want 500", got)
		}
		if got := constants["bpf_rlimit_interval_ns_source_rate"]; got != uint64(time.Second) {
			t.Fatalf("source interval = %v, want %d", got, time.Second)
		}
		if got := constants["bpf_rlimit_interval_ns_victim_rate"]; got != uint64(time.Second) {
			t.Fatalf("victim interval = %v, want %d", got, time.Second)
		}
	})

	t.Run("keeps odd total", func(t *testing.T) {
		constants := irqTracingBPFConstants(3, 1001)
		if got := constants["bpf_rlimit_burst_source_rate"]; got != uint64(501) {
			t.Fatalf("source rate = %v, want 501", got)
		}
		if got := constants["bpf_rlimit_burst_victim_rate"]; got != uint64(500) {
			t.Fatalf("victim rate = %v, want 500", got)
		}
	})
}

func TestValidateMaxEventsPerSecond(t *testing.T) {
	for _, limit := range []uint64{0, 2, 1000} {
		if err := validateMaxEventsPerSecond(limit); err != nil {
			t.Fatalf("validateMaxEventsPerSecond(%d) error = %v", limit, err)
		}
	}

	err := validateMaxEventsPerSecond(1)
	if err == nil || !strings.Contains(err.Error(), "must be 0 or at least 2") {
		t.Fatalf("validateMaxEventsPerSecond(1) error = %v", err)
	}
}

func TestOpenToolstreamSendsResult(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "toolstream.sock")
	server, err := toolstream.NewServer(sockPath)
	if err != nil {
		t.Fatalf("toolstream.NewServer() error = %v", err)
	}
	received := make(chan *IrqTracingResult, 1)
	toolstream.Register(server, irqTracingToolName, func(sess *toolstream.Session, result *IrqTracingResult) error {
		if sess.TaskID != "task-1" {
			t.Errorf("TaskID = %q, want task-1", sess.TaskID)
		}
		received <- result
		return nil
	})
	if err := server.Start(); err != nil {
		t.Fatalf("server.Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("server.Close() error = %v", err)
		}
	})

	previousVersion := versionInfo
	versionInfo = version.Info{Version: "test"}
	t.Cleanup(func() { versionInfo = previousVersion })

	ctx := newOutputFlagContext(t, "--output-storage", sockPath, "--task-id", "task-1")
	client, err := openToolstream(ctx)
	if err != nil {
		t.Fatalf("openToolstream() error = %v", err)
	}
	result := &IrqTracingResult{NMissed: 3}
	if err := client.Send(result); err != nil {
		t.Fatalf("client.Send() error = %v", err)
	}
	if err := client.End(); err != nil {
		t.Fatalf("client.End() error = %v", err)
	}

	select {
	case got := <-received:
		if got.NMissed != 3 {
			t.Fatalf("received result = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Toolstream result")
	}
}

func newOutputFlagContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("irqtracing-test", flag.ContinueOnError)
	set.String("output-path", ".", "")
	set.String("output-storage", "", "")
	set.String("task-id", "", "")
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return cli.NewContext(cli.NewApp(), set, nil)
}
