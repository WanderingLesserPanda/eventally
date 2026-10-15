# syntax=docker/dockerfile:1

# --- Build stage ---------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src

# Cache deps first (go.mod/go.sum rarely change).
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/eventally ./cmd/eventally

# --- Runtime stage -------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S eventally && adduser -S -G eventally eventally
COPY --from=build /out/eventally /usr/local/bin/eventally
USER eventally
EXPOSE 8090
# Templates are parsed from a CWD-relative path at startup; static JS/CSS
# is compiled into the binary via go:embed, so only the templates need
# shipping.
COPY internal/web/templates /app/internal/web/templates
WORKDIR /app
ENTRYPOINT ["eventally"]
