package main

import "testing"

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want LogLevel
	}{
		{"", LogInfo},
		{"info", LogInfo},
		{"INFO", LogInfo},
		{"  debug  ", LogDebug},
		{"Trace", LogTrace},
		{"error", LogError},
		{"garbage", LogInfo},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := parseLogLevel(c.in); got != c.want {
				t.Fatalf("parseLogLevel(%q) = %v; want %v", c.in, got, c.want)
			}
		})
	}
}

func TestLogLevel_String(t *testing.T) {
	cases := []struct {
		lvl  LogLevel
		want string
	}{
		{LogError, "error"},
		{LogInfo, "info"},
		{LogDebug, "debug"},
		{LogTrace, "trace"},
		{LogLevel(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.lvl.String(); got != c.want {
			t.Errorf("%d.String() = %q; want %q", int(c.lvl), got, c.want)
		}
	}
}
