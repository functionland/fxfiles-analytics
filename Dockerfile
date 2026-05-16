FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/fxfiles-analytics ./...

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/fxfiles-analytics /app/fxfiles-analytics
USER nonroot:nonroot
ENV LISTEN_ADDR=:8080
# PG_DSN must be supplied at runtime; there is no sensible default for a
# production container.
EXPOSE 8080
ENTRYPOINT ["/app/fxfiles-analytics"]
