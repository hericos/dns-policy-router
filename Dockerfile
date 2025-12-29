# Build
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/dns-policy-router ./main.go

# Run
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dns-policy-router /dns-policy-router
EXPOSE 1053/udp 1053/tcp
USER nonroot:nonroot
ENTRYPOINT ["/dns-policy-router"]
