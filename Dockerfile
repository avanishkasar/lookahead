FROM --platform=$BUILDPLATFORM golang:1.27.1 AS builder
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o /lookahead-controller ./cmd/lookahead-controller

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=builder /lookahead-controller /lookahead-controller
USER 65532:65532
ENTRYPOINT ["/lookahead-controller"]
