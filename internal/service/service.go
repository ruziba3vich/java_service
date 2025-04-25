package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ruziba3vich/java_service/genprotos/genprotos/compiler_service"
	"github.com/ruziba3vich/java_service/internal/storage"
	logger "github.com/ruziba3vich/prodonik_lgger"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type JavaExecutorServer struct {
	compiler_service.UnimplementedCodeExecutorServer
	clients map[string]*storage.JavaClient
	mu      sync.Mutex
	logger  *logger.Logger
}

func NewJavaExecutorServer(logger *logger.Logger) *JavaExecutorServer {
	return &JavaExecutorServer{
		clients: make(map[string]*storage.JavaClient),
		logger:  logger,
	}
}

func (s *JavaExecutorServer) removeClient(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if client, exists := s.clients[sessionID]; exists {
		s.logger.Info("Removing Java client session", map[string]any{"session_id": sessionID})
		client.Cleanup()
		delete(s.clients, sessionID)
		s.logger.Info("Java client session removed", map[string]any{"session_id": sessionID})
	} else {
		s.logger.Warn("Java client session to remove not found (already removed?)", map[string]any{"session_id": sessionID})
	}
}

func (s *JavaExecutorServer) Execute(stream compiler_service.CodeExecutor_ExecuteServer) error {
	sessionID := ""
	var client *storage.JavaClient
	var clientAddr string

	if p, ok := peer.FromContext(stream.Context()); ok {
		clientAddr = p.Addr.String()
	}
	s.logger.Info("New Java Execute stream started", map[string]any{"client_addr": clientAddr})

	defer func() {
		if sessionID != "" {
			s.logger.Info("Java Execute stream ended. Initiating cleanup for session",
				map[string]any{"session_id": sessionID})
			s.removeClient(sessionID)
		} else {
			s.logger.Info("Java Execute stream ended before session was established",
				map[string]any{"client_addr": clientAddr})
		}
	}()

	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.logger.Info("Stream closed by client (EOF)", map[string]any{"session_id": sessionID, "client_addr": clientAddr})
				return nil
			}
			st, _ := status.FromError(err)
			if st.Code() == codes.Canceled || st.Code() == codes.DeadlineExceeded {
				s.logger.Warn("Stream context cancelled or deadline exceeded",
					map[string]any{"session_id": sessionID, "error": err.Error(), "code": st.Code()})
				return err
			}
			s.logger.Error(fmt.Sprintf("Stream receive error: %v", err),
				map[string]any{"session_id": sessionID, "error": err.Error()})
			return status.Errorf(codes.Internal, "stream receive error: %v", err)
		}

		if client == nil {
			sessionID = req.SessionId
			if sessionID == "" {
				sessionID = uuid.NewString()
				s.logger.Warn("No session_id provided by client, generated new one",
					map[string]any{"client_addr": clientAddr, "generated_session_id": sessionID})
			}
			s.logger.Info("Initial request received, establishing session", map[string]any{"session_id": sessionID, "client_addr": clientAddr})

			s.mu.Lock()

			if existingClient, exists := s.clients[sessionID]; exists {
				if existingClient.CtxDone() {
					s.logger.Warn("Stale session found, cleaning up previous instance before creating new one",
						map[string]any{"session_id": sessionID})
					s.mu.Unlock()
					s.removeClient(sessionID)
					s.mu.Lock()
					if _, exists := s.clients[sessionID]; exists {
						s.mu.Unlock()
						s.logger.Error("Session race condition detected after cleaning stale session", map[string]any{"session_id": sessionID})
						return status.Errorf(codes.Aborted, "session creation race condition for %s", sessionID)
					}
				} else {
					s.mu.Unlock()
					s.logger.Error("Attempted to create an already active session", map[string]any{"session_id": sessionID})
					return status.Errorf(codes.AlreadyExists, "session %s is already active", sessionID)
				}
			}

			clientCtx, clientCancel := context.WithTimeout(context.Background(), 90*time.Second)
			linkedCtx, linkedCancel := context.WithCancel(stream.Context())

			go func(sessID string) {
				<-linkedCtx.Done()
				s.logger.Info(fmt.Sprintf("Stream context done (%v), cancelling client context for session",
					linkedCtx.Err()), map[string]any{"session_id": sessID})
				clientCancel()
				linkedCancel()
			}(sessionID)

			client = storage.NewJavaClient(sessionID, clientCtx, clientCancel, s.logger)
			s.clients[sessionID] = client
			s.logger.Info("New Java client created and stored", map[string]any{"session_id": sessionID})
			s.mu.Unlock()

			go client.SendResponses(stream)

		} else {
			if req.SessionId != sessionID {
				s.logger.Warn(fmt.Sprintf("Received message with mismatched session ID: expected %s, got %s", sessionID, req.SessionId),
					map[string]any{"session_id": sessionID, "received_session_id": req.SessionId})
				client.SendResponse(&compiler_service.ExecuteResponse{
					SessionId: sessionID,
					Payload: &compiler_service.ExecuteResponse_Error{
						Error: &compiler_service.Error{
							ErrorText: fmt.Sprintf("Mismatched session ID: expected %s, got %s", sessionID, req.SessionId),
						},
					},
				})
				continue
			}
		}

		switch payload := req.Payload.(type) {
		case *compiler_service.ExecuteRequest_Code:
			s.logger.Info("Received Code payload for Java execution", map[string]any{"session_id": sessionID})
			if payload.Code.Language != "java" {
				s.logger.Warn(fmt.Sprintf("Unsupported language received: %s, expected 'java'", payload.Code.Language),
					map[string]any{"session_id": sessionID, "language": payload.Code.Language})
				client.SendResponse(&compiler_service.ExecuteResponse{
					SessionId: sessionID,
					Payload: &compiler_service.ExecuteResponse_Error{
						Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Unsupported language '%s'. This service only supports 'java'.", payload.Code.Language)},
					},
				})
				continue
			}
			s.logger.Info("Starting Java execution process", map[string]any{"session_id": sessionID})
			go client.ExecuteJava(payload.Code.SourceCode)

		case *compiler_service.ExecuteRequest_Input:
			s.logger.Info(fmt.Sprintf("Received Input payload: %q", payload.Input.InputText),
				map[string]any{"session_id": sessionID, "input_length": len(payload.Input.InputText)})
			client.HandleInput(payload.Input.InputText)

		default:
			s.logger.Warn(fmt.Sprintf("Received unknown payload type: %T", payload),
				map[string]any{"session_id": sessionID, "payload_type": fmt.Sprintf("%T", payload)})
			client.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: sessionID,
				Payload: &compiler_service.ExecuteResponse_Error{
					Error: &compiler_service.Error{ErrorText: "Unknown or unsupported request payload type"},
				},
			})
		}
	}
}
