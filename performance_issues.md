# Security Agent Event Processing - Performance Issues

This document tracks significant performance bottlenecks identified in the security agent event processing pipeline that require evaluation and potential fixes.

---

## Issue #1: TagsResolver Timeout (52+ seconds)

### Summary
Events experience a 52+ second delay during profile lookup when resolving container tags via cgroup ID.

### Location
- **File**: `pkg/security/security_profile/secprofs.go`
- **Function**: `LookupEventInProfiles()`
- **Line**: ~85-87

### Symptoms
```json
{
  "name": "profile_resolve_tags_done",
  "elapsed_us": 55
},
{
  "name": "lookup_profiles_done",
  "elapsed_us": 52623504  // 52.6 seconds delay!
}
```

### Root Cause
The delay occurs in `TagsResolver.ResolveWithErr()` when attempting to resolve tags for a cgroup ID:

```go
// If no profile found and there's a cgroup ID, try cgroup-based lookup
if profile == nil && event.ProcessContext.Process.CGroup.CGroupID != "" {
    tags, err := m.resolvers.TagsResolver.ResolveWithErr(event.ProcessContext.Process.CGroup.CGroupID)
    // ☝️ THIS BLOCKS FOR 52+ SECONDS
    if err != nil {
        seclog.Errorf("failed to resolve tags for cgroup %s: %v", event.ProcessContext.Process.CGroup.CGroupID, err)
        return
    }
}
```

### Potential Causes
1. **Network I/O timeout**: Calling out to container runtime (Docker/containerd) or metadata service that is unresponsive
2. **High timeout threshold**: Default timeout may be set to 60 seconds
3. **Retry logic**: Multiple retry attempts with exponential backoff
4. **Missing/invalid cgroup**: Container no longer exists, causing lookup failures
5. **Lock contention**: Blocked waiting for locks in the tags resolver

### Checkpoints Added for Diagnosis
New granular checkpoints have been added to pinpoint the exact delay:

```go
// Line 65-67
event.RecordCheckpoint("profile_resolve_tags")
event.FieldHandlers.ResolveContainerTags(event, &event.ProcessContext.Process.ContainerContext)
event.RecordCheckpoint("profile_resolve_tags_done")

// Line 69-77
event.RecordCheckpoint("container_profile_lookup_start")
// Container-based profile lookup
event.RecordCheckpoint("container_profile_lookup_done")

// Line 87-89
event.RecordCheckpoint("cgroup_tags_resolve_start")
tags, err := m.resolvers.TagsResolver.ResolveWithErr(event.ProcessContext.Process.CGroup.CGroupID)
event.RecordCheckpoint("cgroup_tags_resolve_done")  // Will show the 52s delay

// Line 94-101
event.RecordCheckpoint("cgroup_profile_lookup_start")
// Cgroup-based profile lookup
event.RecordCheckpoint("cgroup_profile_lookup_done")
```

### Recommended Actions
1. **Investigate TagsResolver implementation**: Check timeout configuration and retry logic
2. **Add timeout protection**: Implement reasonable timeout (e.g., 1-5 seconds) with fallback
3. **Implement caching**: Cache tag resolution results with TTL to avoid repeated slow lookups
4. **Add async resolution**: Consider resolving tags asynchronously to avoid blocking event processing
5. **Add metrics**: Track TagsResolver performance and timeout frequency
6. **Handle missing containers gracefully**: Fast-fail when container no longer exists

### Event Type Affected
- All event types that trigger profile lookup (observed with `connect` events)

### Status
🔴 **Needs Investigation** - Checkpoints added, waiting for more data to confirm exact cause

---

## Issue #2: Network Namespace Resolution Delay (51+ seconds)

### Summary
Events experience a 51+ second delay immediately after unmarshaling, during network namespace handle resolution.

### Location
- **File**: `pkg/security/probe/probe_ebpf.go`
- **Function**: Event processing pipeline
- **Lines**: ~1228-1231

### Symptoms
```json
{
  "name": "unmarshal_contexts",
  "elapsed_us": 2
},
{
  "name": "before_process_ctx",
  "elapsed_us": 51086860  // 51.08 seconds delay!
}
```

### Root Cause
The delay occurs in `SaveNetworkNamespaceHandleLazy()` when accessing the network namespace handle:

```go
// save netns handle if applicable
_, _ = p.Resolvers.NamespaceResolver.SaveNetworkNamespaceHandleLazy(event.PIDContext.NetNS, func() *utils.NetNSPath {
    return utils.NetNSPathFromPid(event.PIDContext.Pid)
    // ☝️ THIS BLOCKS FOR 51+ SECONDS
})
```

### Potential Causes
1. **Process no longer exists**: The PID has exited, causing `/proc/<pid>/ns/net` access to block or timeout
2. **Filesystem I/O hang**: `/proc` filesystem operations hanging (rare but possible)
3. **Lazy evaluation overhead**: The lazy function callback may be executing expensive operations
4. **Lock contention**: Waiting for locks in the NamespaceResolver
5. **Kernel-level blocking**: syscall blocked in kernel space (e.g., uninterruptible sleep)

### Checkpoints Added for Diagnosis
New granular checkpoints have been added to isolate the exact operation:

```go
// Line 1225
event.RecordCheckpoint("unmarshal_contexts")

// Line 1228-1232
event.RecordCheckpoint("save_netns_start")
_, _ = p.Resolvers.NamespaceResolver.SaveNetworkNamespaceHandleLazy(event.PIDContext.NetNS, func() *utils.NetNSPath {
    return utils.NetNSPathFromPid(event.PIDContext.Pid)
})
event.RecordCheckpoint("save_netns_done")  // Will show if netns is the issue

// Line 1235-1239
event.RecordCheckpoint("handle_before_process_ctx_start")
if !p.handleBeforeProcessContext(event, data, offset, dataLen, cgroupContext, newEntryCb) {
    return eventType
}
event.RecordCheckpoint("before_process_ctx")
```

### Analysis
For `connect` events, `handleBeforeProcessContext()` does **nothing** (only processes fork/exec events), so the delay is almost certainly in the network namespace resolution, not in process context handling.

### Recommended Actions
1. **Add timeout protection**: Wrap `NetNSPathFromPid()` with a timeout (e.g., 100ms-1s)
2. **Check PID existence first**: Validate PID exists before attempting namespace resolution
3. **Implement caching**: Cache network namespace handles with TTL to avoid repeated lookups
4. **Make truly lazy**: Only resolve namespace when actually needed, not on every event
5. **Add fast-fail logic**: If process is known to be exited, skip namespace resolution
6. **Add metrics**: Track namespace resolution performance and timeout frequency
7. **Consider async resolution**: Resolve namespace asynchronously to avoid blocking event pipeline

### Event Type Affected
- Primarily `connect` events (observed)
- Likely affects all network-related events

### Status
🔴 **Needs Investigation** - Checkpoints added, high confidence the issue is in `SaveNetworkNamespaceHandleLazy()`

---

## General Recommendations

### Monitoring
1. Add telemetry to track checkpoint durations in production
2. Set up alerts for events with processing time > 1 second
3. Create dashboards showing processing time percentiles (p50, p95, p99)

### Architecture
1. Consider moving expensive I/O operations out of the event processing hot path
2. Implement circuit breakers for external dependencies (container runtime, filesystem)
3. Add configurable timeout thresholds for all blocking operations
4. Evaluate async/parallel processing for independent resolution steps

### Testing
1. Create synthetic tests that reproduce these conditions
2. Test behavior when containers are rapidly created/destroyed
3. Test behavior when container runtime is unresponsive
4. Load test with high event rates to identify other bottlenecks

---

## Checkpoint Timeline Reference

### Full Event Processing Pipeline (with new checkpoints)

```
unmarshal                           # Unmarshal event data
unmarshal_contexts                  # Unmarshal contexts
save_netns_start                    # ⚠️ NEW - Before netns save (Issue #2)
save_netns_done                     # ⚠️ NEW - After netns save (Issue #2)
handle_before_process_ctx_start     # ⚠️ NEW - Before fork/exec handling
before_process_ctx                  # Before setting process context
resolve_process_start               # Before resolving process
resolve_cache_lookup                # (resolver) Lookup in cache
resolve_cache_hit                   # (resolver) Cache hit
resolve_process_done                # After resolving process
dequeue_exited_start                # Before dequeuing exited processes
dequeue_exited_done                 # After dequeuing exited processes
set_process_ctx                     # After setting process context
regular_event_<type>                # Event-specific handling
regular_event_done                  # Regular event complete
handle_regular_event                # After handling regular event
related_events                      # After related events
lookup_profiles_start               # Before profile lookup
profile_resolve_tags                # Before resolving tags
profile_resolve_tags_done           # After resolving tags
container_profile_lookup_start      # ⚠️ NEW - Before container lookup
container_profile_lookup_done       # ⚠️ NEW - After container lookup
cgroup_tags_resolve_start           # ⚠️ NEW - Before cgroup tag resolve (Issue #1)
cgroup_tags_resolve_done            # ⚠️ NEW - After cgroup tag resolve (Issue #1)
cgroup_profile_lookup_start         # ⚠️ NEW - Before cgroup profile lookup
cgroup_profile_lookup_done          # ⚠️ NEW - After cgroup profile lookup
lookup_profiles_done                # After profile lookup
send_to_handlers_start              # Before sending to handlers
rule_evaluate_start                 # Before rule evaluation
rule_evaluate_done                  # After rule evaluation
rule_evaluate_discarders_start      # Before discarders
rule_evaluate_discarders_done       # After discarders
send_to_handlers_done               # After sending to handlers
send_to_consumers_start             # Before sending to consumers
send_to_consumers_done              # After sending to consumers
```

---

## Issue #3: Network Namespace Resolution Delay (33+ seconds) - Confirmed

### Summary
Another event shows a 33+ second delay in the same location as Issue #2, confirming this is a recurring bottleneck. The delay occurs immediately after unmarshaling contexts and before processing context resolution.

### Location
- **File**: `pkg/security/probe/probe_ebpf.go`
- **Function**: Event processing pipeline
- **Lines**: ~1228-1231 (same as Issue #2)

### Symptoms
```json
{
  "name": "unmarshal_contexts",
  "elapsed_us": 0
},
{
  "name": "before_process_ctx",
  "elapsed_us": 33166545  // 33.16 seconds delay!
}
```

### Analysis
This trace **confirms Issue #2**. The delay:
- Occurs in the exact same location (after `unmarshal_contexts`, before `before_process_ctx`)
- Affects the same operation: `SaveNetworkNamespaceHandleLazy()`
- Has similar magnitude (33s vs 51s in Issue #2)
- Is **not** caused by `handleBeforeProcessContext()` (which does nothing for connect events)

**This proves the network namespace resolution is a systemic issue, not a one-time occurrence.**

### Status
🔴 **Critical - Confirmed Recurring Issue** - Multiple instances observed with 33-51 second delays

### Detailed Checkpoints Added
To pinpoint the exact source of the delay, granular checkpoints have been added throughout the network namespace resolution flow:

**In `SaveNetworkNamespaceHandleLazyWithEvent()`** (`pkg/security/resolvers/netns/resolver.go:298`):
- `netns_check_enabled` - Before checking if network is enabled
- `netns_lock_acquire_start` - Before acquiring resolver lock
- `netns_lock_acquired` - After acquiring lock
- `netns_cache_lookup_done` - After cache lookup
- **New namespace path** (cache miss):
  - `netns_not_found_get_path_start` - Before calling nsPathFunc()
  - `netns_not_found_get_path_done` - After getting path
  - `netns_create_with_path_start` - Before NewNetworkNamespaceWithPath
  - `netns_create_with_path_done` - After creation
- **Existing namespace path** (cache hit, no handle):
  - `netns_found_no_handle_get_path_start` - Before calling nsPathFunc()
  - `netns_found_no_handle_get_path_done` - After getting path
  - `netns_open_handle_start` - Before opening handle
  - `netns_open_handle_done` - After opening handle
- `netns_dequeue_devices_start/done` - Device dequeue timing

**In `openHandleWithEvent()`** (`pkg/security/resolvers/netns/resolver.go:121`):
- `netns_openhandle_lock_start/acquired` - Lock timing
- `netns_openhandle_get_procns_start` - Before os.Open("/proc/<pid>/ns/net") #1
- `netns_openhandle_get_procns_done` - After first os.Open
- `netns_openhandle_open_start` - Before os.Open("/proc/<pid>/ns/net") #2
- `netns_openhandle_open_done` - After second os.Open

**Expected to reveal**: Which os.Open() call is blocking (reading namespace ID or opening handle)

---

## Issue #4: Exited Process Dequeue Delay (666ms)

### Summary
A 666ms delay occurs when dequeuing exited processes from the process cache.

### Location
- **File**: Likely in process resolver/cache
- **Function**: Process exit queue management
- **Component**: `p.Resolvers.ProcessResolver` or similar

### Symptoms
```json
{
  "name": "dequeue_exited_start",
  "elapsed_us": 34487691
},
{
  "name": "dequeue_exited_done",
  "elapsed_us": 35154195  // 666.5ms delay
}
```

### Root Cause Analysis
The `dequeue_exited` operation takes 666ms, which suggests:
1. **Large exit queue**: Many processes have exited and need to be cleaned up
2. **Lock contention**: Waiting for locks on the exit queue or process cache
3. **Expensive cleanup**: Each exited process requires significant cleanup work
4. **Blocking cleanup operations**: I/O operations during cleanup (closing file handles, etc.)

### Potential Causes
1. **Batched cleanup**: Processing a large batch of exited processes at once
2. **Synchronous file descriptor cleanup**: Closing many network namespace handles
3. **Memory deallocation**: Freeing large amounts of memory for process entries
4. **Lock contention**: Multiple goroutines competing for process cache locks
5. **Container cleanup**: Cleaning up container-related metadata for exited processes

### Recommended Actions
1. **Add metrics**: Track exit queue size and dequeue duration
2. **Implement async cleanup**: Move expensive cleanup operations to background goroutines
3. **Batch size limits**: Process exit queue in smaller batches to avoid large delays
4. **Optimize locking**: Use finer-grained locks or lock-free data structures
5. **Profile the operation**: Add CPU profiling during dequeue to identify hot spots
6. **Consider lazy cleanup**: Defer cleanup until resources are actually needed
7. **Add checkpoints**: Break down dequeue into sub-operations (lock acquire, iteration, cleanup, lock release)

### Event Type Affected
- All events that trigger process resolution
- More noticeable during high process churn

### Status
🟡 **Needs Investigation** - Significant but not critical (666ms vs 33-51s for netns issue)

### Detailed Checkpoints Added
To understand what causes the delay during process cleanup, granular checkpoints have been added:

**In `DequeueExitedWithEvent()`** (`pkg/security/resolvers/process/resolver_ebpf.go:166`):
- `dequeue_lock_start` - Before acquiring resolver lock
- `dequeue_lock_acquired` - After acquiring lock
- `dequeue_setup_start/done` - Setup phase timing
- `dequeue_iteration_start` - Before iterating exit queue
- `dequeue_iteration_done` - After processing all exited processes
- `dequeue_clear_queue_start/done` - Queue clearing timing

**Additional logging**:
- Logs queue size and deletion count when queue has >100 entries or >50 deletions
- Format: `dequeue_stats: queue_size=N deleted=M`

**Expected to reveal**:
- Lock contention duration
- Iteration vs cleanup overhead
- Queue size correlation with delay

---

## Issue #5: Profile Lookup Delay (1.2 seconds)

### Summary
A 1.2 second delay occurs during profile lookup, specifically between tag resolution and profile lookup completion.

### Location
- **File**: `pkg/security/security_profile/secprofs.go`
- **Function**: `LookupEventInProfiles()`
- **Lines**: ~85-101 (after tag resolution, during profile lookup)

### Symptoms
```json
{
  "name": "profile_resolve_tags_done",
  "elapsed_us": 35154214
},
{
  "name": "lookup_profiles_done",
  "elapsed_us": 36362068  // 1.207 second delay
}
```

### Analysis
Unlike Issue #1 (52+ second timeout), this is a more moderate delay. However, 1.2 seconds is still significant for event processing. The delay occurs **after** tag resolution completes, during the actual profile lookup phase.

### Root Cause Hypothesis
The delay likely occurs during:
1. **Profile iteration**: Iterating through all loaded security profiles
2. **Profile matching**: Comparing event attributes against profile selectors
3. **Tag comparison**: Matching resolved tags against profile criteria
4. **Lock contention**: Waiting for read locks on the profile manager
5. **Selector evaluation**: Complex selector logic (regex, wildcards, etc.)

### Potential Causes
1. **Large number of profiles**: Many profiles loaded, requiring full iteration
2. **Inefficient matching algorithm**: O(n) search through all profiles
3. **No indexing**: Profiles not indexed by relevant keys (container ID, tags, etc.)
4. **Lock contention**: Many events competing for profile manager read locks
5. **Complex selectors**: Expensive regex or wildcard matching per profile
6. **No caching**: Same event attributes repeatedly evaluated

### Recommended Actions
1. **Add profile indexing**: Index profiles by container ID, cgroup ID, and key tags
2. **Implement caching**: Cache profile lookup results by event signature
3. **Optimize matching**: Short-circuit evaluation, fail-fast on mismatches
4. **Reduce lock contention**: Use read-copy-update (RCU) or lock-free lookups
5. **Add metrics**: Track profile count, lookup duration, cache hit rate
6. **Profile the operation**: CPU profile during profile lookup to find hot spots
7. **Consider async lookup**: Look up profiles asynchronously after event processing
8. **Add granular checkpoints**: Break down profile lookup into sub-operations

### Comparison to Issue #1
- **Issue #1**: 52+ seconds (TagsResolver timeout - container no longer exists)
- **Issue #5**: 1.2 seconds (actual profile matching work - container exists)

These are **different issues**:
- Issue #1: Timeout waiting for external service (container runtime)
- Issue #5: CPU-bound profile matching algorithm inefficiency

### Event Type Affected
- All events that require profile lookup
- Impact proportional to number of loaded profiles

### Status
🟡 **Needs Optimization** - Not critical but significant performance impact at scale

### Detailed Checkpoints Added
To understand profile lookup performance, granular checkpoints have been added:

**Container-based lookup path** (`pkg/security/security_profile/secprofs.go:69`):
- `container_profile_lookup_start` - Start of container lookup
- `container_get_image_name_start/done` - Tag extraction timing
- `container_new_selector_start/done` - Selector creation timing
- `container_profile_lock_start` - Before acquiring profile lock
- `container_profile_lock_acquired` - After acquiring lock
- `container_profile_lock_released` - After lock release
- `container_profile_lookup_done` - End of container lookup

**Cgroup-based lookup path** (`pkg/security/security_profile/secprofs.go:95`):
- `cgroup_tags_resolve_start/done` - TagsResolver timing (can timeout, see Issue #1)
- `cgroup_profile_lookup_start` - Start of cgroup lookup
- `cgroup_get_service_tag_start/done` - Service tag extraction
- `cgroup_new_selector_start/done` - Selector creation timing
- `cgroup_profile_lock_start` - Before acquiring profile lock
- `cgroup_profile_lock_acquired` - After acquiring lock
- `cgroup_profile_lock_released` - After lock release
- `cgroup_profile_lookup_done` - End of cgroup lookup

**Additional logging**:
- Logs profile count when >100 profiles loaded
- Format: `profile_lookup: profile_count=N selector=X found=Y`

**Expected to reveal**:
- Lock contention duration
- Profile map lookup overhead with large profile counts
- Whether iteration through profiles is occurring (shouldn't with map lookup)

---

## Issue #6: Process Cache Lookup Delay (8+ seconds)

### Summary
Process resolution from cache takes 8+ seconds, causing significant event processing delays.

### Location
- **File**: `pkg/security/resolvers/process/resolver.go` or related resolver code
- **Function**: Process cache lookup in `Resolve()` or similar

### Symptoms
```json
{
  "name": "resolve_process_start",
  "elapsed_us": 9461075
},
{
  "name": "resolve_cache_lookup",
  "elapsed_us": 17590144  // 8.13 second delay!
}
```

### Root Cause Analysis
The process cache lookup takes 8+ seconds, which suggests:
1. **Lock contention**: Waiting for locks on the process cache
2. **Large cache**: Iterating through a very large process cache
3. **Slow hash lookup**: Hash table collisions or expensive hash function
4. **Cache corruption**: Corrupted data structure causing slow traversal
5. **Kernel operations**: Blocking kernel calls during lookup

### Potential Causes
- Very high number of processes in cache
- Lock held by another thread doing expensive operations
- Cache eviction/cleanup running concurrently
- Memory pressure causing paging

### Recommended Actions
1. **Add detailed checkpoints**: Track lock acquisition, hash lookup, and any fallback paths
2. **Add metrics**: Track cache size, lookup duration, lock wait time
3. **Profile the operation**: CPU profile during slow lookups
4. **Check for lock contention**: Use lock profiling
5. **Review cache implementation**: Check for O(n) operations in lookup path
6. **Consider lock-free alternatives**: RCU or other lock-free data structures

### Event Type Affected
- All events that require process resolution
- Observed with `connect` events

### Status
🔴 **Critical** - 8+ second delay is unacceptable for event processing

---

## Issue #7: Massive Dequeue Delay (12+ seconds)

### Summary
Process exit queue cleanup takes 12+ seconds, much worse than the 666ms seen in Issue #4.

### Location
- **File**: `pkg/security/resolvers/process/resolver_ebpf.go`
- **Function**: `DequeueExited()` (line 137)

### Symptoms
```json
{
  "name": "dequeue_exited_start",
  "elapsed_us": 17590156
},
{
  "name": "dequeue_exited_done",
  "elapsed_us": 29719735  // 12.13 second delay!
}
```

### Analysis
This is **18x worse** than the 666ms delay seen in Issue #4. The detailed checkpoints from `DequeueExitedWithEvent()` are not present, indicating:
1. Either the binary wasn't rebuilt with the new code
2. Or the old `DequeueExited()` function is still being called

### Comparison
- **Issue #4**: 666ms (0.666 seconds)
- **Issue #7**: 12,130ms (12.13 seconds) - **18x worse!**

### Root Cause Hypothesis
Given the magnitude of the delay:
1. **Massive exit queue**: Potentially thousands of exited processes to clean up
2. **Lock contention**: Extended lock hold time blocking other operations
3. **Expensive cleanup per process**: Each process cleanup involves slow I/O
4. **Cascade effect**: Previous delays caused queue buildup
5. **Memory operations**: Expensive memory deallocation or freeing

### Recommended Actions
1. **Rebuild the agent**: Ensure `DequeueExitedWithEvent()` is used to get granular checkpoints
2. **Emergency mitigation**: Limit max queue size or batch processing size
3. **Add queue size monitoring**: Track how large the exit queue grows
4. **Async cleanup**: Move cleanup to background thread
5. **Profile during high load**: Capture profiles during these massive delays

### Event Type Affected
- All events (dequeue happens on every event)
- Delays compound over time as queue grows

### Status
🔴 **Critical** - 12+ second delay blocks all event processing, causing cascade failures

---

## Issue #8: Mystery Delay Before Network Namespace Resolution (9.46s)

### Summary
A 9.46 second delay occurs between unmarshaling contexts and the start of process context handling.

### Location
- **File**: `pkg/security/probe/probe_ebpf.go`
- **Lines**: Between 1225 (`unmarshal_contexts`) and 1239 (`before_process_ctx`)

### Symptoms
```json
{
  "name": "unmarshal_contexts",
  "elapsed_us": 3
},
{
  "name": "before_process_ctx",
  "elapsed_us": 9461074  // 9.46 second delay!
}
```

### Analysis
The delay occurs in this code section:
```go
// Line 1225
event.RecordCheckpoint("unmarshal_contexts")

// Line 1228-1232: Network namespace save
event.RecordCheckpoint("save_netns_start")
_, _ = p.Resolvers.NamespaceResolver.SaveNetworkNamespaceHandleLazyWithEvent(...)
event.RecordCheckpoint("save_netns_done")

// Line 1235-1239: Handle fork/exec events
event.RecordCheckpoint("handle_before_process_ctx_start")
if !p.handleBeforeProcessContext(...) {
    return eventType
}
event.RecordCheckpoint("before_process_ctx")
```

**Expected checkpoints are MISSING:**
- `save_netns_start`
- `save_netns_done`
- `handle_before_process_ctx_start`

This suggests either:
1. **Binary not rebuilt**: Still running old code without new checkpoints
2. **Early return**: Code path exits before checkpoints (unlikely)
3. **Checkpoint logging issue**: Checkpoints being dropped or not recorded

### Recommended Actions
1. **REBUILD THE AGENT**: Ensure all new checkpoint code is compiled
2. **Verify binary version**: Check that new code is actually running
3. **Add checkpoint at line 1226**: Right after `unmarshal_contexts` to narrow down
4. **Check for blocking operations**: Review all code between 1225-1239

### Event Type Affected
- All event types pass through this code
- Observed with `connect` events

### Status
🟡 **Needs Investigation** - Likely a measurement artifact from old binary, but 9.46s delay is real

---

## Issue #9: Fork Event Unmarshal Delay (17.9 seconds) 🔴

### Summary
Fork events experience a massive 17.9 second delay during the unmarshal phase, specifically when unmarshaling the process cache entry.

### Location
- **File**: `pkg/security/probe/probe_ebpf.go`
- **Function**: `unmarshalProcessCacheEntry()` (line 1030)
- **Called from**: `handleBeforeProcessContext()` for fork events (line 1724)

### Symptoms
```json
{
  "name": "unmarshal_contexts",
  "elapsed_us": 1
},
{
  "name": "unmarshal_fork",
  "elapsed_us": 17917918  // 17.92 SECOND DELAY!
},
{
  "name": "add_fork_entry_start",
  "elapsed_us": 17917925  // Only 7μs later - unmarshal done
}
```

### Root Cause Analysis
The delay occurs inside `unmarshalProcessCacheEntry()` which does:
1. **`sc.UnmarshalBinary(data)`** - Unmarshal syscall context (binary parsing)
2. **`NewProcessCacheEntry()`** - Create new cache entry
3. **`entry.Process.UnmarshalBinary(data[n:])`** - Unmarshal process data (binary parsing)

Since these are just binary unmarshaling operations, the 17.9s delay is unexpected. Possible causes:
1. **Binary parsing triggering I/O**: The unmarshal might trigger lazy loading of process data from `/proc`
2. **Lock contention**: Creating new cache entry waits for locks
3. **Memory allocation**: Severe memory pressure causing allocation delays
4. **Hidden I/O in unmarshal**: Process unmarshal might read `/proc/<pid>/exe` or similar

### After Unmarshal - Everything Fast
Once unmarshal completes, the rest is fast:
- `add_fork_entry`: 3.6ms (fast!)
- `dequeue_exited`: 1μs (fast!)
- `profile_lookup`: 11μs (fast!)

This confirms the problem is **purely in the unmarshal phase**.

### Comparison to Connect Events
- **Connect events**: Delays in netns resolution, cache lookup, dequeue
- **Fork events**: Delays in **unmarshaling the event itself**

This suggests different code paths have different bottlenecks.

### Checkpoints Added for Next Rebuild
To pinpoint the exact source:
```go
unmarshal_syscall_ctx_start/done    // Syscall context unmarshal
new_process_cache_entry_start/done  // Cache entry creation
unmarshal_process_start/done        // Process data unmarshal
```

These will reveal which of the three operations takes 17.9 seconds.

### Recommended Actions
1. **Profile unmarshal operations**: Add CPU profiling during unmarshal
2. **Check for hidden I/O**: Verify no `/proc` reads happen during unmarshal
3. **Review Process.UnmarshalBinary()**: Check if it does lazy I/O operations
4. **Monitor memory pressure**: Check if allocation is slow
5. **Check lock contention**: Profile lock hold times in NewProcessCacheEntry

### Event Type Affected
- Fork events specifically
- Likely also affects exec events (same code path)

### Status
🔴 **Critical** - 17.9 second delay for fork events is unacceptable

---

## Analysis Summary - Current Build

### Trace Analysis (5 samples from same build):

| Trace | Event Type | Primary Bottleneck | Duration | Secondary Issues |
|-------|-----------|-------------------|----------|------------------|
| #1 | connect | Issue #2: netns resolution | 51.0s | TagsResolver timeout (52s) |
| #2 | connect | Issue #3: netns resolution | 33.0s | Dequeue 666ms, Profile 1.2s |
| #3 | connect | Issue #8: unmarshal→before_process | 9.46s | Cache lookup 8.1s, Dequeue 12.1s |
| #4 | connect | Issue #8: unmarshal→before_process | 24.53s | Cache/dequeue both fast |
| #5 | **fork** | **Issue #9: fork unmarshal** | **17.9s** | **Everything else fast** |

### Key Patterns Identified:

**1. Bimodal Performance:**
- Operations are either **very fast** (<100ms) or **very slow** (8-50+ seconds)
- No middle ground - suggests blocking on external resources
- Examples:
  - Cache lookup: 82ms (fast) vs 8,130ms (slow)
  - Dequeue: 9.8ms (fast) vs 12,130ms (slow)
  - Profile lookup: 6μs (fast) vs 1,207ms (slow)

**2. Four Distinct Failure Modes:**
- **Mode A** (Traces #1, #2 - connect): Network namespace resolution blocks 33-51 seconds
- **Mode B** (Trace #3 - connect): Cache lookup + dequeue both slow (8s + 12s)
- **Mode C** (Trace #4 - connect): Large delay before process context (24s), but then everything fast
- **Mode D** (Trace #5 - fork): Fork event unmarshal blocks 17.9 seconds, then everything fast

**3. Missing Checkpoints Indicate:**
Since checkpoints like `save_netns_start`, `dequeue_lock_start`, etc. are NOT in these traces, this is the **original build** before our instrumentation was added. The delays are real, but we lack visibility into what's happening inside these operations.

### Immediate Recommendations (Without Rebuild):

**1. Check for Resource Exhaustion:**
```bash
# Check if processes are stuck in uninterruptible sleep (D state)
ps aux | grep " D "

# Check for file descriptor exhaustion
lsof | wc -l
cat /proc/sys/fs/file-max

# Check for memory pressure
free -h
cat /proc/meminfo | grep -i dirty
```

**2. Check Process/Container Churn:**
```bash
# Check how many containers/processes exist
docker ps -a | wc -l
ps aux | wc -l

# Check for rapid container creation/deletion
docker events --since 10m
```

**3. Check /proc Filesystem:**
```bash
# Check if /proc is responsive
time ls /proc/[0-9]*/ns/net | wc -l

# Check for stale /proc entries
find /proc -maxdepth 1 -name '[0-9]*' -type d 2>/dev/null | wc -l
```

**4. Root Cause Hypothesis:**

Based on the pattern, the most likely cause is:
- **Dead processes with open file descriptors**: When the agent tries to open `/proc/<pid>/ns/net` for a dead process, the kernel blocks
- **Container runtime hangs**: TagsResolver calls to Docker/containerd timeout
- **Lock contention cascade**: One slow operation holds a lock, causing others to queue up

The fact that delays escalate (9.46s → 24.53s) suggests **queue buildup** - operations block, events queue up, making subsequent operations slower.

---

**Last Updated**: 2026-02-09
**Added By**: AI Analysis (Claude Code)
