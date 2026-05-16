FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/fxfiles-analytics ./...

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/fxfiles-analytics /app/fxfiles-analytics
USER nonroot:nonroot
ENV LISTEN_ADDR=:8080 \
    DATA_DIR=/app/data
VOLUME /app/data
EXPOSE 8080
ENTRYPOINT ["/app/fxfiles-analytics"]
