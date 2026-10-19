package galleton

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNoWindowPreservesProcessAttributes(t *testing.T) {
	cmd := exec.Command("unused.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	noWindow(cmd)
	if !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW|windows.CREATE_NEW_PROCESS_GROUP {
		t.Fatalf("unexpected process attributes: %+v", cmd.SysProcAttr)
	}
}

func TestNoWindowChildHasNoConsoleWindow(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestNoWindowHelperProcess$")
	cmd.Env = append(os.Environ(), "WALLAPOP_TEST_NO_WINDOW=1")
	noWindow(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW {
		t.Fatal("missing console-suppression attributes")
	}
	// CombinedOutput also verifies that redirected handles still work.
	output, err := cmd.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("PASS")) {
		t.Fatalf("child process: %v\n%s", err, output)
	}
}

func TestNoWindowHelperProcess(t *testing.T) {
	if os.Getenv("WALLAPOP_TEST_NO_WINDOW") != "1" {
		return
	}
	// CREATE_NO_WINDOW suppresses the console window, not the console's code
	// page. GetConsoleCP may still succeed for a windowless console process.
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")
	if err := proc.Find(); err != nil {
		t.Fatal(err)
	}
	window, _, _ := proc.Call()
	if window != 0 {
		t.Fatalf("child has a console window: %d", window)
	}
}
