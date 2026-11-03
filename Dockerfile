# Built by WeChat Cloud Hosting from this directory; listens on port 80.
FROM golang:1.27-alpine AS build
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN go build -trimpath -ldflags="-s -w" -o /petsit .

FROM alpine:3.21
# ffmpeg pulls frames out of videos for the content check.
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=build /petsit /petsit
EXPOSE 80
CMD ["/petsit"]
