package terraform

import (
	"strings"

	"github.com/kamronarabi/structura/pkg/schema"
)

// Mapping a Terraform resource type to a node kind is what turns a flat list
// of cloud resources into something shaped like an architecture. The table is
// deliberately finite.
//
// Real Terraform is dominated by plumbing: IAM roles and policy attachments,
// security groups, route tables, subnets, log groups. A large module can
// declare a hundred of them for six things a reader would draw on a diagram.
// Emitting a node for each would make the graph both unreadable and too large
// to hand to a model, which is the opposite of the point. So recognized types
// become nodes, known plumbing is skipped silently, and anything else is
// reported in aggregate so the omission is visible rather than hidden.

type resourceClass struct {
	kind  schema.NodeKind
	tech  string
	layer schema.Layer
}

var resourceTypes = map[string]resourceClass{
	// Compute
	"aws_lambda_function":            {schema.KindService, "lambda", schema.LayerContainer},
	"aws_ecs_service":                {schema.KindService, "ecs", schema.LayerContainer},
	"aws_ecs_task_definition":        {schema.KindService, "ecs", schema.LayerContainer},
	"aws_instance":                   {schema.KindService, "ec2", schema.LayerContainer},
	"aws_batch_job_definition":       {schema.KindService, "batch", schema.LayerContainer},
	"aws_sfn_state_machine":          {schema.KindService, "step-functions", schema.LayerContainer},
	"aws_apprunner_service":          {schema.KindService, "app-runner", schema.LayerContainer},
	"aws_cognito_user_pool":          {schema.KindService, "cognito", schema.LayerContainer},
	"aws_ecr_repository":             {schema.KindCloudResource, "ecr", schema.LayerContainer},
	"aws_secretsmanager_secret":      {schema.KindCloudResource, "secrets-manager", schema.LayerContainer},
	"google_cloud_run_service":       {schema.KindService, "cloud-run", schema.LayerContainer},
	"google_cloudfunctions_function": {schema.KindService, "cloud-functions", schema.LayerContainer},
	"azurerm_linux_function_app":     {schema.KindService, "azure-functions", schema.LayerContainer},
	"azurerm_container_app":          {schema.KindService, "container-apps", schema.LayerContainer},

	// Relational and document stores
	"aws_db_instance":                    {schema.KindDatastore, "rds", schema.LayerContainer},
	"aws_rds_cluster":                    {schema.KindDatastore, "aurora", schema.LayerContainer},
	"aws_rds_cluster_instance":           {schema.KindDatastore, "aurora", schema.LayerContainer},
	"aws_dynamodb_table":                 {schema.KindDatastore, "dynamodb", schema.LayerContainer},
	"aws_documentdb_cluster":             {schema.KindDatastore, "documentdb", schema.LayerContainer},
	"aws_redshift_cluster":               {schema.KindDatastore, "redshift", schema.LayerContainer},
	"aws_neptune_cluster":                {schema.KindDatastore, "neptune", schema.LayerContainer},
	"aws_timestreamwrite_table":          {schema.KindDatastore, "timestream", schema.LayerContainer},
	"google_sql_database_instance":       {schema.KindDatastore, "cloud-sql", schema.LayerContainer},
	"google_bigtable_instance":           {schema.KindDatastore, "bigtable", schema.LayerContainer},
	"google_firestore_database":          {schema.KindDatastore, "firestore", schema.LayerContainer},
	"azurerm_postgresql_flexible_server": {schema.KindDatastore, "postgres", schema.LayerContainer},
	"azurerm_cosmosdb_account":           {schema.KindDatastore, "cosmosdb", schema.LayerContainer},

	// Caches and search
	"aws_elasticache_cluster":           {schema.KindDatastore, "elasticache", schema.LayerContainer},
	"aws_elasticache_replication_group": {schema.KindDatastore, "elasticache", schema.LayerContainer},
	"aws_elasticache_serverless_cache":  {schema.KindDatastore, "elasticache", schema.LayerContainer},
	"aws_opensearch_domain":             {schema.KindDatastore, "opensearch", schema.LayerContainer},
	"aws_elasticsearch_domain":          {schema.KindDatastore, "elasticsearch", schema.LayerContainer},

	// Object and file storage
	"aws_s3_bucket":           {schema.KindDatastore, "s3", schema.LayerContainer},
	"aws_efs_file_system":     {schema.KindDatastore, "efs", schema.LayerContainer},
	"google_storage_bucket":   {schema.KindDatastore, "gcs", schema.LayerContainer},
	"azurerm_storage_account": {schema.KindDatastore, "azure-storage", schema.LayerContainer},

	// Messaging
	"aws_sqs_queue":                        {schema.KindQueue, "sqs", schema.LayerContainer},
	"aws_sns_topic":                        {schema.KindQueue, "sns", schema.LayerContainer},
	"aws_kinesis_stream":                   {schema.KindQueue, "kinesis", schema.LayerContainer},
	"aws_kinesis_firehose_delivery_stream": {schema.KindQueue, "firehose", schema.LayerContainer},
	"aws_msk_cluster":                      {schema.KindQueue, "msk", schema.LayerContainer},
	"aws_cloudwatch_event_bus":             {schema.KindQueue, "eventbridge", schema.LayerContainer},
	"aws_cloudwatch_event_rule":            {schema.KindQueue, "eventbridge", schema.LayerContainer},
	"aws_mq_broker":                        {schema.KindQueue, "amazon-mq", schema.LayerContainer},
	"google_pubsub_topic":                  {schema.KindQueue, "pubsub", schema.LayerContainer},
	"google_pubsub_subscription":           {schema.KindQueue, "pubsub", schema.LayerContainer},
	"azurerm_servicebus_queue":             {schema.KindQueue, "service-bus", schema.LayerContainer},
	"azurerm_eventhub":                     {schema.KindQueue, "event-hubs", schema.LayerContainer},

	// Edge and routing. These matter beyond the diagram: they are where rate
	// limiting and TLS termination live, which the Phase 3 profiler looks for.
	"aws_lb":                        {schema.KindCloudResource, "alb", schema.LayerContainer},
	"aws_alb":                       {schema.KindCloudResource, "alb", schema.LayerContainer},
	"aws_elb":                       {schema.KindCloudResource, "elb", schema.LayerContainer},
	"aws_api_gateway_rest_api":      {schema.KindCloudResource, "api-gateway", schema.LayerContainer},
	"aws_apigatewayv2_api":          {schema.KindCloudResource, "api-gateway", schema.LayerContainer},
	"aws_cloudfront_distribution":   {schema.KindCloudResource, "cloudfront", schema.LayerContainer},
	"aws_route53_record":            {schema.KindCloudResource, "route53", schema.LayerContainer},
	"aws_appsync_graphql_api":       {schema.KindCloudResource, "appsync", schema.LayerContainer},
	"google_compute_global_address": {schema.KindCloudResource, "gclb", schema.LayerContainer},
	"azurerm_application_gateway":   {schema.KindCloudResource, "app-gateway", schema.LayerContainer},

	// Boundaries: things that contain other things.
	"aws_eks_cluster":            {schema.KindBoundary, "eks", schema.LayerContext},
	"aws_ecs_cluster":            {schema.KindBoundary, "ecs", schema.LayerContext},
	"aws_vpc":                    {schema.KindBoundary, "vpc", schema.LayerContext},
	"google_container_cluster":   {schema.KindBoundary, "gke", schema.LayerContext},
	"azurerm_kubernetes_cluster": {schema.KindBoundary, "aks", schema.LayerContext},
	"azurerm_resource_group":     {schema.KindBoundary, "resource-group", schema.LayerContext},
}

// plumbingPrefixes are resource types that exist to make the interesting ones
// work. They are skipped without a diagnostic, because reporting them would
// be reporting almost the entire file.
var plumbingPrefixes = []string{
	"aws_iam_", "aws_security_group", "aws_subnet", "aws_route", "aws_network_acl",
	"aws_vpc_endpoint", "aws_internet_gateway", "aws_nat_gateway", "aws_eip",
	"aws_cloudwatch_log_", "aws_cloudwatch_metric_", "aws_kms_", "aws_acm_",
	"aws_lambda_permission", "aws_lambda_layer", "aws_s3_bucket_policy",
	"aws_s3_bucket_public_access_block", "aws_s3_bucket_versioning",
	"aws_s3_bucket_server_side_encryption", "aws_s3_bucket_lifecycle",
	"aws_s3_bucket_ownership", "aws_s3_bucket_acl", "aws_s3_bucket_cors",
	"aws_db_subnet_group", "aws_db_parameter_group", "aws_db_option_group",
	"aws_elasticache_subnet_group", "aws_elasticache_parameter_group",
	"aws_autoscaling_", "aws_appautoscaling_", "aws_ssm_parameter",
	"aws_secretsmanager_secret_version", "aws_sns_topic_subscription",
	"aws_sqs_queue_policy", "aws_lb_listener", "aws_lb_target_group_attachment",
	"aws_api_gateway_method", "aws_api_gateway_integration", "aws_api_gateway_resource",
	"aws_api_gateway_deployment", "aws_apigatewayv2_route", "aws_apigatewayv2_integration",
	"aws_ecr_lifecycle_policy", "aws_default_", "aws_flow_log",
	"google_project_iam", "google_service_account", "google_compute_firewall",
	"google_compute_subnetwork", "google_compute_route",
	"azurerm_role_assignment", "azurerm_subnet", "azurerm_network_security",
	"random_", "null_resource", "time_", "tls_private_key", "local_file",
	"terraform_data", "archive_file",
}

// classifyResource reports how a resource type should appear in the graph.
func classifyResource(resourceType string) (class resourceClass, recognized, plumbing bool) {
	if c, ok := resourceTypes[resourceType]; ok {
		return c, true, false
	}
	for _, prefix := range plumbingPrefixes {
		if strings.HasPrefix(resourceType, prefix) {
			return resourceClass{}, false, true
		}
	}
	return resourceClass{}, false, false
}

// provider extracts the provider name from a resource type, which is the
// portion before the first underscore.
func provider(resourceType string) string {
	if i := strings.Index(resourceType, "_"); i > 0 {
		return resourceType[:i]
	}
	return resourceType
}
