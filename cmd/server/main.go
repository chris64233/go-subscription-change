// Command server 启动订阅套餐变更服务的 HTTP API。
//
// 环境变量：
//
//	ADDR      监听地址（默认 :8080）
//	WAL_PATH  事件日志路径；为空时使用纯内存存储（重启丢失）
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	sub "github.com/chris64233/go-subscription-change/subscription"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	wal := os.Getenv("WAL_PATH")

	store, err := sub.Open(wal)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	svc := sub.NewService(store, func() time.Time { return time.Now().UTC() })
	srv := sub.NewServer(svc)

	log.Printf("subscription change service listening on %s (wal=%q)", addr, wal)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
