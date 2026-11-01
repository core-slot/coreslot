//go:build unix

package main

import (
	"os/exec"
	"syscall"
	"testing"
)

// The supervisor kills the command's group while the command is unreaped, and never after:
// its pid may then name another process's group. Mutation: dropping the reaped check in
// KillGroup signals the reaped group.
func TestKillAfterReapIsRefused(t *testing.T) {
	var sent []int
	saved := killGroup
	t.Cleanup(func() { killGroup = saved })
	killGroup = func(pgid int) error {
		sent = append(sent, pgid)
		return saved(pgid)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sup := newSupervisor(cmd, true)
	if !sup.KillGroup() {
		t.Fatal("KillGroup of the running command refused")
	}
	if code := superviseChild(cmd, sup, false); code != 128+int(syscall.SIGKILL) {
		t.Fatalf("status %d, want %d", code, 128+int(syscall.SIGKILL))
	}
	if sup.KillGroup() {
		t.Error("KillGroup after the reap reported a kill")
	}
	if len(sent) != 1 || sent[0] != cmd.Process.Pid {
		t.Errorf("signalled groups %v, want only %d, before the reap", sent, cmd.Process.Pid)
	}
}
