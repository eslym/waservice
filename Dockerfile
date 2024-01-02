FROM golang:1.21-alpine AS builder

ADD . /src

WORKDIR /src

RUN go build -o /usr/local/bin/waservice

FROM alpine:3.14

COPY --from=builder /usr/local/bin/waservice /usr/local/bin/waservice

CMD ["waservice"]
