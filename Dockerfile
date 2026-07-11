# Stage 1: build the static relay binary
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay

# Stage 2: runtime with the claude CLI (needs Node)
FROM node:22-slim
RUN npm install -g @anthropic-ai/claude-code \
    && useradd --create-home --shell /usr/sbin/nologin relay
COPY --from=build /out/relay /usr/local/bin/relay

USER relay
# Persists the CLI login across restarts (mount as a named volume).
VOLUME /home/relay/.claude

ENV RELAY_BIND=0.0.0.0:18082
EXPOSE 18082

HEALTHCHECK --interval=30s --timeout=3s \
  CMD ["node", "-e", "fetch('http://127.0.0.1:18082/health').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"]

ENTRYPOINT ["relay"]
