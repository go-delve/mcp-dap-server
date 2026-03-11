---
name: debug-binary
description: |
  Assembly-level debugging of a compiled binary (no source required) using mcp-dap-server.
  TRIGGER when: user asks to debug a binary without source code, reverse-engineer a binary, analyze assembly, inspect machine code, work with registers and memory addresses, or debug a stripped binary.
  DO NOT TRIGGER when: source code is available (use debug-source), analyzing a crash dump (use debug-core-dump), or attaching to a running process (use debug-attach).
---

# Binary / Assembly-Level Debug Workflow

## Pre-flight checklist

Before starting, confirm:
1. **Absolute path** to the compiled binary
2. **Architecture** (x86-64, ARM64, etc.) — affects register names and calling conventions
3. **Language/runtime** (Go → Delve; C/C++/Rust → GDB)
4. **Symbols available?** (`file binary` or `nm binary | head` to check — stripped binaries have no function names)
5. **What behavior are you trying to understand?** Form a hypothesis.

> **Note:** Without source, you navigate by function names (if unstripped), memory addresses, and assembly instructions. This requires reading disassembly output.

---

## Step-by-Step Workflow

### 1. Start the session

```json
debug(mode="binary", path="/abs/path/to/binary", stopOnEntry=true)
```

Using `stopOnEntry=true` pauses at the entry point before any code runs, giving you time to orient.

**Choose debugger:**
- Go binary: `debugger="delve"`
- C/C++/Rust binary: `debugger="gdb"`

If the binary crashes immediately, add breakpoints before continuing.

### 2. Get initial context

```json
context()
```

Even without source, this shows:
- **Current function name** (if symbols available, e.g., `main.main`)
- **Instruction pointer address** (e.g., `0x004012a0`)
- **Stack trace** with addresses
- Any debug info embedded in the binary

Note the `instructionPointerReference` from the stack trace — you need it for disassembly.

### 3. Disassemble the current function

```json
disassemble(address="0x<instructionPointerReference>", count=40)
```

Read the output to understand what the function does:

**Key instructions to recognize:**
```
call <addr>     → function call (note the target address/name)
ret             → function return
mov dst, src    → data movement
cmp a, b        → comparison (sets flags, followed by conditional jump)
test a, a       → null/zero check
je/jne/jl/jg   → conditional branches
lea             → compute effective address
push/pop        → stack manipulation
```

### 4. Set breakpoints

**By function name** (if symbols exist):
```json
breakpoint(function="main.processRequest")
breakpoint(function="runtime.panic")
```

**By address** (always works):
```json
breakpoint(function="*0x00401234")
```

Note: GDB uses `*0xADDR` syntax for address-based breakpoints.

Then run to the breakpoint:
```json
continue()
```

### 5. Inspect registers and memory

**x86-64 registers:**
```json
evaluate(expression="$rax")   // return value / first result
evaluate(expression="$rdi")   // first function argument
evaluate(expression="$rsi")   // second function argument
evaluate(expression="$rdx")   // third argument
evaluate(expression="$rsp")   // stack pointer
evaluate(expression="$rip")   // instruction pointer
evaluate(expression="$rbp")   // frame pointer
```

**Memory at address:**
```json
evaluate(expression="*(long*)0x<address>")
evaluate(expression="*(char**)0x<address>")
```

**x86-64 System V calling convention** (Linux, macOS):
- Args: `rdi, rsi, rdx, rcx, r8, r9` (first 6 integer args)
- Return: `rax` (first), `rdx` (second)
- Preserved across calls: `rbx, rbp, r12-r15`
- Caller-saved (may change): `rax, rcx, rdx, rsi, rdi, r8-r11`

**ARM64 calling convention:**
- Args: `x0-x7`
- Return: `x0` (first), `x1` (second)
- Stack pointer: `sp`
- Preserved: `x19-x28`

### 6. Step through assembly

```json
step(mode="in")    // step one instruction
step(mode="over")  // step over (skip function call body)
step(mode="out")   // run until current function returns
```

After each step, call `context()` to see the new instruction pointer.

**Track state changes:**
- Which registers change after each instruction?
- Does a `cmp` result in the expected branch?
- Does a `call` go to the expected address?

### 7. Navigate the call graph

When you see a `call <addr>` you want to understand:

1. Disassemble the target: `disassemble(address="0x<target>", count=30)`
2. Set a breakpoint there: `breakpoint(function="*0x<target>")`
3. Continue and inspect state inside that function

Work your way through the call graph by disassembling each function of interest.

### 8. Identify the logic

Common patterns in disassembly:

**Null check (Go/C):**
```asm
test   rax, rax       ; is rax zero?
je     <error_handler> ; jump if zero (nil)
```

**Bounds check:**
```asm
cmp    rbx, rcx       ; index vs length
jae    <panic>        ; jump if above-or-equal (out of bounds)
```

**String length:**
```asm
lea    rax, [rip+<str>] ; load string address
mov    rbx, <length>    ; load length
```

**Function prologue:**
```asm
push   rbp
mov    rbp, rsp
sub    rsp, <size>    ; allocate stack frame
```

**Function epilogue:**
```asm
add    rsp, <size>    ; deallocate stack frame
pop    rbp
ret
```

### 9. Form and test hypotheses

Based on the disassembly:
1. State your hypothesis: "I think this branch at 0x401234 is taken when X"
2. Set a breakpoint before the branch
3. Continue, inspect the comparison operands
4. Confirm whether the branch is taken as expected

### 10. Conclude

State findings:
> **Function** `main.processRequest` **at** `0x401280` **validates the input length at** `0x4012a4`. When the length exceeds 255, the branch at `0x4012b0` jumps to the error path at `0x401340`. The issue is that the check uses unsigned comparison (`jae`), but the length variable is signed — a negative length passes the check.

### 11. Clean up

```json
stop()
```

---

## Quick Reference

### Disassembly workflow
```
context() → get current address
    ↓
disassemble(address=<addr>, count=40)
    ↓
Identify interesting branch or call
    ↓
Set breakpoint at target
    ↓
continue() → arrive at target
    ↓
inspect registers + disassemble again
```

### When symbols are stripped

Use `info(kind="modules")` to find loaded libraries and their base addresses. Map raw addresses to library offsets to identify what is being called. Look for recognizable patterns in disassembly (system call numbers, library function signatures).

### When you see a crash/panic in the disassembly

Look for calls to:
- Go: `runtime.panic`, `runtime.throw`, `runtime.sigpanic`
- C: `__stack_chk_fail`, `abort`, `raise`
- These are called immediately before the program terminates

Disassemble backwards from the crash call to find what condition triggered it.
