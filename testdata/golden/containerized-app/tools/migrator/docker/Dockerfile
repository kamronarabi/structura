# "docker" names the directory's role, not a component. The name has to come
# from the directory above it, or the diagram grows a box labelled "docker".
FROM golang:1.22-alpine
RUN go install github.com/pressly/goose/v3/cmd/goose@latest
ENV DATABASE_URL=postgres://orders-db:5432/orders
ENTRYPOINT ["goose"]
