# Chaos case 1 (product-catalog runs out of memory): the alerts that stay on while the general
# alerts in alerts.tf are off. They fire in the order the outage unfolds, memory first, so the
# first one gives a head start before customers see errors.

locals {
  product_catalog = { name = "k8s.deployment.name", values = ["product-catalog"] }
}

# A1: product-catalog's memory is climbing towards its limit.
resource "swo_alert" "catalog_memory_high" {
  name                  = "otel-demo: Catalog memory high"
  enabled               = true
  description           = "product-catalog is using more than 85% of its memory limit."
  severity              = "WARNING"
  trigger_reset_actions = true
  notification_actions  = local.notify_slack

  conditions = [
    {
      metric_name         = "k8s.container.memory_limit_utilization"
      aggregation_type    = "MAX"
      threshold           = ">=0.85"
      duration            = "2m"
      group_by_metric_tag = ["k8s.pod.name"]
      include_tags        = [local.this_cluster, local.demo_namespace, local.product_catalog]
    },
  ]
}

# A2: product-catalog ran out of memory and Kubernetes killed it.
resource "swo_alert" "catalog_oom_killed" {
  name                  = "otel-demo: Catalog OOMKilled"
  enabled               = true
  description           = "product-catalog was restarted because it ran out of memory (OOMKilled)."
  severity              = "CRITICAL"
  trigger_reset_actions = true
  notification_actions  = local.notify_slack

  conditions = [
    {
      metric_name         = "k8s.container.restarts"
      aggregation_type    = "MAX"
      threshold           = ">=1"
      duration            = "1m"
      group_by_metric_tag = ["k8s.pod.name"]
      include_tags = [
        local.this_cluster,
        local.demo_namespace,
        local.product_catalog,
        { name = "k8s.container.status.last_terminated_reason", values = ["OOMKilled"] },
      ]
    },
  ]
}

# A3: the storefront is failing because it cannot list products. The frontend's healthy
# error rate is 7–9%, so the threshold sits well above it.
resource "swo_alert" "product_pages_failing" {
  name                  = "otel-demo: Product pages failing"
  enabled               = true
  description           = "More than 20% of frontend HTTP requests are failing."
  severity              = "CRITICAL"
  trigger_reset_actions = true
  notification_actions  = local.notify_slack

  conditions = [
    {
      metric_name         = "composite.http.server.request.duration.error_rate"
      aggregation_type    = "AVG"
      threshold           = ">20"
      duration            = "2m"
      group_by_metric_tag = ["service.name"]
      include_tags        = [{ name = "service.name", values = ["frontend"] }]
    },
  ]
}
