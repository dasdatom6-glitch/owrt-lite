//go:build windows

package sysproxy

import (
	"fmt"
	"os/exec"
	"syscall"
)

var (
	wininet           = syscall.NewLazyDLL("wininet.dll")
	internetSetOption = wininet.NewProc("InternetSetOptionW")
)

const (
	INTERNET_OPTION_SETTINGS_CHANGED = 39
	INTERNET_OPTION_REFRESH          = 37
)

func notifySettingsChanged() {
	internetSetOption.Call(0, INTERNET_OPTION_SETTINGS_CHANGED, 0, 0)
	internetSetOption.Call(0, INTERNET_OPTION_REFRESH, 0, 0)
}

// SetProxy sets the system proxy to the given address (e.g., "127.0.0.1:8080")
func SetProxy(addr string) error {
	fmt.Printf("[SYSPROXY] Enabling system proxy: %s\n", addr)
	// 1. Set ProxyServer
	cmd := exec.Command("reg", "add", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings", "/v", "ProxyServer", "/t", "REG_SZ", "/d", addr, "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("set ProxyServer: %w", err)
	}

	// 2. Set ProxyEnable = 1
	cmd = exec.Command("reg", "add", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings", "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("set ProxyEnable: %w", err)
	}

	// 3. Set ProxyOverride = "<local>" to bypass local addresses
	cmd = exec.Command("reg", "add", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings", "/v", "ProxyOverride", "/t", "REG_SZ", "/d", "<local>", "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("set ProxyOverride: %w", err)
	}

	// Notify system
	notifySettingsChanged()

	return nil
}

// ClearProxy disables the system proxy
func ClearProxy() error {
	cmd := exec.Command("reg", "add", "HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings", "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		return err
	}

	// Notify system
	notifySettingsChanged()

	return nil
}
