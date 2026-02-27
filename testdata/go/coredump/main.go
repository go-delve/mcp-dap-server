package main

import (
	"os"
	"syscall"
)

func crash(x int, msg string) {
	_, _ = x, msg
	syscall.Kill(os.Getpid(), syscall.SIGABRT)
	select {}
}

func main() {
	crash(42, "core dump test")
}
