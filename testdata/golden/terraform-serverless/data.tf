resource "aws_db_instance" "orders" {
  identifier     = "orders-primary"
  engine         = "postgres"
  engine_version = "16.3"
  instance_class = "db.t4g.medium"
  password       = var.db_password
  count          = 1
}

resource "aws_dynamodb_table" "sessions" {
  name     = "sessions"
  for_each = toset(["a", "b"])
}

resource "aws_elasticache_replication_group" "cache" {
  replication_group_id = "orders-cache"
}

data "aws_s3_bucket" "shared_assets" {
  bucket = "acme-shared-assets"
}
