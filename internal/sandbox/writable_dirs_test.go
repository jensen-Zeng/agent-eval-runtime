package sandbox

import "testing"

func TestValidateWritableDirs(t *testing.T) {
	for _, dir := range []string{"/stage_runs", "/scratch_1", "/task-data"} {
		if err := ValidateWritableDirs([]string{dir}); err != nil {
			t.Errorf("ValidateWritableDirs(%q): %v", dir, err)
		}
	}
	for _, dirs := range [][]string{
		{"/"}, {"/app"}, {"/tmp"}, {"/logs"}, {"/tests"}, {"/opt"}, {"/usr"}, {"/etc"},
		{"/proc"}, {"/sys"}, {"/dev"}, {"/var"}, {"/bin"}, {"/sbin"}, {"/lib"}, {"/lib64"},
		{"/home"}, {"/root"}, {"/run"}, {"/nested/path"}, {"relative"}, {"/stage_runs/child"},
		{"/stage_runs", "/stage_runs"}, {"/a", "/b", "/c", "/d", "/e"},
	} {
		if err := ValidateWritableDirs(dirs); err == nil {
			t.Errorf("ValidateWritableDirs(%v) unexpectedly succeeded", dirs)
		}
	}
}
