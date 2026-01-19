# OpenAI Gemini Proxy

An OpenAI-compatible proxy server that translates OpenAI API requests to Google's Gemini CLI application. Instead of calling the Gemini API directly, this proxy leverages the gemini-cli (or gemini-fast.js) to interact with Gemini models.

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

2. Update `docker-compose.yml` with your Google Cloud project ID:
```yaml
environment:
  - GOOGLE_CLOUD_PROJECT=your-project-id
```

3. Build and run:
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

- `PORT` - Server port (default: `8080`)
- `GEMINI_SCRIPT_PATH` - Path to the gemini-fast.js script (required)
- `LEAN_PROMPT_PATH` - Path to the lean system prompt file (default: `/root/.gemini/lean_system.md`)
- `GOOGLE_CLOUD_PROJECT` - Google Cloud project ID (required for Vertex AI)
- `GOOGLE_CLOUD_LOCATION` - Google Cloud location (default: `global`)
- `GOOGLE_GENAI_USE_VERTEXAI` - Enable Vertex AI (default: `true`)

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

