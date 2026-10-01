package command

import (
	"context"
	"os/exec"
	"time"
)

// prettyName is the computer name macOS shows in Finder, AirDrop and
// Settings › General › About, such as "Sam's MacBook Pro".
func prettyName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "ComputerName").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
