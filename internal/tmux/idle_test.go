package tmux

import (
	"io/fs"
	"testing"
)

func fakeProc(t *testing.T, files map[string]string) {
	t.Helper()
	old := procFS
	procFS = func(p string) ([]byte, error) {
		s, ok := files[p]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(s), nil
	}
	t.Cleanup(func() { procFS = old })
}

func TestPaneIdle(t *testing.T) {
	t.Setenv("SHELL", "/opt/bin/myshell")
	fakeProc(t, map[string]string{
		// idle shell: tpgid == pgrp, no children
		"/proc/100/stat":              "100 (bash) S 1 100 100 34816 100 4194304 0 0",
		"/proc/100/comm":              "bash\n",
		"/proc/100/task/100/children": "",
		// comm with spaces and ')': parse after the last ')'
		"/proc/101/stat":              "101 (a) b (c)) S 1 101 101 34816 101 4194304",
		"/proc/101/comm":              "zsh\n",
		"/proc/101/task/101/children": "",
		// shell with a foreground child: tpgid != pgrp
		"/proc/102/stat":              "102 (bash) S 1 102 102 34816 205 4194304",
		"/proc/102/comm":              "bash\n",
		"/proc/102/task/102/children": "205 ",
		// background job: tpgid == pgrp but a child exists
		"/proc/103/stat":              "103 (bash) S 1 103 103 34816 103 4194304",
		"/proc/103/comm":              "bash\n",
		"/proc/103/task/103/children": "300 ",
		// tpgid != pgrp without children is not idle
		"/proc/104/stat":              "104 (zsh) S 1 104 104 34816 210 0",
		"/proc/104/comm":              "zsh\n",
		"/proc/104/task/104/children": "",
		"/proc/105/stat":              "105 (bash) S 1",
		"/proc/105/comm":              "bash\n",
		"/proc/106/stat":              "106 (bash) S 1 106 106 34816 106 0",
		"/proc/106/comm":              "bash\n",
		// a program exec'd directly as the pane process (-- sleep 60)
		"/proc/107/stat":              "107 (sleep) S 1 107 107 34816 107 0",
		"/proc/107/comm":              "sleep\n",
		"/proc/107/task/107/children": "",
		// the user's $SHELL basename counts as a shell
		"/proc/108/stat":              "108 (myshell) S 1 108 108 34816 108 0",
		"/proc/108/comm":              "myshell\n",
		"/proc/108/task/108/children": "",
		// login shell spelling
		"/proc/109/stat":              "109 (-fish) S 1 109 109 34816 109 0",
		"/proc/109/comm":              "-fish\n",
		"/proc/109/task/109/children": "",
	})
	c := Client{}
	cases := []struct {
		pid     int
		idle    bool
		wantErr bool
	}{
		{100, true, false},
		{101, true, false},
		{102, false, false},
		{103, false, false},
		{104, false, false},
		{105, false, true}, // malformed stat
		{106, false, true}, // stat readable but children file missing
		{107, false, false},
		{108, true, false},
		{109, true, false},
		{999, true, false}, // missing process
	}
	for _, tc := range cases {
		idle, err := c.PaneIdle(tc.pid)
		if (err != nil) != tc.wantErr || idle != tc.idle {
			t.Errorf("pid %d: idle %v err %v, want idle %v err %v", tc.pid, idle, err, tc.idle, tc.wantErr)
		}
	}
}
