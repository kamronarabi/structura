terraform {
  required_version = ">= 1.6"
}

provider "aws" {
  region = var.region
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "environment" {
  type = string
}

variable "db_password" {
  type      = string
  sensitive = true
}

locals {
  name_prefix = "orders-${var.region}"
  queue_name  = "${local.name_prefix}-jobs"
}

resource "aws_sqs_queue" "jobs" {
  name                       = local.queue_name
  visibility_timeout_seconds = 300
}

resource "aws_sqs_queue" "jobs_dlq" {
  name = "${local.queue_name}-dlq"
}

resource "aws_s3_bucket" "uploads" {
  bucket = "${local.name_prefix}-uploads"
}

resource "aws_lambda_function" "api" {
  function_name = "${local.name_prefix}-api"
  runtime       = "provided.al2023"
  handler       = "bootstrap"

  environment {
    variables = {
      QUEUE_URL    = aws_sqs_queue.jobs.url
      BUCKET_NAME  = aws_s3_bucket.uploads.id
      DATABASE_URL = "postgres://app@${aws_db_instance.orders.address}:5432/orders"
      TABLE_NAME   = aws_dynamodb_table.sessions.name
      SEARCH_HOST  = var.search_endpoint
    }
  }
}

resource "aws_lambda_function" "worker" {
  function_name = "${local.name_prefix}-worker"
  runtime       = "provided.al2023"
  handler       = "bootstrap"

  environment {
    variables = {
      QUEUE_URL = aws_sqs_queue.jobs.url
      DLQ_URL   = aws_sqs_queue.jobs_dlq.url
    }
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${local.name_prefix}-lambda"
  assume_role_policy = "{}"
}

resource "aws_iam_role_policy_attachment" "lambda_basic" {
  role       = aws_iam_role.lambda.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_security_group" "db" {
  name   = "${local.name_prefix}-db"
  vpc_id = module.network.vpc_id
}

module "network" {
  source = "terraform-aws-modules/vpc/aws"
  name   = "${local.name_prefix}-vpc"
}

resource "aws_cognito_user_pool" "users" {
  name = "${local.name_prefix}-users"
}
