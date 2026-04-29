---
name: debug-core-dump
description: |
  Post-mortem analysis of a core dump file using mcp-dap-server.
  TRIGGER when: user asks to analyze a core dump, investigate a crash, figure out why a program crashed, analyze a segfault, or examine a .core / core.* / core file.
  DO NOT TRIGGER when: the program is still running (use debug-attach), debugging from source interactively (use debug-source), or no core file exists yet (run the program first with core dumps enabled).
---

# Post-Mortem Core Dump Analysis Workflow

## Pre-flight checklist

Before starting, confirm:
1. **Absolute path** to the binary that crashed (required for Delve; optional for GDB, which can auto-detect it from the core file)
2. **Absolute path** to the core dump file (usually `core`, `core.<PID>`, or `core.XXXX`)
3. **Language** (Go → Delve; C/C++ → GDB)
4. **Do the binary and core match?** A rebuilt binary won't match the core dump.

> **Key insight:** Execution is frozen. You cannot step forward. You are reading a snapshot of memory and registers at the moment of crash.

---

## Step-by-Step Workflow

### 1. Start the core dump session

**Go:**
```json
debug(mode="core", path="/abs/path/to/binary", coreFilePath="/abs/path/to/core", debugger="delve")
```

**C/C++ (with explicit binary):**
```json
debug(mode="core", path="/abs/path/to/binary", coreFilePath="/abs/path/to/core", debugger="gdb")
```

**C/C++ (auto-detect binary from core file):**
```json
debug(mode="core", coreFilePath="/abs/path/to/core", debugger="gdb")
```

Expected: The debugger loads the core and positions at the crash frame. You see the crash location, stack trace, and local variables.

If loading fails:
- Check paths are absolute and files are readable
- Confirm binary matches core (same build, not recompiled after crash)
- For Go: `dlv version` to confirm Delve is installed
- For C/C++: check GDB 14+ is installed (`gdb --version`)

### 2. Get the full picture of the crash

```json
context()
```

This is your most important call. Extract:
- **Crash function and line** — where exactly did the program die?
- **Signal** — what killed it? (see signal guide below)
- **Local variables at the crash frame** — any nil pointers? Invalid values?
- **Full stack trace** — what sequence of calls led to the crash?

**Signal interpretation guide:**

| Signal | Meaning | Common causes |
|--------|---------|--------------|
| `SIGSEGV` | Segmentation fault | Nil pointer deref, use-after-free, buffer overflow, stack overflow |
| `SIGABRT` | Abort | Go runtime panic, C assert(), double-free, explicit abort() |
| `SIGFPE` | Arithmetic error | Division by zero, integer overflow |
| `SIGBUS` | Bus error | Misaligned memory access, unmapped file region |
| `SIGILL` | Illegal instruction | Compiler bug, corrupted binary |
| `SIGPIPE` | Broken pipe | Write to closed socket/pipe |

### 3. Examine crash frame variables

Look at every variable shown in `context()`:
- Any **nil pointer** being dereferenced? → SIGSEGV
- Any **index** being used on a nil or zero-length slice? → SIGSEGV
- Any **value that looks corrupt** (e.g., negative size, astronomically large number)?

Use `evaluate()` to drill into values not shown automatically:
```json
evaluate(expression="err.Error()")
evaluate(expression="request.Header")
evaluate(expression="items[0]")
evaluate(expression="p.next")
```

### 4. Walk the call stack

Work backwards through the stack trace. For each frame of interest:
```json
context(frameId=<N>)
```

Ask for each frame:
- What argument was passed to the function that crashed?
- Was that argument already nil/invalid when it was passed?
- Which caller is responsible for the bad value?

This traces the bad value back to its origin.

### 5. Check other threads / goroutines

```json
info(kind="threads")
```

In multi-threaded/concurrent programs:
- Another thread may have corrupted shared memory before the crash
- A goroutine in an unexpected state may indicate a race condition
- Look for threads at suspicious locations

Inspect interesting threads:
```json
context(threadId=<ID>)
```

### 6. Evaluate suspicious expressions

Based on your hypothesis, test specific values:
```json
evaluate(expression="config.MaxRetries")
evaluate(expression="len(pool.connections)")
evaluate(expression="handler != nil")
```

For Go, you can evaluate method calls if the receiver is valid:
```json
evaluate(expression="user.String()")
```

For C/C++ with GDB:
```json
evaluate(expression="*(struct Node*)ptr")
evaluate(expression="str->length")
```

### 7. Pattern-based diagnosis

Use the signal + stack + variables to match a pattern:

**Nil pointer (SIGSEGV on field access):**
```
→ Which struct pointer is nil?
→ Who created/returned that pointer?
→ Was nil return from a function call checked?
→ Fix: add nil check before dereference, or fix the function to not return nil
```

**Buffer overflow (SIGSEGV in memory ops):**
```
→ Is there an index operation near the crash?
→ What is the length/capacity of the buffer?
→ Was bounds checking skipped?
→ Fix: add bounds check, use safe slice operations
```

**Runtime panic / abort (SIGABRT in Go):**
```
→ Look for panic message in variables or stack
→ Common: index out of range, nil map write, concurrent map read/write
→ Fix: depends on the specific panic type
```

**Infinite recursion (stack overflow → SIGSEGV):**
```
→ Very deep stack with the same function repeating
→ What is the base case? Is it reachable?
→ Fix: add/fix the base case, or convert to iterative
```

**Double-free / use-after-free (SIGABRT in C/C++):**
```
→ malloc/free mismatch, or pointer used after free()
→ Look for shared ownership without reference counting
→ Fix: use RAII/smart pointers, or fix ownership model
```

### 8. Conclude

Answer:
1. **What crashed?** (function, file, line number)
2. **What signal?** (and what it means for this case)
3. **What bad value caused it?** (which variable, what value)
4. **Where did that value come from?** (trace through the call stack)
5. **Root cause?** (the code defect that needs fixing)

State it clearly:
> **Crash at** `server.go:142` **in** `handleRequest`. **Signal:** SIGSEGV.
> **Cause:** `conn.writer` is nil. `conn` is non-nil but was not fully initialized because `newConn()` returns early on timeout without setting `writer`.
> **Fix:** Either return an error from `newConn()` on timeout (rather than a partially initialized struct), or add a nil check before `conn.writer.Write()`.

### 9. Clean up

```json
stop()
```

---

## How to present findings

Structure your response as:
1. **One-line summary** of what crashed and why
2. **Evidence** — the specific values/stack frames that prove it
3. **Root cause** — the code defect
4. **Suggested fix** — what change would prevent this
