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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/ccfos/huatuo/internal/bpf"
	bpfabi "github.com/ccfos/huatuo/internal/bpf/abi"
	"github.com/ccfos/huatuo/internal/log"
	"github.com/ccfos/huatuo/internal/profiler"
	"github.com/ccfos/huatuo/internal/toolstream"
	"github.com/ccfos/huatuo/internal/version"
)

//go:generate $BPF_COMPILE $BPF_INCLUDE -s $BPF_DIR/irq_tracing.c -o $BPF_DIR/irq_tracing.o

const (
	irqTracingToolName        = "irqtracing"
	allCPUsTarget             = int32(bpfabi.IrqTracingTargetAllCpus)
	maxEventsPerSecondFlag    = "max-events-per-second"
	defaultMaxEventsPerSecond = uint64(0)
	maxDurationSeconds        = int64(math.MaxInt64) / int64(time.Second)
	sourceRateLimiterName     = "source_rate"
	victimRateLimiterName     = "victim_rate"
)

// Set by Makefile via -ldflags -X. Must live in package main; an empty
// value falls back to version.Devel via version.Resolve.
var (
	AppVersion   string
	AppGitCommit string
	AppBuildTime string
	versionInfo  version.Info
)

// IrqTracingResult is the payload emitted by the CLI. A non-zero NMissed means
// the flame graph is incomplete.
type IrqTracingResult struct {
	FlameData *profiler.ProfileData `json:"flamedata"`
	NMissed   uint64                `json:"nmissed"`
}

func mainAction(cliCtx *cli.Context) (returnErr error) {
	targetCPU := int32(cliCtx.Int64("target-cpu"))
	duration := cliCtx.Int("duration")
	outputPath := cliCtx.String("output-path")
	bpfPath := cliCtx.String("bpf-path")
	maxEventsPerSecond := cliCtx.Uint64(maxEventsPerSecondFlag)

	client, err := openToolstream(cliCtx)
	if err != nil {
		return err
	}
	if client != nil {
		defer func() {
			if err := client.End(); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close toolstream: %w", err))
			}
		}()
	}

	if err := bpf.Init(&bpf.Option{
		KeepaliveTimeout: duration,
	}); err != nil {
		return fmt.Errorf("init bpf: %w", err)
	}
	defer bpf.Shutdown()

	bpfBytes, err := os.ReadFile(bpfPath)
	if err != nil {
		return fmt.Errorf("read bpf object: %w", err)
	}

	b, err := bpf.LoadBPFFromBytes(
		fmt.Sprintf("irqtracing_%d.o", time.Now().UnixNano()),
		bpfBytes,
		irqTracingBPFConstants(targetCPU, maxEventsPerSecond),
	)
	if err != nil {
		return fmt.Errorf("load bpf: %w", err)
	}
	defer b.Close()

	ctx, cancel := context.WithCancel(cliCtx.Context)
	defer cancel()

	if err := b.AttachWithOptions([]bpf.AttachOption{
		{ProgramName: "probe_softirq_raise", Symbol: "irq/softirq_raise"},
		{ProgramName: "probe_softirq_entry", Symbol: "irq/softirq_entry"},
	}); err != nil {
		return fmt.Errorf("attach: %w", err)
	}

	signalWait := make(chan os.Signal, 1)
	signal.Notify(signalWait, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-time.After(time.Duration(duration) * time.Second):
	case <-ctx.Done():
		return fmt.Errorf("caller requests stop")
	case sig := <-signalWait:
		return fmt.Errorf("received signal %s", sig)
	}

	// Freeze the collection point before reading the drop counter.
	nmissed, err := detachAndReadDroppedSamples(b)
	if err != nil {
		return err
	}
	if nmissed > 0 {
		fmt.Fprintf(os.Stderr,
			"irqtracing: %d samples dropped; the flame graph is incomplete\n", nmissed)
	}

	flameData, err := buildFlameGraph(b)
	if err != nil {
		return fmt.Errorf("build flamegraph: %w", err)
	}

	result := IrqTracingResult{
		FlameData: flameData,
		NMissed:   nmissed,
	}

	if client != nil {
		if err := client.Send(&result); err != nil {
			return fmt.Errorf("send result: %w", err)
		}
		return nil
	}

	return writeResult(outputPath, &result, cliCtx.App.Writer)
}

// irqTracingBPFConstants keeps the CLI limit as one total budget while the
// BPF probes enforce independent source and victim budgets. An odd remainder
// goes to source so the two limits still sum to the requested maximum.
func irqTracingBPFConstants(targetCPU int32, maxEventsPerSecond uint64) map[string]any {
	constants := map[string]any{"target_cpu": targetCPU}
	if maxEventsPerSecond == 0 {
		return constants
	}

	sourceRate := maxEventsPerSecond/2 + maxEventsPerSecond%2
	victimRate := maxEventsPerSecond / 2
	bpf.NewRateLimiter(sourceRateLimiterName, sourceRate).Constants(constants)
	bpf.NewRateLimiter(victimRateLimiterName, victimRate).Constants(constants)
	return constants
}

func openToolstream(cliCtx *cli.Context) (*toolstream.Client, error) {
	sockPath := cliCtx.String("output-storage")
	if sockPath == "" {
		return nil, nil
	}

	client, err := toolstream.NewClient(toolstream.ClientOptions{
		SockPath: sockPath,
		ToolName: irqTracingToolName,
		Version:  versionInfo.Version,
		TaskID:   cliCtx.String("task-id"),
	})
	if err != nil {
		return nil, fmt.Errorf("--output-storage: %w", err)
	}

	return client, nil
}

func validateOutputFlags(cliCtx *cli.Context) error {
	sockPath := cliCtx.String("output-storage")
	taskID := cliCtx.String("task-id")
	if sockPath == "" {
		if taskID != "" {
			return errors.New("--task-id requires --output-storage")
		}
		return nil
	}
	if cliCtx.IsSet("output-path") {
		return errors.New("--output-path and --output-storage are mutually exclusive")
	}
	if taskID == "" {
		return errors.New("--task-id is required with --output-storage")
	}
	return nil
}

func validateTargetCPU(targetCPU int64) error {
	if targetCPU < int64(allCPUsTarget) || targetCPU > math.MaxInt32 {
		return fmt.Errorf("--target-cpu must be -1 or between 0 and %d", math.MaxInt32)
	}
	return nil
}

func validateDuration(duration int) error {
	if duration <= 0 || int64(duration) > maxDurationSeconds {
		return fmt.Errorf("--duration must be between 1 and %d seconds", maxDurationSeconds)
	}
	return nil
}

func validateMaxEventsPerSecond(limit uint64) error {
	if limit == 1 {
		return fmt.Errorf("--%s must be 0 or at least 2", maxEventsPerSecondFlag)
	}
	return nil
}

// writeResult marshals result to a JSON file under outputPath and prints the
// file path to stdout so the caller can locate it.
func writeResult(outputPath string, result *IrqTracingResult, output io.Writer) error {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	file := filepath.Join(outputPath, fmt.Sprintf("irqtracing_%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return fmt.Errorf("write result: %w", err)
	}

	if _, err := fmt.Fprintln(output, file); err != nil {
		return fmt.Errorf("print result path: %w", err)
	}
	return nil
}

func main() {
	app := cli.NewApp()
	app.Name = irqTracingToolName
	app.Usage = "collect irq/softirq source and victim stacks and build a flame graph"
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:     "bpf-path",
			Usage:    "path to the irqtracing BPF object file",
			Required: true,
		},
		&cli.Int64Flag{
			Name:  "target-cpu",
			Value: int64(allCPUsTarget),
			Usage: "cpu to trace; omit or set to -1 to trace all CPUs",
		},
		&cli.IntFlag{
			Name:  "duration",
			Value: 3,
			Usage: "collect duration in seconds",
		},
		&cli.Uint64Flag{
			Name:  maxEventsPerSecondFlag,
			Value: defaultMaxEventsPerSecond,
			Usage: "limit source and victim stack samples to N events/sec in total (0 = unlimited)",
		},
		&cli.StringFlag{
			Name:  "output-path",
			Value: ".",
			Usage: "directory to write the result JSON file; mutually exclusive with --output-storage",
		},
		&cli.StringFlag{
			Name:  "output-storage",
			Usage: "unix socket path to send the result to; mutually exclusive with --output-path",
		},
		&cli.StringFlag{
			Name:  "task-id",
			Usage: "task ID to associate with the Toolstream session",
		},
	}

	app.Before = func(ctx *cli.Context) error {
		if err := validateTargetCPU(ctx.Int64("target-cpu")); err != nil {
			return err
		}
		if err := validateDuration(ctx.Int("duration")); err != nil {
			return err
		}
		if err := validateMaxEventsPerSecond(ctx.Uint64(maxEventsPerSecondFlag)); err != nil {
			return err
		}
		if err := validateOutputFlags(ctx); err != nil {
			return err
		}
		log.SetOutput(io.Discard)
		return nil
	}

	versionInfo = version.Wire(app, version.Seed{
		Name:      irqTracingToolName,
		Version:   AppVersion,
		GitCommit: AppGitCommit,
		BuildTime: AppBuildTime,
	})

	app.Action = mainAction
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "irqtracing:", err)
		os.Exit(1)
	}
}
