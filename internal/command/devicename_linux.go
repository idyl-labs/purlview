package command

import "os"

// prettyName is systemd's pretty hostname (`hostnamectl set-hostname
// --pretty`), kept in /etc/machine-info; most machines have none.
func prettyName() string {
	b, err := os.ReadFile("/etc/machine-info")
	if err != nil {
		return ""
	}
	return machineInfoPretty(string(b))
}
