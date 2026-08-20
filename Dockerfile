# Build the go application into a binary
#
# The builder image is pinned deliberately. golang:alpine is a moving tag, and
# Go 1.27 excludes golang.org/x/net/http2/server.go via the build constraint
# "!(go1.27 && !http2legacy)" — x/net delegates to the standard library there and
# stops exporting TrailerPrefix, which google.golang.org/grpc v1.81.1 still
# references. An unpinned base therefore breaks the image build the day the tag
# moves, with no change on our side. Unpin only after bumping grpc to a release
# that no longer uses http2.TrailerPrefix.
FROM golang:1.26-alpine AS builder
RUN apk --update add ca-certificates
WORKDIR /app
COPY . ./
RUN go mod tidy -diff
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gatus .

# Run Tests inside docker image if you don't have a configured go environment
#RUN apk update && apk add --virtual build-dependencies build-base gcc
#RUN go test ./... -mod vendor

# Run the binary on an empty container
FROM scratch
COPY --from=builder /app/gatus .
COPY --from=builder /app/config.yaml ./config/config.yaml
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENV GATUS_CONFIG_PATH=""
ENV GATUS_LOG_LEVEL="INFO"
ENV PORT="8080"
EXPOSE ${PORT}
ENTRYPOINT ["/gatus"]
