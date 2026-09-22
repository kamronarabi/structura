# A Dockerfile starting FROM postgres is shipping a Postgres.
FROM postgres:16-alpine
COPY schema.sql /docker-entrypoint-initdb.d/01-schema.sql
EXPOSE 5432
