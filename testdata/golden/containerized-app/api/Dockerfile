# The shape that hides the language from anything reading only the final
# stage: the toolchain is in the builder and the runtime is empty.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/api ./cmd/api

FROM scratch
COPY --from=build /out/api /api
ENV DATABASE_URL=postgres://orders-db:5432/orders
ENV LOG_LEVEL=info
EXPOSE 8080
ENTRYPOINT ["/api"]
