package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckNotNested(t *testing.T) {
	cases := []struct {
		name        string
		source      string
		target      string
		wantErr     bool
	}{
		{"separate trees", "/a/src", "/b/mnt", false},
		{"identical", "/a", "/a", true},
		{"target inside source", "/a", "/a/sub", true},
		{"target deeply inside source", "/a", "/a/b/c/d", true},
		{"source inside target", "/a/sub", "/a", true},
		{"source prefix but not ancestor", "/a/src", "/a/srcX", false},
		{"siblings under same parent", "/a/x", "/a/y", false},
		{"child name begins with two dots", "/a", "/a/..mnt", true},
		{"sibling name begins with two dots", "/a/src", "/a/..mnt", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNotNested(tc.source, tc.target)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkNotNested(%q, %q) = %v, wantErr=%v", tc.source, tc.target, err, tc.wantErr)
			}
		})
	}
}

func TestResolveCaseMode(t *testing.T) {
	dir := t.TempDir()
	// On Linux ext4/tmpfs the probe should report case-sensitive.
	cases := []struct {
		mode    string
		want    bool
		wantErr bool
	}{
		{"on", true, false},
		{"off", false, false},
		{"insensitive", true, false},
		{"sensitive", false, false},
		{"bogus", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			got, err := resolveCaseMode(tc.mode, dir)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectCaseInsensitiveOnTempDir(t *testing.T) {
	// We can not assert a specific outcome because it varies by host
	// filesystem (Linux ext4: false; macOS APFS-default: true). The
	// probe must at least complete without error and not leave probe
	// files behind.
	dir := t.TempDir()
	before, _ := os.ReadDir(dir)
	if _, err := detectCaseInsensitive(dir); err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	after, _ := os.ReadDir(dir)
	if len(before) != len(after) {
		var names []string
		for _, e := range after {
			names = append(names, e.Name())
		}
		t.Errorf("probe left files behind: %v", names)
	}
	_ = filepath.Join // keep filepath import in case other helpers grow
}
