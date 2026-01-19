package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv" // Added for strconv.Quote
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	nodeExec              = "node"
	maxScanTokenSize      = 10 * 1024 * 1024 // 10MB buffer for scanner
)

// Configuration with defaults (overridable via environment variables)
type Config struct {
	// Server configuration
	Port                    string
	GeminiScriptPath        string
	LeanPromptPath          string

	// Security configuration
	MaxImageSize            int64
	ImageDownloadTimeout    int
	MaxRequestBodySize      int64
	DisableSSRFProtection   bool

	// Concurrency configuration
	MaxConcurrentRequests   int
	CleanupWorkers          int
	CleanupQueueSize        int

	// Temp directory configuration
	TempCleanupInterval     int // minutes
	TempFileMaxAge          int // minutes

	// Request timeout configuration
	RequestTimeout          int // minutes
}

var (
	config               Config
	baseTempDir          string
	cleanupQueue         chan string
	cleanupWaitGroup     sync.WaitGroup
	requestCounter       uint64
	requestSemaphore     chan struct{}
	activeRequests       int64
)

func main() {
	// Load configuration from environment variables
	config = loadConfig()
	printConfig()

	// Startup validation
	if err := validateStartup(); err != nil {
		log.Fatalf("Startup validation failed: %v", err)
	}

	// Initialize base temp directory
	if err := initTempDirectory(); err != nil {
		log.Fatalf("Failed to initialize temp directory: %v", err)
	}

	// Initialize request semaphore for concurrency limiting
	requestSemaphore = make(chan struct{}, config.MaxConcurrentRequests)

	// Initialize session cleanup queue
	cleanupQueue = make(chan string, config.CleanupQueueSize)

	// Start cleanup workers
	for i := 0; i < config.CleanupWorkers; i++ {
		go sessionCleanupWorker(i)
	}

	// Start background temp directory cleanup goroutine
	go backgroundTempCleanup()

	// Defer cleanup in correct order: cleanup queue FIRST, then temp directory
	defer func() {
		log.Printf("Shutting down: closing cleanup queue")
		close(cleanupQueue)
		cleanupWaitGroup.Wait()
		log.Printf("All cleanup workers finished")
		cleanupTempDirectory()
		log.Printf("Temp directory cleaned up")
	}()

	// Wrap handlers with timeout to prevent hanging requests
	http.Handle("/v1/chat/completions", http.TimeoutHandler(
		http.HandlerFunc(handleChatCompletions),
		time.Duration(config.RequestTimeout)*time.Minute,
		"Request timeout: Gemini API took too long to respond",
	))
	http.HandleFunc("/v1/models", handleModels)
	http.HandleFunc("/health", handleHealth)

	log.Printf("Server listening on :%s", config.Port)
	log.Fatal(http.ListenAndServe(":"+config.Port, nil))
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body size
	r.Body = http.MaxBytesReader(w, r.Body, config.MaxRequestBodySize)

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

	// Create a unique temporary directory for this request using base temp dir
	requestID := generateID()
	requestTempDir := filepath.Join(baseTempDir, requestID)
	if err := os.MkdirAll(requestTempDir, 0755); err != nil {
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
	leanPromptContent, err := os.ReadFile(config.LeanPromptPath)
	var finalSystemPrompt string
	if err != nil {
		log.Printf("Warning: Failed to read lean system prompt from %s: %v", config.LeanPromptPath, err)
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
	args := []string{config.GeminiScriptPath, "--output-format", "stream-json"}

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

	// Acquire semaphore slot (blocks if at max concurrency)
	active := atomic.AddInt64(&activeRequests, 1)
	log.Printf("Acquiring process slot (active: %d/%d)", active, config.MaxConcurrentRequests)
	requestSemaphore <- struct{}{}
	defer func() {
		<-requestSemaphore
		active := atomic.AddInt64(&activeRequests, -1)
		log.Printf("Released process slot (active: %d/%d)", active, config.MaxConcurrentRequests)
	}()

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

	// Queue session cleanup using the captured sessionID
	if sessionID != "" {
		select {
		case cleanupQueue <- sessionID:
			log.Printf("Queued session %s for cleanup", sessionID)
		default:
			log.Printf("Warning: Cleanup queue full, session %s may leak", sessionID)
		}
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

// handleHealth provides a health check endpoint
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	health := make(map[string]interface{})
	health["status"] = "ok"

	// Check if gemini script exists
	if _, err := os.Stat(config.GeminiScriptPath); err != nil {
		health["status"] = "degraded"
		health["gemini_script"] = "not found"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		health["gemini_script"] = "ok"
	}

	// Check if node is available
	if _, err := exec.LookPath(nodeExec); err != nil {
		health["status"] = "degraded"
		health["node"] = "not found"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		health["node"] = "ok"
	}

	// Check if temp directory is writable
	tempDir := os.TempDir()
	testFile := filepath.Join(tempDir, ".gemini-proxy-health-check")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		health["status"] = "degraded"
		health["temp_dir"] = "not writable"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		os.Remove(testFile)
		health["temp_dir"] = "ok"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health)
}

// validateStartup performs startup validation checks
func validateStartup() error {
	// Check if gemini script exists
	if _, err := os.Stat(config.GeminiScriptPath); err != nil {
		return fmt.Errorf("gemini script not found at %s: %w", config.GeminiScriptPath, err)
	}
	log.Printf("✓ Gemini script found: %s", config.GeminiScriptPath)

	// Check if node is installed
	if _, err := exec.LookPath(nodeExec); err != nil {
		return fmt.Errorf("node executable not found: %w", err)
	}
	log.Printf("✓ Node.js found")

	// Check if temp directory is writable
	tempDir := os.TempDir()
	testFile := filepath.Join(tempDir, ".gemini-proxy-startup-test")
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		return fmt.Errorf("temp directory not writable: %w", err)
	}
	os.Remove(testFile)
	log.Printf("✓ Temp directory writable: %s", tempDir)

	// Warn if lean prompt path doesn't exist (non-fatal)
	if _, err := os.Stat(config.LeanPromptPath); err != nil {
		log.Printf("⚠ Lean prompt file not found at %s (will use default system prompt)", config.LeanPromptPath)
	} else {
		log.Printf("✓ Lean prompt file found: %s", config.LeanPromptPath)
	}

	log.Printf("✓ All startup validations passed")
	return nil
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

		// Check size after decoding
		if int64(len(imageData)) > config.MaxImageSize {
			return "", fmt.Errorf("decoded image size (%d bytes) exceeds maximum allowed size (%d bytes)", len(imageData), config.MaxImageSize)
		}
	} else if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		// Remote URL

		// SSRF protection - block private IP ranges using proper IP parsing
		parsedURL, err := url.Parse(imageURL)
		if err != nil {
			return "", fmt.Errorf("invalid image URL: %w", err)
		}

		// Validate hostname is not a private IP (unless SSRF protection is disabled)
		if !config.DisableSSRFProtection {
			host := parsedURL.Hostname()
			if err := validatePublicHost(host); err != nil {
				return "", err
			}
		}

		req, err := http.NewRequest("GET", imageURL, nil)
		if err != nil {
			return "", fmt.Errorf("failed to create request for image URL: %w", err)
		}
		// Set User-Agent to avoid 403 Forbidden from some servers
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")

		client := &http.Client{
			Timeout: time.Duration(config.ImageDownloadTimeout) * time.Second,
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to download image from URL: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("failed to download image, status code: %d", resp.StatusCode)
		}

		// Check Content-Length header if available
		if resp.ContentLength > config.MaxImageSize {
			return "", fmt.Errorf("image size (%d bytes) exceeds maximum allowed size (%d bytes)", resp.ContentLength, config.MaxImageSize)
		}

		// Use LimitReader to prevent reading more than config.MaxImageSize
		limitedReader := io.LimitReader(resp.Body, config.MaxImageSize+1)
		imageData, err = io.ReadAll(limitedReader)
		if err != nil {
			return "", fmt.Errorf("failed to read image data from response: %w", err)
		}

		// Check if we hit the limit
		if int64(len(imageData)) > config.MaxImageSize {
			return "", fmt.Errorf("image size exceeds maximum allowed size (%d bytes)", config.MaxImageSize)
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

// generateID creates a unique ID for requests and responses
// Uses combination of timestamp, atomic counter, and random bytes to ensure uniqueness
func generateID() string {
	// Atomic counter for uniqueness within same millisecond
	counter := atomic.AddUint64(&requestCounter, 1)

	// Random bytes for additional entropy
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		// Fallback to timestamp + counter if random fails
		return fmt.Sprintf("%x-%x", time.Now().UnixNano(), counter)
	}

	// Combine timestamp, counter, and random bytes
	return fmt.Sprintf("%x-%x-%s", time.Now().UnixNano(), counter, hex.EncodeToString(randomBytes))
}

// validatePublicHost checks if a hostname resolves to a public IP address
// Returns error if the host is localhost, private IP, or link-local
func validatePublicHost(host string) error {
	// Check for localhost variations
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
		return fmt.Errorf("access to localhost is not allowed")
	}

	// Try to parse as IP first
	ip := net.ParseIP(host)
	if ip != nil {
		return validatePublicIP(ip)
	}

	// If not an IP, resolve the hostname
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve hostname: %w", err)
	}

	// Check all resolved IPs
	for _, ip := range ips {
		if err := validatePublicIP(ip); err != nil {
			return fmt.Errorf("hostname resolves to private IP %s: %w", ip, err)
		}
	}

	return nil
}

// validatePublicIP checks if an IP is public (not private, loopback, or link-local)
func validatePublicIP(ip net.IP) error {
	if ip.IsLoopback() {
		return fmt.Errorf("loopback addresses are not allowed")
	}
	if ip.IsPrivate() {
		return fmt.Errorf("private IP addresses are not allowed")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("link-local addresses are not allowed")
	}
	if ip.IsMulticast() {
		return fmt.Errorf("multicast addresses are not allowed")
	}
	return nil
}

// initTempDirectory creates and initializes the base temp directory
func initTempDirectory() error {
	tempDir := filepath.Join(os.TempDir(), "gemini-proxy")

	// Clean up any stale directories from previous runs
	if _, err := os.Stat(tempDir); err == nil {
		log.Printf("Cleaning up stale temp directory: %s", tempDir)
		if err := os.RemoveAll(tempDir); err != nil {
			return fmt.Errorf("failed to clean up stale temp directory: %w", err)
		}
	}

	// Create fresh temp directory
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return fmt.Errorf("failed to create base temp directory: %w", err)
	}

	baseTempDir = tempDir
	log.Printf("✓ Base temp directory initialized: %s", baseTempDir)
	return nil
}

// cleanupTempDirectory removes the base temp directory on shutdown
func cleanupTempDirectory() {
	if baseTempDir != "" {
		log.Printf("Cleaning up base temp directory: %s", baseTempDir)
		if err := os.RemoveAll(baseTempDir); err != nil {
			log.Printf("Warning: Failed to clean up base temp directory: %v", err)
		}
	}
}

// backgroundTempCleanup periodically cleans up old request directories
func backgroundTempCleanup() {
	ticker := time.NewTicker(time.Duration(config.TempCleanupInterval) * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		if baseTempDir == "" {
			continue
		}

		entries, err := os.ReadDir(baseTempDir)
		if err != nil {
			log.Printf("Warning: Failed to read temp directory for cleanup: %v", err)
			continue
		}

		now := time.Now()
		cleanedCount := 0

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			fullPath := filepath.Join(baseTempDir, entry.Name())
			info, err := entry.Info()
			if err != nil {
				continue
			}

			// Remove directories older than configured max age
			if now.Sub(info.ModTime()) > time.Duration(config.TempFileMaxAge)*time.Minute {
				if err := os.RemoveAll(fullPath); err != nil {
					log.Printf("Warning: Failed to clean up old temp directory %s: %v", fullPath, err)
				} else {
					cleanedCount++
				}
			}
		}

		if cleanedCount > 0 {
			log.Printf("Background cleanup: removed %d old temp directories", cleanedCount)
		}
	}
}

// sessionCleanupWorker processes session cleanup requests from the queue
func sessionCleanupWorker(workerID int) {
	cleanupWaitGroup.Add(1)
	defer cleanupWaitGroup.Done()

	log.Printf("Session cleanup worker %d started", workerID)

	for sessionID := range cleanupQueue {
		// Retry logic with exponential backoff
		maxRetries := 3
		for attempt := 1; attempt <= maxRetries; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

			deleteCmd := exec.CommandContext(ctx, nodeExec, config.GeminiScriptPath, "--delete-session", sessionID)
			output, err := deleteCmd.CombinedOutput()
			cancel()

			if err == nil {
				log.Printf("Worker %d: Successfully deleted session %s", workerID, sessionID)
				break
			}

			if attempt < maxRetries {
				backoff := time.Duration(attempt*attempt) * time.Second
				log.Printf("Worker %d: Failed to delete session %s (attempt %d/%d): %v. Retrying in %v...",
					workerID, sessionID, attempt, maxRetries, err, backoff)
				time.Sleep(backoff)
			} else {
				log.Printf("Worker %d: Failed to delete session %s after %d attempts: %v, Output: %s",
					workerID, sessionID, maxRetries, err, string(output))
			}
		}
	}

	log.Printf("Session cleanup worker %d stopped", workerID)
}

// loadConfig loads configuration from environment variables with defaults
func loadConfig() Config {
	cfg := Config{
		// Defaults
		Port:                  getEnv("PORT", "8080"),
		GeminiScriptPath:      getEnv("GEMINI_SCRIPT_PATH", "gemini-fast.js"),
		LeanPromptPath:        getEnv("LEAN_PROMPT_PATH", "/root/.gemini/lean_system.md"),
		MaxImageSize:          getEnvInt64("MAX_IMAGE_SIZE_MB", 10) * 1024 * 1024,
		ImageDownloadTimeout:  getEnvInt("IMAGE_DOWNLOAD_TIMEOUT_SEC", 30),
		MaxRequestBodySize:    getEnvInt64("MAX_REQUEST_BODY_SIZE_MB", 1) * 1024 * 1024,
		DisableSSRFProtection: getEnvBool("DISABLE_SSRF_PROTECTION", false),
		MaxConcurrentRequests: getEnvInt("MAX_CONCURRENT_REQUESTS", 10),
		CleanupWorkers:        getEnvInt("CLEANUP_WORKERS", 3),
		CleanupQueueSize:      getEnvInt("CLEANUP_QUEUE_SIZE", 100),
		TempCleanupInterval:   getEnvInt("TEMP_CLEANUP_INTERVAL_MIN", 5),
		TempFileMaxAge:        getEnvInt("TEMP_FILE_MAX_AGE_MIN", 60),
		RequestTimeout:        getEnvInt("REQUEST_TIMEOUT_MIN", 5),
	}

	return cfg
}

// printConfig logs the current configuration
func printConfig() {
	log.Printf("=== Configuration ===")
	log.Printf("Server:")
	log.Printf("  PORT: %s", config.Port)
	log.Printf("  GEMINI_SCRIPT_PATH: %s", config.GeminiScriptPath)
	log.Printf("  LEAN_PROMPT_PATH: %s", config.LeanPromptPath)
	log.Printf("Security:")
	log.Printf("  MAX_IMAGE_SIZE: %d MB", config.MaxImageSize/(1024*1024))
	log.Printf("  IMAGE_DOWNLOAD_TIMEOUT: %d seconds", config.ImageDownloadTimeout)
	log.Printf("  MAX_REQUEST_BODY_SIZE: %d MB", config.MaxRequestBodySize/(1024*1024))
	if config.DisableSSRFProtection {
		log.Printf("  SSRF_PROTECTION: ⚠️  DISABLED (private network mode)")
	} else {
		log.Printf("  SSRF_PROTECTION: ✓ ENABLED")
	}
	log.Printf("Concurrency:")
	log.Printf("  MAX_CONCURRENT_REQUESTS: %d", config.MaxConcurrentRequests)
	log.Printf("  CLEANUP_WORKERS: %d", config.CleanupWorkers)
	log.Printf("  CLEANUP_QUEUE_SIZE: %d", config.CleanupQueueSize)
	log.Printf("Temp Directory:")
	log.Printf("  CLEANUP_INTERVAL: %d minutes", config.TempCleanupInterval)
	log.Printf("  FILE_MAX_AGE: %d minutes", config.TempFileMaxAge)
	log.Printf("Timeouts:")
	log.Printf("  REQUEST_TIMEOUT: %d minutes", config.RequestTimeout)
	log.Printf("====================")
}

// Helper functions for environment variable parsing
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
		log.Printf("Warning: Invalid integer value for %s: %s, using default: %d", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.ParseInt(value, 10, 64); err == nil {
			return intValue
		}
		log.Printf("Warning: Invalid int64 value for %s: %s, using default: %d", key, value, defaultValue)
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
		log.Printf("Warning: Invalid boolean value for %s: %s, using default: %t", key, value, defaultValue)
	}
	return defaultValue
}