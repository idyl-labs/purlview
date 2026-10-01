package command

import (
	"strconv"
	"strings"
)

// machineInfoPretty reads PRETTY_HOSTNAME from an /etc/machine-info file, an
// environment-style file whose values may be quoted.
func machineInfoPretty(file string) string {
	for _, line := range strings.Split(file, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_HOSTNAME=")
		if !ok {
			continue
		}
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			if value[0] == '"' {
				if unquoted, err := strconv.Unquote(value); err == nil {
					return unquoted
				}
			}
			return value[1 : len(value)-1]
		}
		return value
	}
	return ""
}
