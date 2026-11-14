# Backend image for a Hugging Face Docker Space (or any container host).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/tape ./cmd/tape

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/tape /tape
# Spaces run containers as uid 1000 and route traffic to port 7860. The
# demo keeps its log in /tmp: it survives restarts of the process, not of
# the container, which is all a demo needs.
USER 1000:1000
ENV PORT=7860 \
    TAPE_DATA_DIR=/tmp/tape \
    TAPE_WAL_SYNC=interval \
    TAPE_TRUST_PROXY=true \
    TAPE_SIM=true
EXPOSE 7860
ENTRYPOINT ["/tape"]
