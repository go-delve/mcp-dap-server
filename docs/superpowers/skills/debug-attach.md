---
name: debug-attach
description: |
  Live debugging by attaching to a running process using mcp-dap-server.
  TRIGGER when: user asks to debug a running process, diagnose a live process by PID, attach to an already-running program, or investigate live CPU/memory/deadlock issues.
  DO NOT TRIGGER when: debugging from source (use debug-source), analyzing a crash dump (use debug-core-dump), or the process hasn't started yet (use debug-source or debug-binary).
---

# Live Attach Debug Workflow

## Pre-flight checklist

Before starting, gather:
1. **PID** of the target process (use `ps aux | grep <name>` or `pgrep <name>`)
2. **What is the observed problem?** (high CPU, hang, wrong behavior, memory leak)
3. **Is this a production process?** Setting breakpoints will pause it for all users — be careful.
4. **Language/runtime** — Go processes use Delve; others may need GDB

## Important warnings

- Attaching pauses the process. In production, this affects real users.
- After `stop()`, the target process is **terminated** — plan accordingly.
- You may need `sudo` or `ptrace_scope=0` permissions.

---

## Step-by-Step Workflow

### 1. Attach to the process

```json
debug(mode="attach", processId=<PID>)
```

Expected: The process pauses. You see the current execution location, stack trace, and variables.

If attach fails:
- Verify PID is still running: `ps -p <PID>`
- Check ptrace permissions: `cat /proc/sys/kernel/yama/ptrace_scope` (0 = unrestricted)
- Try with elevated permissions if needed
- Process may have already exited

### 2. Understand what the process was doing

Immediately call:
```json
context()
```

This shows the state at the moment of pause. Key questions:
- **Where is it?** What function and file?
- **Why is it there?** Does the stack trace make sense?
- **What are the local values?** Do they look reasonable?

If the process was in a system call (I/O, sleep, mutex wait), the stack will show that explicitly.

### 3. Check all threads / goroutines

```json
info(kind="threads")
```

This is critical for concurrent programs. Look for:
- Threads blocked on the **same mutex or channel** → potential deadlock
- **More threads than expected** → goroutine leak
- Threads in **unexpected functions** → processing wrong data or stuck in error path

For each suspicious thread:
```json
context(threadId=<ID>)
```

### 4. Scenario-specific investigation

#### High CPU usage

Pause the process several times and look for patterns:
```json
pause()   // if it was resumed
context()
```

Do this 3-5 times. If the same function keeps appearing, that's the hot path.

Look for:
- Tight loops with no I/O or sleep
- Repeated work that should be cached
- Unexpected recursion or redundant computation

#### Deadlock / hang (process not progressing)

After attach, all threads should be visible. Look for:
- Thread A blocked waiting for lock X
- Thread B holding lock X and waiting for lock Y
- Thread C holding lock Y and waiting for lock X

Use `context(threadId=<ID>)` on each blocked thread to see what lock/channel it's waiting on.

**Red flag:** `sync.(*Mutex).Lock` or `<-chan` in every goroutine's stack → classic deadlock.

#### Memory growth / leak

Check sizes of collections:
```json
evaluate(expression="len(cache)")
evaluate(expression="len(connections)")
evaluate(expression="cap(buffer)")
```

Look for:
- Maps/slices that grow but never shrink
- Connection pools that accumulate but don't close
- Goroutines accumulating in `info(kind="threads")`

#### Unexpected behavior / wrong results

Set a targeted breakpoint at the function that produces wrong output:
```json
breakpoint(function="packageName.FunctionName")
continue()
```

When it hits, inspect inputs and internal state to find where the logic diverges.

### 5. Iterate

Resume the process and let it run to your next observation point:
```json
continue()
```

Or manually pause again:
```json
pause()
context()
```

### 6. Conclude and detach

State findings clearly:
> **The process is stuck in** `FunctionName` **at** `file.go:42` **because** `mutex.Lock()` **is blocked waiting for a lock held by goroutine** `threadId=3`.
> **Root cause:** goroutine 3 is holding lock A while waiting for lock B; goroutine 1 holds lock B while waiting for lock A — circular deadlock.

Then clean up:
```json
stop()
```

---

## Decision Tree

```
Process attached
    │
    ├─ Is every thread blocked? → Deadlock
    │      → Check all threads for mutual lock dependencies
    │
    ├─ Is one thread consuming CPU? → Hot path / infinite loop
    │      → Pause multiple times, look for repeating call site
    │
    ├─ Are threads growing over time? → Goroutine leak
    │      → Find goroutines that never finish
    │
    └─ Thread behavior looks normal → Behavioral bug
           → Set breakpoints at the function producing wrong output
```

## How to present findings

> **Diagnosis:** The process is experiencing a deadlock between goroutines 1 and 3.
> **Evidence:** Goroutine 1 is at `sync.Mutex.Lock` waiting for lock B (held by goroutine 3). Goroutine 3 is at `sync.Mutex.Lock` waiting for lock A (held by goroutine 1).
> **Root cause:** `ProcessRequest` acquires locks in order A→B, while `HandleCallback` acquires them B→A. This creates a lock-ordering inversion.
> **Fix:** Establish a consistent lock acquisition order across all code paths.
