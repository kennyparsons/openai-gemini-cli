# --- Stage 1: Builder (Go & Asset Fetching) ---
FROM golang:alpine AS builder

WORKDIR /src

# Install git for cloning and curl for downloading
RUN apk add --no-cache git curl

# 1. Build the Proxy
RUN git clone https://github.com/kennyparsons/openai-gemini-cli.git . \
    && go build -o /openai-gemini-proxy .

# 2. Download the JS Script
RUN curl -L -o /gemini-fast.js https://github.com/kennyparsons/fast-gemini/releases/latest/download/gemini-fast.js


# --- Stage 2: Final (Node Alpine) ---
FROM node:lts-alpine

# 1. Create directory for lean system prompt
RUN mkdir -p /root/.gemini

# 2. Copy artifacts from the builder stage
COPY --from=builder /openai-gemini-proxy /openai-gemini-proxy
COPY --from=builder /gemini-fast.js /gemini-fast.js
COPY --from=builder /src/lean_system.md /root/.gemini/lean_system.md

# 3. Configure Environment
ENV GEMINI_SCRIPT_PATH="/gemini-fast.js"
ENV LEAN_PROMPT_PATH="/root/.gemini/lean_system.md"
WORKDIR /workspace

# 4. Set Entrypoint
ENTRYPOINT ["/openai-gemini-proxy"]

