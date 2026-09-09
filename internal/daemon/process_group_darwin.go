package daemon

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func processBootID() (string, error) {
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err == nil && boot == "" {
		err = fmt.Errorf("machine boot identity is unavailable")
	}
	return boot, err
}

func inspectProcessGroup(pgid int) (processGroupSnapshot, error) {
	var result processGroupSnapshot
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return result, err
	}
	for _, p := range processes {
		if int(p.Eproc.Pgid) != pgid {
			continue
		}
		if int(p.Proc.P_pid) == pgid {
			result.leaderBirth = fmt.Sprintf("%d:%d", p.Proc.P_starttime.Sec, p.Proc.P_starttime.Usec)
		}
		// SZOMB is 5 in Darwin's sys/proc.h. Zombies cannot hold files or
		// execute commands; their parent may reap them after cleanup returns.
		if p.Proc.P_stat != 5 {
			result.pids = append(result.pids, int(p.Proc.P_pid))
		}
	}
	return result, nil
}
