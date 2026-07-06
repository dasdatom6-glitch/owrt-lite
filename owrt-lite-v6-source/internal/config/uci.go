package config

import (
	"bufio"
	"os"
	"strings"
)

type UCI struct {
	ListenAddr   string
	ReadTimeout  string
	WriteTimeout string
	MaxInflight  int
	Nodes        []string
}

func LoadUCI(path string) (UCI, error) {
	var u UCI
	f, err := os.Open(path)
	if err != nil {
		return u, err
	}
	defer f.Close()
	inTarget := false
	inNode := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "config") {
			inTarget = strings.Contains(line, "owrtlite")
			inNode = strings.Contains(line, "node")
			continue
		}
		if strings.HasPrefix(line, "option") {
			parts := strings.Fields(line)
			if len(parts) < 3 {
				continue
			}
			key := parts[1]
			val := strings.Join(parts[2:], " ")
			val = strings.Trim(val, "'\"")
			if inTarget {
				switch key {
				case "listen":
					u.ListenAddr = val
				case "read_timeout":
					u.ReadTimeout = val
				case "write_timeout":
					u.WriteTimeout = val
				case "max_inflight":
					u.MaxInflight = atoi(val)
				}
			} else if inNode {
				if key == "url" {
					u.Nodes = append(u.Nodes, val)
				}
			}
		}
	}
	return u, nil
}

func atoi(s string) int {
	n := 0
	sign := 1
	for i, r := range s {
		if i == 0 && r == '-' {
			sign = -1
			continue
		}
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n * sign
}
