// Command fakesvc is a small stand-in upstream used by the smoke test. It
// serves a health endpoint, a JSON echo endpoint and an SSE stream so the
// gateway can be exercised without touching a real service.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := flag.Int("port", 18084, "port to listen on")
	name := flag.String("name", "fake", "service name reported by /health")
	fail := flag.Bool("fail", false, "return 500 from the health endpoint")
	flag.Parse()

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if *fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","service":%q}`, *name)
	})

	mux.HandleFunc("/v1/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		for _, chunk := range []string{"data: alpha\n\n", "data: beta\n\n", "data: gamma\n\n"} {
			_, _ = w.Write([]byte(chunk))
			fl.Flush()
			time.Sleep(120 * time.Millisecond)
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"echo":true,"service":%q,"path":%q,"method":%q,"auth":%q}`,
			*name, r.URL.Path, r.Method, r.Header.Get("Authorization"))
	})

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	log.Printf("fakesvc %s listening on %s", *name, addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
