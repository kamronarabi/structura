Fixture 1: a Compose monolith with Postgres, Redis, RabbitMQ, and Elasticsearch.

Pins: image-based node classification, depends_on edges, environment-variable
hints (connection strings, bare hostnames, external URLs), credential
redaction, and the exclusion of non-reference variables such as LOG_LEVEL and
APP_ENV.
