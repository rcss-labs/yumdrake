# ElastiCache for Valkey, encrypted at rest and in transit, private.
#
# NOTE: with encryption in transit on, clients must connect with TLS.
# ecs.tf sets YUMDRAKE_VALKEY_TLS=true for both services, which makes the
# application connect over TLS (needs a version that reads it; see the README).

resource "aws_elasticache_subnet_group" "this" {
  name       = "${local.name}-cache"
  subnet_ids = var.private_subnet_ids
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${local.name}-cache"
  description          = "Yumdrake sessions and wake-ups"

  engine               = "valkey"
  engine_version       = var.cache_engine_version
  node_type            = var.cache_node_type
  parameter_group_name = var.cache_parameter_group_name
  port                 = 6379

  num_cache_clusters         = var.cache_node_count
  automatic_failover_enabled = var.cache_node_count > 1
  multi_az_enabled           = var.cache_node_count > 1

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = [aws_security_group.cache.id]

  at_rest_encryption_enabled = true
  transit_encryption_enabled = true

  snapshot_retention_limit = 1
  apply_immediately        = false
}
