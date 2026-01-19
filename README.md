# OpenAI Gemini Proxy

An OpenAI-compatible proxy server for Google's Gemini API. This proxy allows you to use Gemini models with any OpenAI-compatible client by translating requests between the OpenAI and Gemini API formats.

## Features

- OpenAI-compatible API endpoints (`/v1/chat/completions`, `/v1/models`)
- Support for streaming and non-streaming responses
- Configurable via environment variables
- Docker support with multi-stage builds
- Customizable system prompts
- Works with Google Cloud Vertex AI

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

The proxy supports all Gemini models available through the Vertex AI API. Common models include:
- `gemini-2.0-flash-exp`
- `gemini-1.5-pro`
- `gemini-1.5-flash`

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
1. Builder stage: Clones the repository and builds the Go binary
2. Final stage: Minimal Node.js Alpine image with the proxy and gemini-fast.js

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

## License

This project is open source and available under the MIT License.

## Contributing

Contributions are welcome. Please open an issue or submit a pull request.

