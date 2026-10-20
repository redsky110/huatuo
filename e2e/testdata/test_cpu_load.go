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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var sink atomic.Uint64

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cpu-load:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "load" {
		return errors.New("use load --control <command file>")
	}
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	control := flags.String("control", "", "workload command file")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	return load(*control)
}

func load(path string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	stopWorkload := func() {}
	defer func() {
		stop()
		stopWorkload()
	}()
	previous := ""
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			command := string(data)
			if command == previous {
				continue
			}
			var mode, generation string
			var milli, count int
			if _, err := fmt.Sscan(command, &mode, &milli, &count, &generation); err != nil {
				return fmt.Errorf("command must be '<idle|user|system|mixed|burst> <millicpu> <workers> <generation>': %w", err)
			}
			if count < 1 || count > 3 || milli < 0 || milli > 2000 ||
				(mode != "idle" && mode != "user" && mode != "system" && mode != "mixed" && mode != "burst") ||
				(mode == "burst" && milli != 0) {
				return fmt.Errorf("invalid workload command %q", command)
			}
			stopWorkload()
			stopWorkload = startWorkload(ctx, mode, milli, count)
			previous = command
			fmt.Println("ready", generation)
		}
	}
}

func startWorkload(ctx context.Context, mode string, milli, count int) func() {
	workerCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	runtime.GOMAXPROCS(count + 1)
	if mode == "burst" {
		workers.Add(1)
		go func() {
			defer workers.Done()
			pulseWorkload(workerCtx, count, 200*time.Millisecond, 300*time.Millisecond)
		}()
	} else if mode != "idle" {
		for range count {
			workers.Add(1)
			go func() {
				defer workers.Done()
				burn(workerCtx, mode, float64(milli)/1000)
			}()
		}
	}
	return func() { cancel(); workers.Wait() }
}

func pulseWorkload(ctx context.Context, count int, busy, idle time.Duration) {
	for ctx.Err() == nil {
		// Idle gaps refill the bandwidth reserve before the next >quota pulse.
		pulse, cancel := context.WithTimeout(ctx, busy)
		var workers sync.WaitGroup
		for range count {
			workers.Add(1)
			go func() {
				defer workers.Done()
				burn(pulse, "user", 0)
			}()
		}
		workers.Wait()
		cancel()
		timer := time.NewTimer(idle)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func burn(ctx context.Context, mode string, rate float64) {
	fd, err := unix.Open("/dev/zero", unix.O_RDONLY, 0)
	if err != nil {
		panic(err)
	}
	defer unix.Close(fd)
	buffer := make([]byte, 64*1024)
	var stamp unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &stamp); err != nil {
		panic(err)
	}
	initialCPU := stamp.Nano()
	start := time.Now()
	for ctx.Err() == nil {
		if mode != "system" {
			x := uint64(1)
			for range 100000 {
				x = x*6364136223846793005 + 1
			}
			sink.Store(x)
		}
		if mode != "user" {
			for range 128 {
				if _, err := unix.Read(fd, buffer); err != nil {
					panic(err)
				}
			}
		}
		if err := unix.ClockGettime(unix.CLOCK_PROCESS_CPUTIME_ID, &stamp); err != nil {
			panic(err)
		}
		// Include runtime/timer threads, which are also charged to this container.
		if rate > 0 {
			due := time.Duration(float64(stamp.Nano()-initialCPU) / rate)
			if delay := due - time.Since(start); delay > 0 {
				time.Sleep(delay)
			}
		}
	}
}
