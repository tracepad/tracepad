package main

import "testing"

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in      []string
		cmd     string
		numArgs int
	}{
		{nil, "serve", 0},
		{[]string{"--listen", ":9999"}, "serve", 2}, // spec 001 #1: leading flag = default command
		{[]string{"serve", "--listen", ":9999"}, "serve", 2},
		{[]string{"version"}, "version", 0},
	}
	for _, c := range cases {
		cmd, args := splitCommand(c.in)
		if cmd != c.cmd || len(args) != c.numArgs {
			t.Errorf("splitCommand(%v) = %q, %v", c.in, cmd, args)
		}
	}
}

func TestDisplayHost(t *testing.T) {
	cases := map[string]string{
		":4318":          "localhost:4318",
		"0.0.0.0:4318":   "localhost:4318",
		"[::]:4318":      "localhost:4318",
		"127.0.0.1:4318": "127.0.0.1:4318",
		"myhost:4318":    "myhost:4318",
	}
	for in, want := range cases {
		if got := displayHost(in); got != want {
			t.Errorf("displayHost(%q) = %q, want %q", in, got, want)
		}
	}
}
