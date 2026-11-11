FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bin/reports-api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /bin/reports-api /bin/reports-api
ENTRYPOINT ["/bin/reports-api"]
