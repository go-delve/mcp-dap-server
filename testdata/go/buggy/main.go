package main

import "fmt"

// binarySearch returns the index of target in sorted nums, or -1 if not found.
func binarySearch(nums []int, target int) int {
	lo, hi := 0, len(nums) // BUG: hi should be len(nums)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if nums[mid] == target {
			return mid
		} else if nums[mid] < target {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return -1
}

func main() {
	nums := []int{1, 3, 5, 7, 9, 11, 13}
	tests := []struct {
		target int
		want   int
	}{
		{7, 3},
		{1, 0},
		{13, 6},
		{4, -1},
		{15, -1}, // panics: target > all elements drives hi out of bounds
	}
	for _, tc := range tests {
		got := binarySearch(nums, tc.target)
		if got != tc.want {
			fmt.Printf("FAIL: binarySearch(nums, %d) = %d, want %d\n", tc.target, got, tc.want)
		} else {
			fmt.Printf("PASS: binarySearch(nums, %d) = %d\n", tc.target, got)
		}
	}
}
