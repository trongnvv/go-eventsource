package eventsource

import (
	"bytes"
	"container/list"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/patrickmn/go-cache"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type EventMessage struct {
	Channel string
	Id      string
	Event   string
	Data    string
}

type retryMessage struct {
	retry time.Duration
}

type eventSource struct {
	customHeadersFunc func(*http.Request) [][]byte

	sink           chan message
	staled         chan *consumer
	add            chan *consumer
	close          chan bool
	idleTimeout    time.Duration
	retry          time.Duration
	timeout        time.Duration
	closeOnTimeout bool
	gzip           bool
	natsConn       *nats.Conn
	cache          *cache.Cache
	consumersLock  sync.RWMutex
	topic          string
	consumers      map[string]*list.List
}

type Settings struct {
	// SetTimeout sets the write timeout for individual messages. The
	// default is 2 seconds.
	Timeout time.Duration

	// CloseOnTimeout sets whether a write timeout should close the
	// connection or just drop the message.
	//
	// If the connection gets closed on a timeout, it's the client's
	// responsibility to re-establish a connection. If the connection
	// doesn't get closed, messages might get sent to a potentially dead
	// client.
	//
	// The default is true.
	CloseOnTimeout bool

	// Sets the timeout for an idle connection. The default is 30 minutes.
	IdleTimeout time.Duration

	// Gzip sets whether to use gzip Content-Encoding for clients which
	// support it.
	//
	// The default is false.
	Gzip    bool
	NatsURL string
	Topic   string
}

func DefaultSettings() *Settings {
	return &Settings{
		Timeout:        2 * time.Second,
		CloseOnTimeout: true,
		IdleTimeout:    30 * time.Minute,
		Gzip:           false,
		NatsURL:        "",
		Topic:          "default",
	}
}

// EventSource interface provides methods for sending messages and closing all connections.
type EventSource interface {
	// Handler it should implement ServerHTTP method
	http.Handler

	// SendEventMessage send message to all consumers
	SendEventMessage(channel, id, data, event string)
	//SendEventMessage(em *EventMessage)

	// SendRetryMessage send retry message to all consumers
	SendRetryMessage(duration time.Duration)

	// ConsumersCount count consumers count
	ConsumersCount() int

	// Close and clear all consumers
	Close()
}

type message interface {
	// The message to be sent to clients
	prepareMessage() []byte
}

func (m *EventMessage) prepareMessage() []byte {
	var data bytes.Buffer
	if len(m.Id) > 0 {
		data.WriteString(fmt.Sprintf("id: %s\n", strings.Replace(m.Id, "\n", "", -1)))
	}
	if len(m.Event) > 0 {
		data.WriteString(fmt.Sprintf("event: %s\n", strings.Replace(m.Event, "\n", "", -1)))
	}
	if len(m.Data) > 0 {
		lines := strings.Split(m.Data, "\n")
		for _, line := range lines {
			data.WriteString(fmt.Sprintf("data: %s\n", line))
		}
	}
	data.WriteString("\n")
	return data.Bytes()
}

func controlProcess(es *eventSource) {
	for {
		select {
		case em := <-es.sink:
			channel := em.(*EventMessage).Channel
			_, ok := es.consumers[channel]
			if !ok {
				break
			}
			message := em.prepareMessage()

			func() {
				es.consumersLock.RLock()
				defer es.consumersLock.RUnlock()
				// send all group consumers via channel
				for e := es.consumers[channel].Front(); e != nil; e = e.Next() {
					c := e.Value.(*consumer)

					// Only send this message if the consumer isn't staled
					if !c.staled {
						select {
						case c.in <- message:
						default:
						}
					}
				}
			}()
		case <-es.close:
			close(es.sink)
			close(es.add)
			close(es.staled)
			close(es.close)

			func() {
				es.consumersLock.RLock()
				defer es.consumersLock.RUnlock()
				for group := range es.consumers {
					fmt.Println("close data consumers group:", group)

					for e := es.consumers[group].Front(); e != nil; e = e.Next() {
						c := e.Value.(*consumer)
						close(c.in)
					}
				}
			}()
			func() {
				es.consumersLock.Lock()
				defer es.consumersLock.Unlock()
				for group := range es.consumers {
					fmt.Println("close group:", group)
					es.consumers[group].Init()
					delete(es.consumers, group)
				}
			}()

			return
		case c := <-es.add:
			func() {
				es.consumersLock.Lock()
				defer es.consumersLock.Unlock()
				_, ok := es.consumers[c.group]
				if !ok {
					es.consumers[c.group] = list.New()
				} else {
					//fmt.Println("add consumer", es.consumers[c.group].Len())
				}
				es.consumers[c.group].PushBack(c)
			}()
		case c := <-es.staled:
			toRemoveEls := make([]*list.Element, 0, 1)
			_, ok := es.consumers[c.group]
			if !ok {
				break
			}

			func() {
				es.consumersLock.RLock()
				defer es.consumersLock.RUnlock()

				for e := es.consumers[c.group].Front(); e != nil; e = e.Next() {
					if e.Value.(*consumer) == c {
						toRemoveEls = append(toRemoveEls, e)
					}
				}
			}()
			func() {
				es.consumersLock.Lock()
				defer es.consumersLock.Unlock()

				for _, e := range toRemoveEls {
					es.consumers[c.group].Remove(e)
				}
				if es.consumers[c.group].Len() == 0 {
					delete(es.consumers, c.group)
				}
				//fmt.Println("len group", c.group, es.consumers[c.group].Len())

			}()
			close(c.in)
		}
	}
}

// New creates new EventSource instance.
func New(settings *Settings, customHeadersFunc func(*http.Request) [][]byte) EventSource {
	if settings == nil {
		settings = DefaultSettings()
	}

	es := new(eventSource)
	if settings.NatsURL != "" {
		nc, err := nats.Connect(settings.NatsURL)
		if err != nil {
			log.Fatalf("nats url connect fail %v", err)
		}
		es.natsConn = nc
	}

	es.customHeadersFunc = customHeadersFunc
	es.sink = make(chan message, 1)
	es.close = make(chan bool)
	es.staled = make(chan *consumer, 1)
	es.add = make(chan *consumer)
	es.consumers = make(map[string]*list.List)
	es.timeout = settings.Timeout
	es.idleTimeout = settings.IdleTimeout
	es.closeOnTimeout = settings.CloseOnTimeout
	es.gzip = settings.Gzip
	es.topic = settings.Topic
	es.cache = cache.New(time.Minute, 2*time.Minute)
	if es.natsConn != nil {
		go adapters(es)
	}
	go controlProcess(es)
	return es
}

func adapters(es *eventSource) {
	_, err := es.natsConn.Subscribe("sse.adapters."+es.topic, func(m *nats.Msg) {
		fmt.Printf("Received a message: %s\n", string(m.Data))
		var em *EventMessage
		err := json.Unmarshal(m.Data, &em)
		if err != nil {
			return
		}
		//fmt.Printf("Received a message: %s\n", em)
		if _, found := es.cache.Get(es.topic + em.Id); !found {
			es.cache.Set(es.topic+em.Id, "existed", cache.DefaultExpiration)
			es.sendMessage(em)
		}
	})
	if err != nil {
		return
	}
}

func (es *eventSource) Close() {
	es.close <- true
}

func (es *eventSource) auth(accessToken string) bool {
	return true
}

// ServeHTTP implements http.Handler interface.
func (es *eventSource) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if !es.auth(req.URL.Query().Get("access_token")) {
		resp.Header().Set("Access-Control-Allow-Origin", "*")
		resp.Header().Set("Content-Type", "text/event-stream")
		resp.Header().Set("Cache-Control", "no-cache")
		resp.Header().Set("Connection", "keep-alive")
		http.Error(resp, "Unauthorized!", http.StatusUnauthorized)
		return
	}
	cons, err := newConsumer(resp, req, es)
	if err != nil {
		log.Print("Can't create connection to a consumer: ", err)
		resp.Header().Set("Access-Control-Allow-Origin", "*")
		resp.Header().Set("Content-Type", "text/event-stream")
		resp.Header().Set("Cache-Control", "no-cache")
		resp.Header().Set("Connection", "keep-alive")
		http.Error(resp, "InternalServerError!", http.StatusInternalServerError)
		return
	}
	es.add <- cons
}

func (es *eventSource) sendMessage(m message) {
	es.sink <- m
}

func (es *eventSource) SendEventMessage(channel, id, data, event string) {
	if id == "" {
		id = uuid.New().String()
	}

	em := &EventMessage{channel, id, event, data}
	es.cache.Set(es.topic+id, "existed", cache.DefaultExpiration)
	dataPub, _ := json.Marshal(em)
	_ = es.natsConn.Publish("sse.adapters."+es.topic, dataPub)
	es.sendMessage(em)
}

func (m *retryMessage) prepareMessage() []byte {
	return []byte(fmt.Sprintf("retry: %d\n\n", m.retry/time.Millisecond))
}

func (es *eventSource) SendRetryMessage(t time.Duration) {
	es.sendMessage(&retryMessage{t})
}

func (es *eventSource) ConsumersCount() int {
	es.consumersLock.RLock()
	defer es.consumersLock.RUnlock()
	total := 0
	for _, v := range es.consumers {
		total += v.Len()
	}
	return total
}
