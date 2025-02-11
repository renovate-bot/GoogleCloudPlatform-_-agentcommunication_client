// Copyright 2025 Google LLC
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

// Code generated by protoc-gen-go_gapic. DO NOT EDIT.

package agentcommunication

import (
	"context"
	"log/slog"
	"math"

	agentcommunicationpb "github.com/GoogleCloudPlatform/agentcommunication_client/gapic/agentcommunicationpb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/option/internaloption"
	"google.golang.org/api/option"
	gtransport "google.golang.org/api/transport/grpc"
	"google.golang.org/grpc"
)

var newClientHook clientHook

// CallOptions contains the retry settings for each method of Client.
type CallOptions struct {
	StreamAgentMessages []gax.CallOption
	SendAgentMessage    []gax.CallOption
}

func defaultGRPCClientOptions() []option.ClientOption {
	return []option.ClientOption{
		internaloption.WithDefaultEndpoint("agentcommunication.googleapis.com:443"),
		internaloption.WithDefaultEndpointTemplate("agentcommunication.UNIVERSE_DOMAIN:443"),
		internaloption.WithDefaultMTLSEndpoint("agentcommunication.mtls.googleapis.com:443"),
		internaloption.WithDefaultUniverseDomain("googleapis.com"),
		internaloption.WithDefaultAudience("https://agentcommunication.googleapis.com/"),
		internaloption.WithDefaultScopes(DefaultAuthScopes()...),
		internaloption.EnableJwtWithScope(),
		internaloption.EnableNewAuthLibrary(),
		option.WithGRPCDialOption(grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(math.MaxInt32))),
	}
}

func defaultCallOptions() *CallOptions {
	return &CallOptions{
		StreamAgentMessages: []gax.CallOption{},
		SendAgentMessage:    []gax.CallOption{},
	}
}

// internalClient is an interface that defines the methods available from Agent Communication API.
type internalClient interface {
	Close() error
	setGoogleClientInfo(...string)
	Connection() *grpc.ClientConn
	StreamAgentMessages(context.Context, ...gax.CallOption) (agentcommunicationpb.AgentCommunication_StreamAgentMessagesClient, error)
	SendAgentMessage(context.Context, *agentcommunicationpb.SendAgentMessageRequest, ...gax.CallOption) (*agentcommunicationpb.SendAgentMessageResponse, error)
}

// Client is a client for interacting with Agent Communication API.
// Methods, except Close, may be called concurrently. However, fields must not be modified concurrently with method calls.
//
// AgentCommunication service allowing agents to send and receiving messages.
type Client struct {
	// The internal transport-dependent client.
	internalClient internalClient

	// The call options for this service.
	CallOptions *CallOptions
}

// Wrapper methods routed to the internal client.

// Close closes the connection to the API service. The user should invoke this when
// the client is no longer required.
func (c *Client) Close() error {
	return c.internalClient.Close()
}

// setGoogleClientInfo sets the name and version of the application in
// the `x-goog-api-client` header passed on each request. Intended for
// use by Google-written clients.
func (c *Client) setGoogleClientInfo(keyval ...string) {
	c.internalClient.setGoogleClientInfo(keyval...)
}

// Connection returns a connection to the API service.
//
// Deprecated: Connections are now pooled so this method does not always
// return the same resource.
func (c *Client) Connection() *grpc.ClientConn {
	return c.internalClient.Connection()
}

// StreamAgentMessages bi-di streaming between the server and resource on a communication channel.
func (c *Client) StreamAgentMessages(ctx context.Context, opts ...gax.CallOption) (agentcommunicationpb.AgentCommunication_StreamAgentMessagesClient, error) {
	return c.internalClient.StreamAgentMessages(ctx, opts...)
}

// SendAgentMessage send a message to a client. This is equivalent to sending a message via
// StreamAgentMessages with a single message and waiting for the response.
// Channel ID and Resource ID are required to be sent in the header.
func (c *Client) SendAgentMessage(ctx context.Context, req *agentcommunicationpb.SendAgentMessageRequest, opts ...gax.CallOption) (*agentcommunicationpb.SendAgentMessageResponse, error) {
	return c.internalClient.SendAgentMessage(ctx, req, opts...)
}

// gRPCClient is a client for interacting with Agent Communication API over gRPC transport.
//
// Methods, except Close, may be called concurrently. However, fields must not be modified concurrently with method calls.
type gRPCClient struct {
	// Connection pool of gRPC connections to the service.
	connPool gtransport.ConnPool

	// Points back to the CallOptions field of the containing Client
	CallOptions **CallOptions

	// The gRPC API client.
	client agentcommunicationpb.AgentCommunicationClient

	// The x-goog-* metadata to be sent with each request.
	xGoogHeaders []string

	logger *slog.Logger
}

// NewClient creates a new agent communication client based on gRPC.
// The returned client must be Closed when it is done being used to clean up its underlying connections.
//
// AgentCommunication service allowing agents to send and receiving messages.
func NewClient(ctx context.Context, opts ...option.ClientOption) (*Client, error) {
	clientOpts := defaultGRPCClientOptions()
	if newClientHook != nil {
		hookOpts, err := newClientHook(ctx, clientHookParams{})
		if err != nil {
			return nil, err
		}
		clientOpts = append(clientOpts, hookOpts...)
	}

	connPool, err := gtransport.DialPool(ctx, append(clientOpts, opts...)...)
	if err != nil {
		return nil, err
	}
	client := Client{CallOptions: defaultCallOptions()}

	c := &gRPCClient{
		connPool:    connPool,
		client:      agentcommunicationpb.NewAgentCommunicationClient(connPool),
		CallOptions: &client.CallOptions,
		logger:      internaloption.GetLogger(opts),
	}
	c.setGoogleClientInfo()

	client.internalClient = c

	return &client, nil
}

// Connection returns a connection to the API service.
//
// Deprecated: Connections are now pooled so this method does not always
// return the same resource.
func (c *gRPCClient) Connection() *grpc.ClientConn {
	return c.connPool.Conn()
}

// setGoogleClientInfo sets the name and version of the application in
// the `x-goog-api-client` header passed on each request. Intended for
// use by Google-written clients.
func (c *gRPCClient) setGoogleClientInfo(keyval ...string) {
	kv := append([]string{"gl-go", gax.GoVersion}, keyval...)
	kv = append(kv, "gapic", getVersionClient(), "gax", gax.Version, "grpc", grpc.Version)
	c.xGoogHeaders = []string{
		"x-goog-api-client", gax.XGoogHeader(kv...),
	}
}

// Close closes the connection to the API service. The user should invoke this when
// the client is no longer required.
func (c *gRPCClient) Close() error {
	return c.connPool.Close()
}

func (c *gRPCClient) StreamAgentMessages(ctx context.Context, opts ...gax.CallOption) (agentcommunicationpb.AgentCommunication_StreamAgentMessagesClient, error) {
	ctx = gax.InsertMetadataIntoOutgoingContext(ctx, c.xGoogHeaders...)
	var resp agentcommunicationpb.AgentCommunication_StreamAgentMessagesClient
	opts = append((*c.CallOptions).StreamAgentMessages[0:len((*c.CallOptions).StreamAgentMessages):len((*c.CallOptions).StreamAgentMessages)], opts...)
	err := gax.Invoke(ctx, func(ctx context.Context, settings gax.CallSettings) error {
		var err error
		c.logger.DebugContext(ctx, "api streaming client request", "serviceName", serviceName, "rpcName", "StreamAgentMessages")
		resp, err = c.client.StreamAgentMessages(ctx, settings.GRPC...)
		c.logger.DebugContext(ctx, "api streaming client response", "serviceName", serviceName, "rpcName", "StreamAgentMessages")
		return err
	}, opts...)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *gRPCClient) SendAgentMessage(ctx context.Context, req *agentcommunicationpb.SendAgentMessageRequest, opts ...gax.CallOption) (*agentcommunicationpb.SendAgentMessageResponse, error) {
	ctx = gax.InsertMetadataIntoOutgoingContext(ctx, c.xGoogHeaders...)
	opts = append((*c.CallOptions).SendAgentMessage[0:len((*c.CallOptions).SendAgentMessage):len((*c.CallOptions).SendAgentMessage)], opts...)
	var resp *agentcommunicationpb.SendAgentMessageResponse
	err := gax.Invoke(ctx, func(ctx context.Context, settings gax.CallSettings) error {
		var err error
		resp, err = executeRPC(ctx, c.client.SendAgentMessage, req, settings.GRPC, c.logger, "SendAgentMessage")
		return err
	}, opts...)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
