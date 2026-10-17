FROM golang:1.26.9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/dra-driver-dri ./cmd/dra-driver-dri

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/dra-driver-dri /dra-driver-dri
ENTRYPOINT ["/dra-driver-dri"]
