FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/pxc-node-classifier .

FROM hashicorp/nomad:2.0.3
COPY --from=build /out/pxc-node-classifier /usr/local/bin/pxc-node-classifier
ENTRYPOINT ["pxc-node-classifier"]
