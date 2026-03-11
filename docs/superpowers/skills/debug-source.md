---
name: debug-source
description: |
  Live source-level debugging of a Go or C/C++ program using mcp-dap-server.
  TRIGGER when: user asks to debug a program from source, find a bug, step through code, or inspect runtime state of a running program.
  DO NOT TRIGGER when: debugging a core dump (use debug-core-dump), attaching to an existing process (use debug-attach), or debugging a binary without source (use debug-binary).
---

# Live Source Debug Workflow

## Pre-flight checklist

Before starting, gather:
1. **Absolute path** to the source file or directory
2. **Language** (Go or C/C++) — determines which debugger to use
3. **What is the bug or behavior to investigate?** — forms your hypothesis
4. **Which function/file is most likely involved?** — where to set the first breakpoint

## Quick reference

| Language | Debugger | Mode |
|----------|----------|------|
| Go | `delve` | `source` |
| C/C++/Rust | `gdb` | `binary` (compile first: `gcc -g -O0`) |

---

## Step-by-Step Workflow

### 1. Start the session

**Go:**
```json
debug(mode="source", path="/abs/path/to/main.go", debugger="delve")
```

**C/C++ (compile first, then binary mode):**
```json
debug(mode="binary", path="/abs/path/to/binary", debugger="gdb")
```

Expected: debugger starts, stops at entry or breakpoint. You receive stack trace + variables.

If this fails:
- Go: check `dlv` is in `$PATH` (`dlv version`)
- C/C++: check cpptools adapter; compile with `-g -O0`
- Ensure path is absolute

### 2. Set strategic breakpoints

Set breakpoints _before_ continuing, at the function(s) you want to investigate:

```json
breakpoint(file="/abs/path/to/file.go", line=42)
breakpoint(function="packageName.FunctionName")
```

Choose breakpoint locations based on your hypothesis:
- Entry to the suspicious function
- Just before the condition you think is wrong
- At error return paths

### 3. Run to the first interesting point

```json
continue()
```

Output includes: current file/line, stack trace, all local variables.

**What to look for:**
- Is the current location where you expected?
- Are variable values what you expect at this point?
- Is the call stack expected, or is something surprising calling this function?

### 4. Inspect state in depth

Refresh context at any time:
```json
context()
```

Drill into specific values:
```json
evaluate(expression="user.Address.City")
evaluate(expression="items[0]")
evaluate(expression="len(queue)")
evaluate(expression="err.Error()")
```

**Decision guide:**
- Value is nil when it shouldn't be → trace back where it was set
- Value is wrong → find where it was assigned incorrectly
- Value is correct here → the bug is downstream; add a later breakpoint

### 5. Step through logic

```json
step(mode="over")   // execute current line, stay in same function
step(mode="in")     // step into the function being called
step(mode="out")    // run until current function returns
```

**When to use each:**
- `over`: when you don't suspect the called function is the problem
- `in`: when the called function is suspicious
- `out`: when you've seen enough in the current function

After each step, check if values changed as expected.

### 6. Check threads (concurrent programs)

```json
info(kind="threads")
```

Look for goroutines/threads in unexpected states. For each suspicious thread:
```json
context(threadId=<ID>)
```

**Red flags:**
- Multiple threads at the same mutex/channel → potential deadlock
- A goroutine stuck in an unexpected location
- Unexpectedly few or many goroutines

### 7. Modify state to test hypotheses (optional)

If the debugger supports it:
```json
set-variable(variablesReference=<ref>, name="count", value="0")
```

Then `continue()` to see if the fix works. This confirms your hypothesis before writing code.

### 8. Clean up

```json
stop()
```

---

## Common Patterns and Root Causes

| Symptom | Likely cause | What to check |
|---------|-------------|---------------|
| `nil` pointer panic | Missing nil check | Where was the pointer created/returned? |
| Wrong value in calculation | Logic error or uninitialized var | Step through the computation |
| Function called with wrong args | Caller bug | Step out to the caller, inspect args |
| Infinite loop | Missing termination condition | Evaluate the loop condition, check iterator |
| Goroutine deadlock | Lock ordering or missing unlock | Check all goroutines with `info(kind="threads")` |

## How to present findings

State clearly:
> **Bug found at** `file.go:42` in `FunctionName`.
> **Variable** `x` **has value** `nil` **when it should be** `*User{...}`.
> **Root cause:** `getUserByID()` returns `nil` on cache miss without error, but the caller doesn't check for nil before dereferencing.
> **Fix:** Add nil check in caller or return an error from `getUserByID()` on cache miss.
