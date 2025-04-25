package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ruziba3vich/java_service/genprotos/genprotos/compiler_service"
	"github.com/ruziba3vich/java_service/pkg/config"
	logger "github.com/ruziba3vich/prodonik_lgger"
)

type JavaClient struct {
	sessionID string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	tempFile  string
	done      chan struct{}
	sendChan  chan *compiler_service.ExecuteResponse
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	wg        sync.WaitGroup
	logger    *logger.Logger
	cfg       *config.Config
}

func NewJavaClient(sessionID string, ctx context.Context, cancel context.CancelFunc, cfg *config.Config, logger *logger.Logger) *JavaClient {
	return &JavaClient{
		sessionID: sessionID,
		done:      make(chan struct{}),
		sendChan:  make(chan *compiler_service.ExecuteResponse, 100),
		ctx:       ctx,
		cancel:    cancel,
		logger:    logger,
		cfg:       cfg,
	}
}

func (c *JavaClient) SendResponse(resp *compiler_service.ExecuteResponse) {
	if c.ctx.Err() != nil {
		c.logger.Warn(fmt.Sprintf("Context cancelled, dropping response: %T", resp.Payload),
			map[string]any{"session_id": c.sessionID, "payload_type": fmt.Sprintf("%T", resp.Payload)})
		return
	}
	select {
	case c.sendChan <- resp:
	case <-c.ctx.Done():
		c.logger.Warn(fmt.Sprintf("Context cancelled while sending response: %T", resp.Payload),
			map[string]any{"session_id": c.sessionID, "payload_type": fmt.Sprintf("%T", resp.Payload)})
	default:
		c.logger.Warn("Dropped response: channel full",
			map[string]any{"session_id": c.sessionID, "payload_type": fmt.Sprintf("%T", resp.Payload)})
	}
}

func (c *JavaClient) SendResponses(stream compiler_service.CodeExecutor_ExecuteServer) {
	defer c.logger.Info("SendResponses goroutine finished", map[string]any{"session_id": c.sessionID})
	for {
		select {
		case <-c.ctx.Done():
			c.logger.Info(fmt.Sprintf("Context done in SendResponses: %v", c.ctx.Err()),
				map[string]any{"session_id": c.sessionID})
			for {
				select {
				case _, ok := <-c.sendChan:
					if !ok {
						return
					}
				default:
					return
				}
			}
		case resp, ok := <-c.sendChan:
			if !ok {
				c.logger.Info("Send channel closed", map[string]any{"session_id": c.sessionID})
				return
			}
			if err := stream.Send(resp); err != nil {
				c.logger.Error(fmt.Sprintf("Failed to send response: %v", err),
					map[string]any{"session_id": c.sessionID, "error": err})
				c.cancel()
				return
			}
		}
	}
}

func (c *JavaClient) HandleInput(input string) {
	c.mu.Lock()
	stdin := c.stdin
	c.mu.Unlock()

	if stdin == nil {
		c.logger.Error(fmt.Sprintf("Received input %q but stdin is nil (process not running or already finished?)", input),
			map[string]any{"session_id": c.sessionID, "input": input})
		return
	}

	c.logger.Info(fmt.Sprintf("Writing input to stdin: %q", input),
		map[string]any{"session_id": c.sessionID, "input": input})
	if _, err := fmt.Fprintf(stdin, "%s\n", input); err != nil {
		if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			c.logger.Warn(fmt.Sprintf("Error writing to stdin (pipe closed): %v", err),
				map[string]any{"session_id": c.sessionID, "error": err})
		} else {
			c.logger.Error(fmt.Sprintf("Error writing to stdin: %v", err),
				map[string]any{"session_id": c.sessionID, "error": err})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload: &compiler_service.ExecuteResponse_Error{
					Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Failed to write input: %v", err)},
				},
			})
		}
	}
}

func (c *JavaClient) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Info("Starting cleanup", map[string]any{"session_id": c.sessionID})

	c.cancel()

	if c.stdin != nil {
		c.logger.Info("Closing stdin pipe", map[string]any{"session_id": c.sessionID})
		c.stdin.Close()
		c.stdin = nil
	}

	if c.cmd != nil && c.cmd.Process != nil {
		pid := c.cmd.Process.Pid
		c.logger.Info(fmt.Sprintf("Attempting to kill process %d", pid),
			map[string]any{"session_id": c.sessionID, "pid": pid})
		if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			c.logger.Error(fmt.Sprintf("Failed to kill process %d: %v", pid, err),
				map[string]any{"session_id": c.sessionID, "pid": pid, "error": err})
		} else {
			c.logger.Info(fmt.Sprintf("Process %d killed or already done", pid), map[string]any{"session_id": c.sessionID, "pid": pid})
		}
		c.cmd.Process.Release()
	}
	c.cmd = nil

	if c.tempFile != "" {
		c.logger.Info(fmt.Sprintf("Removing host temp file: %s", c.tempFile),
			map[string]any{"session_id": c.sessionID, "file": c.tempFile})
		if err := os.Remove(c.tempFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.logger.Error(fmt.Sprintf("Failed to remove host temp file %s: %v", c.tempFile, err),
				map[string]any{"session_id": c.sessionID, "file": c.tempFile, "error": err})
		}
		c.tempFile = ""
	}

	c.cleanupContainerFiles()

	select {
	case <-c.done:
	default:
		close(c.done)
	}

	c.logger.Info("Cleanup finished", map[string]any{"session_id": c.sessionID})
}

func (c *JavaClient) cleanupContainerFiles() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	containerSrcPath := filepath.Join(c.cfg.ContainerWorkingDir, c.cfg.JavaSourceFileName)
	containerClassPath := filepath.Join(c.cfg.ContainerWorkingDir, c.cfg.JavaClassName+".class")

	rmCmdStr := fmt.Sprintf("rm -f %s %s", containerSrcPath, containerClassPath)
	cmd := exec.CommandContext(ctx, "docker", "exec", c.cfg.JavaContainerName, "sh", "-c", rmCmdStr)

	c.logger.Info("Attempting to clean up container files", map[string]any{
		"session_id": c.sessionID,
		"command":    strings.Join(cmd.Args, " "),
	})

	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Warn("Failed to clean up container files", map[string]any{
			"session_id": c.sessionID,
			"error":      err.Error(),
			"output":     string(output),
		})
	} else {
		c.logger.Info("Container files cleanup command executed", map[string]any{"session_id": c.sessionID})
	}
}

func (c *JavaClient) ReadOutputs(stdout, stderr io.Reader) {
	defer c.wg.Done()

	outputWg := sync.WaitGroup{}
	outputWg.Add(2)

	go func() {
		defer outputWg.Done()
		buf := make([]byte, 1024)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				outputChunk := string(buf[:n])
				c.logger.Debug(fmt.Sprintf("STDOUT Raw Chunk (%d bytes): %q", n, outputChunk),
					map[string]any{"session_id": c.sessionID, "bytes": n})
				c.SendResponse(&compiler_service.ExecuteResponse{
					SessionId: c.sessionID,
					Payload: &compiler_service.ExecuteResponse_Output{
						Output: &compiler_service.Output{OutputText: outputChunk},
					},
				})

				trimmedChunk := strings.TrimSpace(outputChunk)
				if strings.HasSuffix(trimmedChunk, ":") || strings.HasSuffix(trimmedChunk, "?") || strings.HasSuffix(trimmedChunk, ": ") || strings.HasSuffix(trimmedChunk, "? ") {
					c.logger.Info("Detected possible input prompt, sending WAITING_FOR_INPUT",
						map[string]any{"session_id": c.sessionID, "chunk": trimmedChunk})
					c.SendResponse(&compiler_service.ExecuteResponse{
						SessionId: c.sessionID,
						Payload: &compiler_service.ExecuteResponse_Status{
							Status: &compiler_service.Status{State: "WAITING_FOR_INPUT"},
						},
					})
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
					c.logger.Info("STDOUT closed/EOF", map[string]any{"session_id": c.sessionID})
				} else {
					c.logger.Error(fmt.Sprintf("Error reading stdout: %v", err),
						map[string]any{"session_id": c.sessionID, "error": err})
					c.SendResponse(&compiler_service.ExecuteResponse{
						SessionId: c.sessionID,
						Payload: &compiler_service.ExecuteResponse_Error{
							Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Internal error reading stdout: %v", err)},
						},
					})
				}
				break
			}
		}
		c.logger.Info("STDOUT reader goroutine finished", map[string]any{"session_id": c.sessionID})
	}()

	go func() {
		defer outputWg.Done()
		buf := make([]byte, 1024)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				errorChunk := string(buf[:n])
				c.logger.Debug(fmt.Sprintf("STDERR Raw Chunk (%d bytes): %q", n, errorChunk),
					map[string]any{"session_id": c.sessionID, "bytes": n})
				c.SendResponse(&compiler_service.ExecuteResponse{
					SessionId: c.sessionID,
					Payload: &compiler_service.ExecuteResponse_Error{
						Error: &compiler_service.Error{ErrorText: errorChunk},
					},
				})
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
					c.logger.Info("STDERR closed/EOF", map[string]any{"session_id": c.sessionID})
				} else {
					c.logger.Error(fmt.Sprintf("Error reading stderr: %v", err),
						map[string]any{"session_id": c.sessionID, "error": err})
					c.SendResponse(&compiler_service.ExecuteResponse{
						SessionId: c.sessionID,
						Payload: &compiler_service.ExecuteResponse_Error{
							Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Internal error reading stderr: %v", err)},
						},
					})
				}
				break
			}
		}
		c.logger.Info("STDERR reader goroutine finished", map[string]any{"session_id": c.sessionID})
	}()

	outputWg.Wait()
	c.logger.Info("ReadOutputs completed (both stdout/stderr readers finished)", map[string]any{"session_id": c.sessionID})
}

func (c *JavaClient) ExecuteJava(code string) {
	defer c.Cleanup()
	defer close(c.done)
	defer func() {
		time.Sleep(50 * time.Millisecond)
		close(c.sendChan)
		c.logger.Info("Send channel closed at the end of ExecuteJava", map[string]any{"session_id": c.sessionID})
	}()

	c.logger.Info("Starting ExecuteJava", map[string]any{"session_id": c.sessionID})

	tempFile, err := os.CreateTemp("", fmt.Sprintf("java-%s-*.java", c.sessionID))
	if err != nil {
		c.logger.Error(fmt.Sprintf("Failed to create temp file: %v", err), map[string]any{"session_id": c.sessionID, "error": err})
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Failed to create temp file: %v", err)}},
		})
		return
	}
	tempFilePath := tempFile.Name()
	if _, err := tempFile.WriteString(code); err != nil {
		c.logger.Error(fmt.Sprintf("Failed to write code to temp file %s: %v", tempFilePath, err), map[string]any{"session_id": c.sessionID, "file": tempFilePath, "error": err})
		tempFile.Close()
		os.Remove(tempFilePath)
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Failed to write code to temp file: %v", err)}},
		})
		return
	}
	tempFile.Close()

	c.mu.Lock()
	c.tempFile = tempFilePath
	c.mu.Unlock()
	c.logger.Info(fmt.Sprintf("Code written to host temp file: %s", tempFilePath), map[string]any{"session_id": c.sessionID, "file": tempFilePath})

	containerSrcPath := filepath.Join(c.cfg.ContainerWorkingDir, c.cfg.JavaSourceFileName)
	copyCtx, copyCancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer copyCancel()

	copyCmd := exec.CommandContext(copyCtx, "docker", "cp", tempFilePath, fmt.Sprintf("%s:%s", c.cfg.JavaContainerName, containerSrcPath))
	c.logger.Info(fmt.Sprintf("Copying %s to %s:%s", tempFilePath, c.cfg.JavaContainerName, containerSrcPath),
		map[string]any{"session_id": c.sessionID, "source": tempFilePath, "destination_container": c.cfg.JavaContainerName, "destination_path": containerSrcPath})

	if output, err := copyCmd.CombinedOutput(); err != nil {
		c.logger.Error(fmt.Sprintf("Failed to copy code to container: %v, Output: %s", err, string(output)),
			map[string]any{"session_id": c.sessionID, "error": err, "output": string(output)})
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Failed to copy code into execution environment: %v", err)}},
		})
		return
	}
	c.logger.Info("Successfully copied code to container", map[string]any{"session_id": c.sessionID})

	compileCtx, compileCancel := context.WithTimeout(c.ctx, c.cfg.CompilationTimeout)
	defer compileCancel()

	compileCmd := exec.CommandContext(compileCtx, "docker", "exec", c.cfg.JavaContainerName,
		"javac", "-d", c.cfg.ContainerWorkingDir, containerSrcPath)

	c.logger.Info("Compiling code inside container", map[string]any{
		"session_id": c.sessionID,
		"command":    strings.Join(compileCmd.Args, " "),
	})
	c.SendResponse(&compiler_service.ExecuteResponse{
		SessionId: c.sessionID,
		Payload:   &compiler_service.ExecuteResponse_Status{Status: &compiler_service.Status{State: "COMPILING"}},
	})

	compileOutputBytes, compileErr := compileCmd.CombinedOutput()
	compileOutput := string(compileOutputBytes)

	if compileErr != nil {
		if errors.Is(compileErr, context.DeadlineExceeded) {
			c.logger.Error("Compilation timed out", map[string]any{"session_id": c.sessionID, "timeout": c.cfg.CompilationTimeout})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Compilation timed out after %s", c.cfg.CompilationTimeout)}},
			})
		} else if errors.Is(compileErr, context.Canceled) {
			c.logger.Info("Compilation cancelled by context", map[string]any{"session_id": c.sessionID})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload:   &compiler_service.ExecuteResponse_Status{Status: &compiler_service.Status{State: "CANCELLED"}},
			})
		} else {
			c.logger.Error(fmt.Sprintf("Compilation failed: %v, Output: %s", compileErr, compileOutput),
				map[string]any{"session_id": c.sessionID, "error": compileErr, "output": compileOutput})
			errorMessage := fmt.Sprintf("Compilation failed:\n%s", compileOutput)
			if exitErr, ok := compileErr.(*exec.ExitError); ok {
				errorMessage = fmt.Sprintf("Compilation failed (exit code %d):\n%s", exitErr.ExitCode(), compileOutput)
			}
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: errorMessage}},
			})
		}
		return
	}

	if len(compileOutput) > 0 {
		c.logger.Info(fmt.Sprintf("Compilation successful with output: %s", compileOutput),
			map[string]any{"session_id": c.sessionID, "output": compileOutput})
	} else {
		c.logger.Info("Compilation successful (no output)", map[string]any{"session_id": c.sessionID})
	}

	execCtx, execCancel := context.WithTimeout(c.ctx, c.cfg.ExecutionTimeout)
	defer execCancel()

	runCmd := exec.CommandContext(execCtx, "docker", "exec", "-i",
		c.cfg.JavaContainerName, "java", "-cp", c.cfg.ContainerWorkingDir, c.cfg.JavaClassName)

	c.logger.Info("Preparing execution command", map[string]any{
		"session_id": c.sessionID,
		"command":    strings.Join(runCmd.Args, " "),
	})

	stdinPipe, err := runCmd.StdinPipe()
	if err != nil {
		c.logger.Error(fmt.Sprintf("Failed to create stdin pipe: %v", err), map[string]any{"session_id": c.sessionID, "error": err})
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Internal error: Failed to create stdin pipe: %v", err)}},
		})
		return
	}

	stdoutPipe, err := runCmd.StdoutPipe()
	if err != nil {
		c.logger.Error(fmt.Sprintf("Failed to create stdout pipe: %v", err), map[string]any{"session_id": c.sessionID, "error": err})
		stdinPipe.Close()
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Internal error: Failed to create stdout pipe: %v", err)}},
		})
		return
	}

	stderrPipe, err := runCmd.StderrPipe()
	if err != nil {
		c.logger.Error(fmt.Sprintf("Failed to create stderr pipe: %v", err), map[string]any{"session_id": c.sessionID, "error": err})
		stdinPipe.Close()
		stdoutPipe.Close()
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Internal error: Failed to create stderr pipe: %v", err)}},
		})
		return
	}

	c.mu.Lock()
	c.cmd = runCmd
	c.stdin = stdinPipe
	c.mu.Unlock()

	c.wg.Add(1)
	go c.ReadOutputs(stdoutPipe, stderrPipe)

	c.logger.Info("Starting Java execution", map[string]any{"session_id": c.sessionID})
	c.SendResponse(&compiler_service.ExecuteResponse{
		SessionId: c.sessionID,
		Payload:   &compiler_service.ExecuteResponse_Status{Status: &compiler_service.Status{State: "RUNNING"}},
	})

	if err := runCmd.Start(); err != nil {
		c.logger.Error(fmt.Sprintf("Failed to start Java execution command: %v", err), map[string]any{"session_id": c.sessionID, "error": err})
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Failed to start execution: %v", err)}},
		})
		c.mu.Lock()
		if c.stdin != nil {
			c.stdin.Close()
			c.stdin = nil
		}
		c.mu.Unlock()
		stdoutPipe.Close()
		stderrPipe.Close()
		c.wg.Wait()
		return
	}

	pid := runCmd.Process.Pid
	c.logger.Info(fmt.Sprintf("Java execution started with PID %d", pid), map[string]any{"session_id": c.sessionID, "pid": pid})

	c.logger.Info("Waiting for Java execution to complete", map[string]any{"session_id": c.sessionID})
	waitErr := runCmd.Wait()

	c.logger.Info(fmt.Sprintf("Java execution command finished (Wait err: %v). Waiting for output processing.", waitErr),
		map[string]any{"session_id": c.sessionID, "error": waitErr})

	c.mu.Lock()
	if c.stdin != nil {
		c.stdin.Close()
		c.stdin = nil
	}
	c.mu.Unlock()

	c.wg.Wait()
	c.logger.Info("Output processing finished", map[string]any{"session_id": c.sessionID})

	if waitErr != nil {
		if errors.Is(waitErr, context.Canceled) {
			c.logger.Info("Execution cancelled by context", map[string]any{"session_id": c.sessionID, "error": waitErr})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload:   &compiler_service.ExecuteResponse_Status{Status: &compiler_service.Status{State: "CANCELLED"}},
			})
		} else if errors.Is(waitErr, context.DeadlineExceeded) {
			c.logger.Error("Execution timed out", map[string]any{"session_id": c.sessionID, "timeout": c.cfg.ExecutionTimeout, "error": waitErr})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Execution timed out after %s", c.cfg.ExecutionTimeout)}},
			})
			c.mu.Lock()
			if c.cmd != nil && c.cmd.Process != nil {
				c.cmd.Process.Kill()
			}
			c.mu.Unlock()
		} else if exitErr, ok := waitErr.(*exec.ExitError); ok {
			c.logger.Error(fmt.Sprintf("Execution failed with exit code %d: %v", exitErr.ExitCode(), waitErr),
				map[string]any{"session_id": c.sessionID, "exit_code": exitErr.ExitCode(), "error": waitErr})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload: &compiler_service.ExecuteResponse_Status{
					Status: &compiler_service.Status{State: fmt.Sprintf("RUNTIME_ERROR (Exit Code %d)", exitErr.ExitCode())},
				},
			})
		} else {
			c.logger.Error(fmt.Sprintf("Execution wait error: %v", waitErr), map[string]any{"session_id": c.sessionID, "error": waitErr})
			c.SendResponse(&compiler_service.ExecuteResponse{
				SessionId: c.sessionID,
				Payload:   &compiler_service.ExecuteResponse_Error{Error: &compiler_service.Error{ErrorText: fmt.Sprintf("Execution error: %v", waitErr)}},
			})
		}
	} else {
		c.logger.Info("Execution completed successfully", map[string]any{"session_id": c.sessionID})
		c.SendResponse(&compiler_service.ExecuteResponse{
			SessionId: c.sessionID,
			Payload:   &compiler_service.ExecuteResponse_Status{Status: &compiler_service.Status{State: "EXECUTION_COMPLETE"}},
		})
	}

	c.logger.Info("ExecuteJava finished", map[string]any{"session_id": c.sessionID})
}

func (c *JavaClient) CtxDone() bool {
	select {
	case <-c.ctx.Done():
		return true
	default:
		return false
	}
}
