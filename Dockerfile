# Backend image. Built and run by Render (see render.yaml); works on any
# container host that sets PORT.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tape ./cmd/tape

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/tape /tape
USER 1000:1000
# The demo keeps its log in /tmp, which the free tier wipes whenever the
# service sleeps: every wake starts with fresh books. One proxy (Render's)
# sits in front, so the client address is the right-most X-Forwarded-For.
ENV TAPE_DATA_DIR=/tmp/tape \
    TAPE_WAL_SYNC=interval \
    TAPE_PROXY_HOPS=1 \
    TAPE_SIM=true
EXPOSE 8081
ENTRYPOINT ["/tape"]
