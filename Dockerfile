# Build: golang:1.27-alpine (anything >= the go 1.24 directive in go.mod works)
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/give-me-real-gpu ./cmd/give-me-real-gpu

# Run: shell-less distroless as the nonroot (65532) user
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/give-me-real-gpu /give-me-real-gpu
WORKDIR /etc/give-me-real-gpu
EXPOSE 15000
USER nonroot:nonroot
ENTRYPOINT ["/give-me-real-gpu"]
CMD ["-config", "/etc/give-me-real-gpu/config.json"]
