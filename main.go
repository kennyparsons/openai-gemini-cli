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

	// Determine if streaming is requested
	isStream := req.Stream

	// Format messages into a single transcript for gemini-cli
	geminiInput := buildGeminiInput(req.Messages)

	// Build the gemini-cli command
	args := []string{}
	if isStream {
		args = append(args, "--output-format", "stream-json")
	} else {
		args = append(args, "--output-format", "json")
	}

	// Use specified model, or default if not provided/recognized
	model := req.Model
	if model == "" {
		// Default to a common Gemini model if OpenAI model name is used
		model = "gemini-2.5-flash"
	} else if strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "davinci-") {
		// Map common OpenAI models to a suitable Gemini model
		model = "gemini-2.5-flash" // Or implement more sophisticated mapping
	}
	args = append(args, "-m", model)

	cmd := exec.Command(geminiCLI, args...)

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

	models := OpenAIModelsResponse{
		Object: "list",
		Data: []OpenAIModel{
			{ID: "gemini-2.5-flash", Object: "model", Created: 1678886400, OwnedBy: "google"},
			{ID: "gemini-1.5-pro", Object: "model", Created: 1678886400, OwnedBy: "google"},
			{ID: "gemini-1.0-pro", Object: "model", Created: 1678886400, OwnedBy: "google"},
			// Add more supported models if known
		},
	}
	json.NewEncoder(w).Encode(models)
}

// buildGeminiInput formats the OpenAI messages into a single string for gemini-cli stdin.
func buildGeminiInput(messages []OpenAIMessage) string {
	var builder strings.Builder
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			builder.WriteString("System: " + msg.Content + "\n")
		case "user":
			builder.WriteString("User: " + msg.Content + "\n")
		case "assistant":
			builder.WriteString("Model: " + msg.Content + "\n")
		}
	}
	// Append "Model:" at the end if the last message was "user" to prime for completion
	if len(messages) > 0 && messages[len(messages)-1].Role == "user" {
		builder.WriteString("Model:")
	}
	return builder.String()
}

// generateID creates a simple placeholder ID for OpenAI responses
func generateID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}
