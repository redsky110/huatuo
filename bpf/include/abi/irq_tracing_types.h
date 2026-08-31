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

#ifndef __BPF_ABI_IRQ_TRACING_H__
#define __BPF_ABI_IRQ_TRACING_H__

#include "bpf_abi.h"

enum irq_tracing_target_cpu {
	IRQ_TRACING_TARGET_ALL_CPUS = -1,
};

enum irq_tracing_stream {
	IRQ_TRACING_STREAM_SOURCE = 0,
	IRQ_TRACING_STREAM_VICTIM,
	IRQ_TRACING_STREAM_MAX,
};

enum irq_tracing_stack_id {
	IRQ_TRACING_STACK_ID_NONE = 0xffffffffU,
};

struct irq_tracing_stack_key {
	u32 ustack_id;
	u32 kstack_id;
	u32 pid;
	u32 vec;
	u8 comm[COMPAT_TASK_COMM_LEN];
};

BPF_ABI_EXPORT(irq_tracing_stack_key);
BPF_ABI_EXPORT_ENUM(irq_tracing_target_cpu);
BPF_ABI_EXPORT_ENUM(irq_tracing_stream);
BPF_ABI_EXPORT_ENUM(irq_tracing_stack_id);

#endif /* __BPF_ABI_IRQ_TRACING_H__ */
