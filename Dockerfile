FROM golang:1.27-alpine AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY controllers/ controllers/
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -a -o omni-controller .

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/omni-controller .
USER 65532:65532
ENTRYPOINT ["/omni-controller"]
