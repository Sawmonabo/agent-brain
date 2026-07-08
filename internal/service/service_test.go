package service

import (
	"errors"
	"runtime"
	"testing"
)

var errFake = errors.New("fake")

func TestDetectWSL2(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		readErr bool
		want    bool
	}{
		{"wsl2 kernel", "Linux version 5.15.167.4-microsoft-standard-WSL2", false, true},
		{"wsl1 kernel", "Linux version 4.4.0-19041-Microsoft", false, true},
		{"native linux", "Linux version 6.8.0-45-generic (buildd@lcy02)", false, false},
		{"unreadable", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			read := func(string) ([]byte, error) {
				if tc.readErr {
					return nil, errFake
				}
				return []byte(tc.content), nil
			}
			if got := detectWSL2(read); got != tc.want {
				t.Fatalf("detectWSL2 = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewControllerConstructsWithoutTouchingSystem(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("phase 2 targets darwin/linux")
	}
	controller, err := NewController("/usr/local/bin/agent-brain")
	if err != nil {
		t.Fatal(err)
	}
	if controller == nil {
		t.Fatal("nil controller")
	}
	// Construction only — Install/Start would touch the live system and
	// are exercised manually (exit criteria), never in tests.
}

func TestNewControllerRejectsRelativePath(t *testing.T) {
	t.Parallel()
	if _, err := NewController("agent-brain"); err == nil {
		t.Fatal("relative binary path accepted")
	}
}
