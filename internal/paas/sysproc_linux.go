package paas

import "syscall"

func sysProcAttr() *syscall.SysProcAttr {
	// Own process group so stop() can signal children too; die with paasd.
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
