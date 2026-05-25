package cli

import (
	"flag"
	"os"
	"slices"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantPos     []string
		wantDebug   bool
		wantKeepCwd bool
		wantConfig  string
	}{
		{"flags before source", []string{"--debug", "--keep-cwd", "src"}, []string{"src"}, true, true, ""},
		{"flags after source", []string{"src", "--debug", "--keep-cwd"}, []string{"src"}, true, true, ""},
		{"flags both sides", []string{"--debug", "src", "--keep-cwd"}, []string{"src"}, true, true, ""},
		{"value flag after source", []string{"src", "--config", "p.txt"}, []string{"src"}, false, false, "p.txt"},
		{"value flag before source", []string{"--config", "p.txt", "src"}, []string{"src"}, false, false, "p.txt"},
		{"no source", []string{"--debug"}, nil, true, false, ""},
		{"nothing", nil, nil, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("run", flag.ContinueOnError)
			var debug, keepCwd bool
			var config string
			fs.BoolVar(&debug, "debug", false, "")
			fs.BoolVar(&keepCwd, "keep-cwd", false, "")
			fs.StringVar(&config, "config", "", "")
			pos, err := parseInterspersed(fs, tc.args)
			if err != nil {
				t.Fatalf("parseInterspersed: %v", err)
			}
			if !slices.Equal(pos, tc.wantPos) {
				t.Errorf("positionals = %v, want %v", pos, tc.wantPos)
			}
			if debug != tc.wantDebug || keepCwd != tc.wantKeepCwd || config != tc.wantConfig {
				t.Errorf("flags: debug=%v keepCwd=%v config=%q; want %v/%v/%q",
					debug, keepCwd, config, tc.wantDebug, tc.wantKeepCwd, tc.wantConfig)
			}
		})
	}
}

func TestSplitAtDoubleDash(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantLeft []string
		wantCmd  []string
	}{
		{"empty", nil, nil, nil},
		{"source only", []string{"."}, []string{"."}, nil},
		{"flags then source", []string{"--debug", "."}, []string{"--debug", "."}, nil},
		{"dashdash then cmd", []string{"--", "bash"}, []string{}, []string{"bash"}},
		{"source then cmd", []string{".", "--", "pnpm", "test"}, []string{"."}, []string{"pnpm", "test"}},
		{"flags source cmd with cmd flags", []string{"--debug", ".", "--", "claude", "--dangerously"}, []string{"--debug", "."}, []string{"claude", "--dangerously"}},
		{"trailing dashdash empty cmd", []string{".", "--"}, []string{"."}, []string{}},
		{"only dashdash", []string{"--"}, []string{}, []string{}},
		{"cmd contains dashdash", []string{".", "--", "sh", "-c", "echo --"}, []string{"."}, []string{"sh", "-c", "echo --"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			left, cmd := splitAtDoubleDash(tc.args)
			if !slices.Equal(left, tc.wantLeft) {
				t.Errorf("left = %v, want %v", left, tc.wantLeft)
			}
			if !slices.Equal(cmd, tc.wantCmd) {
				t.Errorf("cmd = %v, want %v", cmd, tc.wantCmd)
			}
		})
	}
}

func TestSplitAtDoubleDash_FirstSeparatorWins(t *testing.T) {
	// Only the first "--" splits; later ones belong to the command.
	left, cmd := splitAtDoubleDash([]string{"src", "--", "env", "--", "x"})
	if !slices.Equal(left, []string{"src"}) {
		t.Errorf("left = %v", left)
	}
	if !slices.Equal(cmd, []string{"env", "--", "x"}) {
		t.Errorf("cmd = %v", cmd)
	}
}

func TestResolveCommand(t *testing.T) {
	t.Run("explicit command passes through", func(t *testing.T) {
		got := resolveCommand([]string{"pnpm", "test"})
		if !slices.Equal(got, []string{"pnpm", "test"}) {
			t.Errorf("got %v", got)
		}
	})

	t.Run("empty falls back to $SHELL", func(t *testing.T) {
		t.Setenv("SHELL", "/usr/bin/fish")
		got := resolveCommand(nil)
		if !slices.Equal(got, []string{"/usr/bin/fish"}) {
			t.Errorf("got %v, want [/usr/bin/fish]", got)
		}
	})

	t.Run("empty $SHELL falls back to /bin/sh", func(t *testing.T) {
		os.Unsetenv("SHELL")
		got := resolveCommand([]string{})
		if !slices.Equal(got, []string{"/bin/sh"}) {
			t.Errorf("got %v, want [/bin/sh]", got)
		}
	})
}
