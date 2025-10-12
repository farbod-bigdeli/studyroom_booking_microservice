FROM golang:1.22-alpine AS build
WORKDIR /src
# bring only this service’s code
COPY . .
# tidy (if needed) and build
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -o /bin/app

FROM alpine:3.19
COPY --from=build /bin/app /app
EXPOSE 8080 9002
ENTRYPOINT ["/app"]
