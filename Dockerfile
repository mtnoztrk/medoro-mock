FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY templates ./templates
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /medoro-mock .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /medoro-mock .
COPY scenarios.json medoro_merchant.pem medoro_gateway.pem ./
EXPOSE 2727
ENTRYPOINT ["./medoro-mock"]
