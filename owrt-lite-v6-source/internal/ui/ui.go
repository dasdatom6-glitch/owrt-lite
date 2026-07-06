package ui

import (
	_ "embed"
	"net/http"
)

//go:embed static/index.html
var indexHTML []byte

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
}
