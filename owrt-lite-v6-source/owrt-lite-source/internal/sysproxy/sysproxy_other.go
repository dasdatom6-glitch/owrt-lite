//go:build !windows && !linux

package sysproxy

import "fmt"

func SetProxy(addr string) error {
	return fmt.Errorf("not supported on this OS")
}

func ClearProxy() error {
	return nil
}
