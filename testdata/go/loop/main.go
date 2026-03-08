package main

import "time"

func main() {
	x := 0
	for {
		x++
		time.Sleep(10 * time.Millisecond)
	}
}
