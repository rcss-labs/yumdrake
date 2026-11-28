# ECS on Fargate: the orchestrator behind the load balancer, the runner with no
# inbound traffic and with scaling on queue depth.
#
# Comments marked "read from plan N onward" name settings that the application
# reads only from that plan of the implementation onward; earlier versions
# ignore them, so setting them is harmless, but the feature they switch on is
# not there until then.

resource "aws_ecs_cluster" "this" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_cloudwatch_log_group" "orchestrator" {
  name              = "/${local.name}/orchestrator"
  retention_in_days = var.cloudwatch_log_retention_days
}

resource "aws_cloudwatch_log_group" "runner" {
  name              = "/${local.name}/runner"
  retention_in_days = var.cloudwatch_log_retention_days
}

locals {
  # Settings both services read. Secrets are never plain values: the database
  # password comes from Secrets Manager (see secrets below).
  common_environment = concat(
    [
      { name = "AWS_REGION", value = var.region },
      { name = "YUMDRAKE_ENV", value = "prod" }, # read from plan 4 onward
      { name = "YUMDRAKE_LOG_LEVEL", value = var.log_level },
      { name = "YUMDRAKE_DATABASE_URL", value = local.database_url }, # no password: PGPASSWORD below
      { name = "YUMDRAKE_VALKEY_ADDR", value = local.valkey_addr },
      { name = "YUMDRAKE_VALKEY_TLS", value = "true" },                  # read from plan 11 (task 7) onward; the cache has encryption in transit
      { name = "YUMDRAKE_SECRETS_MODE", value = "kms" },                 # read from plan 5 onward
      { name = "YUMDRAKE_KMS_KEY_ID", value = aws_kms_key.secrets.arn }, # read from plan 5 onward
    ],
    var.aws_endpoint_url == "" ? [] : [
      { name = "YUMDRAKE_AWS_ENDPOINT_URL", value = var.aws_endpoint_url }, # read from plan 5 onward
    ],
  )

  # The database URL has no password. The PostgreSQL client library reads the
  # PGPASSWORD environment variable when the URL has none, and ECS fills it from
  # the "password" field of the secret that RDS manages. Check that your version
  # of the application's database driver does this (see the README).
  common_secrets = [
    { name = "PGPASSWORD", valueFrom = "${aws_db_instance.this.master_user_secret[0].secret_arn}:password::" },
  ]

  orchestrator_environment = concat(local.common_environment, [
    { name = "YUMDRAKE_PUBLIC_URL", value = local.public_url },
    { name = "YUMDRAKE_HTTP_ADDR", value = ":8080" },
    { name = "YUMDRAKE_LOG_BUCKET", value = aws_s3_bucket.log_archive.id }, # read from plan 5 onward
    { name = "YUMDRAKE_METRICS", value = var.metrics_mode },                # read from plan 11 (task 4) onward
    # The dimensions the alarms and scaling expect on every metric (Name=value,Name=value; empty for none).
    { name = "YUMDRAKE_METRICS_DIMENSIONS", value = join(",", [for k, v in var.metric_dimensions : "${k}=${v}"]) },
  ])

  runner_environment = concat(local.common_environment, [
    { name = "YUMDRAKE_RUNNER_CONCURRENCY", value = tostring(var.runner_concurrency) }, # read from plan 4 onward
    { name = "YUMDRAKE_ARTIFACT_BUCKET", value = aws_s3_bucket.config_copies.id },      # read from plan 5 onward
    { name = "YUMDRAKE_ARTIFACT_KMS_KEY_ID", value = aws_kms_key.config_copies.arn },   # read from plan 5 onward
  ])

  repository_credentials = var.registry_credentials_secret_arn == null ? {} : {
    repositoryCredentials = { credentialsParameter = var.registry_credentials_secret_arn }
  }
}

# ---------------------------------------------------------------- orchestrator

resource "aws_ecs_task_definition" "orchestrator" {
  family                   = "${local.name}-orchestrator"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.orchestrator_cpu
  memory                   = var.orchestrator_memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.orchestrator_task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }

  container_definitions = jsonencode([
    merge({
      name      = "orchestrator"
      image     = local.orchestrator_image
      essential = true

      portMappings           = [{ containerPort = 8080, protocol = "tcp" }]
      environment            = local.orchestrator_environment
      secrets                = local.common_secrets
      readonlyRootFilesystem = true
      stopTimeout            = 30

      # The image has no shell, so the check is the binary's own subcommand.
      healthCheck = {
        command     = ["CMD", "/yumdrake-orchestrator", "healthcheck"]
        interval    = 15
        timeout     = 5
        retries     = 3
        startPeriod = 30
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.orchestrator.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "orchestrator"
        }
      }
    }, local.repository_credentials)
  ])
}

resource "aws_ecs_service" "orchestrator" {
  name            = "${local.name}-orchestrator"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.orchestrator.arn
  desired_count   = var.orchestrator_desired_count
  launch_type     = "FARGATE"

  health_check_grace_period_seconds  = 90
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  propagate_tags                     = "SERVICE"

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [aws_security_group.orchestrator.id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.orchestrator.arn
    container_name   = "orchestrator"
    container_port   = 8080
  }

  depends_on = [aws_lb_listener.https]
}

# ---------------------------------------------------------------- runner

resource "aws_ecs_task_definition" "runner" {
  family                   = "${local.name}-runner"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.runner_cpu
  memory                   = var.runner_memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.runner_task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }

  container_definitions = jsonencode([
    merge({
      name      = "runner"
      image     = local.runner_image
      essential = true

      # No port mappings: the runner takes no inbound traffic. Its own health
      # endpoint listens on loopback inside the container only.
      environment            = local.runner_environment
      secrets                = local.common_secrets
      readonlyRootFilesystem = true
      stopTimeout            = 30

      healthCheck = {
        command     = ["CMD", "/yumdrake-runner", "healthcheck"]
        interval    = 15
        timeout     = 5
        retries     = 3
        startPeriod = 30
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.runner.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "runner"
        }
      }
    }, local.repository_credentials)
  ])
}

resource "aws_ecs_service" "runner" {
  name            = "${local.name}-runner"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.runner.arn
  desired_count   = var.runner_min_count
  launch_type     = "FARGATE"

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  propagate_tags                     = "SERVICE"

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [aws_security_group.runner.id]
    assign_public_ip = false
  }

  # Scaling owns the count after creation.
  lifecycle {
    ignore_changes = [desired_count]
  }
}

resource "aws_appautoscaling_target" "runner" {
  service_namespace  = "ecs"
  scalable_dimension = "ecs:service:DesiredCount"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.runner.name}"
  min_capacity       = var.runner_min_count
  max_capacity       = var.runner_max_count
}

# Add runners while the queue is deeper than the target, remove them when it is
# shallower. QueueDepth is emitted by the orchestrator (see metrics_mode).
resource "aws_appautoscaling_policy" "runner_queue_depth" {
  name               = "${local.name}-runner-queue-depth"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.runner.service_namespace
  scalable_dimension = aws_appautoscaling_target.runner.scalable_dimension
  resource_id        = aws_appautoscaling_target.runner.resource_id

  target_tracking_scaling_policy_configuration {
    target_value       = var.runner_queue_depth_target
    scale_out_cooldown = 60
    scale_in_cooldown  = 300

    customized_metric_specification {
      namespace   = "Yumdrake"
      metric_name = "QueueDepth"
      statistic   = "Average"

      dynamic "dimensions" {
        for_each = var.metric_dimensions
        content {
          name  = dimensions.key
          value = dimensions.value
        }
      }
    }
  }
}
