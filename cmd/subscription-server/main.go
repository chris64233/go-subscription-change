// Command subscription-server 启动订阅套餐变更服务的内存版 HTTP API。
//
//	go run ./cmd/subscription-server -addr :8080
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	gosubscriptionchange "github.com/chris64233/go-subscription-change"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	flag.Parse()

	store := gosubscriptionchange.NewStore()
	svc := gosubscriptionchange.NewService(store)
	handler := gosubscriptionchange.NewAPI(svc)

	log.Printf("subscription change service listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: handler}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("server error: %v", err)
		os.Exit(1)
	}
}
