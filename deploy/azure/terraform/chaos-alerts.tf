# Chaos-case alerts: the only Azure alerts, one per chaos case, so Slack carries just the case
# being shown and the Sherlocks bot investigates only that.
#
# They are AKS platform metric alerts rather than Log Analytics queries, so they keep firing
# when the workspace hits its daily cap and drops Container Insights data.

locals {
  # One entry per chaos case. Stateful, so each posts once when it fires and once when it resolves.
  chaos_alerts = {
    product_catalog_oom = {
      name        = "otel-demo - product-catalog OOMKilled"
      description = "Chaos case 1: product-catalog keeps running out of memory, so Kubernetes restarts it and it is not ready."
      severity    = 1
      metric      = "kube_pod_status_ready"
      aggregation = "Average"
      threshold   = 0.5
      dimensions = [
        { name = "namespace", operator = "Include", values = [var.alert_namespace] },
        { name = "pod", operator = "StartsWith", values = ["product-catalog-"] },
        { name = "condition", operator = "Include", values = ["false"] },
      ]
      unit = "not ready"
    }
  }
}

resource "azurerm_monitor_metric_alert" "chaos" {
  for_each = local.chaos_alerts

  name                = each.value.name
  description         = each.value.description
  resource_group_name = azurerm_resource_group.demo.name
  scopes              = [azurerm_kubernetes_cluster.aks.id]
  severity            = each.value.severity
  frequency           = "PT1M"
  window_size         = "PT5M"
  auto_mitigate       = true
  tags                = var.tags

  criteria {
    metric_namespace = "Microsoft.ContainerService/managedClusters"
    metric_name      = each.value.metric
    aggregation      = each.value.aggregation
    operator         = "GreaterThan"
    threshold        = each.value.threshold

    dynamic "dimension" {
      for_each = each.value.dimensions
      content {
        name     = dimension.value.name
        operator = dimension.value.operator
        values   = dimension.value.values
      }
    }
  }

  action {
    action_group_id = azurerm_monitor_action_group.slack.id
    webhook_properties = {
      unit          = each.value.unit
      metrics_url   = local.aks_metrics
      dashboard_url = local.container_insights
    }
  }
}
