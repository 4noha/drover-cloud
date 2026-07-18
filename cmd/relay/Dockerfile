# Cloud Run 用 cloud-relay（CGO 不要・静的・distroless）。
# build: モジュールルートを文脈にして
#   docker build -f cmd/relay/Dockerfile -t cloud-relay .
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/cloud-relay ./cmd/relay

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cloud-relay /cloud-relay
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/cloud-relay"]
