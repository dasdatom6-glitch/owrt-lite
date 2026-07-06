package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"owrt-lite/internal/config"
	"owrt-lite/internal/node"
	"owrt-lite/internal/server"
	"owrt-lite/internal/sysproxy"
)

func getenv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func main() {
	var addr string
	var readTimeoutStr string
	var writeTimeoutStr string
	var inflightLimitStr string
	var nm *node.Manager

	uciPath := getenv("UCI_PATH", "")

	if uciPath != "" {
		if cfg, err := config.LoadUCI(uciPath); err == nil {
			addr = cfg.ListenAddr
			readTimeoutStr = cfg.ReadTimeout
			writeTimeoutStr = cfg.WriteTimeout
			inflightLimitStr = strconv.Itoa(cfg.MaxInflight)
			var urls []*url.URL
			for _, s := range cfg.Nodes {
				if u, err := url.Parse(s); err == nil && u.Host != "" {
					urls = append(urls, u)
				}
			}
			nm = node.New(urls)
		}
	}
	if nm == nil {
		var urls []*url.URL
		for _, s := range strings.Split(getenv("NODES", ""), ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if u, err := url.Parse(s); err == nil && u.Host != "" {
				urls = append(urls, u)
			}
		}
		if len(urls) > 0 {
			nm = node.New(urls)
		}
	}

	if nm == nil {
		nm = node.New(nil)
	}
	if err := nm.LoadState("nodes_state.json"); err == nil {
		fmt.Println("Loaded saved nodes from nodes_state.json")
	}

	if addr == "" {
		addr = getenv("LISTEN_ADDR", ":8080")
	}
	if readTimeoutStr == "" {
		readTimeoutStr = getenv("READ_TIMEOUT", "5s")
	}
	if writeTimeoutStr == "" {
		writeTimeoutStr = getenv("WRITE_TIMEOUT", "10s")
	}
	if inflightLimitStr == "" {
		inflightLimitStr = getenv("MAX_INFLIGHT", "0")
	}

	readTimeout, _ := time.ParseDuration(readTimeoutStr)
	writeTimeout, _ := time.ParseDuration(writeTimeoutStr)
	inflightLimit, _ := strconv.Atoi(inflightLimitStr)

	s := server.New(inflightLimit, nm, addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       30 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		MaxHeaderBytes:    4096,
	}

	s.SetShutdownCallback(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		os.Exit(0)
	})

	browse := addr
	if strings.HasPrefix(addr, ":") {
		browse = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		browse = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	fmt.Println("owrt-lite listening addr:", addr, "browse:", "http://"+browse)
	fmt.Println("Default admin password:", s.GetPassword())

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		fmt.Println("Shutting down...")
		_ = sysproxy.ClearProxy()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Println("server error:", err)
	}
}
