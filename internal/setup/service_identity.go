package setup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func managedServicePID(ctx context.Context, sys serviceSystem) (int, error) {
	var out string
	var err error
	switch sys.GOOS() {
	case "darwin":
		out, err = sys.Run(ctx, "launchctl", "print", "gui/"+uidString(sys.UID())+"/"+LaunchAgentLabel)
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				if key, value, ok := strings.Cut(strings.TrimSpace(line), " = "); ok && key == "pid" {
					return parseServicePID(value)
				}
			}
			return 0, fmt.Errorf("launch agent %s has no running PID", LaunchAgentLabel)
		}
	case "linux":
		out, err = sys.Run(ctx, "systemctl", "--user", "show", DefaultServiceName+".service", "--property=MainPID", "--value")
		if err == nil {
			return parseServicePID(strings.TrimSpace(out))
		}
	default:
		return 0, fmt.Errorf("no managed service on %s", sys.GOOS())
	}
	return 0, fmt.Errorf("cannot inspect setup-managed service: %w (%s)", err, strings.TrimSpace(out))
}

func parseServicePID(value string) (int, error) {
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("service has no valid running PID: %q", value)
	}
	return pid, nil
}

// A responsive socket alone is not proof that setup owns that daemon. Refuse
// to change its config or install a competing service, even with --force.
func verifyServiceOwner(ctx context.Context, sys System, socket string, pid int) error {
	if sys.GOOS() == "windows" {
		return verifyScheduledTaskOwner(ctx, sys, socket, pid)
	}
	managedPID, err := managedServicePID(ctx, sys)
	if err != nil {
		return fmt.Errorf("runner PID %d already owns %s, but its setup-managed service cannot be verified: %w; use the existing service manager or explicitly migrate the runner before rerunning setup", pid, socket, err)
	}
	if managedPID != pid {
		return fmt.Errorf("runner PID %d owns %s, but the setup-managed service runs PID %d; use the existing service manager or explicitly migrate the runner before rerunning setup", pid, socket, managedPID)
	}
	return nil
}

// Task Scheduler does not report a task's PID. The runner is setup's when the
// task is running and the socket's process runs the task's command.
func verifyScheduledTaskOwner(ctx context.Context, sys System, socket string, pid int) error {
	task, err := queryScheduledTask(ctx, sys)
	if err == nil && task.State != "Running" {
		err = fmt.Errorf("scheduled task %s is %s", scheduledTaskName, strings.ToLower(task.State))
	}
	var image string
	if err == nil {
		image, err = sys.ProcessImage(pid)
	}
	if err != nil {
		return fmt.Errorf("runner PID %d already owns %s, but its setup-managed service cannot be verified: %w; use the existing service manager or explicitly migrate the runner before rerunning setup", pid, socket, err)
	}
	if !sys.SameFile(image, task.Command) {
		return fmt.Errorf("runner PID %d owns %s and runs %s, but scheduled task %s runs %s; use the existing service manager or explicitly migrate the runner before rerunning setup", pid, socket, image, scheduledTaskName, task.Command)
	}
	return nil
}
