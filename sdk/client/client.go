// Package client ports packages/sdk/client: the low-level JSON-RPC client
// for a DSH SDK runtime. It speaks the sdk/protocol wire over a caller-owned
// transport, fans server notifications out to filtered subscriptions, and
// applies the typed result contracts. Go adaptation: process spawning and
// the dispose ladder belong to the dsh-subprocess seam; this client takes
// any transport peer (an in-process pipe pair in tests, caller-wired stdio
// in production).
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"dshgo/sdk/protocol"
)

// ClosedError is the runtime-gone condition: the transport closed, the
// process exited, or it was never launchable.
type ClosedError struct{ Message string }

func (e *ClosedError) Error() string { return e.Message }

// RequestTimeoutError is a request that exceeded its budget. There is no
// wire-level cancel: a timed-out request keeps running server-side until the
// runtime closes.
type RequestTimeoutError struct {
	Method    string
	TimeoutMs int64
}

func (e *RequestTimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %dms", e.Method, e.TimeoutMs)
}

// ProtocolError is a runtime answer outside its documented contract.
type ProtocolError struct{ Message string }

func (e *ProtocolError) Error() string { return e.Message }

// Notification is one server notification fanned out to subscriptions.
type Notification struct {
	Method string
	Params map[string]any
}

// NotificationFilter selects which notifications one subscription receives.
type NotificationFilter func(Notification) bool

// subscription is the client-side queue for one subscriber.
type subscription struct {
	filter NotificationFilter
	client *Client
	id     string

	mu      sync.Mutex
	queue   []Notification
	waiters []chan Notification
	failure error
	closed  bool
}

// Next awaits the next matching notification. After the client closed or the
// runtime died it fails immediately (a born-failed handle never had a
// producer); after Close it fails at once (the queue is dropped).
func (s *subscription) Next(ctx context.Context) (Notification, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	for len(s.queue) > 0 {
		notification := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		return notification, nil
	}
	if s.failure != nil {
		failure := s.failure
		s.mu.Unlock()
		return Notification{}, failure
	}
	waiter := make(chan Notification, 1)
	s.waiters = append(s.waiters, waiter)
	s.mu.Unlock()
	select {
	case notification := <-waiter:
		return notification, nil
	case <-ctx.Done():
		s.mu.Lock()
		for index, candidate := range s.waiters {
			if candidate == waiter {
				s.waiters = append(s.waiters[:index], s.waiters[index+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return Notification{}, ctx.Err()
	}
}

// TryNext drains one already-delivered notification without waiting.
func (s *subscription) TryNext() (Notification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Notification{}, false
	}
	notification := s.queue[0]
	s.queue = s.queue[1:]
	return notification, true
}

// Close detaches from the client: queued items drop and pending waiters
// fail.
func (s *subscription) Close() {
	if s.client != nil {
		s.client.detach(s.id)
	}
	s.fail(errors.New("subscription closed"))
}

func (s *subscription) fail(err error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.failure = err
	waiters := s.waiters
	s.queue, s.waiters = nil, nil
	s.mu.Unlock()
	for _, waiter := range waiters {
		waiter <- Notification{}
	}
}

func (s *subscription) deliver(notification Notification) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if len(s.waiters) > 0 {
		waiter := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.mu.Unlock()
		waiter <- notification
		return
	}
	s.queue = append(s.queue, notification)
	s.mu.Unlock()
}

// Client is the typed JSON-RPC client for one SDK runtime.
type Client struct {
	peer    protocol.Peer
	timeout time.Duration

	mu            sync.Mutex
	serial        int
	subscriptions map[string]*subscription
	closed        bool
	closeErr      error
}

// New builds the typed client over any transport peer.
func New(peer protocol.Peer, defaultTimeout time.Duration) *Client {
	return &Client{
		peer:          peer,
		timeout:       defaultTimeout,
		subscriptions: map[string]*subscription{},
	}
}

// NewOverTransport builds the client over a line transport and installs the
// notification dispatch into it.
func NewOverTransport(transport *protocol.LineTransport, defaultTimeout time.Duration) *Client {
	client := New(transport, defaultTimeout)
	transport.OnNotification(func(method string, params map[string]any) {
		client.dispatch(Notification{Method: method, Params: params})
	})
	return client
}

func (c *Client) dispatch(notification Notification) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	subs := make([]*subscription, 0, len(c.subscriptions))
	for _, sub := range c.subscriptions {
		subs = append(subs, sub)
	}
	c.mu.Unlock()
	for _, sub := range subs {
		if sub.filter != nil && !sub.filter(notification) {
			continue
		}
		sub.deliver(notification)
	}
}

func (c *Client) detach(id string) {
	c.mu.Lock()
	delete(c.subscriptions, id)
	c.mu.Unlock()
}

// Request sends one JSON-RPC request and awaits its result. The error is a
// *protocol.JsonRpcResponseError on a protocol error response, a
// *RequestTimeoutError on timeout, and a *ClosedError when the runtime is
// gone.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	closed, closeErr := c.closed, c.closeErr
	c.mu.Unlock()
	if closed {
		if closeErr != nil {
			return nil, closeErr
		}
		return nil, &ClosedError{Message: "DeepSeek Harness runtime client is closed"}
	}
	callCtx := ctx
	cancel := context.CancelFunc(func() {})
	if c.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, c.timeout)
	}
	defer cancel()
	result, err := c.peer.Request(callCtx, method, params)
	if err != nil {
		var rpcErr *protocol.JsonRpcResponseError
		if errors.As(err, &rpcErr) {
			return nil, rpcErr
		}
		if ctx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, &RequestTimeoutError{Method: method, TimeoutMs: c.timeout.Milliseconds()}
		}
		return nil, &ClosedError{Message: err.Error()}
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		return nil, &ProtocolError{Message: fmt.Sprintf("runtime result is not JSON-serializable: %v", encodeErr)}
	}
	return json.RawMessage(encoded), nil
}

// Initialize performs the process-wide handshake and enforces the server
// identity contract.
func (c *Client) Initialize(ctx context.Context, params protocol.InitializeParams) (protocol.InitializeResult, error) {
	raw, err := c.Request(ctx, protocol.MethodInitialize, params)
	if err != nil {
		return protocol.InitializeResult{}, err
	}
	var decoded protocol.InitializeResult
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.ServerInfo.Name == "" || decoded.ServerInfo.Version == "" {
		return protocol.InitializeResult{}, &ProtocolError{
			Message: fmt.Sprintf("initialize returned no server identity: %s", string(raw)),
		}
	}
	return decoded, nil
}

// Prompt queues one prompt and returns its durable inbox identity.
func (c *Client) Prompt(ctx context.Context, sessionID string, contentBlocks []json.RawMessage) (string, error) {
	raw, err := c.Request(ctx, protocol.MethodSessionPrompt, protocol.SessionPromptParams{
		SessionID:     sessionID,
		ContentBlocks: contentBlocks,
	})
	if err != nil {
		return "", err
	}
	var decoded protocol.SessionPromptResult
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.MessageID == "" {
		return "", &ProtocolError{
			Message: fmt.Sprintf("session/prompt returned no message id: %s", string(raw)),
		}
	}
	return decoded.MessageID, nil
}

// Shutdown requests the protocol shutdown. The surrounding process wiring
// owns the exit.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.Request(ctx, protocol.MethodShutdown, nil)
	return err
}

// Subscribe registers one notification subscription. After Close the handle
// is born failed: there is no producer left, so Next fails instead of
// waiting forever.
func (c *Client) Subscribe(filter NotificationFilter) *subscription {
	c.mu.Lock()
	if c.closed {
		closeErr := c.closeErr
		c.mu.Unlock()
		failure := error(&ClosedError{Message: "DeepSeek Harness runtime closed"})
		if closeErr != nil {
			failure = closeErr
		}
		return &subscription{filter: filter, failure: failure, closed: true}
	}
	id := fmt.Sprintf("%d", c.serial)
	c.serial++
	sub := &subscription{filter: filter, client: c, id: id}
	c.subscriptions[id] = sub
	c.mu.Unlock()
	return sub
}

// Close marks the client closed and fails every subscription. The
// underlying transport's Close belongs to the wiring that owns it.
func (c *Client) Close(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if cause == nil {
		cause = error(&ClosedError{Message: "DeepSeek Harness runtime closed"})
	}
	c.closeErr = cause
	subs := make([]*subscription, 0, len(c.subscriptions))
	for _, sub := range c.subscriptions {
		subs = append(subs, sub)
	}
	c.subscriptions = map[string]*subscription{}
	c.mu.Unlock()
	for _, sub := range subs {
		sub.fail(cause)
	}
}
