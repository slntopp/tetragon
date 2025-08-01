// SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause)
/* Copyright Authors of Cilium */

#include "vmlinux.h"
#include "api.h"
#include "bpf_tracing.h"
#include "common.h"
#include "process.h"

#define MAX_PIDS 32768

struct update_data {
	__u32 bit;
	__u32 cnt;
	__u32 pids[MAX_PIDS];
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __s32);
	__type(value, struct update_data);
} execve_map_update_data SEC(".maps");

char _license[] __attribute__((section("license"), used)) = "Dual BSD/GPL";

#ifdef __V612_BPF_PROG
#define LOOPS MAX_PIDS
#else
#define LOOPS 1024
#endif

#ifdef __LARGE_BPF_PROG
FUNC_INLINE void
__execve_map_update(struct update_data *data)
{
	struct execve_map_value *curr;
	__u32 idx, pid;

	bpf_for(idx, 0, LOOPS)
	{
		asm volatile("%[idx] &= 0x7fff;\n"
			     : [idx] "+r"(idx));
		if (data->cnt == idx)
			break;

		pid = data->pids[idx];
		curr = execve_map_get_noinit(pid);
		if (curr)
			lock_and(&curr->bin.mb_bitset, ~(1 << data->bit));
	}
}
#else
FUNC_INLINE void
__execve_map_update(struct update_data *data)
{
}
#endif /* __LARGE_BPF_PROG */

__attribute__((section("seccomp"), used)) int
execve_map_update(void *ctx)
{
	struct update_data *data;
	__u32 idx = 0;

	data = map_lookup_elem(&execve_map_update_data, &idx);
	if (!data)
		return -1;
	__execve_map_update(data);
	return 0;
}
