package main

import (
	"fmt"
	"net/http"
	"time"
)

var counter int

func incr(w http.ResponseWriter, _ *http.Request) {
	counter++ // set breakpoint here in tests
	fmt.Fprintf(w, "counter=%d\n", counter)
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte("ok"))
}

func main() {
	http.HandleFunc("/incr", incr)
	http.HandleFunc("/health", health)
	server := &http.Server{Addr: ":8080"}
	go server.ListenAndServe()
	for {
		time.Sleep(1 * time.Second)
	}
}
