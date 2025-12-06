package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	defaultPort = "8080"
	geminiCLI   = "gemini" // Assuming 'gemini' is the executable name
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	http.HandleFunc("/v1/chat/completions", handleChatCompletions)
	http.HandleFunc("/v1/models", handleModels) // Dummy models endpoint

	log.Printf("Server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req OpenAICompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("Incoming request. Requested Model: %s", req.Model)

	// Determine if streaming is requested
	isStream := req.Stream

	// Extract system prompt and format messages
	systemPrompt, geminiInput := parseMessages(req.Messages)

	// Create a temporary file for the system prompt
	sysPromptFile, err := os.CreateTemp("", "gemini-sys-prompt-*.md")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create temp system prompt file: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.Remove(sysPromptFile.Name()) // Clean up temp file

	// Write system prompt to file
	if _, err := sysPromptFile.WriteString(systemPrompt); err != nil {
		http.Error(w, fmt.Sprintf("Failed to write to temp system prompt file: %v", err), http.StatusInternalServerError)
		return
	}
	sysPromptFile.Close()

	// Build the gemini-cli command
	args := []string{}
	if isStream {
		args = append(args, "--output-format", "stream-json")
	} else {
		args = append(args, "--output-format", "json")
	}

	// Use specified model, or default if not provided/recognized
	model := req.Model
	switch model {
	case "gemini-2.5-flash":
		// Explicitly supported
	case "gemini-2.5-pro":
		// Explicitly supported
	case "gemini-3-pro-preview":
		// Explicitly supported
	// If the model is empty, or an OpenAI model name, or any other unsupported model
	default:
		log.Printf("Requested model '%s' not explicitly supported. Defaulting to 'gemini-2.5-flash'.", model)
		model = "gemini-2.5-flash"
	}
	args = append(args, "-m", model)
	// Explicitly disable extensions and MCP servers
	args = append(args, "--extensions", "", "--allowed-mcp-server-names", "")

	log.Printf("Calling gemini-cli with args: %v", args)

	cmd := exec.Command(geminiCLI, args...)
	// Run in temp dir to avoid polluting context with current directory files
	cmd.Dir = os.TempDir()

	// Set environment variable for system prompt
	cmd.Env = append(os.Environ(), "GEMINI_SYSTEM_MD="+sysPromptFile.Name())

	// Pipe the formatted input to gemini-cli's stdin
	cmd.Stdin = strings.NewReader(geminiInput)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create stdout pipe: %v", err), http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create stderr pipe: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to start gemini-cli: %v", err), http.StatusInternalServerError)
		return
	}

	// Read stderr in a goroutine to prevent blocking
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("Gemini CLI stderr: %s", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Error reading Gemini CLI stderr: %v", err)
		}
	}()


	var sessionID string
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		scanner := bufio.NewScanner(stdout) // Use bufio.Scanner for line-by-line reading
		for scanner.Scan() {
			line := scanner.Text()
			if len(line) == 0 {
				continue // Skip empty lines
			}

			var geminiEvent GeminiStreamEvent
			if err := json.Unmarshal([]byte(line), &geminiEvent); err != nil {
				log.Printf("Error unmarshalling gemini stream event line '%s': %v", line, err)
				continue
			}

			if geminiEvent.Type == "init" {
				sessionID = geminiEvent.SessionID
			}

			if geminiEvent.Type == "message" && geminiEvent.Role == "assistant" && geminiEvent.Delta {
				// Translate gemini delta to OpenAI SSE format
				chunk := OpenAICompletionChunk{
					ID:      "chatcmpl-" + generateID(), // Placeholder ID
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   model, // Use the model name sent to gemini-cli
					Choices: []OpenAIChoice{
						{
							Index: 0,
							Delta: OpenAIDelta{
									Content: geminiEvent.Content,
							},
						},
					},
				}
				jsonChunk, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", jsonChunk) // Double newline for SSE format
				w.(http.Flusher).Flush()
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Error reading Gemini CLI stdout stream: %v", err)
		}

	} else {
		// Non-streaming response
		var geminiResponse GeminiCompletionResponse
		// Read the entire stdout for non-streaming
		outputBytes, readErr := io.ReadAll(stdout)
		if readErr != nil {
			http.Error(w, fmt.Sprintf("Failed to read gemini-cli output: %v", readErr), http.StatusInternalServerError)
			return
		}
		
		if err := json.Unmarshal(outputBytes, &geminiResponse); err != nil {
			http.Error(w, fmt.Sprintf("Failed to decode gemini-cli response '%s': %v", string(outputBytes), err), http.StatusInternalServerError)
			return
		}

		response := OpenAICompletionResponse{
			ID:      "chatcmpl-" + generateID(),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []OpenAIChoice{
				{
					Index: 0,
					Message: OpenAIMessage{
						Role:    "assistant",
						Content: geminiResponse.Response,
					},
				},
			},
			// TODO: Populate Usage based on geminiResponse.Stats if available and mapped
		}
		json.NewEncoder(w).Encode(response)
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("gemini-cli command finished with error: %v", err)
		// For non-stream, if an error occurred during cmd.Wait, and we haven't sent response, send 500.
		// For stream, error will be logged but not propagated as headers already sent.
		if !isStream && w.Header().Get("Content-Type") != "application/json" { // Check if response was already written
			http.Error(w, fmt.Sprintf("Gemini CLI process failed: %v", err), http.StatusInternalServerError)
		}
	}

	// Clean up session in a goroutine
	if sessionID != "" {
		go func(id string) {
			deleteCmd := exec.Command(geminiCLI, "--delete-session", id)
			output, err := deleteCmd.CombinedOutput()
			if err != nil {
				log.Printf("Failed to delete session %s: %v, Output: %s", id, err, string(output))
			} else {
				log.Printf("Successfully deleted session %s", id)
			}
		}(sessionID)
	}
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Println("Incoming request for /v1/models")

	models := OpenAIModelsResponse{
		Object: "list",
		Data: []OpenAIModel{
			{ID: "gemini-2.5-flash", Object: "model", Created: 1678886400, OwnedBy: "google"},
			{ID: "gemini-2.5-pro", Object: "model", Created: 1678886400, OwnedBy: "google"},
			{ID: "gemini-3-pro-preview", Object: "model", Created: 1678886400, OwnedBy: "google"},
		},
	}
	json.NewEncoder(w).Encode(models)
}

// parseMessages extracts the system prompt and formats the remaining messages for gemini-cli.
func parseMessages(messages []OpenAIMessage) (string, string) {
	var systemPromptBuilder strings.Builder
	var transcriptBuilder strings.Builder
	
	// Default system prompt if none provided
	defaultSystemPrompt := "You are a helpful AI assistant."
	hasSystemPrompt := false

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			systemPromptBuilder.WriteString(msg.Content + "\n")
			hasSystemPrompt = true
		case "user":
			transcriptBuilder.WriteString("User: " + msg.Content + "\n")
		case "assistant":
			transcriptBuilder.WriteString("Model: " + msg.Content + "\n")
		}
	}
	
	systemPrompt := systemPromptBuilder.String()
	if !hasSystemPrompt {
		systemPrompt = defaultSystemPrompt
	}

	// Append "Model:" at the end if the last message was "user" to prime for completion
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		transcriptBuilder.WriteString("Model:")
	}
	
	return systemPrompt, transcriptBuilder.String()
}

// generateID creates a simple placeholder ID for OpenAI responses
func generateID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
