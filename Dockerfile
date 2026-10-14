FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /auth-proxy .

# distroless/static ships the public CA bundle. If the corporate proxy does TLS
# inspection, mount its root CA and set SSL_CERT_FILE (see README).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /auth-proxy /auth-proxy
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/auth-proxy"]
