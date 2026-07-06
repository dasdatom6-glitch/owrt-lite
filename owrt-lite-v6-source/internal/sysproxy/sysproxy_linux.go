//go:build linux

package sysproxy

import (
	"fmt"
	"os/exec"
	"strings"
)

// SetProxy sets the system proxy on Linux GNOME when gsettings is available.
// In headless/non-GNOME environments it is a no-op so the web API does not fail.
func SetProxy(addr string) error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil
	}
	host, port, found := strings.Cut(addr, ":")
	if !found {
		return fmt.Errorf("invalid address format")
	}

	cmds := [][]string{
		{"gsettings", "set", "org.gnome.system.proxy", "mode", "manual"},
		{"gsettings", "set", "org.gnome.system.proxy.http", "host", host},
		{"gsettings", "set", "org.gnome.system.proxy.http", "port", port},
		{"gsettings", "set", "org.gnome.system.proxy.https", "host", host},
		{"gsettings", "set", "org.gnome.system.proxy.https", "port", port},
	}
	for _, c := range cmds {
		if err := exec.Command(c[0], c[1:]...).Run(); err != nil {
			return fmt.Errorf("cmd %v failed: %w", c, err)
		}
	}
	return nil
}

// ClearProxy disables the system proxy on Linux GNOME when gsettings is available.
func ClearProxy() error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return nil
	}
	return exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "none").Run()
}
