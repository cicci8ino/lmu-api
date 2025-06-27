FROM golang:1.24 AS build
WORKDIR /src

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /out/server .

FROM gcr.io/distroless/static-debian12 AS server
COPY --from=build /out/server /server
ENTRYPOINT ["/server"]