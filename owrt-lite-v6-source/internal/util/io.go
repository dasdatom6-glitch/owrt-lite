package util

import (
	"io"
	"net"
)

func ReadAllSmall(r io.Reader) []byte {
	if r == nil {
		return nil
	}
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 4096 {
				break
			}
		}
		if err != nil {
			break
		}
	}
	return buf
}

func IsPrivateIP(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
