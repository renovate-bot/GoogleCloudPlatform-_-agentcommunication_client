// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package client is an AgentCommunication client library.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/agentcommunication_client/gapic"
	cm "cloud.google.com/go/compute/metadata"
	"google.golang.org/api/option"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	acpb "github.com/GoogleCloudPlatform/agentcommunication_client/gapic/agentcommunicationpb"
)

func init() {
	logger = log.New(os.Stderr, "", log.Ldate|log.Ltime|log.Lshortfile)
}

var (
	// DebugLogging enables debug logging.
	DebugLogging = false
	// ErrConnectionClosed is an error indicating that the connection was closed by the caller.
	ErrConnectionClosed = errors.New("connection closed")
	// ErrMessageTimeout is an error indicating message send timed out.
	ErrMessageTimeout = errors.New("timed out waiting for response")
	// ErrResourceExhausted is an error indicating that the server responded to the send with
	// ResourceExhausted.
	ErrResourceExhausted = errors.New("resource exhausted")
	// ErrGettingInstanceToken is an error indicating that the instance token could not be retrieved.
	ErrGettingInstanceToken = errors.New("error getting instance token")

	logger *log.Logger
)

// Connection is an AgentCommunication connection.
type Connection struct {
	client      *agentcommunication.Client
	stream      acpb.AgentCommunication_StreamAgentMessagesClient
	closed      chan struct{}
	streamReady chan struct{}
	sends       chan *acpb.StreamAgentMessagesRequest
	closeErr    error
	resourceID  string
	channelID   string

	messages     chan *acpb.MessageBody
	responseSubs map[string]chan *status.Status
	responseMx   sync.Mutex

	regional          bool
	timeToWaitForResp time.Duration
}

func loggerPrintf(format string, v ...any) {
	if DebugLogging {
		logger.Output(2, fmt.Sprintf(format, v...))
	}
}

// Close the connection.
func (c *Connection) Close() {
	c.close(ErrConnectionClosed)
}

func (c *Connection) close(err error) {
	loggerPrintf("closing connection with err: %v", err)
	st, _ := status.FromError(err)
	loggerPrintf("closing connection with status: %+v", st)
	select {
	case <-c.closed:
		return
	default:
		close(c.closed)
		c.closeErr = err
		c.client.Close()
	}
}

func (c *Connection) waitForResponse(key string, channel chan *status.Status) error {
	timer := time.NewTimer(c.timeToWaitForResp)
	defer timer.Stop()
	select {
	case st := <-channel:
		if st != nil {
			switch st.Code() {
			case codes.OK:
			case codes.ResourceExhausted:
				return fmt.Errorf("%w: %s", ErrResourceExhausted, st.Message())
			default:
				return fmt.Errorf("unexpected status: %+v", st)
			}
		}
	case <-timer.C:
		return fmt.Errorf("%w: timed out waiting for response, MessageID: %q", ErrMessageTimeout, key)
	case <-c.closed:
		return fmt.Errorf("connection closed with err: %w", c.closeErr)
	}
	c.responseMx.Lock()
	delete(c.responseSubs, key)
	c.responseMx.Unlock()
	return nil
}

func (c *Connection) sendWithResp(req *acpb.StreamAgentMessagesRequest, channel chan *status.Status) error {
	loggerPrintf("Sending message %+v", req)

	select {
	case <-c.closed:
		return fmt.Errorf("connection closed with err: %w", c.closeErr)
	case c.sends <- req:
	}

	return c.waitForResponse(req.GetMessageId(), channel)
}

// SendMessage sends a message to the client. Will automatically retry on message timeout (temporary
// disconnects) and in the case of ResourceExhausted with a backoff. Because retries are limited
// the returned error can in some cases be one of ErrMessageTimeout or ErrResourceExhausted, in
// which case send should be retried by the caller.
func (c *Connection) SendMessage(msg *acpb.MessageBody) error {
	var err error
	// Retry 4 times.
	for i := 1; i <= 5; i++ {
		err := c.sendMessage(msg)
		if errors.Is(err, ErrResourceExhausted) {
			// Start with 250ms sleep, then simply multiply by iteration.
			time.Sleep(time.Duration(i*250) * time.Millisecond)
			continue
		} else if errors.Is(err, ErrMessageTimeout) {
			continue
		}
		return err
	}
	return err
}

func (c *Connection) sendMessage(msg *acpb.MessageBody) error {
	req := &acpb.StreamAgentMessagesRequest{
		MessageId: uuid.New().String(),
		Type:      &acpb.StreamAgentMessagesRequest_MessageBody{MessageBody: msg},
	}

	channel := make(chan *status.Status)
	c.responseMx.Lock()
	c.responseSubs[req.GetMessageId()] = channel
	c.responseMx.Unlock()

	select {
	case <-c.closed:
		return fmt.Errorf("connection closed with err: %w", c.closeErr)
	case c.streamReady <- struct{}{}: // Only sends if the stream is ready to send.
	}

	return c.sendWithResp(req, channel)
}

// Receive messages, Receive should be called continuously for the life of the stream connection,
// any delay (>500ms) in Receive when there are queued messages will cause the server to disconnect the
// stream. This means handling the MessageBody from Receive should not be blocking, offload message
// handling to another goroutine and immediately call Receive again.
func (c *Connection) Receive() (*acpb.MessageBody, error) {
	select {
	case msg := <-c.messages:
		return msg, nil
	case <-c.closed:
		return nil, fmt.Errorf("connection closed with err: %w", c.closeErr)
	}
}

func (c *Connection) streamSend(req *acpb.StreamAgentMessagesRequest, streamClosed chan struct{}) error {
	if err := c.stream.Send(req); err != nil {
		if err != io.EOF && !errors.Is(err, io.EOF) {
			// Something is very broken, just close the stream here.
			loggerPrintf("Unexpected send error, closing connection: %v", err)
			c.close(err)
			return err
		}
		// EOF error means the stream is closed, this should be picked up by recv, but that could be
		// blocked, close our sends for now and just allow the caller handle it, SendMessage will wait
		// for response which will never come and auto retry. acknowledgeMessage will fail and prevent
		// the message from being passed on to message handlers, allowing recv to handle the stream
		// close error.
		loggerPrintf("Error sending message, stream closed.")
		select {
		case <-streamClosed:
		default:
			close(streamClosed)
		}
		return ErrConnectionClosed
	}
	return nil
}

func (c *Connection) send(streamClosed chan struct{}) {
	for {
		select {
		case req := <-c.sends:
			if err := c.streamSend(req, streamClosed); err != nil {
				return
			}
		case <-c.closed:
			c.stream.CloseSend()
			return
		case <-streamClosed:
			return
		}
	}
}

// recv keeps receiving and acknowledging new messages.
func (c *Connection) recv(ctx context.Context, streamClosed chan struct{}) {
	loggerPrintf("Receiving messages")
	var unavailableRetries int
	var resourceExhaustedRetries int
	for {
		resp, err := c.stream.Recv()
		if err != nil {
			select {
			case <-streamClosed:
			default:
				close(streamClosed)
			}
			select {
			case <-c.closed:
				return
			default:
			}
			st, ok := status.FromError(err)
			if ok && st.Code() == codes.ResourceExhausted {
				loggerPrintf("Resource exhausted, sleeping before reconnect: %v", err)
				if resourceExhaustedRetries > 9 {
					// Max sleep of 10s.
					time.Sleep(10 * time.Second)
				} else {
					time.Sleep(time.Duration(resourceExhaustedRetries+1) * time.Second)
				}
				resourceExhaustedRetries++
			} else if ok && st.Code() == codes.Unavailable {
				// Retry max 5 times (2s total).
				if unavailableRetries > 5 {
					loggerPrintf("Stream returned Unavailable, exceeded max number of reconnects, closing connection: %v", err)
					c.close(err)
					return
				}
				loggerPrintf("Stream returned Unavailable, will reconnect: %v", err)
				// Sleep for 200ms * num of unavailableRetries, first retry is immediate.
				time.Sleep(time.Duration(unavailableRetries*200) * time.Millisecond)
				unavailableRetries++
			} else if err != io.EOF && !errors.Is(err, io.EOF) && (ok && st.Code() != codes.Canceled) && (ok && st.Code() != codes.DeadlineExceeded) {
				// EOF is a normal stream close, Canceled will be set by the server when stream timeout is
				// reached, DeadlineExceeded would be because of the client side deadline we set.
				loggerPrintf("Unexpected error, closing connection: %v", err)
				c.close(err)
				return
			}
			loggerPrintf("Creating new stream")
			if err := c.createStream(ctx); err != nil {
				loggerPrintf("Error creating new stream: %v", err)
				c.close(err)
			}
			return
		}
		// Reset retries.
		unavailableRetries = 0
		resourceExhaustedRetries = 0
		switch resp.GetType().(type) {
		case *acpb.StreamAgentMessagesResponse_MessageBody:
			// Acknowledge message first, if this ack fails dont forward the message on to the handling
			// logic since that indicates a stream disconnect.
			if err := c.acknowledgeMessage(resp.GetMessageId(), streamClosed); err != nil {
				loggerPrintf("Error acknowledging message %q: %v", resp.GetMessageId(), err)
				continue
			}
			c.messages <- resp.GetMessageBody()
		case *acpb.StreamAgentMessagesResponse_MessageResponse:
			st := resp.GetMessageResponse().GetStatus()
			c.responseMx.Lock()
			for key, sub := range c.responseSubs {
				if key != resp.GetMessageId() {
					continue
				}
				select {
				case sub <- status.FromProto(st):
				default:
				}
			}
			c.responseMx.Unlock()
		}
	}
}

func (c *Connection) acknowledgeMessage(messageID string, streamClosed chan struct{}) error {
	ackReq := &acpb.StreamAgentMessagesRequest{
		MessageId: messageID,
		Type:      &acpb.StreamAgentMessagesRequest_MessageResponse{},
	}
	select {
	case <-c.closed:
		return fmt.Errorf("connection closed with err: %w", c.closeErr)
	default:
		return c.streamSend(ackReq, streamClosed)
	}
}

func (c *Connection) createStream(ctx context.Context) error {
	loggerPrintf("Creating stream.")
	token, err := cm.Get("instance/service-accounts/default/identity?audience=agentcommunication.googleapis.com&format=full")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrGettingInstanceToken, err)
	}

	ctx = metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
		"authentication":                  "Bearer " + token,
		"agent-communication-resource-id": c.resourceID,
		"agent-communication-channel-id":  c.channelID,
	}))

	loggerPrintf("Using ResourceID %q", c.resourceID)
	loggerPrintf("Using ChannelID %q", c.channelID)

	// Set a timeout for the stream, this is well above service side timeout.
	cnclCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	c.stream, err = c.client.StreamAgentMessages(cnclCtx)
	if err != nil {
		cancel()
		return fmt.Errorf("error creating stream: %v", err)
	}

	streamClosed := make(chan struct{})
	go c.recv(ctx, streamClosed)
	go c.send(streamClosed)

	req := &acpb.StreamAgentMessagesRequest{
		MessageId: uuid.New().String(),
		Type: &acpb.StreamAgentMessagesRequest_RegisterConnection{
			RegisterConnection: &acpb.RegisterConnection{ResourceId: c.resourceID, ChannelId: c.channelID}}}

	channel := make(chan *status.Status)
	c.responseMx.Lock()
	c.responseSubs[req.GetMessageId()] = channel
	c.responseMx.Unlock()
	if err := c.sendWithResp(req, channel); err != nil {
		cancel()
		return err
	}

	go func() {
		defer cancel()
		for {
			select {
			case <-c.streamReady:
			case <-streamClosed:
				return
			}
		}
	}()
	loggerPrintf("Stream established.")
	return nil
}

// CreateConnection creates a new connection.
func CreateConnection(ctx context.Context, channelID string, regional bool, opts ...option.ClientOption) (*Connection, error) {
	conn := &Connection{
		regional:          regional,
		channelID:         channelID,
		closed:            make(chan struct{}),
		messages:          make(chan *acpb.MessageBody),
		responseSubs:      make(map[string]chan *status.Status),
		streamReady:       make(chan struct{}),
		sends:             make(chan *acpb.StreamAgentMessagesRequest),
		timeToWaitForResp: 2 * time.Second,
	}

	zone, err := cm.Zone()
	if err != nil {
		return nil, err
	}
	projectNum, err := cm.NumericProjectID()
	if err != nil {
		return nil, err
	}
	instanceID, err := cm.InstanceID()
	if err != nil {
		return nil, err
	}
	conn.resourceID = fmt.Sprintf("projects/%s/zones/%s/instances/%s", projectNum, zone, instanceID)

	location := zone
	if conn.regional {
		location = location[:len(location)-2]
	}

	defaultOpts := []option.ClientOption{
		option.WithoutAuthentication(), // Do not use oauth.
		option.WithGRPCDialOption(grpc.WithTransportCredentials(credentials.NewTLS(nil))), // Because we disabled Auth we need to specifically enable TLS.
		option.WithGRPCDialOption(grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 60 * time.Second, Timeout: 10 * time.Second})),
		option.WithEndpoint(fmt.Sprintf("%s-agentcommunication.googleapis.com:443", location)),
	}

	opts = append(defaultOpts, opts...)

	conn.client, err = agentcommunication.NewClient(ctx, opts...)
	if err != nil {
		return nil, err
	}

	if err := conn.createStream(ctx); err != nil {
		conn.close(err)
		return nil, err
	}

	return conn, nil
}
