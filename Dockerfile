# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/pr-sentinel ./cmd/pr-sentinel

# distroless/static inclui certificados TLS e um usuário não-root (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pr-sentinel /pr-sentinel
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/pr-sentinel"]
# Modo padrão: servidor de webhook. A Action sobrescreve com `args: [action]`.
CMD ["server"]
