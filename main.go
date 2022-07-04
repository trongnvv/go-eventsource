package main

import (
	"flag"
	"github.com/nats-io/nats.go"
	"go-eventsource/eventsource"
	"log"
	"math/rand"
	"net/http"
	"time"
)

func main() {
	var serverNumber string
	flag.StringVar(&serverNumber, "server", "1", "server number")
	flag.Parse()

	setting := &eventsource.Settings{
		Timeout:        2 * time.Second,
		CloseOnTimeout: true,
		IdleTimeout:    30 * time.Minute,
		Gzip:           false,
		NatsURL:        nats.DefaultURL,
		Topic:          "default",
	}

	es := eventsource.New(
		setting,
		func(req *http.Request) [][]byte {
			return [][]byte{
				[]byte("X-Accel-Buffering: no"),
				[]byte("Access-Control-Allow-Origin: *"),
			}
		},
	)
	defer es.Close()

	http.Handle("/events", es)
	go func() {
		for {
			es.SendEventMessage("abcd", "", "server "+serverNumber, "message")
			log.Printf("Hello has been sent (consumers: %d)", es.ConsumersCount())

			time.Sleep(time.Duration(rand.Intn(10-1)+1) * time.Second)
		}
	}()
	log.Print("Open URL http://localhost:8080/ in your browser.")
	err := http.ListenAndServe(":800"+serverNumber, nil)
	if err != nil {
		log.Fatal(err)
	}
}
