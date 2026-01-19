# OpenAI Gemini Proxy

An OpenAI-compatible proxy server that translates OpenAI API requests to Google's Gemini CLI application. Instead of calling the Gemini API directly, this proxy leverages the [gemini-fast](https://github.com/kennyparsons/fast-gemini) CLI to interact with Gemini models.

## Important Note

This is an extremely opinionated proxy/shim. It's not for everyone and even this README will have some holes. If you have questions or run into issues, please [open an issue](https://github.com/kennyparsons/openai-gemini-cli/issues).

## Why Use This Proxy?

This proxy is beneficial in scenarios where:
- The Gemini CLI has more generous API call limits on the free tier compared to direct API access
- Enterprise CLI options are supported but direct API calls are not available

## Features

- OpenAI-compatible API endpoints (`/v1/chat/completions`, `/v1/models`)
- Translates OpenAI v1 API format to Gemini CLI commands
- Support for streaming and non-streaming responses
- Configurable via environment variables
- Docker support with multi-stage builds
- Customizable system prompts
- Works with Google Cloud Vertex AI through the CLI

## Quick Start

### Using Docker Compose

1. Clone the repository:
```bash
git clone https://github.com/kennyparsons/openai-gemini-cli.git
cd openai-gemini-cli
```

2. Create your docker-compose.yml from the example:
```bash
cp docker-compose.example.yml docker-compose.yml
```

3. Update `docker-compose.yml` with your Google Cloud project ID:
```yaml
environment:
  - GOOGLE_CLOUD_PROJECT=your-project-id
```

4. Build and run:
```bash
docker compose up --build
```

The server will be available at `http://localhost:8080`.

### Using Go

1. Build the binary:
```bash
go build -o openai-gemini-proxy
```

2. Run the server:
```bash
GEMINI_SCRIPT_PATH=/path/to/gemini-fast.js ./openai-gemini-proxy
```

## Configuration

### Environment Variables

**Server Configuration:**
- `PORT` - Server port (default: `8080`)
- `GEMINI_SCRIPT_PATH` - Path to the gemini-fast.js script (required)
- `LEAN_PROMPT_PATH` - Path to the lean system prompt file (default: `/root/.gemini/lean_system.md`)

**Google Cloud Configuration:**
- `GOOGLE_CLOUD_PROJECT` - Google Cloud project ID (required for Vertex AI)
- `GOOGLE_CLOUD_LOCATION` - Google Cloud location (default: `global`)
- `GOOGLE_GENAI_USE_VERTEXAI` - Enable Vertex AI (default: `true`)

**Security Configuration:**
- `DISABLE_SSRF_PROTECTION` - Set to `true` to disable SSRF protection for private network deployments (default: `false`)
  - **Warning:** Only disable this in trusted private networks (e.g., home servers)
  - When enabled (default), blocks image downloads from private IPs, localhost, and link-local addresses
  - When disabled, allows image downloads from any IP address
- `MAX_IMAGE_SIZE_MB` - Maximum image size in megabytes (default: `10`)
- `IMAGE_DOWNLOAD_TIMEOUT_SEC` - Image download timeout in seconds (default: `30`)
- `MAX_REQUEST_BODY_SIZE_MB` - Maximum request body size in megabytes (default: `1`)

**Concurrency Configuration:**
- `MAX_CONCURRENT_REQUESTS` - Maximum concurrent Gemini CLI processes (default: `10`)
  - Controls how many requests can be processed simultaneously
  - Prevents resource exhaustion under high load
  - Requests queue when limit is reached
- `CLEANUP_WORKERS` - Number of session cleanup worker threads (default: `3`)
- `CLEANUP_QUEUE_SIZE` - Size of the cleanup queue buffer (default: `100`)

**Temp Directory Configuration:**
- `TEMP_CLEANUP_INTERVAL_MIN` - Background cleanup interval in minutes (default: `5`)
- `TEMP_FILE_MAX_AGE_MIN` - Maximum age of temp files in minutes before cleanup (default: `60`)

**Timeout Configuration:**
- `REQUEST_TIMEOUT_MIN` - Request timeout in minutes (default: `5`)
  - Maximum time allowed for a single request to complete
  - Prevents hanging requests from consuming resources

### Configuration Best Practices

**For Docker Deployments:**
All configuration is done via environment variables. Example `docker-compose.yml`:

```yaml
environment:
  - PORT=8080
  - GEMINI_SCRIPT_PATH=/app/gemini-fast.js
  - MAX_CONCURRENT_REQUESTS=20
  - MAX_IMAGE_SIZE_MB=50
  - DISABLE_SSRF_PROTECTION=true  # For private networks only
```

**For High-Traffic Deployments:**
- Increase `MAX_CONCURRENT_REQUESTS` (e.g., `20` or `30`)
- Increase `CLEANUP_WORKERS` proportionally (e.g., `5` or `10`)
- Increase `REQUEST_TIMEOUT_MIN` if processing large images (e.g., `10`)
- Increase `MAX_IMAGE_SIZE_MB` if needed (e.g., `50`)

**For Low-Resource Environments:**
- Decrease `MAX_CONCURRENT_REQUESTS` (e.g., `5`)
- Decrease `CLEANUP_WORKERS` (e.g., `2`)
- Decrease `TEMP_FILE_MAX_AGE_MIN` for more aggressive cleanup (e.g., `30`)

**Configuration Logging:**
On startup, the proxy logs all active configuration values for easy verification:
```
=== Configuration ===
Server:
  PORT: 8080
  GEMINI_SCRIPT_PATH: /app/gemini-fast.js
Security:
  MAX_IMAGE_SIZE: 10 MB
  SSRF_PROTECTION: ✓ ENABLED
Concurrency:
  MAX_CONCURRENT_REQUESTS: 10
...
```

### Google Cloud Authentication

The proxy requires Google Cloud credentials. When using Docker, mount your credentials file:

```yaml
volumes:
  - /root/.config/gcloud/application_default_credentials.json:/root/.config/gcloud/application_default_credentials.json
```

For local development, authenticate using:
```bash
gcloud auth application-default login
```

## How It Works

1. Client sends an OpenAI-compatible request to the proxy
2. Proxy translates the request to Gemini CLI format
3. Proxy executes the gemini-fast.js CLI application with the translated request
4. CLI application communicates with Google's Gemini service
5. Proxy translates the CLI response back to OpenAI format
6. Client receives an OpenAI-compatible response

This architecture allows you to benefit from CLI-specific features, quotas, and authentication methods while maintaining compatibility with OpenAI tooling.

## API Usage

The proxy implements OpenAI-compatible endpoints. Use it with any OpenAI client:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-2.0-flash-exp",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'
```

### Supported Models

The proxy explicitly supports the following models:
- `gemini-2.5-flash`
- `gemini-2.5-pro`
- `gemini-3-pro-preview`

Any unsupported or unrecognized model name will automatically fall back to `gemini-2.5-flash`.

## Development

### Project Structure

- `main.go` - Main server implementation
- `types.go` - Type definitions for OpenAI and Gemini API structures
- `Dockerfile` - Multi-stage Docker build configuration
- `docker-compose.yml` - Docker Compose configuration
- `lean_system.md` - Default system prompt

### Building from Source

Requirements:
- Go 1.25.5 or later
- Node.js (for running gemini-fast.js)

```bash
go build -o openai-gemini-proxy
```

## Docker

The Docker image uses a multi-stage build:
1. Builder stage: Clones the repository, builds the Go proxy binary, and downloads gemini-fast.js
2. Final stage: Minimal Node.js Alpine image with the proxy binary and CLI application

Build the image:
```bash
docker build -t gemini-proxy .
```

Run the container:
```bash
docker run -p 8080:8080 \
  -e GOOGLE_CLOUD_PROJECT=your-project-id \
  -v ~/.config/gcloud:/root/.config/gcloud \
  gemini-proxy
```

## Architecture

```
OpenAI Client → Proxy Server (Go) → gemini-fast.js (Node.js) → Google Gemini Service
                     ↓
              Translates API formats
```

The proxy acts as a translation layer between OpenAI's API format and the Gemini CLI application, allowing you to use CLI-specific features and quotas while maintaining OpenAI compatibility.

## License

This project is open source and available under the MIT License.

## Contributing

Contributions are welcome. Please open an issue or submit a pull request.

