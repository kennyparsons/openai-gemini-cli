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

# 1. Copy artifacts from the builder stage
COPY --from=builder /openai-gemini-proxy /openai-gemini-proxy
COPY --from=builder /gemini-fast.js /gemini-fast.js

# 2. Configure Environment
ENV GEMINI_SCRIPT_PATH="/gemini-fast.js"
WORKDIR /workspace

# 3. Set Entrypoint
ENTRYPOINT ["/openai-gemini-proxy"]

