ARG BUILDER_IMAGE=library/golang:1.26-alpine
FROM --platform=$BUILDPLATFORM ${BUILDER_IMAGE} AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/kno-agent ./cmd/kno-agent

FROM scratch
COPY --from=builder /out/kno-agent /kno-agent
USER 0
ENTRYPOINT ["/kno-agent"]


