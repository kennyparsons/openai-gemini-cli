package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv" // Added for strconv.Quote
	"strings"
	"time"
)

const (
	defaultPort             = "8080"
	nodeExec                = "node"
	defaultGeminiScript     = "/Users/kenny.parsons/dmz/kennyparsons/gemini-speed/build/gemini-fast-v3.mjs"
	defaultLeanPromptPath   = "/root/.gemini/lean_system.md"
	maxScanTokenSize        = 10 * 1024 * 1024 // 10MB buffer for scanner
)

var (
	geminiScript   string
	leanPromptPath string
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	// Initialize geminiScript from environment variable or use default
	geminiScript = os.Getenv("GEMINI_SCRIPT_PATH")
	if geminiScript == "" {
		geminiScript = defaultGeminiScript
		log.Printf("GEMINI_SCRIPT_PATH not set, using default: %s", geminiScript)
	} else {
		log.Printf("Using GEMINI_SCRIPT_PATH from environment: %s", geminiScript)
	}

	// Initialize leanPromptPath from environment variable or use default
	leanPromptPath = os.Getenv("LEAN_PROMPT_PATH")
	if leanPromptPath == "" {
		leanPromptPath = defaultLeanPromptPath
		log.Printf("LEAN_PROMPT_PATH not set, using default: %s", leanPromptPath)
	} else {
		log.Printf("Using LEAN_PROMPT_PATH from environment: %s", leanPromptPath)
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

	// Log the incoming request payload
	requestPayload, _ := json.MarshalIndent(req, "", "  ")
	log.Printf("Incoming Request Payload:\n%s", requestPayload)

	log.Printf("Incoming request. Requested Model: %s", req.Model)

	// Determine if streaming is requested
	isStream := req.Stream

	// Create a unique temporary directory for this request
	requestTempDir, err := os.MkdirTemp("", "gemini-proxy-request-*")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create request temporary directory: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(requestTempDir) // Ensure cleanup of the temp directory

	log.Printf("Created request temporary directory: %s", requestTempDir)

	// Extract system prompt and format messages
	systemPromptContent, geminiInput, err := parseMessages(req.Messages, requestTempDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse messages: %v", err), http.StatusBadRequest)
		return
	}

	// Create a temporary file for the system prompt within the request's temp directory
	sysPromptFile, err := os.CreateTemp(requestTempDir, "sys-prompt-*.md")
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to create temp system prompt file: %v", err), http.StatusInternalServerError)
		return
	}
	// No need to os.Remove(sysPromptFile.Name()) here, as os.RemoveAll(requestTempDir) will handle it

	// Read Lean System Prompt
	leanPromptContent, err := os.ReadFile(leanPromptPath)
	var finalSystemPrompt string
	if err != nil {
		log.Printf("Warning: Failed to read lean system prompt from %s: %v", leanPromptPath, err)
		finalSystemPrompt = systemPromptContent
	} else {
		finalSystemPrompt = string(leanPromptContent) + "\n" + systemPromptContent
	}

	// Write system prompt to file
	if _, err := sysPromptFile.WriteString(finalSystemPrompt); err != nil {
		http.Error(w, fmt.Sprintf("Failed to write to temp system prompt file: %v", err), http.StatusInternalServerError)
		return
	}
	sysPromptFile.Close()

	// Build the gemini-cli command
	// Always use stream-json to ensure we can capture the session ID for cleanup
	args := []string{geminiScript, "--output-format", "stream-json"}

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
	args = append(args, "--extensions", "[]", "--allowed-mcp-server-names", "[]")

	log.Printf("Calling gemini-cli with args: %v", args)

	cmd := exec.Command(nodeExec, args...)
	// Set gemini-cli's working directory to the request's temp directory
	cmd.Dir = requestTempDir

	// Set environment variable for system prompt, using the absolute path to the temp file
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
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, maxScanTokenSize)
		for scanner.Scan() {
			log.Printf("Gemini CLI stderr: %s", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Error reading Gemini CLI stderr: %v", err)
		}
	}()

	var sessionID string
	var fullResponseBuilder strings.Builder

	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
	}

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxScanTokenSize)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
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
			if isStream {
				// Streaming mode: Send SSE chunk immediately
				chunk := OpenAICompletionChunk{
					ID:      "chatcmpl-" + generateID(),
					Object:  "chat.completion.chunk",
					Created: time.Now().Unix(),
					Model:   model,
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
				fmt.Fprintf(w, "data: %s\n\n", jsonChunk)
				w.(http.Flusher).Flush()
			} else {
				// Non-streaming mode: Buffer the content
				fullResponseBuilder.WriteString(geminiEvent.Content)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading Gemini CLI stdout stream: %v", err)
	}

	// Handle final response for non-streaming requests
	if !isStream {
		response := OpenAICompletionResponse{
			ID:      "chatcmpl-" + generateID(),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []OpenAIChoice{
				{
					Index: 0,
					Message: OpenAIMessage{ // OpenAIMessage here for output should behave as simple string content
						Role:    "assistant",
						Content: json.RawMessage(strconv.Quote(fullResponseBuilder.String())), // Corrected assignment
					},
				},
			},
			// Usage stats could technically be parsed from the 'result' event in the stream if needed
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}

	if err := cmd.Wait(); err != nil {
		log.Printf("gemini-cli command finished with error: %v", err)
		if !isStream && w.Header().Get("Content-Type") != "application/json" { // Check if response was already written
			http.Error(w, fmt.Sprintf("Gemini CLI process failed: %v", err), http.StatusInternalServerError)
		}
	}

	// Clean up session in a goroutine using the captured sessionID
	if sessionID != "" {
		go func(id string) {
			deleteCmd := exec.Command(nodeExec, geminiScript, "--delete-session", id)
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

// parseMessages extracts the system prompt and formats the remaining messages for gemini-cli, handling multimodal content.
func parseMessages(messages []OpenAIMessage, requestTempDir string) (string, string, error) {
	var systemPromptBuilder strings.Builder
	var transcriptBuilder strings.Builder

	// Default system prompt if none provided
	defaultSystemPrompt := "You are a helpful AI assistant."
	hasSystemPrompt := false

	for _, msg := range messages {
		switch msg.Role {
		case "system":
			// System messages are assumed to be text-only
			if len(msg.ParsedContent) > 0 && msg.ParsedContent[0].Type == "text" {
				systemPromptBuilder.WriteString(msg.ParsedContent[0].Text + "\n")
				hasSystemPrompt = true
			}
		case "user":
			transcriptBuilder.WriteString("User: ")
			for _, part := range msg.ParsedContent {
				switch part.Type {
				case "text":
					transcriptBuilder.WriteString(part.Text)
				case "image_url":
					if part.ImageURL != nil && part.ImageURL.URL != "" {
						imagePath, err := handleImageURL(part.ImageURL.URL, requestTempDir)
							if err != nil {
								return "", "", fmt.Errorf("failed to handle image URL: %w", err)
							}
						transcriptBuilder.WriteString(fmt.Sprintf(" @%s", imagePath))
					} else {
						log.Printf("Warning: image_url content part found but ImageURL or URL is empty.")
					}
				}
			}
			transcriptBuilder.WriteString("\n")
		case "assistant":
			transcriptBuilder.WriteString("Model: ")
			for _, part := range msg.ParsedContent {
				// Assuming assistant messages are primarily text-based for now
				if part.Type == "text" {
					transcriptBuilder.WriteString(part.Text)
				}
			}
			transcriptBuilder.WriteString("\n")
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

	return systemPrompt, transcriptBuilder.String(), nil
}

// handleImageURL downloads/decodes an image and saves it to a temp file in the specified directory.
func handleImageURL(imageURL string, destDir string) (string, error) {
	// Generate a unique filename
	fileName := "image-" + generateID()
	var fileExtension string

	var imageData []byte
	var err error

	if strings.HasPrefix(imageURL, "data:image/") {
		// Base64 Data URI
		parts := strings.SplitN(imageURL, ";base64,", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid base64 image data URI")
		}

		// Extract MIME type for extension
	mimeType := strings.TrimPrefix(parts[0], "data:")
	slashIndex := strings.Index(mimeType, "/")
	if slashIndex != -1 {
		fileExtension = "." + mimeType[slashIndex+1:]
		}
		
		imageData, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return "", fmt.Errorf("failed to decode base64 image: %w", err)
		}
	} else if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		// Remote URL
		req, err := http.NewRequest("GET", imageURL, nil)
		if err != nil {
			return "", fmt.Errorf("failed to create request for image URL: %w", err)
		}
		// Set User-Agent to avoid 403 Forbidden from some servers
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to download image from URL: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("failed to download image, status code: %d", resp.StatusCode)
		}
		
		imageData, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("failed to read image data from response: %w", err)
		}

		// Infer extension from Content-Type header
		contentType := resp.Header.Get("Content-Type")
		if strings.Contains(contentType, "image/jpeg") {
			fileExtension = ".jpg"
		} else if strings.Contains(contentType, "image/png") {
			fileExtension = ".png"
		} else if strings.Contains(contentType, "image/gif") {
			fileExtension = ".gif"
		} else if strings.Contains(contentType, "image/webp") {
			fileExtension = ".webp"
		} else {
			// Fallback if Content-Type is not specific enough, or try to guess from URL
			log.Printf("Warning: Could not infer image extension from Content-Type: %s. Trying from URL.", contentType)
			u, parseErr := url.Parse(imageURL)
			if parseErr == nil {
				ext := filepath.Ext(u.Path)
				if ext != "" {
					fileExtension = ext
				}
			}
			if fileExtension == "" {
				fileExtension = ".bin" // Default to binary if no extension found
			}
		}

	} else {
		return "", fmt.Errorf("unsupported image URL scheme: %s", imageURL)
	}

	// Ensure extension is not empty
	if fileExtension == "" {
		fileExtension = ".dat" // Default if no extension could be determined
	}

	fullPath := filepath.Join(destDir, fileName+fileExtension)
	if err := os.WriteFile(fullPath, imageData, 0644); err != nil {
		return "", fmt.Errorf("failed to write image to temp file: %w", err)
	}
	
	// Return the filename relative to the temp directory (which is cmd.Dir)
	return fileName + fileExtension, nil
}

// generateID creates a simple placeholder ID for OpenAI responses
func generateID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}