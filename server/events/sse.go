// Package events based on https://thoughtbot.com/blog/writing-a-server-sent-events-server-in-go
package events

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/pl"
	"github.com/navidrome/navidrome/utils/singleton"
)

// Broker 是 SSE 事件总线，负责把服务端事件推送给所有连接的客户端。
type Broker interface {
	http.Handler
	SendMessage(ctx context.Context, event Event)
	SendBroadcastMessage(ctx context.Context, event Event)
}

const (
	keepAliveFrequency = 15 * time.Second
	writeTimeOut       = 5 * time.Second
	bufferSize         = 1
)

type (
	message struct {
		id        uint64
		event     string
		data      string
		senderCtx context.Context
	}
	messageChan chan message
	clientsChan chan client
	client      struct {
		id             string
		address        string
		username       string
		userAgent      string
		clientUniqueId string
		displayString  string
		msgC           chan message
	}
)

// String 返回客户端的可读标识，用于日志。
func (c client) String() string {
	return c.displayString
}

// broker 通过 channel 串行化订阅、退订与发布，
// 从而无需对客户端集合加锁（见 listen）。
type broker struct {
	// Events are pushed to this channel by the main events-gathering routine
	publish messageChan

	// New client connections
	subscribing clientsChan

	// Closed client connections
	unsubscribing clientsChan
}

// GetBroker 返回全局事件总线单例，并启动其事件循环。
func GetBroker() Broker {
	return singleton.GetInstance(func() *broker {
		// Instantiate a broker
		broker := &broker{
			publish:       make(messageChan, 2),
			subscribing:   make(clientsChan, 1),
			unsubscribing: make(clientsChan, 1),
		}

		// Set it running - listening and broadcasting events
		go broker.listen()
		return broker
	})
}

// SendBroadcastMessage 广播事件给所有客户端，忽略来源过滤。
func (b *broker) SendBroadcastMessage(ctx context.Context, evt Event) {
	ctx = broadcastToAll(ctx)
	b.SendMessage(ctx, evt)
}

// SendMessage 发布事件。上下文会随消息一起传递，供投递时判断接收范围。
func (b *broker) SendMessage(ctx context.Context, evt Event) {
	msg := b.prepareMessage(ctx, evt)
	log.Trace("Broker received new event", "type", msg.event, "data", msg.data)
	b.publish <- msg
}

// prepareMessage 把事件序列化为待发送的消息。
func (b *broker) prepareMessage(ctx context.Context, event Event) message {
	msg := message{}
	msg.data = event.Data(event)
	msg.event = event.Name(event)
	msg.senderCtx = ctx
	return msg
}

// writeEvent writes a message to the given io.Writer, formatted as a Server-Sent Event.
// If the writer is a http.Flusher, it flushes the data immediately instead of buffering it.
//
// writeEvent 按 SSE 格式写出一条事件并立即 flush。
// 设置写超时以免慢客户端长期占用连接；设置失败只记日志，不影响写出。
func writeEvent(ctx context.Context, w io.Writer, event message, timeout time.Duration) error {
	if err := setWriteTimeout(w, timeout); err != nil {
		log.Debug(ctx, "Error setting write timeout", err)
	}

	_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.id, event.event, event.data)
	if err != nil {
		return err
	}

	// If the writer is a http.Flusher, flush the data immediately.
	if flusher, ok := w.(http.Flusher); ok && flusher != nil {
		flusher.Flush()
	}
	return nil
}

// setWriteTimeout 逐层解包 ResponseWriter 直到找到支持设置写超时的实现。
// 中间件常会包装 ResponseWriter，故需沿 Unwrap 链向下查找。
func setWriteTimeout(rw io.Writer, timeout time.Duration) error {
	for {
		switch t := rw.(type) {
		case interface{ SetWriteDeadline(time.Time) error }:
			return t.SetWriteDeadline(time.Now().Add(timeout))
		case interface{ Unwrap() http.ResponseWriter }:
			rw = t.Unwrap()
		default:
			return fmt.Errorf("%T - %w", rw, http.ErrNotSupported)
		}
	}
}

// ServeHTTP 处理 SSE 长连接。
//
// 必须支持 Flush，否则事件会滞留在缓冲区无法实时送达。
// X-Accel-Buffering 头用于关闭 Nginx 的响应缓冲，否则代理后事件同样会被攒住。
// 连接期间持续从自己的消息通道读取，写失败即断开。
func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user, _ := request.UserFrom(ctx)

	// Make sure that the writer supports flushing.
	_, ok := w.(http.Flusher)
	if !ok {
		log.Error(r, "Streaming unsupported! Events cannot be sent to this client", "address", r.RemoteAddr,
			"userAgent", r.UserAgent(), "user", user.UserName)
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Tells Nginx to not buffer this response. See https://stackoverflow.com/a/33414096
	w.Header().Set("X-Accel-Buffering", "no")

	// Each connection registers its own message channel with the Broker's connections registry
	c := b.subscribe(r)
	defer b.unsubscribe(c)
	log.Debug(ctx, "Started new EventStream connection", "client", c.String())

	for event := range pl.ReadOrDone(ctx, c.msgC) {
		log.Trace(ctx, "Sending event to client", "event", event, "client", c.String())
		err := writeEvent(ctx, w, event, writeTimeOut)
		if err != nil {
			log.Debug(ctx, "Error sending event to client. Closing connection", "event", event, "client", c.String(), err)
			return
		}
	}
	log.Trace(ctx, "Client EventStream connection closed", "client", c.String())
}

// subscribe 注册一个新客户端。
func (b *broker) subscribe(r *http.Request) client {
	ctx := r.Context()
	user, _ := request.UserFrom(ctx)
	clientUniqueId, _ := request.ClientUniqueIdFrom(ctx)
	c := client{
		id:             id.NewRandom(),
		username:       user.UserName,
		address:        r.RemoteAddr,
		userAgent:      r.UserAgent(),
		clientUniqueId: clientUniqueId,
	}
	if log.IsGreaterOrEqualTo(log.LevelTrace) {
		c.displayString = fmt.Sprintf("%s (%s - %s - %s - %s)", c.id, c.username, c.address, c.clientUniqueId, c.userAgent)
	} else {
		c.displayString = fmt.Sprintf("%s (%s - %s - %s)", c.id, c.username, c.address, c.clientUniqueId)
	}

	c.msgC = make(chan message, bufferSize)

	// Signal the broker that we have a new client
	b.subscribing <- c
	return c
}

// unsubscribe 注销客户端。
func (b *broker) unsubscribe(c client) {
	b.unsubscribing <- c
}

// shouldSend 判断某条消息是否应发给指定客户端。
//
// 规则：显式广播的全发；非客户端触发的（如扫描完成）全发；
// 由某客户端操作触发的，不回发给发起者本身（它已在本地更新过），
// 只发给同一用户的其他连接。
func (b *broker) shouldSend(msg message, c client) bool {
	if broadcastToAll, ok := msg.senderCtx.Value(broadcastToAllKey).(bool); ok && broadcastToAll {
		return true
	}
	clientUniqueId, originatedFromClient := request.ClientUniqueIdFrom(msg.senderCtx)
	if !originatedFromClient {
		return true
	}
	if c.clientUniqueId == clientUniqueId {
		return false
	}
	if username, ok := request.UsernameFrom(msg.senderCtx); ok {
		return username == c.username
	}
	return true
}

// listen 是事件总线主循环。
//
// 订阅、退订、发布、心跳都在这个单一 goroutine 中处理，
// 因此客户端集合无需加锁。
// 新客户端接入时立刻推一条 serverStart，便于前端识别服务端重启。
// 每 15 秒发心跳，防止中间代理因空闲而切断连接。
func (b *broker) listen() {
	keepAlive := time.NewTicker(keepAliveFrequency)
	defer keepAlive.Stop()

	clients := map[client]struct{}{}
	var eventId uint64

	getNextEventId := func() uint64 {
		eventId++
		return eventId
	}

	for {
		select {
		case c := <-b.subscribing:
			// A new client has connected.
			// Register their message channel
			clients[c] = struct{}{}
			log.Debug("Client added to EventStream broker", "numActiveClients", len(clients), "newClient", c.String())

			// Send a serverStart event to new client
			msg := b.prepareMessage(context.Background(),
				&ServerStart{StartTime: consts.ServerStart, Version: consts.Version})
			sendOrDrop(c, msg)

		case c := <-b.unsubscribing:
			// A client has detached, and we want to
			// stop sending them messages.
			close(c.msgC)
			delete(clients, c)
			log.Debug("Removed client from EventStream broker", "numActiveClients", len(clients), "client", c.String())

		case msg := <-b.publish:
			msg.id = getNextEventId()
			log.Trace("Got new published event", "event", msg)
			// We got a new event from the outside!
			// Send event to all connected clients
			for c := range clients {
				if b.shouldSend(msg, c) {
					log.Trace("Putting event on client's queue", "client", c.String(), "event", msg)
					sendOrDrop(c, msg)
				}
			}

		case ts := <-keepAlive.C:
			// Send a keep alive message every 15 seconds to all connected clients
			if len(clients) == 0 {
				continue
			}
			msg := b.prepareMessage(context.Background(), &KeepAlive{TS: ts.Unix()})
			msg.id = getNextEventId()
			for c := range clients {
				log.Trace("Putting a keepalive event on client's queue", "client", c.String(), "event", msg)
				sendOrDrop(c, msg)
			}
		}
	}
}

// sendOrDrop 非阻塞投递。
// 客户端队列满说明其消费不及，此时丢弃该事件——
// 阻塞会拖住整个事件循环，影响所有其他客户端。
func sendOrDrop(client client, msg message) {
	select {
	case client.msgC <- msg:
	default:
		if log.IsGreaterOrEqualTo(log.LevelTrace) {
			log.Trace("Event dropped because client's channel is full", "event", msg, "client", client.String())
		}
	}
}

// NoopBroker 返回不做任何事的事件总线，用于关闭事件推送的场景。
func NoopBroker() Broker {
	return noopBroker{}
}

// noopBroker 是 Broker 的空实现。
type noopBroker struct {
	http.Handler
}

func (b noopBroker) SendBroadcastMessage(context.Context, Event) {}

func (noopBroker) SendMessage(context.Context, Event) {}
