# Chaos-case alerts: the only Azure alerts, one per chaos case, so Slack carries just the case
# being shown and the Sherlocks bot investigates only that.

locals {
  demo_pods = "KubePodInventory | where Namespace == '${var.alert_namespace}'"

  # One entry per chaos case. No dimension split, so each case raises one alert whichever pod
  # it hits; stateful, so it posts once when it fires and once when it resolves.
  chaos_alerts = {
    product_catalog_oom = {
      name        = "otel-demo - product-catalog OOMKilled"
      description = "Chaos case 1: product-catalog's last restart was an OOMKill. It ran out of memory and Kubernetes restarted it."
      severity    = 1
      query       = "${local.demo_pods} | where Name startswith 'product-catalog-' | where tostring(parse_json(ContainerLastStatus).reason) == 'OOMKilled'"
      unit        = "records"
      dashboard   = local.container_insights
    }
  }
}

resource "azurerm_monitor_scheduled_query_rules_alert_v2" "chaos" {
  for_each = local.chaos_alerts

  name                    = each.value.name
  description             = each.value.description
  resource_group_name     = azurerm_resource_group.demo.name
  location                = azurerm_resource_group.demo.location
  scopes                  = [azurerm_log_analytics_workspace.logs.id]
  severity                = each.value.severity
  evaluation_frequency    = "PT5M"
  window_duration         = "PT5M"
  auto_mitigation_enabled = true
  tags                    = var.tags

  criteria {
    query                   = each.value.query
    time_aggregation_method = "Count"
    operator                = "GreaterThan"
    threshold               = 0

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.slack.id]
    custom_properties = {
      unit          = each.value.unit
      dashboard_url = each.value.dashboard
    }
  }
}
