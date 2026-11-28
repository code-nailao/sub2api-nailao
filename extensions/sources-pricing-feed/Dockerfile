ARG GO_IMAGE=golang:1.26.5-alpine
FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pricing-feed ./cmd/pricing-feed

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /pricing-feed /pricing-feed
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/pricing-feed"]
