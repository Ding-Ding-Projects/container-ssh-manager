FROM node:24.19.0-alpine AS frontend
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.26.6-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/web/dist ./web/dist
ARG VERSION=development
ARG UPDATED_AT=unavailable
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.updatedAt=${UPDATED_AT}" -o /manager ./cmd/manager

FROM docker:29.2.1-cli
RUN apk add --no-cache ca-certificates tzdata
COPY --from=backend /manager /usr/local/bin/manager
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["manager","healthcheck"]
ENTRYPOINT ["manager"]
