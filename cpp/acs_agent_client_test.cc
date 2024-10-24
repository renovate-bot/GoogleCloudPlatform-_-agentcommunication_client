#include "third_party/agentcommunication_client/cpp/acs_agent_client.h"

#include <chrono>
#include <memory>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include "google/cloud/agentcommunication/v1/agent_communication.grpc.pb.h"
#include "testing/base/public/gmock.h"
#include "testing/base/public/gunit.h"
#include "third_party/absl/base/thread_annotations.h"
#include "third_party/absl/functional/any_invocable.h"
#include "third_party/absl/log/absl_log.h"
#include "third_party/absl/log/globals.h"
#include "third_party/absl/status/status.h"
#include "third_party/absl/status/statusor.h"
#include "third_party/absl/strings/str_cat.h"
#include "third_party/absl/synchronization/mutex.h"
#include "third_party/absl/time/clock.h"
#include "third_party/absl/time/time.h"
#include "third_party/agentcommunication_client/cpp/acs_agent_helper.h"
#include "third_party/agentcommunication_client/cpp/fake_acs_agent_server_reactor.h"
#include "third_party/grpc/include/grpc/grpc.h"
#include "third_party/grpc/include/grpcpp/channel.h"
#include "third_party/grpc/include/grpcpp/create_channel.h"
#include "third_party/grpc/include/grpcpp/security/credentials.h"
#include "third_party/grpc/include/grpcpp/support/channel_arguments.h"
#include "third_party/grpc/include/grpcpp/support/status.h"

namespace agent_communication {
namespace {

// Alias of the stub type used in the ACS Agent Communication service in a .cc
// file.
using AcsStub =
    ::google::cloud::agentcommunication::v1::AgentCommunication::Stub;
using Response =
    ::google::cloud::agentcommunication::v1::StreamAgentMessagesResponse;
using Request =
    ::google::cloud::agentcommunication::v1::StreamAgentMessagesRequest;
using MessageBody = ::google::cloud::agentcommunication::v1::MessageBody;
using agent_communication::AgentConnectionId;

// Custom client channel to hold the response from server.
struct CustomClientChannel {
  absl::Mutex mtx;
  std::vector<Response> responses ABSL_GUARDED_BY(mtx);
};

// Custom server channel to hold the request from client. Also used to control
// the server behavior on whether to delay the response and for how long.
struct CustomServerChannel {
  absl::Mutex mtx;
  std::vector<Request> requests ABSL_GUARDED_BY(mtx);
  bool delay_response ABSL_GUARDED_BY(mtx) = false;
  absl::Duration delay_duration ABSL_GUARDED_BY(mtx) = absl::Seconds(3);
};

// Waits for the condition to be true by polling it every sleep_duration till
// timeout.
bool WaitUntil(absl::AnyInvocable<bool()> condition, absl::Duration timeout,
               absl::Duration sleep_duration) {
  absl::Time deadline = absl::Now() + timeout;
  while (absl::Now() < deadline) {
    if (condition()) {
      return true;
    }
    absl::SleepFor(sleep_duration);
  }
  return false;
}

// Test fixture for AcsAgentClient.
// It sets up a fake ACS Agent server and create a client to connect to server.
class AcsAgentClientTest : public ::testing::Test {
 protected:
  AcsAgentClientTest()
      : service_([this](Request request) {
          // Callback to be invoked in OnReadDone() of the server reactor.
          absl::MutexLock lock(&custom_server_channel_.mtx);
          custom_server_channel_.requests.push_back(std::move(request));
          if (custom_server_channel_.delay_response) {
            absl::SleepFor(custom_server_channel_.delay_duration);
          }
        }),
        server_(&service_) {
    absl::SetGlobalVLogLevel(0);
  }

  void SetUp() override {
    grpc::ChannelArguments channel_args;
    // Keepalive settings
    channel_args.SetInt(GRPC_ARG_KEEPALIVE_TIME_MS, 600 * 1000);  // 600 seconds
    channel_args.SetInt(GRPC_ARG_KEEPALIVE_TIMEOUT_MS,
                        100 * 1000);  // 100 seconds
    std::shared_ptr<::grpc::Channel> channel = grpc::CreateCustomChannel(
        server_.GetServerAddress(), grpc::InsecureChannelCredentials(),
        channel_args);
    std::chrono::system_clock::time_point deadline =
        std::chrono::system_clock::now() + std::chrono::seconds(10);
    ASSERT_TRUE(channel->WaitForConnected(deadline));
    stub_ = google::cloud::agentcommunication::v1::AgentCommunication::NewStub(
        channel);

    // Make sure server does not delay response.
    SetServerDelay(false, absl::ZeroDuration());

    // Create the client. Upon receipt of the Response from server, the client
    // will write the response to custom_client_channel_.
    client_ = AcsAgentClient::Create(
        std::move(stub_), AgentConnectionId(), [this](Response response) {
          absl::MutexLock lock(&custom_client_channel_.mtx);
          custom_client_channel_.responses.push_back(std::move(response));
          ABSL_VLOG(2) << "response read: "
                       << absl::StrCat(custom_client_channel_.responses.back());
        });
    ASSERT_OK(client_);

    // Wait for the registration request to be acknowledged by the server, and
    // then clear the responses.
    ASSERT_TRUE(WaitUntil(
        [this]() {
          absl::MutexLock lock(&custom_client_channel_.mtx);
          return custom_client_channel_.responses.size() == 1;
        },
        absl::Seconds(10), absl::Seconds(1)));
    {
      absl::MutexLock lock(&custom_client_channel_.mtx);
      custom_client_channel_.responses.clear();
    }

    // Wait for the registration request to be received by the server, and then
    // clear the requests.
    ASSERT_TRUE(WaitUntil(
        [this]() {
          absl::MutexLock lock(&custom_server_channel_.mtx);
          return custom_server_channel_.requests.size() == 1;
        },
        absl::Seconds(10), absl::Seconds(1)));
    {
      absl::MutexLock lock(&custom_server_channel_.mtx);
      custom_server_channel_.requests.clear();
    }
  }

  void TearDown() override {
    ABSL_VLOG(2) << "Shutting down fake server during teardown of tests.";
    std::thread wait_for_reactor_termination_([this]() {
      grpc::Status status = (*client_)->AwaitReactor();
      ABSL_VLOG(1) << "reactor terminate status is: " << status.error_code();
    });
    std::chrono::system_clock::time_point deadline =
        std::chrono::system_clock::now() + std::chrono::seconds(2);
    server_.GetServer()->Shutdown(deadline);
    server_.GetServer()->Wait();
    wait_for_reactor_termination_.join();
  }

  // Sets the server whether to delay the response for the given duration.
  void SetServerDelay(bool delay_response, absl::Duration delay_duration) {
    absl::MutexLock lock(&custom_server_channel_.mtx);
    custom_server_channel_.delay_response = delay_response;
    custom_server_channel_.delay_duration = delay_duration;
  }

  std::thread read_message_thread_;
  std::unique_ptr<AcsStub> stub_;
  FakeAcsAgentServiceImpl service_;
  FakeAcsAgentServer server_;
  CustomClientChannel custom_client_channel_;
  CustomServerChannel custom_server_channel_;
  absl::StatusOr<std::unique_ptr<AcsAgentClient>> client_;
};

TEST_F(AcsAgentClientTest, TestClientSendMessagesRepeatedlySuccessful) {
  // Make sure server does not delay response.
  SetServerDelay(false, absl::ZeroDuration());

  // Send 50 messages to the server, expect an OK status.
  for (int i = 0; i < 50; ++i) {
    MessageBody message_body;
    message_body.mutable_body()->set_value(absl::StrCat("message_", i));
    ASSERT_OK((*client_)->SendMessage(std::move(message_body)));
  }

  // Wait for the response to be read by the client. It should happen instantly.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_client_channel_.mtx);
        return custom_client_channel_.responses.size() == 50;
      },
      absl::Seconds(10), absl::Seconds(1)));
  // Wait for the acks to be read by the server. It should happen instantly.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_server_channel_.mtx);
        return custom_server_channel_.requests.size() == 50;
      },
      absl::Seconds(10), absl::Seconds(1)));

  // Verify that all acks are received by the client. They should be
  // delivered in order and have the same message id as the requests received by
  // the server. And verify all requests are received by the server as well.
  {
    absl::MutexLock lock1(&custom_client_channel_.mtx);
    absl::MutexLock lock2(&custom_server_channel_.mtx);
    for (int i = 0; i < 50; ++i) {
      EXPECT_TRUE(custom_client_channel_.responses[i].has_message_response());
      EXPECT_EQ(custom_client_channel_.responses[i]
                    .message_response()
                    .status()
                    .code(),
                0);
      EXPECT_EQ(custom_client_channel_.responses[i].message_id(),
                custom_server_channel_.requests[i].message_id());
      EXPECT_EQ(
          custom_server_channel_.requests[i].message_body().body().value(),
          absl::StrCat("message_", i));
    }
    custom_client_channel_.responses.clear();
    custom_server_channel_.requests.clear();
  }
}

TEST_F(AcsAgentClientTest, TestSendMessageTimeout) {
  // Make sure server does delay response.
  SetServerDelay(true, absl::Seconds(3));

  // Send a message to the server, expect timeout status.
  MessageBody message_body;
  message_body.mutable_body()->set_value("hello_world");
  absl::Status send_message_status = (*client_)->SendMessage(message_body);
  ASSERT_EQ(send_message_status.code(), absl::StatusCode::kDeadlineExceeded);

  // Wait for the responses to be read by the client.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_client_channel_.mtx);
        return custom_client_channel_.responses.size() == 5;
      },
      absl::Seconds(10), absl::Seconds(1)));

  // Verify that client receive 5 responses from the server and verify that
  // server receives 5 requests from the client with the same content because
  // the client will retry to send the request 5 times.
  {
    absl::MutexLock lock1(&custom_client_channel_.mtx);
    absl::MutexLock lock2(&custom_server_channel_.mtx);
    ASSERT_EQ(custom_server_channel_.requests.size(), 5);
    for (int i = 0; i < 5; ++i) {
      EXPECT_EQ(custom_client_channel_.responses[i].message_id(),
                custom_server_channel_.requests[i].message_id());
      EXPECT_EQ(custom_client_channel_.responses[i]
                    .message_response()
                    .status()
                    .code(),
                0);
      EXPECT_EQ(
          custom_server_channel_.requests[i].message_body().body().value(),
          "hello_world");
    }
    custom_client_channel_.responses.clear();
    custom_server_channel_.requests.clear();
  }
}

TEST_F(AcsAgentClientTest, TestClientReadMessagesRepeatedlySuccessful) {
  // Make sure server does not delay response.
  SetServerDelay(false, absl::ZeroDuration());

  // Server sends 50 messages to the client.
  std::vector<std::string> message_ids(50);
  for (int i = 0; i < 50; ++i) {
    // Create a response with message body and write it to client by server.
    auto response = std::make_unique<Response>();
    response->set_message_id(absl::StrCat(absl::ToUnixMicros(absl::Now())));
    message_ids[i] = response->message_id();
    response->mutable_message_body()->mutable_body()->set_value(
        absl::StrCat("message_", i));
    auto response_copy = std::make_unique<Response>(*response);
    service_.AddResponse(std::move(response));
  }

  // Wait for the response to be read by the client. It should happen instantly.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_client_channel_.mtx);
        return custom_client_channel_.responses.size() == 50;
      },
      absl::Seconds(10), absl::Seconds(1)));
  // Wait for the acks to be read by the server. It should happen instantly.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_server_channel_.mtx);
        return custom_server_channel_.requests.size() == 50;
      },
      absl::Seconds(10), absl::Seconds(1)));

  // Verify that client receives all 50 messages with right content and server
  // has received 50 acks with right message ids.
  {
    absl::MutexLock lock1(&custom_client_channel_.mtx);
    absl::MutexLock lock2(&custom_server_channel_.mtx);
    for (int i = 0; i < 50; ++i) {
      EXPECT_EQ(custom_client_channel_.responses[i].message_id(),
                message_ids[i]);
      EXPECT_EQ(
          custom_client_channel_.responses[i].message_body().body().value(),
          absl::StrCat("message_", i));
      EXPECT_TRUE(custom_server_channel_.requests[i].has_message_response());
      EXPECT_EQ(
          custom_server_channel_.requests[i].message_response().status().code(),
          0);
      EXPECT_EQ(custom_server_channel_.requests[i].message_id(),
                message_ids[i]);
    }
    custom_client_channel_.responses.clear();
    custom_server_channel_.requests.clear();
  }
}

TEST_F(AcsAgentClientTest, TestReadSuccessfullyAfterWritingRepeatedly) {
  // Make sure server does not delay response.
  SetServerDelay(false, absl::ZeroDuration());

  // Client and Server send a message to each other, in this order, repeat 50
  // times.
  std::vector<std::string> message_ids_sent_by_server(50);
  for (int i = 0; i < 50; ++i) {
    // Client sends a request to the server, expect an OK status.
    Request request;
    request.set_message_id(absl::StrCat(absl::ToUnixMicros(absl::Now())));
    request.mutable_message_body()->mutable_body()->set_value(
        absl::StrCat("hello_world_", i));
    ASSERT_OK((*client_)->AddRequest(request));

    // Server sends a response to the client.
    auto response = std::make_unique<Response>();
    response->set_message_id(absl::StrCat(absl::ToUnixMicros(absl::Now())));
    message_ids_sent_by_server[i] = response->message_id();
    response->mutable_message_body()->mutable_body()->set_value(
        absl::StrCat("message_", i));
    auto response_copy = std::make_unique<Response>(*response);
    service_.AddResponse(std::move(response));
  }

  // Wait for the response to be read by the client.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_client_channel_.mtx);
        return custom_client_channel_.responses.size() == 100;
      },
      absl::Seconds(10), absl::Seconds(1)));
  // Wait for the request to be read by the server.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_server_channel_.mtx);
        return custom_server_channel_.requests.size() == 100;
      },
      absl::Seconds(10), absl::Seconds(1)));

  // Verify that server has received 50 acks and 50 message bodies. The
  // message id of acks should match message_ids of responses sent by the
  // server. They may not be right next to each other, but they should be in
  // order. Also record the message ids sent by the client.
  std::vector<std::string> message_ids_sent_by_client(50);
  {
    absl::MutexLock lock(&custom_server_channel_.mtx);
    int ack_count = 0;
    int message_body_count = 0;
    for (const Request& request : custom_server_channel_.requests) {
      if (request.has_message_response()) {
        EXPECT_EQ(request.message_response().status().code(), 0);
        EXPECT_EQ(request.message_id(),
                  message_ids_sent_by_server[ack_count++]);
      }
      if (request.has_message_body()) {
        message_ids_sent_by_client[message_body_count] = request.message_id();
        EXPECT_EQ(request.message_body().body().value(),
                  absl::StrCat("hello_world_", message_body_count));
        message_body_count++;
      }
    }
    EXPECT_EQ(ack_count, 50);
    EXPECT_EQ(message_body_count, 50);
    custom_server_channel_.requests.clear();
  }

  // Verify that client has received 50 acks and 50 message bodies in the right
  // order.
  {
    absl::MutexLock lock(&custom_client_channel_.mtx);
    int ack_count = 0;
    int message_body_count = 0;
    for (const Response& response : custom_client_channel_.responses) {
      if (response.has_message_response()) {
        EXPECT_EQ(response.message_response().status().code(), 0);
        EXPECT_EQ(response.message_id(),
                  message_ids_sent_by_client[ack_count++]);
      }
      if (response.has_message_body()) {
        EXPECT_EQ(response.message_id(),
                  message_ids_sent_by_server[message_body_count]);
        EXPECT_EQ(response.message_body().body().value(),
                  absl::StrCat("message_", message_body_count));
        message_body_count++;
      }
    }
    EXPECT_EQ(ack_count, 50);
    EXPECT_EQ(message_body_count, 50);
    custom_client_channel_.responses.clear();
  }
}

TEST_F(AcsAgentClientTest, TestWriteSuccessfullyAfterReadingRepeatedly) {
  // Make sure server does not delay response.
  SetServerDelay(false, absl::ZeroDuration());

  // Server and Client send a message to each other, in this order, repeat 50
  // times.
  std::vector<std::string> message_ids_sent_by_server(50);
  for (int i = 0; i < 50; ++i) {
    // Server sends a response to the client.
    auto response = std::make_unique<Response>();
    response->set_message_id(absl::StrCat(absl::ToUnixMicros(absl::Now())));
    message_ids_sent_by_server[i] = response->message_id();
    response->mutable_message_body()->mutable_body()->set_value(
        absl::StrCat("message_", i));
    auto response_copy = std::make_unique<Response>(*response);
    service_.AddResponse(std::move(response));

    // Client sends a request to the server, expect an OK status.
    Request request;
    request.set_message_id(absl::StrCat(absl::ToUnixMicros(absl::Now())));
    request.mutable_message_body()->mutable_body()->set_value(
        absl::StrCat("hello_world_", i));
    ASSERT_OK((*client_)->AddRequest(request));
  }

  // Wait for the responses to be read by the client.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_client_channel_.mtx);
        return custom_client_channel_.responses.size() == 100;
      },
      absl::Seconds(20), absl::Seconds(1)));
  // Wait for the requests to be read by the server.
  ASSERT_TRUE(WaitUntil(
      [this]() {
        absl::MutexLock lock(&custom_server_channel_.mtx);
        return custom_server_channel_.requests.size() == 100;
      },
      absl::Seconds(20), absl::Seconds(1)));

  // Verify that server has received 50 acks and 50 message bodies.
  std::vector<std::string> message_ids_sent_by_client(50);
  {
    absl::MutexLock lock(&custom_server_channel_.mtx);
    int ack_count = 0;
    int message_body_count = 0;
    for (const Request& request : custom_server_channel_.requests) {
      if (request.has_message_response()) {
        EXPECT_EQ(request.message_response().status().code(), 0);
        EXPECT_EQ(request.message_id(),
                  message_ids_sent_by_server[ack_count++]);
      }
      if (request.has_message_body()) {
        message_ids_sent_by_client[message_body_count] = request.message_id();
        EXPECT_EQ(request.message_body().body().value(),
                  absl::StrCat("hello_world_", message_body_count));
        message_body_count++;
      }
    }
    EXPECT_EQ(ack_count, 50);
    EXPECT_EQ(message_body_count, 50);
    custom_server_channel_.requests.clear();
  }

  // Verify that client has received 50 acks and 50 message bodies.
  {
    absl::MutexLock lock(&custom_client_channel_.mtx);
    int ack_count = 0;
    int message_body_count = 0;
    for (const Response& response : custom_client_channel_.responses) {
      if (response.has_message_response()) {
        EXPECT_EQ(response.message_response().status().code(), 0);
        EXPECT_EQ(response.message_id(),
                  message_ids_sent_by_client[ack_count++]);
      }
      if (response.has_message_body()) {
        EXPECT_EQ(response.message_body().body().value(),
                  absl::StrCat("message_", message_body_count));
        EXPECT_EQ(response.message_id(),
                  message_ids_sent_by_server[message_body_count]);
        message_body_count++;
      }
    }
  }
}

}  // namespace
}  // namespace agent_communication
